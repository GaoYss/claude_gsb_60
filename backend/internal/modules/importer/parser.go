package importer

import (
	"bytes"
	"encoding/csv"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"streetlight/internal/apperr"
)

const (
	// maxFileSize 限制单次上传的文件大小, 防止异常大文件拖垮服务。
	maxFileSize = 5 << 20 // 5MB
	// maxRows 限制单批数据行数, 保证单事务导入在写超时内完成。
	maxRows = 5000
)

// rawRow 是解析后的一行原始数据, LineNo 为文件中的实际行号(表头为第 1 行)。
type rawRow struct {
	LineNo int
	Cells  []string
}

// parseFile 解析上传的 CSV 内容: 兼容 UTF-8(含 BOM) 与 GBK 编码, 校验表头并跳过空行。
func parseFile(content []byte) ([]rawRow, error) {
	if len(content) == 0 {
		return nil, apperr.BadRequest("文件内容为空, 请按模板填写后再上传")
	}
	if len(content) > maxFileSize {
		return nil, apperr.BadRequest("文件大小超过 %dMB 限制, 请拆分后分批导入", maxFileSize>>20)
	}

	text, err := decodeText(content)
	if err != nil {
		return nil, err
	}

	reader := csv.NewReader(bytes.NewReader(text))
	reader.FieldsPerRecord = -1 // 行长度不一致时由表头校验与逐行校验给出可读原因
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, apperr.BadRequest("文件解析失败, 请确认是逗号分隔的 CSV 文件: %v", err)
	}
	if len(records) == 0 {
		return nil, apperr.BadRequest("文件内容为空, 请按模板填写后再上传")
	}

	if err := checkHeader(records[0]); err != nil {
		return nil, err
	}

	rows := make([]rawRow, 0, len(records)-1)
	for index, record := range records[1:] {
		lineNo := index + 2 // 表头占第 1 行
		cells := normalizeCells(record)
		if isEmptyRow(cells) {
			continue
		}
		rows = append(rows, rawRow{LineNo: lineNo, Cells: cells})
	}

	if len(rows) == 0 {
		return nil, apperr.BadRequest("文件中没有数据行, 请按模板填写后再上传")
	}
	if len(rows) > maxRows {
		return nil, apperr.BadRequest("单批最多导入 %d 行, 当前 %d 行, 请拆分后分批导入", maxRows, len(rows))
	}
	return rows, nil
}

// decodeText 将文件字节解码为 UTF-8 文本: 优先按 UTF-8(去 BOM), 失败时按 GBK 解码,
// 兼容旧系统导出的中文 CSV。
func decodeText(content []byte) ([]byte, error) {
	content = bytes.TrimPrefix(content, []byte(utf8BOM))
	if utf8.Valid(content) {
		return content, nil
	}

	decoded, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), content)
	if err != nil {
		return nil, apperr.BadRequest("文件编码无法识别, 请使用 UTF-8 或 GBK 编码的 CSV 文件")
	}
	return decoded, nil
}

// checkHeader 校验表头与模板列完全一致, 不一致时指出具体位置便于用户修正。
func checkHeader(header []string) error {
	if len(header) != len(templateColumns) {
		return apperr.BadRequest(
			"表头与模板不一致: 应为 %d 列, 实际 %d 列, 请下载最新模板填写",
			len(templateColumns), len(header),
		)
	}
	for index, expect := range templateColumns {
		if got := strings.TrimSpace(header[index]); got != expect {
			return apperr.BadRequest(
				"表头与模板不一致: 第 %d 列应为「%s」, 实际为「%s」, 请下载最新模板填写",
				index+1, expect, got,
			)
		}
	}
	return nil
}

// normalizeCells 去除每个单元格的首尾空白, 并把过短的行补齐到模板列数。
func normalizeCells(record []string) []string {
	cells := make([]string, colCount)
	for index := range cells {
		if index < len(record) {
			cells[index] = strings.TrimSpace(record[index])
		}
	}
	return cells
}

// isEmptyRow 判断整行是否所有单元格均为空。
func isEmptyRow(cells []string) bool {
	for _, cell := range cells {
		if cell != "" {
			return false
		}
	}
	return true
}

// readAll 读取上传文件的全部内容并限制大小。
func readAll(reader io.Reader) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, maxFileSize+1))
	if err != nil {
		return nil, apperr.BadRequest("读取上传文件失败: %v", err)
	}
	return content, nil
}
