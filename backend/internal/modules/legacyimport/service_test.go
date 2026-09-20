package legacyimport_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/legacyimport"
	"streetlight/internal/modules/repair"
	"streetlight/pkg/pagination"
)

// harness 组装内存数据库与真实模块。
type harness struct {
	db      *gorm.DB
	lamps   *lamp.Service
	faults  *fault.Service
	repairs *repair.Service
	import_ *legacyimport.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	require.NoError(t, db.AutoMigrate(&lamp.Lamp{}, &fault.Fault{}, &repair.Repair{}, &legacyimport.ImportBatch{}))

	lampRepo := lamp.NewRepository(db)
	lampService := lamp.NewService(lampRepo)
	faultRepo := fault.NewRepository(db)
	faultService := fault.NewService(faultRepo, lampService)
	lampService.SetOpenFaultCounter(faultRepo)
	repairRepo := repair.NewRepository(db)
	_ = repair.NewService(repairRepo, faultService)

	return &harness{
		db:      db,
		lamps:   lampService,
		faults:  faultService,
		repairs: repair.NewService(repairRepo, faultService),
		import_: legacyimport.NewService(db, legacyimport.NewRepository(db), lampRepo, faultRepo),
	}
}

// rowSpec 描述测试文件中的一行, 零值时间表示留空。
type rowSpec struct {
	code, road, ftype, level, source, desc, reporter string
	reportedAt                                       time.Time
	status                                           string
	closedAt                                         time.Time
	repairman, startedAt, finishedAt, result         string
	cost                                             float64
}

func buildWorkbook(t *testing.T, specs []rowSpec) []byte {
	t.Helper()
	file := excelize.NewFile()
	defer file.Close()
	require.NoError(t, file.SetSheetName("Sheet1", "历史台账"))

	headers := []string{
		"路灯编号", "点位(道路名称)", "故障类型", "紧急程度", "故障来源", "故障描述",
		"上报人", "上报人电话", "上报时间", "闭环状态", "闭环时间", "闭环说明",
		"维修人", "维修班组", "维修联系电话", "开工时间", "完工时间", "维修结果",
		"维修内容", "耗材", "维修费用(元)", "备注",
	}
	headerRow := make([]interface{}, len(headers))
	for i, header := range headers {
		headerRow[i] = header
	}
	require.NoError(t, file.SetSheetRow("历史台账", "A1", &headerRow))

	fmtTime := func(value time.Time) string {
		if value.IsZero() {
			return ""
		}
		return value.Format("2006-01-02 15:04:05")
	}

	for index, spec := range specs {
		axis, err := excelize.CoordinatesToCellName(1, index+2)
		require.NoError(t, err)
		row := []interface{}{
			spec.code, spec.road, spec.ftype, spec.level, spec.source, spec.desc,
			spec.reporter, "", fmtTime(spec.reportedAt), spec.status, fmtTime(spec.closedAt), "",
			spec.repairman, "", "", spec.startedAt, spec.finishedAt, spec.result,
			"", "", spec.cost, "",
		}
		// startedAt/finishedAt 在 spec 中是预格式化字符串, 直接透传。
		require.NoError(t, file.SetSheetRow("历史台账", axis, &row))
	}
	var buffer bytes.Buffer
	require.NoError(t, file.Write(&buffer))
	return buffer.Bytes()
}

func seedLamps(t *testing.T, h *harness, codes ...string) {
	t.Helper()
	for _, code := range codes {
		_, err := h.lamps.Create(context.Background(), lamp.CreateRequest{
			Code: code, Name: "测试灯杆", RoadName: "测试路", LampType: lamp.LampTypeLED,
		})
		require.NoError(t, err)
	}
}

func validSpec(code string, reported time.Time) rowSpec {
	return rowSpec{
		code: code, road: "测试路", ftype: "灯不亮", level: "紧急", source: "巡检发现",
		desc: "整灯不亮", reporter: "张工", reportedAt: reported,
		status:     "已关闭",
		closedAt:   reported.Add(3 * time.Hour),
		repairman:  "李师傅",
		startedAt:  reported.Add(30 * time.Minute).Format("2006-01-02 15:04:05"),
		finishedAt: reported.Add(3 * time.Hour).Format("2006-01-02 15:04:05"),
		result:     "已修复",
		cost:       120.5,
	}
}

func TestCommitHappyPathAndReconciliation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedLamps(t, h, "LD-T-001", "LD-T-002")

	now := time.Now()
	file := buildWorkbook(t, []rowSpec{
		validSpec("LD-T-001", now.AddDate(0, -1, 0)),
		validSpec("LD-T-002", now.AddDate(0, -1, -2)),
	})

	result, err := h.import_.Commit(ctx, file, "legacy-2026.xlsx", "admin")
	require.NoError(t, err)
	require.False(t, result.Skipped)
	require.Equal(t, 2, result.Summary.FaultCount)
	require.Equal(t, 2, result.Summary.RepairCount)
	require.InDelta(t, 241.0, result.Summary.TotalCost, 0.001)

	// 记录已落库且带历史标记。
	var legacyFaults int64
	require.NoError(t, h.db.Model(&fault.Fault{}).Where("is_legacy = ?", true).Count(&legacyFaults).Error)
	require.Equal(t, int64(2), legacyFaults)

	var legacyRepairs int64
	require.NoError(t, h.db.Model(&repair.Repair{}).Where("is_legacy = ?", true).Count(&legacyRepairs).Error)
	require.Equal(t, int64(2), legacyRepairs)

	// 故障状态与闭环时间符合文件内容。
	entities, total2, _, err := h.faults.List(ctx, fault.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, int64(2), total2)
	for _, item := range entities {
		require.Equal(t, fault.StatusClosed, item.Status)
		require.NotNil(t, item.ClosedAt)
		require.Equal(t, 1, item.RepairCount)
		require.True(t, item.IsLegacy)
	}

	// 批次记录可查且金额核对一致。
	batches, total, _, err := h.import_.ListBatches(ctx, pagination.Params{PageSize: 10}, "")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, 2, batches[0].FaultCount)
	require.Equal(t, 2, batches[0].RepairCount)
	require.InDelta(t, 241.0, batches[0].TotalCost, 0.001)
	require.Regexp(t, `^DR\d{12}$`, batches[0].BatchNo)
}

func TestDuplicateFileSkipped(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedLamps(t, h, "LD-T-001")

	file := buildWorkbook(t, []rowSpec{validSpec("LD-T-001", time.Now().AddDate(0, -1, 0))})

	first, err := h.import_.Commit(ctx, file, "a.xlsx", "admin")
	require.NoError(t, err)
	require.False(t, first.Skipped)

	// 同一份内容即使改了文件名, 也应识别并跳过。
	second, err := h.import_.Commit(ctx, file, "renamed.xlsx", "admin")
	require.NoError(t, err)
	require.True(t, second.Skipped)
	require.Equal(t, first.Batch.BatchNo, second.DuplicateOf)

	var count int64
	require.NoError(t, h.db.Model(&fault.Fault{}).Count(&count).Error)
	require.Equal(t, int64(1), count, "重复文件不得产生第二条故障")
}

func TestPreviewReportsReadableErrors(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedLamps(t, h, "LD-T-001")

	now := time.Now()
	good := validSpec("LD-T-001", now.AddDate(0, -1, 0))

	badLamp := validSpec("LD-NOPE", now)
	badLamp.road = "测试路"

	badRoad := validSpec("LD-T-001", now.AddDate(0, -2, 0))
	badRoad.code = "LD-T-001"
	// LD-T-001 在第一行已闭环, 这里构造点位不一致
	badRoad.road = "错误道路"

	badTime := validSpec("LD-T-001", now.AddDate(0, -3, 0))
	badTime.closedAt = badTime.reportedAt.Add(-2 * time.Hour) // 闭环早于上报
	badTime.finishedAt = badTime.closedAt.Format("2006-01-02 15:04:05")

	file := buildWorkbook(t, []rowSpec{badLamp, badRoad, badTime, good})
	preview, err := h.import_.Preview(ctx, file, "bad.xlsx")
	require.NoError(t, err)
	require.False(t, preview.Ready)
	require.Len(t, preview.Issues, 3)

	joined := ""
	for _, issue := range preview.Issues {
		for _, message := range issue.Messages {
			joined += message + "\n"
		}
	}
	require.Contains(t, joined, "不存在")
	require.Contains(t, joined, "点位与编号不一致")
	require.Contains(t, joined, "不能早于上报时间")

	// 预检不写任何数据。
	var count int64
	require.NoError(t, h.db.Model(&fault.Fault{}).Count(&count).Error)
	require.Equal(t, int64(0), count)

	// 存在错误行时确认导入应被拒绝。
	_, err = h.import_.Commit(ctx, file, "bad.xlsx", "admin")
	businessErr, ok := apperr.As(err)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, businessErr.Status)
}

func TestDuplicateOpenFaultInSameFileRejected(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedLamps(t, h, "LD-T-001")

	now := time.Now()
	first := rowSpec{
		code: "LD-T-001", road: "测试路", ftype: "灯不亮", level: "普通", source: "巡检发现",
		desc: "第一条未闭环", reporter: "张工", reportedAt: now.Add(-48 * time.Hour),
		status: "待处理",
	}
	second := rowSpec{
		code: "LD-T-001", road: "测试路", ftype: "灯不亮", level: "普通", source: "巡检发现",
		desc: "第二条未闭环", reporter: "张工", reportedAt: now.Add(-24 * time.Hour),
		status: "维修中", repairman: "李师傅",
		startedAt: now.Add(-20 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	file := buildWorkbook(t, []rowSpec{first, second})
	preview, err := h.import_.Preview(ctx, file, "dup.xlsx")
	require.NoError(t, err)
	require.False(t, preview.Ready)
	require.Len(t, preview.Issues, 2, "同一盏灯两条未闭环都应报错")
}

func TestLegacyRecordsCountInBaseButExcludedFromOverdue(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedLamps(t, h, "LD-T-001", "LD-T-002")

	now := time.Now()
	// 历史未闭环(待处理)故障, 上报于很久以前, 应计入故障基数但不计逾期。
	openLegacy := rowSpec{
		code: "LD-T-001", road: "测试路", ftype: "灯不亮", level: "普通", source: "巡检发现",
		desc: "历史遗留未闭环", reporter: "张工", reportedAt: now.AddDate(0, -2, 0),
		status: "待处理",
	}
	closedLegacy := validSpec("LD-T-002", now.AddDate(0, -1, 0))
	file := buildWorkbook(t, []rowSpec{openLegacy, closedLegacy})

	_, err := h.import_.Commit(ctx, file, "legacy.xlsx", "admin")
	require.NoError(t, err)

	total, err := h.faults.Repository().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), total, "历史故障计入故障总数(亮灯率基数)")

	overdue, err := h.faults.Repository().CountPendingBefore(ctx, now.Add(-24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(0), overdue, "历史待处理故障不参与逾期计算")

	// 新登记的超期故障应正常计入逾期。
	newFault, err := h.faults.Create(ctx, fault.CreateRequest{
		LampID: mustLampID(t, h, "LD-T-002"), FaultType: "灯不亮", Description: "新的超期故障",
		ReportedAt: now.Add(-48 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	require.NoError(t, err)
	_ = newFault
	overdue, err = h.faults.Repository().CountPendingBefore(ctx, now.Add(-24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), overdue)
}

func TestConflictWithExistingOpenFault(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedLamps(t, h, "LD-T-001", "LD-T-002")

	// LD-T-001 在台账中已有一条新系统登记的未闭环故障。
	existing, err := h.faults.Create(ctx, fault.CreateRequest{
		LampID: mustLampID(t, h, "LD-T-001"), FaultType: "灯不亮", Description: "现存未闭环",
	})
	require.NoError(t, err)
	require.Equal(t, fault.StatusPending, existing.Status)

	openInFile := rowSpec{
		code: "LD-T-001", road: "测试路", ftype: "灯不亮", level: "普通", source: "巡检发现",
		desc: "又一条未闭环", reporter: "张工", reportedAt: time.Now().Add(-48 * time.Hour),
		status: "待处理",
	}
	closedOther := validSpec("LD-T-002", time.Now().AddDate(0, -1, 0))
	file := buildWorkbook(t, []rowSpec{openInFile, closedOther})

	preview, err := h.import_.Preview(ctx, file, "conflict.xlsx")
	require.NoError(t, err)
	require.False(t, preview.Ready)
	require.Len(t, preview.Issues, 1)
	require.Contains(t, preview.Issues[0].Messages[0], "已有")
}

// mustLampID 按编号取路灯 ID。
func mustLampID(t *testing.T, h *harness, code string) uint {
	t.Helper()
	entities, total, _, err := h.lamps.List(context.Background(), lamp.ListQuery{Keyword: code})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	return entities[0].ID
}

// TestCommitRollsBackOnMidwayFailure 验证维修记录写入阶段失败时, 已写入的批次与故障也整体回滚。
func TestCommitRollsBackOnMidwayFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedLamps(t, h, "LD-T-001", "LD-T-002")

	// 在第二次 repair INSERT 前强制报错: 先放行 fault/batch 写入, 遇到 repair 表即失败。
	repairSeen := 0
	err := h.db.Callback().Create().Before("gorm:create").Register("test_force_repair_failure", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "repair" {
			return
		}
		repairSeen++
		tx.AddError(fmt.Errorf("模拟维修记录写入失败"))
	})
	require.NoError(t, err)

	file := buildWorkbook(t, []rowSpec{
		validSpec("LD-T-001", time.Now().AddDate(0, -1, 0)),
		validSpec("LD-T-002", time.Now().AddDate(0, -1, -2)),
	})
	_, commitErr := h.import_.Commit(ctx, file, "rollback.xlsx", "admin")
	require.Error(t, commitErr)

	var faultCount, repairCount, batchCount int64
	require.NoError(t, h.db.Model(&fault.Fault{}).Count(&faultCount).Error)
	require.NoError(t, h.db.Model(&repair.Repair{}).Count(&repairCount).Error)
	require.NoError(t, h.db.Model(&legacyimport.ImportBatch{}).Count(&batchCount).Error)
	require.Equal(t, int64(0), faultCount, "故障必须随事务回滚")
	require.Equal(t, int64(0), repairCount)
	require.Equal(t, int64(0), batchCount, "批次记录也必须回滚, 避免占用内容指纹")
	require.Greater(t, repairSeen, 0)
}

// TestCommitOngoingLegacyRepair 验证历史"维修中(已开工未完工)"记录的迁移。
func TestCommitOngoingLegacyRepair(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedLamps(t, h, "LD-T-010")

	reported := time.Now().AddDate(0, -1, 0)
	ongoing := rowSpec{
		code: "LD-T-010", road: "测试路", ftype: "灯不亮", level: "普通", source: "巡检发现",
		desc: "历史遗留维修中", reporter: "张工", reportedAt: reported,
		status: "维修中", repairman: "李师傅",
		startedAt: reported.Add(time.Hour).Format("2006-01-02 15:04:05"),
	}
	file := buildWorkbook(t, []rowSpec{ongoing})
	result, err := h.import_.Commit(ctx, file, "ongoing.xlsx", "admin")
	require.NoError(t, err)
	require.Equal(t, 1, result.Summary.FaultCount)
	require.Equal(t, 1, result.Summary.RepairCount)

	device, err := h.lamps.Get(ctx, mustLampID(t, h, "LD-T-010"))
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, device.RunStatus, "历史维修中故障应把路灯联动为维修中")

	entities, total, _, err := h.faults.List(ctx, fault.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, fault.StatusProcessing, entities[0].Status)
}
