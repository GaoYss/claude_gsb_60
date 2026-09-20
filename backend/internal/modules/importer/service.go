package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"time"

	"gorm.io/gorm"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
	"streetlight/pkg/pagination"
)

// batchSortSpec 定义导入批次列表允许的排序字段。
var batchSortSpec = pagination.SortSpec{
	Allowed: map[string]string{
		"batch_no":   "batch_no",
		"file_name":  "file_name",
		"total_rows": "total_rows",
		"total_cost": "total_cost",
		"created_at": "created_at",
	},
	Default: "id",
}

// ValidationError 表示台账文件校验未通过, 携带逐行可读原因, 整批不会写入。
type ValidationError struct {
	Failure *ImportFailure
}

// Error 实现 error 接口。
func (e *ValidationError) Error() string {
	return fmt.Sprintf("台账文件校验未通过: 共 %d 行数据, 其中 %d 行存在问题", e.Failure.TotalRows, e.Failure.ErrorRows)
}

// Service 承载台账迁移导入的编排逻辑: 解析 -> 校验 -> 单事务写入 -> 核对 -> 联动路灯状态。
type Service struct {
	db       *gorm.DB
	batches  *Repository
	lamps    *lamp.Repository
	faults   *fault.Repository
	faultSvc *fault.Service
	repairs  *repair.Repository
}

// NewService 构造台账迁移服务。
func NewService(
	db *gorm.DB,
	batches *Repository,
	lamps *lamp.Repository,
	faults *fault.Repository,
	faultSvc *fault.Service,
	repairs *repair.Repository,
) *Service {
	return &Service{db: db, batches: batches, lamps: lamps, faults: faults, faultSvc: faultSvc, repairs: repairs}
}

// ListBatches 分页查询导入批次。
func (s *Service) ListBatches(ctx context.Context, query BatchListQuery) ([]ImportBatch, int64, pagination.Query, error) {
	page := pagination.Parse(query.Params, batchSortSpec)
	items, total, err := s.batches.List(ctx, query.Keyword, page)
	if err != nil {
		return nil, 0, page, err
	}
	return items, total, page, nil
}

// GetBatch 查询导入批次详情。
func (s *Service) GetBatch(ctx context.Context, id uint) (*ImportBatch, error) {
	return s.batches.GetByID(ctx, id)
}

// Import 执行台账批量导入。
// 同一份文件(内容哈希相同)重复导入时直接跳过并返回已有批次;
// 任何一行校验失败都返回 ValidationError 且整批不写库;
// 写入在单个事务内完成, 提交前按条数与金额合计核对, 不一致即回滚。
func (s *Service) Import(ctx context.Context, fileName string, content []byte) (*ImportResult, error) {
	hash := sha256.Sum256(content)
	fileHash := hex.EncodeToString(hash[:])

	// 幂等: 同一份文件只允许导入一次。
	existing, err := s.batches.GetByHash(ctx, fileHash)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &ImportResult{Duplicated: true, Batch: existing}, nil
	}

	rows, err := parseFile(content)
	if err != nil {
		return nil, err
	}

	validRows, err := s.validate(ctx, rows)
	if err != nil {
		return nil, err
	}

	batch, openLampIDs, duplicated, err := s.writeBatch(ctx, fileName, fileHash, validRows)
	if err != nil {
		return nil, err
	}
	if duplicated {
		// 与并发导入撞了同一文件: 对方已提交, 本次直接跳过。
		return &ImportResult{Duplicated: true, Batch: batch}, nil
	}

	// 提交后联动路灯运行状态, 使导入的未闭环故障计入亮灯率基数(与故障登记保持一致的重算逻辑)。
	for _, lampID := range openLampIDs {
		if err := s.faultSvc.SyncLampRunStatus(ctx, lampID); err != nil {
			slog.Warn("导入后同步路灯运行状态失败", "lamp_id", lampID, "batch_no", batch.BatchNo, "error", err)
		}
	}

	slog.Info("台账迁移导入完成",
		"batch_no", batch.BatchNo,
		"file", fileName,
		"faults", batch.FaultCount,
		"open_faults", batch.OpenFaultCount,
		"repairs", batch.RepairCount,
		"total_cost", batch.TotalCost,
	)
	return &ImportResult{Batch: batch}, nil
}

// validate 加载台账与存量未闭环故障, 对全部数据行做逐行校验。
func (s *Service) validate(ctx context.Context, rows []rawRow) ([]validRow, error) {
	codes := make([]string, 0, len(rows))
	seen := make(map[string]bool)
	for _, row := range rows {
		code := cell(row.Cells, colLampCode)
		if code != "" && !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}

	devices, err := s.lamps.ListByCodes(ctx, codes)
	if err != nil {
		return nil, err
	}
	lampsByCode := make(map[string]*lamp.Lamp, len(devices))
	lampIDs := make([]uint, 0, len(devices))
	for index := range devices {
		device := devices[index]
		lampsByCode[device.Code] = &device
		lampIDs = append(lampIDs, device.ID)
	}

	openFaults, err := s.faults.ListOpenByLampIDs(ctx, lampIDs)
	if err != nil {
		return nil, err
	}
	openByLamp := make(map[uint]*fault.Fault, len(openFaults))
	for index := range openFaults {
		item := openFaults[index]
		if _, ok := openByLamp[item.LampID]; !ok {
			openByLamp[item.LampID] = &item
		}
	}

	validRows, rowErrors := validateRows(rows, lampsByCode, openByLamp, time.Now())
	if len(rowErrors) > 0 {
		return nil, &ValidationError{Failure: &ImportFailure{
			TotalRows: len(rows),
			ErrorRows: len(rowErrors),
			RowErrors: rowErrors,
		}}
	}
	return validRows, nil
}

// writeBatch 在单个事务内写入批次、故障与维修记录, 提交前按条数与金额合计核对。
// 任何一步失败都会回滚, 保证同一批不会只写进去一半。
// duplicated 为 true 时表示并发下同一文件已被其他请求导入, 返回已存在的批次。
func (s *Service) writeBatch(ctx context.Context, fileName, fileHash string, rows []validRow) (*ImportBatch, []uint, bool, error) {
	now := time.Now()
	batch := &ImportBatch{
		FileName:  fileName,
		FileHash:  fileHash,
		TotalRows: len(rows),
	}
	for _, row := range rows {
		if row.closed {
			batch.RepairCount++
			batch.TotalCost += row.cost
		} else {
			batch.OpenFaultCount++
		}
	}
	batch.FaultCount = len(rows)

	openLampIDs := make([]uint, 0, batch.OpenFaultCount)
	faultSeqs := make(map[string]int)
	repairSeqs := make(map[string]int)

	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, nil, false, apperr.Internal("开启导入事务失败: %v", tx.Error)
	}
	rollback := func(err error) (*ImportBatch, []uint, bool, error) {
		_ = tx.Rollback()
		return nil, nil, false, err
	}

	batches := s.batches.WithTx(tx)
	faults := s.faults.WithTx(tx)
	repairs := s.repairs.WithTx(tx)

	batchPrefix := "DR" + now.Format("20060102")
	sequence, err := batches.NextBatchSequence(ctx, batchPrefix)
	if err != nil {
		return rollback(err)
	}
	batch.BatchNo = fmt.Sprintf("%s%04d", batchPrefix, sequence)
	if err := batches.Create(ctx, batch); err != nil {
		if isUniqueViolation(err) {
			// 并发导入同一文件: 回滚后按重复导入处理。
			_ = tx.Rollback()
			existing, findErr := s.batches.GetByHash(ctx, fileHash)
			if findErr != nil {
				return nil, nil, false, findErr
			}
			if existing != nil {
				return existing, nil, true, nil
			}
			return nil, nil, false, apperr.Conflict("导入批次号生成冲突, 请重新上传")
		}
		return rollback(err)
	}

	for _, row := range rows {
		faultEntity := buildFault(row, batch.ID)
		if err := assignFaultNo(ctx, faults, faultSeqs, faultEntity); err != nil {
			return rollback(err)
		}
		if err := faults.Create(ctx, faultEntity); err != nil {
			return rollback(conflictOnUnique(err, "故障单号生成冲突, 请重新上传"))
		}

		if !row.closed {
			openLampIDs = append(openLampIDs, row.lamp.ID)
			continue
		}

		repairEntity := buildRepair(row, faultEntity, batch.ID)
		if err := assignRepairNo(ctx, repairs, repairSeqs, repairEntity); err != nil {
			return rollback(err)
		}
		if err := repairs.Create(ctx, repairEntity); err != nil {
			return rollback(conflictOnUnique(err, "维修单号生成冲突, 请重新上传"))
		}
		if err := faults.UpdateColumns(ctx, faultEntity.ID, map[string]any{
			"repair_count":     1,
			"latest_repair_id": repairEntity.ID,
		}); err != nil {
			return rollback(err)
		}
	}

	// 提交前核对: 实际落库的条数与金额合计必须与解析结果一致, 不一致即回滚, 保证没有漏项。
	faultCount, err := batches.CountFaultsByBatch(ctx, batch.ID)
	if err != nil {
		return rollback(err)
	}
	if faultCount != int64(batch.FaultCount) {
		return rollback(apperr.Internal("导入核对失败: 故障条数不一致(预期 %d, 实际 %d), 本批已回滚", batch.FaultCount, faultCount))
	}
	repairCount, repairCost, err := batches.ReconcileRepairsByBatch(ctx, batch.ID)
	if err != nil {
		return rollback(err)
	}
	if repairCount != int64(batch.RepairCount) {
		return rollback(apperr.Internal("导入核对失败: 维修条数不一致(预期 %d, 实际 %d), 本批已回滚", batch.RepairCount, repairCount))
	}
	if math.Abs(repairCost-batch.TotalCost) > 0.005 {
		return rollback(apperr.Internal("导入核对失败: 维修费用合计不一致(预期 %.2f, 实际 %.2f), 本批已回滚", batch.TotalCost, repairCost))
	}

	if err := tx.Commit().Error; err != nil {
		return nil, nil, false, apperr.Internal("提交导入事务失败: %v", err)
	}
	return batch, openLampIDs, false, nil
}

// buildFault 将校验通过的行转换为故障记录。
func buildFault(row validRow, batchID uint) *fault.Fault {
	entity := &fault.Fault{
		LampID:        row.lamp.ID,
		LampCode:      row.lamp.Code,
		RoadName:      row.lamp.RoadName,
		FaultType:     row.faultType,
		FaultLevel:    row.level,
		Source:        fault.SourceOther,
		Description:   row.description,
		Reporter:      row.reporter,
		ReportedAt:    row.reportedAt,
		ImportBatchID: &batchID,
	}
	if row.closed {
		closedAt := row.closedAt
		entity.Status = fault.StatusClosed
		entity.ClosedAt = &closedAt
		entity.CloseRemark = "历史台账导入闭环"
	} else {
		entity.Status = fault.StatusPending
	}
	return entity
}

// buildRepair 将已闭环行转换为一条已完成的维修记录。
func buildRepair(row validRow, faultEntity *fault.Fault, batchID uint) *repair.Repair {
	finishedAt := row.closedAt
	return &repair.Repair{
		FaultID:       faultEntity.ID,
		FaultNo:       faultEntity.FaultNo,
		LampID:        faultEntity.LampID,
		LampCode:      faultEntity.LampCode,
		Repairman:     row.repairman,
		RepairTeam:    row.repairTeam,
		StartedAt:     row.reportedAt,
		FinishedAt:    &finishedAt,
		Status:        repair.StatusFinished,
		Result:        repair.ResultFixed,
		Content:       row.content,
		Cost:          row.cost,
		Remark:        row.remark,
		ImportBatchID: &batchID,
	}
}

// assignFaultNo 为故障分配单号: 同一日期前缀在批内先取库中流水再本地递增, 避免逐行查询。
func assignFaultNo(ctx context.Context, faults *fault.Repository, seqs map[string]int, entity *fault.Fault) error {
	prefix := "GD" + entity.ReportedAt.Format("20060102")
	sequence, err := nextSequence(seqs, prefix, func() (int, error) {
		return faults.NextSequence(ctx, prefix)
	})
	if err != nil {
		return err
	}
	entity.FaultNo = fmt.Sprintf("%s%04d", prefix, sequence)
	return nil
}

// assignRepairNo 为维修记录分配单号, 规则与 assignFaultNo 相同。
func assignRepairNo(ctx context.Context, repairs *repair.Repository, seqs map[string]int, entity *repair.Repair) error {
	prefix := "WX" + entity.StartedAt.Format("20060102")
	sequence, err := nextSequence(seqs, prefix, func() (int, error) {
		return repairs.NextSequence(ctx, prefix)
	})
	if err != nil {
		return err
	}
	entity.RepairNo = fmt.Sprintf("%s%04d", prefix, sequence)
	return nil
}

// nextSequence 按前缀返回下一个可用流水号, 首次从数据库读取, 之后在批内本地递增。
func nextSequence(seqs map[string]int, prefix string, load func() (int, error)) (int, error) {
	if value, ok := seqs[prefix]; ok {
		seqs[prefix] = value + 1
		return value + 1, nil
	}
	value, err := load()
	if err != nil {
		return 0, err
	}
	seqs[prefix] = value
	return value, nil
}

// conflictOnUnique 将单号唯一冲突转换为可重试的冲突提示, 其余错误原样返回。
func conflictOnUnique(err error, message string) error {
	if isUniqueViolation(err) {
		return apperr.Conflict("%s", message)
	}
	return err
}
