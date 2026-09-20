package importer

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"streetlight/internal/apperr"
	"streetlight/internal/httpx"
	"streetlight/internal/response"
)

// Handler 处理台账迁移导入相关的 HTTP 请求。
type Handler struct {
	service *Service
}

// NewHandler 构造台账迁移处理器。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Template 下载导入模板 CSV。
func (h *Handler) Template(c *gin.Context) {
	content, err := buildTemplate()
	if err != nil {
		response.Fail(c, err)
		return
	}
	filename := url.QueryEscape("台账迁移导入模板.csv")
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+filename)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", content)
}

// Import 上传台账文件并执行批量导入。
// 校验失败时返回 400, data 中携带逐行可读原因, 整批不写库;
// 同一份文件重复上传时返回 200 且 duplicated=true, 表示已跳过。
func (h *Handler) Import(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, apperr.BadRequest("请选择要上传的台账文件(表单字段 file)"))
		return
	}
	if file.Size > maxFileSize {
		response.Fail(c, apperr.BadRequest("文件大小超过 %dMB 限制, 请拆分后分批导入", maxFileSize>>20))
		return
	}

	opened, err := file.Open()
	if err != nil {
		response.Fail(c, apperr.BadRequest("读取上传文件失败: %v", err))
		return
	}
	defer opened.Close()

	content, err := readAll(opened)
	if err != nil {
		response.Fail(c, err)
		return
	}

	result, err := h.service.Import(c.Request.Context(), file.Filename, content)
	if err != nil {
		var validationErr *ValidationError
		if errors.As(err, &validationErr) {
			c.JSON(http.StatusBadRequest, response.Envelope{
				Code:    apperr.CodeInvalidArgument,
				Message: validationErr.Error(),
				Data:    validationErr.Failure,
			})
			return
		}
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// Batches 分页查询导入批次。
func (h *Handler) Batches(c *gin.Context) {
	var query BatchListQuery
	if err := httpx.BindQuery(c, &query); err != nil {
		response.Fail(c, err)
		return
	}
	items, total, page, err := h.service.ListBatches(c.Request.Context(), query)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, response.NewPageData(items, total, page.Page, page.PageSize))
}

// Batch 查询导入批次详情。
func (h *Handler) Batch(c *gin.Context) {
	id, err := httpx.ParseID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	entity, err := h.service.GetBatch(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, entity)
}
