package legacyimport

import (
	"bytes"
	"io"
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"

	"streetlight/internal/apperr"
	"streetlight/internal/httpx"
	"streetlight/internal/response"
)

// Handler 处理历史台账模板下载、预检、导入与批次查询请求。
type Handler struct {
	service *Service
}

// NewHandler 构造历史台账导入处理器。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Template 下载导入模板。
func (h *Handler) Template(c *gin.Context) {
	data, fileName, err := BuildTemplate()
	if err != nil {
		response.Fail(c, apperr.Internal("生成导入模板失败: %s", err.Error()))
		return
	}
	c.DataFromReader(
		http.StatusOK,
		int64(len(data)),
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		bytes.NewReader(data),
		map[string]string{"Content-Disposition": mime.FormatMediaType("attachment", map[string]string{"filename": fileName})},
	)
}

// Preview 上传文件并逐行校验, 只返回问题与核对合计, 不写库。
func (h *Handler) Preview(c *gin.Context) {
	data, fileName, err := readUpload(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	result, err := h.service.Preview(c.Request.Context(), data, fileName)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// Commit 确认导入: 整批事务写入, 重复文件自动跳过。
func (h *Handler) Commit(c *gin.Context) {
	data, fileName, err := readUpload(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	operator := c.PostForm("operator")
	result, err := h.service.Commit(c.Request.Context(), data, fileName, operator)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// ListBatches 查询历史导入批次。
func (h *Handler) ListBatches(c *gin.Context) {
	var query BatchListQuery
	if err := httpx.BindQuery(c, &query); err != nil {
		response.Fail(c, err)
		return
	}
	items, total, page, err := h.service.ListBatches(c.Request.Context(), query.Params, query.Keyword)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, response.NewPageData(items, total, page.Page, page.PageSize))
}

// GetBatch 查询单个批次的核对结果。
func (h *Handler) GetBatch(c *gin.Context) {
	id, err := httpx.ParseID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	batch, err := h.service.GetBatch(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, batch)
}

// readUpload 从 multipart 请求中读取 file 字段的全部字节并限制大小。
func readUpload(c *gin.Context) ([]byte, string, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBytes)

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return nil, "", apperr.BadRequest("请通过 file 字段上传 .xlsx 文件: %s", err.Error())
	}
	if fileHeader.Size > maxUploadBytes {
		return nil, "", apperr.BadRequest("文件超过 20MB 上限, 请拆分后导入")
	}

	src, err := fileHeader.Open()
	if err != nil {
		return nil, "", apperr.BadRequest("读取上传文件失败: %s", err.Error())
	}
	defer src.Close()

	data, err := io.ReadAll(io.LimitReader(src, maxUploadBytes+1))
	if err != nil {
		return nil, "", apperr.BadRequest("读取上传文件失败: %s", err.Error())
	}
	if int64(len(data)) > maxUploadBytes {
		return nil, "", apperr.BadRequest("文件超过 20MB 上限, 请拆分后导入")
	}
	return data, fileHeader.Filename, nil
}
