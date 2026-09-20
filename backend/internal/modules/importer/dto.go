package importer

import "streetlight/pkg/pagination"

// RowError 描述一行校验失败的数据, Reasons 为可直接阅读的中文原因。
type RowError struct {
	LineNo   int      `json:"line_no"`   // 文件中的行号(表头为第 1 行)
	LampCode string   `json:"lamp_code"` // 该行的路灯编号, 便于定位
	Reasons  []string `json:"reasons"`   // 校验失败原因, 可能有多条
}

// ImportFailure 是校验失败时返回给前端的结构化数据, 随 400 响应的 data 字段下发。
type ImportFailure struct {
	TotalRows int        `json:"total_rows"` // 文件数据行数
	ErrorRows int        `json:"error_rows"` // 校验失败的行数
	RowErrors []RowError `json:"row_errors"` // 逐行失败原因
}

// ImportResult 是导入接口的成功响应。
type ImportResult struct {
	Duplicated bool         `json:"duplicated"` // true 表示同一份文件已导入过, 本次跳过
	Batch      *ImportBatch `json:"batch"`
}

// BatchListQuery 导入批次列表查询条件。
type BatchListQuery struct {
	pagination.Params
	Keyword string `form:"keyword"` // 批次号 / 文件名
}
