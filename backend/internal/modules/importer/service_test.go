package importer_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/simplifiedchinese"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/importer"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// harness 使用内存数据库装配真实模块, 验证台账迁移的完整业务流程。
type harness struct {
	db       *gorm.DB
	lamps    *lamp.Service
	faults   *fault.Service
	faultRep *fault.Repository
	imports  *importer.Service
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

	require.NoError(t, db.AutoMigrate(&lamp.Lamp{}, &fault.Fault{}, &repair.Repair{}, &importer.ImportBatch{}))

	lampRepository := lamp.NewRepository(db)
	lampService := lamp.NewService(lampRepository)

	faultRepository := fault.NewRepository(db)
	faultService := fault.NewService(faultRepository, lampService)
	lampService.SetOpenFaultCounter(faultRepository)

	repairRepository := repair.NewRepository(db)

	importService := importer.NewService(
		db,
		importer.NewRepository(db),
		lampRepository,
		faultRepository,
		faultService,
		repairRepository,
	)

	return &harness{
		db:       db,
		lamps:    lampService,
		faults:   faultService,
		faultRep: faultRepository,
		imports:  importService,
	}
}

// createLamp 登记一盏路灯并返回。
func (h *harness) createLamp(t *testing.T, code, road string) *lamp.Lamp {
	t.Helper()
	entity, err := h.lamps.Create(context.Background(), lamp.CreateRequest{
		Code:     code,
		Name:     "测试灯杆",
		RoadName: road,
		LampType: lamp.LampTypeLED,
	})
	require.NoError(t, err)
	return entity
}

// csvContent 把数据行编码为带表头的 CSV 内容。
func csvContent(t *testing.T, rows ...[]string) []byte {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := csv.NewWriter(buffer)
	header := []string{"路灯编号", "点位", "故障类型", "故障等级", "故障描述", "上报人", "上报时间", "闭环时间", "维修人", "维修班组", "维修费用", "维修内容", "备注"}
	require.NoError(t, writer.Write(header))
	for _, row := range rows {
		require.NoError(t, writer.Write(row))
	}
	writer.Flush()
	require.NoError(t, writer.Error())
	return buffer.Bytes()
}

// closedRow 构造一条已闭环的台账行。
func closedRow(code, road, reportedAt, closedAt, cost string) []string {
	return []string{code, road, "灯不亮", "normal", "整灯不亮", "张三", reportedAt, closedAt, "李师傅", "维修一班", cost, "更换驱动电源", ""}
}

// openRow 构造一条未闭环的台账行。
func openRow(code, road, reportedAt string) []string {
	return []string{code, road, "灯光闪烁", "", "夜间间歇性闪烁", "王五", reportedAt, "", "", "", "", "", ""}
}

func countRows(t *testing.T, db *gorm.DB, model any) int64 {
	t.Helper()
	var total int64
	require.NoError(t, db.Model(model).Count(&total).Error)
	return total
}

// TestImportSuccess 验证闭环与未闭环混合的台账能完整导入, 且批次合计与落库结果一致。
func TestImportSuccess(t *testing.T) {
	h := newHarness(t)
	h.createLamp(t, "LD-00001", "中山路")
	device2 := h.createLamp(t, "LD-00002", "建设大道")

	content := csvContent(t,
		closedRow("LD-00001", "中山路", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "186.50"),
		openRow("LD-00002", "建设大道", "2026-09-10 20:15:00"),
	)

	result, err := h.imports.Import(context.Background(), "legacy.csv", content)
	require.NoError(t, err)
	require.False(t, result.Duplicated)

	batch := result.Batch
	require.Equal(t, 2, batch.TotalRows)
	require.Equal(t, 2, batch.FaultCount)
	require.Equal(t, 1, batch.OpenFaultCount)
	require.Equal(t, 1, batch.RepairCount)
	require.InDelta(t, 186.50, batch.TotalCost, 0.001)
	require.NotEmpty(t, batch.BatchNo)

	// 故障与维修记录真实落库并回指批次。
	require.Equal(t, int64(2), countRows(t, h.db, &fault.Fault{}))
	require.Equal(t, int64(1), countRows(t, h.db, &repair.Repair{}))

	var closedFault fault.Fault
	require.NoError(t, h.db.Where("lamp_code = ?", "LD-00001").First(&closedFault).Error)
	require.Equal(t, fault.StatusClosed, closedFault.Status)
	require.NotNil(t, closedFault.ClosedAt)
	require.Equal(t, 1, closedFault.RepairCount)
	require.NotNil(t, closedFault.ImportBatchID)
	require.Equal(t, batch.ID, *closedFault.ImportBatchID)

	var importedRepair repair.Repair
	require.NoError(t, h.db.Where("fault_id = ?", closedFault.ID).First(&importedRepair).Error)
	require.Equal(t, repair.StatusFinished, importedRepair.Status)
	require.Equal(t, repair.ResultFixed, importedRepair.Result)
	require.InDelta(t, 186.50, importedRepair.Cost, 0.001)
	require.NotNil(t, importedRepair.ImportBatchID)

	// 未闭环故障计入亮灯率基数: 路灯运行状态被联动为故障。
	var openFault fault.Fault
	require.NoError(t, h.db.Where("lamp_code = ?", "LD-00002").First(&openFault).Error)
	require.Equal(t, fault.StatusPending, openFault.Status)

	reloaded, err := h.lamps.Get(context.Background(), device2.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusFault, reloaded.RunStatus)
}

// TestImportValidationFailure 验证逐行校验给出可读原因, 且整批不会写入一半。
func TestImportValidationFailure(t *testing.T) {
	h := newHarness(t)
	h.createLamp(t, "LD-00001", "中山路")
	h.createLamp(t, "LD-00002", "建设大道")
	device3 := h.createLamp(t, "LD-00003", "解放路")

	// 系统中已存在未闭环故障的路灯。
	_, err := h.faults.Create(context.Background(), fault.CreateRequest{
		LampID:      device3.ID,
		FaultType:   "灯不亮",
		Description: "存量未闭环故障",
	})
	require.NoError(t, err)

	future := time.Now().Add(48 * time.Hour).Format("2006-01-02 15:04:05")
	content := csvContent(t,
		closedRow("LD-00001", "中山路", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "100"),                                  // 唯一合法行
		closedRow("LD-99999", "中山路", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "100"),                                  // 路灯编号不存在
		closedRow("LD-00001", "错误的点位", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "100"),                                // 点位与台账不符
		[]string{"LD-00001", "中山路", "不存在的类型", "", "描述", "", "2026-08-01 09:30:00", "", "", "", "", "", ""},                 // 非法故障类型
		[]string{"LD-00001", "中山路", "灯不亮", "", "描述", "", future, "", "", "", "", "", ""},                                   // 上报时间晚于当前时间
		closedRow("LD-00001", "中山路", "2026-08-02 09:30:00", "2026-08-01 16:20:00", "100"),                                  // 闭环时间早于上报时间
		[]string{"LD-00001", "中山路", "灯不亮", "", "描述", "", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "", "", "", "", ""}, // 已闭环未填维修人
		[]string{"LD-00001", "中山路", "灯不亮", "", "描述", "", "2026-08-01 09:30:00", "", "", "", "88", "", ""},                  // 未闭环却填写维修费用
		openRow("LD-00002", "建设大道", "2026-09-10 20:15:00"),                                                                 // 文件内未闭环重复(第一次出现)
		openRow("LD-00002", "建设大道", "2026-09-11 08:00:00"),                                                                 // 文件内未闭环重复(应报错)
		openRow("LD-00003", "解放路", "2026-09-10 20:15:00"),                                                                  // 与系统存量未闭环故障重复
	)

	err = func() error {
		_, err := h.imports.Import(context.Background(), "bad.csv", content)
		return err
	}()
	require.Error(t, err)

	validationErr, ok := err.(*importer.ValidationError)
	require.True(t, ok, "应返回 ValidationError, 实际: %T", err)
	failure := validationErr.Failure
	require.Equal(t, 11, failure.TotalRows)
	require.Equal(t, 9, failure.ErrorRows)

	reasons := make(map[int]string)
	for _, rowErr := range failure.RowErrors {
		joined := ""
		for _, reason := range rowErr.Reasons {
			joined += reason + "|"
		}
		reasons[rowErr.LineNo] = joined
	}
	require.Contains(t, reasons[3], "路灯编号不存在")
	require.Contains(t, reasons[4], "点位与台账不符")
	require.Contains(t, reasons[5], "非法的故障类型")
	require.Contains(t, reasons[6], "上报时间不能晚于当前时间")
	require.Contains(t, reasons[7], "闭环时间不能早于上报时间")
	require.Contains(t, reasons[8], "已闭环的故障必须填写维修人")
	require.Contains(t, reasons[9], "未闭环的故障不应填写维修信息")
	require.Contains(t, reasons[11], "与第 10 行重复")
	require.Contains(t, reasons[12], "已存在未闭环故障")

	// 原子性: 有任何行失败, 整批不写库。
	require.Equal(t, int64(1), countRows(t, h.db, &fault.Fault{}), "只应剩存量故障")
	require.Equal(t, int64(0), countRows(t, h.db, &repair.Repair{}))
	require.Equal(t, int64(0), countRows(t, h.db, &importer.ImportBatch{}))
}

// TestImportDuplicateFile 验证同一份文件重复导入会被识别并跳过。
func TestImportDuplicateFile(t *testing.T) {
	h := newHarness(t)
	h.createLamp(t, "LD-00001", "中山路")

	content := csvContent(t, closedRow("LD-00001", "中山路", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "66"))

	first, err := h.imports.Import(context.Background(), "ledger.csv", content)
	require.NoError(t, err)
	require.False(t, first.Duplicated)

	// 同一份文件再次导入: 识别为重复并跳过, 不产生新数据。
	second, err := h.imports.Import(context.Background(), "ledger-重命名.csv", content)
	require.NoError(t, err)
	require.True(t, second.Duplicated)
	require.Equal(t, first.Batch.ID, second.Batch.ID)
	require.Equal(t, int64(1), countRows(t, h.db, &fault.Fault{}))
	require.Equal(t, int64(1), countRows(t, h.db, &importer.ImportBatch{}))

	// 内容有变化的文件是新的批次, 正常导入。
	modified := csvContent(t, closedRow("LD-00001", "中山路", "2026-08-02 09:30:00", "2026-08-02 16:20:00", "88"))
	third, err := h.imports.Import(context.Background(), "ledger.csv", modified)
	require.NoError(t, err)
	require.False(t, third.Duplicated)
	require.Equal(t, int64(2), countRows(t, h.db, &fault.Fault{}))
}

// TestImportExcludedFromOverdue 验证导入记录计入统计基数但不参与逾期计算。
func TestImportExcludedFromOverdue(t *testing.T) {
	h := newHarness(t)
	device := h.createLamp(t, "LD-00001", "中山路")

	// 导入一条 10 天前上报的未闭环故障。
	old := time.Now().Add(-10 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	content := csvContent(t, openRow("LD-00001", "中山路", old))
	_, err := h.imports.Import(context.Background(), "old.csv", content)
	require.NoError(t, err)

	overdueBefore := time.Now().Add(-24 * time.Hour)

	// 不参与逾期计算。
	overdue, err := h.faultRep.CountPendingBefore(context.Background(), overdueBefore)
	require.NoError(t, err)
	require.Equal(t, int64(0), overdue)
	overdueList, err := h.faultRep.ListPendingBefore(context.Background(), overdueBefore, 10)
	require.NoError(t, err)
	require.Empty(t, overdueList)

	// 但仍计入未闭环基数, 且路灯状态联动为故障。
	openTotal, err := h.faultRep.CountOpen(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), openTotal)
	reloaded, err := h.lamps.Get(context.Background(), device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusFault, reloaded.RunStatus)

	// 正常登记的旧故障仍然参与逾期计算, 确认过滤条件没有误伤。
	_, err = h.faults.Create(context.Background(), fault.CreateRequest{
		LampID:      device.ID,
		FaultType:   "灯不亮",
		Description: "先闭环存量故障再登记",
	})
	require.Error(t, err, "有未闭环故障时不允许重复登记")

	// 关闭导入的故障后正常登记一条上报时间很早的故障。
	var imported fault.Fault
	require.NoError(t, h.db.Where("lamp_code = ?", "LD-00001").First(&imported).Error)
	_, err = h.faults.Close(context.Background(), imported.ID, fault.CloseRequest{Remark: "现场复核"})
	require.NoError(t, err)

	_, err = h.faults.Create(context.Background(), fault.CreateRequest{
		LampID:      device.ID,
		FaultType:   "灯不亮",
		Description: "正常登记的超期故障",
		ReportedAt:  time.Now().Add(-30 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	require.NoError(t, err)
	overdue, err = h.faultRep.CountPendingBefore(context.Background(), overdueBefore)
	require.NoError(t, err)
	require.Equal(t, int64(1), overdue)
}

// TestImportFileFormat 验证表头校验、空数据与 GBK 编码兼容。
func TestImportFileFormat(t *testing.T) {
	h := newHarness(t)
	h.createLamp(t, "LD-00001", "中山路")

	// 表头与模板不一致。
	_, err := h.imports.Import(context.Background(), "bad-header.csv", []byte("编号,点位\nLD-00001,中山路\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "表头与模板不一致")

	// 只有表头没有数据行。
	_, err = h.imports.Import(context.Background(), "empty.csv", csvContent(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "没有数据行")

	// GBK 编码的文件可以正常导入。
	gbkContent, err := simplifiedchinese.GB18030.NewEncoder().Bytes(
		csvContent(t, closedRow("LD-00001", "中山路", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "120")),
	)
	require.NoError(t, err)
	result, err := h.imports.Import(context.Background(), "gbk.csv", gbkContent)
	require.NoError(t, err)
	require.Equal(t, 1, result.Batch.FaultCount)
}

// TestImportBatchQuery 验证导入批次可以按列表与详情查询, 用于事后核对。
func TestImportBatchQuery(t *testing.T) {
	h := newHarness(t)
	h.createLamp(t, "LD-00001", "中山路")

	content := csvContent(t, closedRow("LD-00001", "中山路", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "200"))
	imported, err := h.imports.Import(context.Background(), "ledger.csv", content)
	require.NoError(t, err)

	items, total, _, err := h.imports.ListBatches(context.Background(), importer.BatchListQuery{Keyword: "ledger"})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, items, 1)

	detail, err := h.imports.GetBatch(context.Background(), imported.Batch.ID)
	require.NoError(t, err)
	require.Equal(t, imported.Batch.BatchNo, detail.BatchNo)
	require.InDelta(t, 200.0, detail.TotalCost, 0.001)

	_, err = h.imports.GetBatch(context.Background(), 9999)
	require.Error(t, err)
}

// TestImportFaultNoSequence 验证批量导入生成的单号与系统既有单号不冲突。
func TestImportFaultNoSequence(t *testing.T) {
	h := newHarness(t)
	device := h.createLamp(t, "LD-00001", "中山路")
	h.createLamp(t, "LD-00002", "建设大道")

	// 先正常登记并关闭一条故障, 占用当天的单号流水。
	existing, err := h.faults.Create(context.Background(), fault.CreateRequest{
		LampID:      device.ID,
		FaultType:   "灯不亮",
		Description: "存量故障",
	})
	require.NoError(t, err)
	_, err = h.faults.Close(context.Background(), existing.ID, fault.CloseRequest{Remark: "处理完毕"})
	require.NoError(t, err)

	today := time.Now().Format("2006-01-02 15:04:05")
	content := csvContent(t,
		openRow("LD-00001", "中山路", today),
		openRow("LD-00002", "建设大道", today),
	)
	result, err := h.imports.Import(context.Background(), "seq.csv", content)
	require.NoError(t, err)
	require.Equal(t, 2, result.Batch.FaultCount)

	// 全部故障单号唯一。
	var numbers []string
	require.NoError(t, h.db.Model(&fault.Fault{}).Pluck("fault_no", &numbers).Error)
	seen := make(map[string]bool, len(numbers))
	for _, number := range numbers {
		require.False(t, seen[number], "故障单号重复: %s", number)
		seen[number] = true
	}
	require.Len(t, numbers, 3)
}
