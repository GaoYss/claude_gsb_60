package legacyimport

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
	"streetlight/pkg/pagination"
)

// Service 承载旧系统故障/维修台账批量导入的业务规则。
type Service struct {
	db        *gorm.DB
	repo      *Repository
	lampRepo  *lamp.Repository
	faultRepo *fault.Repository
}

// NewService 构造历史台账导入服务。
func NewService(db *gorm.DB, repo *Repository, lampRepo *lamp.Repository, faultRepo *fault.Repository) *Service {
	return &Service{db: db, repo: repo, lampRepo: lampRepo, faultRepo: faultRepo}
}

// Preview 解析并逐行校验文件, 但不写库。返回内容指纹供确认导入时核对。
func (s *Service) Preview(ctx context.Context, data []byte, fileName string) (*PreviewResult, error) {
	parsed, err := parseWorkbook(data, fileName)
	if err != nil {
		return nil, apperr.BadRequest("%s", err.Error())
	}
	duplicate, err := s.repo.GetByContentHash(ctx, parsed.ContentHash)
	if err != nil {
		return nil, err
	}
	// 已成功导入过的文件直接标记重复, 不再逐行校验(避免未闭环行与库中自身记录相互冲突的误报)。
	if duplicate != nil {
		return &PreviewResult{
			FileName:    parsed.FileName,
			FileHash:    parsed.FileHash,
			ContentHash: parsed.ContentHash,
			TotalRows:   duplicate.TotalRows,
			Summary: PreviewSummary{
				FaultCount:  duplicate.FaultCount,
				RepairCount: duplicate.RepairCount,
				TotalCost:   duplicate.TotalCost,
			},
			Issues:      []RowIssue{},
			Ready:       false,
			DuplicateOf: duplicate.BatchNo,
		}, nil
	}

	validated, err := validateRows(ctx, s.lampRepo, s.faultRepo, parsed.Rows)
	if err != nil {
		return nil, err
	}

	issues := validated.Issues
	if issues == nil {
		issues = []RowIssue{}
	}
	return &PreviewResult{
		FileName:       parsed.FileName,
		FileHash:       parsed.FileHash,
		ContentHash:    parsed.ContentHash,
		TotalRows:      len(parsed.Rows),
		ExampleSkipped: parsed.ExampleSkipped,
		Summary:        validated.Summary,
		Issues:         issues,
		Ready:          len(validated.Issues) == 0,
	}, nil
}

// Commit 执行整批导入: 同一文件识别后跳过; 任何一行不通过或写入后核对不一致, 整批不写入。
func (s *Service) Commit(ctx context.Context, data []byte, fileName, operator string) (*CommitResult, error) {
	parsed, err := parseWorkbook(data, fileName)
	if err != nil {
		return nil, apperr.BadRequest("%s", err.Error())
	}

	// 重复文件识别: 以规范化逐行内容的指纹为准, 同一份文件(即使改名/改格式)直接跳过。
	existing, err := s.repo.GetByContentHash(ctx, parsed.ContentHash)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &CommitResult{
			Skipped:     true,
			FileName:    parsed.FileName,
			DuplicateOf: existing.BatchNo,
			Message: fmt.Sprintf("该文件已在批次 %s 导入过(%s), 自动跳过, 未产生重复记录",
				existing.BatchNo, existing.CreatedAt.Format("2006-01-02 15:04")),
			TotalRows: existing.TotalRows,
			Summary: PreviewSummary{
				FaultCount:  existing.FaultCount,
				RepairCount: existing.RepairCount,
				TotalCost:   existing.TotalCost,
			},
		}, nil
	}

	validated, err := validateRows(ctx, s.lampRepo, s.faultRepo, parsed.Rows)
	if err != nil {
		return nil, err
	}
	if len(validated.Issues) > 0 {
		return nil, apperr.BadRequest("仍有 %d 行未通过校验, 请按提示修改后重新上传整份文件", len(validated.Issues))
	}
	if len(validated.Rows) == 0 {
		return nil, apperr.BadRequest("没有可导入的数据行")
	}

	batch, err := s.importInTransaction(ctx, parsed, validated.Rows, operator)
	if err != nil {
		return nil, err
	}

	// 事务提交后刷新受影响路灯的运行状态, 使历史未闭环故障进入亮灯率基数; 失败仅告警, 不影响导入结果。
	s.syncLampStatus(ctx, validated.Rows)

	return &CommitResult{
		Skipped:   false,
		FileName:  parsed.FileName,
		Batch:     batch,
		TotalRows: len(validated.Rows),
		Summary: PreviewSummary{
			FaultCount:  batch.FaultCount,
			RepairCount: batch.RepairCount,
			TotalCost:   batch.TotalCost,
			LampCount:   validated.Summary.LampCount,
		},
		Message: fmt.Sprintf("导入完成: 故障 %d 条、维修 %d 条、费用合计 %.2f 元, 条数与金额已核对一致",
			batch.FaultCount, batch.RepairCount, batch.TotalCost),
	}, nil
}

// ListBatches 分页查询历史导入批次。
func (s *Service) ListBatches(ctx context.Context, params pagination.Params, keyword string) ([]ImportBatch, int64, pagination.Query, error) {
	page := pagination.Parse(params, batchSortSpec)
	items, total, err := s.repo.List(ctx, page, keyword)
	if err != nil {
		return nil, 0, page, err
	}
	return items, total, page, nil
}

// GetBatch 查询单个导入批次。
func (s *Service) GetBatch(ctx context.Context, id uint) (*ImportBatch, error) {
	return s.repo.GetByID(ctx, id)
}

// importInTransaction 在单个事务内写入批次、全部故障与维修记录, 并按 batch_id 回查核对。
func (s *Service) importInTransaction(ctx context.Context, parsed *parsedFile, rows []rowData, operator string) (*ImportBatch, error) {
	var saved ImportBatch

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		prefix := "DR" + time.Now().Format("20060102")
		batchNo, err := nextBatchNo(tx, prefix)
		if err != nil {
			return err
		}

		batch := &ImportBatch{
			BatchNo:     batchNo,
			FileName:    truncate(parsed.FileName, 255),
			FileHash:    parsed.FileHash,
			ContentHash: parsed.ContentHash,
			TotalRows:   len(rows),
			Operator:    strings.TrimSpace(operator),
		}
		if err := tx.Create(batch).Error; err != nil {
			if isUniqueViolation(err) {
				// 并发情况下另一个请求已导入同一文件, 按重复导入处理。
				return apperr.Conflict("该文件正在被其它请求导入或已导入, 请勿重复提交")
			}
			return fmt.Errorf("写入导入批次失败: %w", err)
		}

		faultSeq := newSequenceAllocator(func(prefix string) (int, error) {
			return currentMaxSequence(tx, &fault.Fault{}, "fault_no", prefix)
		})
		repairSeq := newSequenceAllocator(func(prefix string) (int, error) {
			return currentMaxSequence(tx, &repair.Repair{}, "repair_no", prefix)
		})

		faults := make([]fault.Fault, 0, len(rows))
		repairs := make([]repair.Repair, 0, len(rows))
		costSum := 0.0
		repairCount := 0

		for _, data := range rows {
			faultPrefix := "GD" + data.ReportedAt.Format("20060102")
			faultNo, err := faultSeq.next(faultPrefix)
			if err != nil {
				return err
			}

			entity := fault.Fault{
				FaultNo:       faultNo,
				LampID:        data.Lamp.ID,
				LampCode:      data.Lamp.Code,
				RoadName:      data.Lamp.RoadName,
				FaultType:     data.FaultType,
				FaultLevel:    data.FaultLevel,
				Source:        data.Source,
				Description:   data.Description,
				Reporter:      data.Reporter,
				ReporterPhone: data.ReporterPhone,
				ReportedAt:    data.ReportedAt,
				Status:        data.Status,
				CloseRemark:   data.CloseRemark,
				IsLegacy:      true,
				BatchID:       &batch.ID,
			}
			if !fault.IsOpen(data.Status) {
				closedAt := data.ClosedAt
				entity.ClosedAt = &closedAt
			}
			faults = append(faults, entity)
		}

		if err := tx.CreateInBatches(&faults, 200).Error; err != nil {
			if isUniqueViolation(err) {
				return apperr.Conflict("故障单号冲突, 本次导入已整体取消, 请重试")
			}
			return fmt.Errorf("写入历史故障失败: %w", err)
		}

		for index, data := range rows {
			linked := faults[index]
			if !data.HasRepair {
				continue
			}
			repairPrefix := "WX" + data.StartedAt.Format("20060102")
			repairNo, err := repairSeq.next(repairPrefix)
			if err != nil {
				return err
			}

			record := repair.Repair{
				RepairNo:     repairNo,
				FaultID:      linked.ID,
				FaultNo:      linked.FaultNo,
				LampID:       data.Lamp.ID,
				LampCode:     data.Lamp.Code,
				Repairman:    data.Repairman,
				RepairTeam:   data.RepairTeam,
				ContactPhone: data.ContactPhone,
				StartedAt:    data.StartedAt,
				Status:       repair.StatusOngoing,
				Content:      data.Content,
				Materials:    data.Materials,
				Cost:         data.Cost,
				Remark:       data.Remark,
				IsLegacy:     true,
				BatchID:      &batch.ID,
			}
			if !data.FinishedAt.IsZero() {
				finishedAt := data.FinishedAt
				record.FinishedAt = &finishedAt
				record.Status = repair.StatusFinished
				record.Result = data.Result
			}
			repairs = append(repairs, record)
			costSum += data.Cost
			repairCount++
		}

		if len(repairs) > 0 {
			if err := tx.CreateInBatches(&repairs, 200).Error; err != nil {
				if isUniqueViolation(err) {
					return apperr.Conflict("维修单号冲突, 本次导入已整体取消, 请重试")
				}
				return fmt.Errorf("写入历史维修记录失败: %w", err)
			}
			// repairs 与 faults 通过行序对应(仅含有维修的行), 建立故障ID -> 维修ID 映射后回填。
			repairIndex := 0
			for index, data := range rows {
				if !data.HasRepair {
					continue
				}
				if err := tx.Model(&fault.Fault{}).Where("id = ?", faults[index].ID).
					Updates(map[string]any{
						"repair_count":     1,
						"latest_repair_id": repairs[repairIndex].ID,
					}).Error; err != nil {
					return fmt.Errorf("回填故障维修计数失败: %w", err)
				}
				repairIndex++
			}
		}

		// ---- 写入后按 batch_id 回查核对, 任一不符则整体回滚, 杜绝"只写进去一半" ----
		var faultTotal int64
		if err := tx.Model(&fault.Fault{}).Where("batch_id = ?", batch.ID).Count(&faultTotal).Error; err != nil {
			return err
		}
		var repairTotal int64
		if err := tx.Model(&repair.Repair{}).Where("batch_id = ?", batch.ID).Count(&repairTotal).Error; err != nil {
			return err
		}
		var costTotal float64
		if err := tx.Model(&repair.Repair{}).Where("batch_id = ?", batch.ID).
			Select("COALESCE(SUM(cost), 0)").Scan(&costTotal).Error; err != nil {
			return err
		}

		if int(faultTotal) != len(rows) {
			return fmt.Errorf("导入核对失败: 故障期望 %d 条实际 %d 条, 已整体回滚", len(rows), faultTotal)
		}
		if int(repairTotal) != repairCount {
			return fmt.Errorf("导入核对失败: 维修期望 %d 条实际 %d 条, 已整体回滚", repairCount, repairTotal)
		}
		if math.Abs(costTotal-roundCents(costSum)) > 0.005 {
			return fmt.Errorf("导入核对失败: 费用期望 %.2f 元实际 %.2f 元, 已整体回滚", roundCents(costSum), costTotal)
		}

		batch.FaultCount = int(faultTotal)
		batch.RepairCount = int(repairTotal)
		batch.TotalCost = roundCents(costTotal)
		if err := tx.Save(batch).Error; err != nil {
			return fmt.Errorf("更新导入批次核对结果失败: %w", err)
		}
		saved = *batch
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// syncLampStatus 重新计算受影响路灯的运行状态, 规则与故障模块一致:
// 有维修中 -> 维修中; 有待处理 -> 故障; 否则正常(停用灯由人工维护, 这里不覆盖)。
func (s *Service) syncLampStatus(ctx context.Context, rows []rowData) {
	touched := make(map[uint]bool)
	for _, data := range rows {
		if data.Lamp != nil {
			touched[data.Lamp.ID] = true
		}
	}
	for lampID := range touched {
		counts, err := s.faultRepo.StatusCountsForLamp(ctx, lampID)
		if err != nil {
			slog.Warn("历史导入后同步路灯状态失败", "lamp_id", lampID, "error", err)
			continue
		}
		target := lamp.RunStatusNormal
		switch {
		case counts[fault.StatusProcessing] > 0:
			target = lamp.RunStatusMaintenance
		case counts[fault.StatusPending] > 0:
			target = lamp.RunStatusFault
		}
		if err := s.lampRepo.UpdateRunStatus(ctx, lampID, target); err != nil {
			slog.Warn("历史导入后更新路灯运行状态失败", "lamp_id", lampID, "error", err)
		}
	}
}

// PreviewResult 预检结果。
type PreviewResult struct {
	FileName       string         `json:"file_name"`
	FileHash       string         `json:"file_hash"`
	ContentHash    string         `json:"content_hash"`
	TotalRows      int            `json:"total_rows"`
	ExampleSkipped int            `json:"example_skipped"`
	Summary        PreviewSummary `json:"summary"`
	Issues         []RowIssue     `json:"issues"`
	Ready          bool           `json:"ready"`
	DuplicateOf    string         `json:"duplicate_of,omitempty"`
}

// CommitResult 导入结果, Skipped 为 true 表示文件重复被识别跳过。
type CommitResult struct {
	Skipped     bool           `json:"skipped"`
	FileName    string         `json:"file_name"`
	Batch       *ImportBatch   `json:"batch,omitempty"`
	DuplicateOf string         `json:"duplicate_of,omitempty"`
	TotalRows   int            `json:"total_rows"`
	Summary     PreviewSummary `json:"summary"`
	Message     string         `json:"message"`
}

// sequenceAllocator 在事务内为单号分配递增序号, 已见前缀直接内存递增。
type sequenceAllocator struct {
	known map[string]int
	load  func(prefix string) (int, error)
}

func newSequenceAllocator(load func(prefix string) (int, error)) *sequenceAllocator {
	return &sequenceAllocator{known: map[string]int{}, load: load}
}

func (a *sequenceAllocator) next(prefix string) (string, error) {
	current, ok := a.known[prefix]
	if !ok {
		latest, err := a.load(prefix)
		if err != nil {
			return "", err
		}
		current = latest
	}
	current++
	a.known[prefix] = current
	return fmt.Sprintf("%s%04d", prefix, current), nil
}

// currentMaxSequence 查询单号列在指定前缀下的最大数字序号, 没有记录时返回 0。
func currentMaxSequence(tx *gorm.DB, model any, column, prefix string) (int, error) {
	var values []string
	if err := tx.Model(model).
		Where(column+" LIKE ?", prefix+"%").
		Order(column+" DESC").
		Limit(1).
		Pluck(column, &values).Error; err != nil {
		return 0, fmt.Errorf("生成单号失败: %w", err)
	}
	if len(values) == 0 {
		return 0, nil
	}
	var number int
	if _, err := fmt.Sscanf(strings.TrimPrefix(values[0], prefix), "%d", &number); err != nil {
		return 0, nil
	}
	return number, nil
}

// nextBatchNo 在事务内生成批次号。
func nextBatchNo(tx *gorm.DB, prefix string) (string, error) {
	current, err := currentMaxSequence(tx, &ImportBatch{}, "batch_no", prefix)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%04d", prefix, current+1), nil
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// roundCents 保留两位小数, 用于金额核对。
func roundCents(value float64) float64 {
	return math.Round(value*100) / 100
}
