package legacyimport

import "streetlight/pkg/pagination"

// BatchListQuery 批次列表查询条件。
type BatchListQuery struct {
	pagination.Params
	Keyword string `form:"keyword"` // 批次号 / 文件名 / 操作人
}

// maxUploadBytes 单次上传文件大小上限(20MB)。
const maxUploadBytes = 20 << 20
