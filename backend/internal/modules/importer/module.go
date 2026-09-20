package importer

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// Module 台账迁移模块, 负责旧系统故障与维修台账的一次性批量导入。
type Module struct {
	service *Service
	handler *Handler
}

// New 构造台账迁移模块。
func New(
	db *gorm.DB,
	lamps *lamp.Repository,
	faults *fault.Repository,
	faultService *fault.Service,
	repairs *repair.Repository,
) *Module {
	repository := NewRepository(db)
	service := NewService(db, repository, lamps, faults, faultService, repairs)
	return &Module{
		service: service,
		handler: NewHandler(service),
	}
}

// Name 实现 module.Module 接口。
func (m *Module) Name() string { return "台账迁移" }

// Models 实现 module.Module 接口。
func (m *Module) Models() []any { return []any{&ImportBatch{}} }

// RegisterRoutes 实现 module.Module 接口。
func (m *Module) RegisterRoutes(api *gin.RouterGroup) {
	group := api.Group("/imports")
	{
		group.GET("/template", m.handler.Template)
		group.POST("/legacy", m.handler.Import)
		group.GET("/batches", m.handler.Batches)
		group.GET("/batches/:id", m.handler.Batch)
	}
}
