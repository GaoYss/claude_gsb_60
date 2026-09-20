package legacyimport

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// 模板列顺序与表头, 一行同时承载一条故障与其闭环维修信息。
const (
	colLampCode      = "路灯编号"
	colRoadName      = "点位(道路名称)"
	colFaultType     = "故障类型"
	colFaultLevel    = "紧急程度"
	colSource        = "故障来源"
	colDescription   = "故障描述"
	colReporter      = "上报人"
	colReporterPhone = "上报人电话"
	colReportedAt    = "上报时间"
	colCloseStatus   = "闭环状态"
	colClosedAt      = "闭环时间"
	colCloseRemark   = "闭环说明"
	colRepairman     = "维修人"
	colRepairTeam    = "维修班组"
	colContactPhone  = "维修联系电话"
	colStartedAt     = "开工时间"
	colFinishedAt    = "完工时间"
	colRepairResult  = "维修结果"
	colContent       = "维修内容"
	colMaterials     = "耗材"
	colCost          = "维修费用(元)"
	colRemark        = "备注"
)

// templateSheet 数据所在工作表名称。
const templateSheet = "历史台账"

// templateHeaders 按列顺序返回模板表头。
func templateHeaders() []string {
	return []string{
		colLampCode, colRoadName, colFaultType, colFaultLevel, colSource, colDescription,
		colReporter, colReporterPhone, colReportedAt, colCloseStatus, colClosedAt, colCloseRemark,
		colRepairman, colRepairTeam, colContactPhone, colStartedAt, colFinishedAt, colRepairResult,
		colContent, colMaterials, colCost, colRemark,
	}
}

// 模板中允许填写的中文枚举, 同时兼容英文原始值。
var (
	statusOptions = []string{"待处理", "维修中", "已修复", "已关闭"}
	levelOptions  = []string{"一般", "普通", "紧急", "特急"}
	sourceOptions = []string{"巡检发现", "市民上报", "系统告警", "其它"}
	resultOptions = []string{"已修复", "待配件", "观察中", "无法修复"}
)

var statusFromCN = map[string]string{
	"待处理": "pending", "维修中": "processing", "已修复": "repaired", "已关闭": "closed",
	"pending": "pending", "processing": "processing", "repaired": "repaired", "closed": "closed",
}
var levelFromCN = map[string]string{
	"一般": "low", "普通": "normal", "紧急": "high", "特急": "urgent",
	"low": "low", "normal": "normal", "high": "high", "urgent": "urgent",
}
var sourceFromCN = map[string]string{
	"巡检发现": "inspection", "市民上报": "citizen", "系统告警": "monitoring", "其它": "other",
	"inspection": "inspection", "citizen": "citizen", "monitoring": "monitoring", "other": "other",
}
var resultFromCN = map[string]string{
	"已修复": "fixed", "待配件": "pending_parts", "观察中": "observing", "无法修复": "unfixable",
	"fixed": "fixed", "pending_parts": "pending_parts", "observing": "observing", "unfixable": "unfixable",
}

// timeLayouts 模板时间列允许的填写格式。
var timeLayouts = []string{
	"2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC3339, "2006/01/02 15:04:05", "2006-01-02",
}

// exampleRow 模板内置的一行示例, 导入时空行之外的示例需由用户删除。
func exampleRow() []interface{} {
	return []interface{}{
		"LD-000001", "中山路", "灯不亮", "紧急", "巡检发现", "整灯不亮, 疑似驱动电源故障",
		"张工", "13800000000", "2026-08-01 20:15:00", "已关闭", "2026-08-02 10:30:00", "现场复核通过",
		"李师傅", "市政照明一班", "13900001111", "2026-08-01 21:00:00", "2026-08-02 10:30:00",
		"已修复", "更换驱动电源并试亮", "LED 驱动电源 1 个", 210.5, "",
	}
}

// BuildTemplate 生成历史台账导入模板(含填写说明、枚举下拉与一行示例)。
func BuildTemplate() ([]byte, string, error) {
	file := excelize.NewFile()
	defer file.Close()

	// 第一张表放填写说明。
	guideSheet := "填写说明"
	if _, err := file.NewSheet(guideSheet); err != nil {
		return nil, "", err
	}
	guideLines := [][2]string{
		{"历史故障与维修台账导入模板", ""},
		{"", ""},
		{"1. 请在「历史台账」工作表按行填写, 每行 = 一条故障; 若该故障已维修, 在同行补齐维修与闭环信息。"},
		{"2. 带 * 的列为必填: 路灯编号、故障类型、故障描述、上报时间、闭环状态。"},
		{"3. 路灯编号与点位必须已存在于路灯台账, 且点位要与编号实际所在道路一致。"},
		{"4. 时间格式支持 2026-08-01 20:15:00 或 2026-08-01; 闭环时间不得早于上报时间, 完工时间不得早于开工时间。"},
		{"5. 闭环状态取值: 待处理 / 维修中 / 已修复 / 已关闭。"},
		{"   - 待处理、维修中属于未闭环: 不允许填写闭环时间, 同一路灯只允许一条未闭环。"},
		{"   - 已修复、已关闭属于已闭环: 必须填写闭环时间。"},
		{"6. 维修信息分组填写: 维修人与开工时间必须同时填(已开工), 完工时间与维修结果必须同时填(已完工); 只登记故障没有维修过程时整组留空。"},
		{"7. 维修费用为数字(元), 最多两位小数, 不填按 0 计; 导入后按条数与金额合计核对, 不会只导入一半。"},
		{"8. 示例行(第 2 行)仅供参考, 正式导入前请删除。"},
		{"9. 同一份文件重复上传会被自动识别并跳过, 不会产生重复记录。"},
	}
	for index, line := range guideLines {
		cellA, _ := excelize.CoordinatesToCellName(1, index+1)
		cellB, _ := excelize.CoordinatesToCellName(2, index+1)
		if err := file.SetCellValue(guideSheet, cellA, line[0]); err != nil {
			return nil, "", err
		}
		if line[1] != "" {
			if err := file.SetCellValue(guideSheet, cellB, line[1]); err != nil {
				return nil, "", err
			}
		}
	}
	style, _ := file.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}})
	_ = file.SetCellStyle(guideSheet, "A1", "A1", style)
	if err := file.SetColWidth(guideSheet, "A", "A", 110); err != nil {
		return nil, "", err
	}

	// 数据工作表。
	idx, err := file.NewSheet(templateSheet)
	if err != nil {
		return nil, "", err
	}
	headers := templateHeaders()
	headerRow := make([]interface{}, len(headers))
	for i, header := range headers {
		headerRow[i] = header
	}
	if err := file.SetSheetRow(templateSheet, "A1", &headerRow); err != nil {
		return nil, "", err
	}
	if err := markRequiredHeaderRow(file, headers); err != nil {
		return nil, "", err
	}
	example := exampleRow()
	if err := file.SetSheetRow(templateSheet, "A2", &example); err != nil {
		return nil, "", err
	}
	exampleStyle, _ := file.NewStyle(&excelize.Style{Font: &excelize.Font{Color: "909399", Italic: true}})
	if err := file.SetCellStyle(templateSheet, "A2", cellName(len(headers), 2), exampleStyle); err != nil {
		return nil, "", err
	}

	for i := range headers {
		col, _ := excelize.ColumnNumberToName(i + 1)
		width := 16.0
		if headers[i] == colDescription || headers[i] == colContent {
			width = 28
		}
		if headers[i] == colMaterials || headers[i] == colCloseRemark {
			width = 22
		}
		_ = file.SetColWidth(templateSheet, col, col, width)
	}

	// 枚举列下拉(对前 2000 行生效)。
	if err := addDataValidation(file, colFaultLevel, levelOptions, 2000); err != nil {
		return nil, "", err
	}
	if err := addDataValidation(file, colSource, sourceOptions, 2000); err != nil {
		return nil, "", err
	}
	if err := addDataValidation(file, colCloseStatus, statusOptions, 2000); err != nil {
		return nil, "", err
	}
	if err := addDataValidation(file, colRepairResult, resultOptions, 2000); err != nil {
		return nil, "", err
	}

	file.SetActiveSheet(idx)
	if err := file.DeleteSheet("Sheet1"); err != nil {
		return nil, "", err
	}

	var buffer bytes.Buffer
	if err := file.Write(&buffer); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), "历史故障维修台账导入模板.xlsx", nil
}

// markRequiredHeaderRow 将必填列表头标红加粗, 并冻结首行。
func markRequiredHeaderRow(file *excelize.File, headers []string) error {
	required := map[string]bool{
		colLampCode: true, colFaultType: true, colDescription: true,
		colReportedAt: true, colCloseStatus: true,
	}
	redBold, err := file.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "c00000"}})
	if err != nil {
		return err
	}
	for i, header := range headers {
		if !required[header] {
			continue
		}
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := file.SetCellStyle(templateSheet, cell, cell, redBold); err != nil {
			return err
		}
	}
	return file.SetPanes(templateSheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})
}

// addDataValidation 给指定表头列加枚举下拉。
func addDataValidation(file *excelize.File, header string, options []string, rows int) error {
	colIndex := 0
	for i, item := range templateHeaders() {
		if item == header {
			colIndex = i + 1
			break
		}
	}
	col, err := excelize.ColumnNumberToName(colIndex)
	if err != nil {
		return err
	}
	validation := excelize.NewDataValidation(true)
	if err := validation.SetDropList(options); err != nil {
		return err
	}
	validation.SetSqref(fmt.Sprintf("%s2:%s%d", col, col, rows+1))
	return file.AddDataValidation(templateSheet, validation)
}

func cellName(col, row int) string {
	name, _ := excelize.CoordinatesToCellName(col, row)
	return name
}

// parseTimeCell 解析时间单元格, 兼容字符串与 excelize 读出的 time.Time / 浮点数。
func parseTimeCell(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, true
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// normalizedRows 返回去空白、去重排序后的逐行内容, 用于计算内容指纹与跨文件去重。
func normalizedRows(rows [][]string) string {
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = strings.TrimSpace(cell)
		}
		line := strings.Join(cells, "|")
		if strings.Trim(line, "|") == "" {
			continue
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
