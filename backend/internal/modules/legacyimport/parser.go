package legacyimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// maxParseRows 限制单次导入的数据行数, 防止异常文件拖垮服务。
const maxParseRows = 5000

// rawRow 是一行经过表头映射后的原始数据, 值均为已转换为字符串的单元格内容。
type rawRow struct {
	Line  int // Excel 中的物理行号(含表头, 数据从 2 开始)
	Cells map[string]string
}

// parsedFile 是工作簿解析结果。
type parsedFile struct {
	FileName       string
	FileHash       string // 原始字节 SHA256
	ContentHash    string // 规范化逐行内容 SHA256
	Rows           []rawRow
	ExampleSkipped int // 自动识别并跳过的模板示例行数
}

// exampleSignature 通过稳定的文本字段识别模板自带的示例行。
var exampleSignature = map[string]string{
	colLampCode:    "LD-000001",
	colDescription: "整灯不亮, 疑似驱动电源故障",
	colReporter:    "张工",
	colRepairman:   "李师傅",
}

// parseWorkbook 读取上传的 Excel 字节, 完成表头映射、时间/数字单元格转换与指纹计算。
func parseWorkbook(data []byte, fileName string) (*parsedFile, error) {
	file, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("文件不是有效的 Excel 工作簿, 请使用模板(.xlsx): %w", err)
	}
	defer file.Close()

	sheet := templateSheet
	if index, err := file.GetSheetIndex(sheet); err != nil || index < 0 {
		sheets := file.GetSheetList()
		if len(sheets) == 0 {
			return nil, fmt.Errorf("工作簿中没有任何工作表")
		}
		sheet = sheets[0] // 兼容用户改了工作表名的情况, 仍按表头识别
	}

	grid, err := file.GetRows(sheet)
	if err != nil {
		return nil, fmt.Errorf("读取工作表 %s 失败: %w", sheet, err)
	}
	if len(grid) == 0 {
		return nil, fmt.Errorf("工作表 %s 为空, 请按模板填写后再上传", sheet)
	}

	headerIndex, err := mapHeaders(grid[0])
	if err != nil {
		return nil, err
	}

	date1904 := false
	if options, err := file.GetWorkbookProps(); err == nil && options.Date1904 != nil {
		date1904 = *options.Date1904
	}

	rows := make([]rawRow, 0, len(grid)-1)
	exampleSkipped := 0
	for lineNumber, physical := range grid[1:] {
		line := lineNumber + 2
		if line-1 > maxParseRows+1 {
			return nil, fmt.Errorf("数据行数超过单次上限 %d 行, 请拆分后导入", maxParseRows)
		}

		cells := make(map[string]string, len(headerIndex))
		blank := true
		for header, index := range headerIndex {
			value := ""
			if index < len(physical) {
				axis, _ := excelize.CoordinatesToCellName(index+1, line)
				value, err = readCell(file, sheet, axis, header, date1904)
				if err != nil {
					return nil, fmt.Errorf("第 %d 行 %s 列: %w", line, header, err)
				}
			}
			cells[header] = value
			if strings.TrimSpace(value) != "" {
				blank = false
			}
		}
		if blank {
			continue
		}
		if isExampleRow(cells) {
			exampleSkipped++
			continue
		}
		rows = append(rows, rawRow{Line: line, Cells: cells})
	}

	if len(rows) == 0 {
		return nil, fmt.Errorf("没有可导入的数据行(空行与示例行已忽略), 请按模板填写后再上传")
	}

	rawForHash := make([][]string, 0, len(rows))
	for _, row := range rows {
		ordered := make([]string, len(templateHeaders()))
		for i, header := range templateHeaders() {
			ordered[i] = row.Cells[header]
		}
		rawForHash = append(rawForHash, ordered)
	}

	fileSum := sha256.Sum256(data)
	contentSum := sha256.Sum256([]byte(normalizedRows(rawForHash)))

	return &parsedFile{
		FileName:       fileName,
		FileHash:       hex.EncodeToString(fileSum[:]),
		ContentHash:    hex.EncodeToString(contentSum[:]),
		Rows:           rows,
		ExampleSkipped: exampleSkipped,
	}, nil
}

// mapHeaders 校验表头并建立 表头 -> 列下标 的映射, 允许调整列顺序。
func mapHeaders(headerRow []string) (map[string]int, error) {
	indexByHeader := make(map[string]int, len(templateHeaders()))
	for i, cell := range headerRow {
		name := strings.TrimSpace(strings.TrimPrefix(cell, "*"))
		if name != "" {
			indexByHeader[name] = i
		}
	}
	missing := make([]string, 0)
	for _, header := range templateHeaders() {
		if _, ok := indexByHeader[header]; !ok {
			missing = append(missing, header)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("表头缺少 %d 列: %s; 请重新下载最新模板", len(missing), strings.Join(missing, "、"))
	}
	return indexByHeader, nil
}

// readCell 读取单元格并按列类型转换: 时间列兼容 Excel 日期序列值, 费用列取原始数字。
func readCell(file *excelize.File, sheet, axis, header string, date1904 bool) (string, error) {
	cellType, err := file.GetCellType(sheet, axis)
	if err != nil {
		return "", err
	}

	raw, err := file.GetCellValue(sheet, axis, excelize.Options{RawCellValue: cellType == excelize.CellTypeNumber})
	if err != nil {
		return "", err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	if cellType == excelize.CellTypeNumber {
		switch header {
		case colReportedAt, colClosedAt, colStartedAt, colFinishedAt:
			serial, convErr := strconv.ParseFloat(raw, 64)
			if convErr != nil {
				return "", fmt.Errorf("日期数值无法识别: %s", raw)
			}
			parsed, convErr := excelize.ExcelDateToTime(serial, date1904)
			if convErr != nil {
				return "", fmt.Errorf("日期数值无法识别: %s", raw)
			}
			return parsed.Format("2006-01-02 15:04:05"), nil
		case colCost:
			// 统一成两位小数字符串, 便于指纹稳定与金额核对。
			amount, convErr := strconv.ParseFloat(raw, 64)
			if convErr != nil {
				return "", fmt.Errorf("维修费用不是数字: %s", raw)
			}
			return strconv.FormatFloat(amount, 'f', 2, 64), nil
		}
	}
	return raw, nil
}

// isExampleRow 判断是否为模板内置示例行。
func isExampleRow(cells map[string]string) bool {
	for header, expect := range exampleSignature {
		if strings.TrimSpace(cells[header]) != expect {
			return false
		}
	}
	return true
}

// formatCost 将模板中的费用字符串规范化为浮点与两位小数串。
func formatCost(value string) (float64, string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "￥")
	value = strings.TrimPrefix(value, "¥")
	value = strings.TrimSuffix(value, "元")
	value = strings.ReplaceAll(value, ",", "")
	if value == "" {
		return 0, "", nil
	}
	amount, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, "", fmt.Errorf("维修费用不是数字: %s", value)
	}
	if amount < 0 {
		return 0, "", fmt.Errorf("维修费用不能为负数: %s", value)
	}
	return amount, strconv.FormatFloat(amount, 'f', 2, 64), nil
}
