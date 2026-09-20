package legacyimport

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
)

// Module 旧系统故障/维修台账批量导入模块。
type Module struct {
	repository *Repository
	service    *Service
	handler    *Handler
}

// New 构造历史台账导入模块, 复用路灯与故障模块的只读仓储完成校验。
func New(db *gorm.DB, lampRepo *lamp.Repository, faultRepo *fault.Repository) *Module {
	repository := NewRepository(db)
	service := NewService(db, repository, lampRepo, faultRepo)
	return &Module{
		repository: repository,
		service:    service,
		handler:    NewHandler(service),
	}
}

// Name 实现 module.Module 接口。
func (m *Module) Name() string { return "历史台账导入" }

// Models 实现 module.Module 接口。
func (m *Module) Models() []any { return []any{&ImportBatch{}} }

// RegisterRoutes 实现 module.Module 接口。
func (m *Module) RegisterRoutes(api *gin.RouterGroup) {
	group := api.Group("/legacy")
	{
		group.GET("/template", m.handler.Template)
		group.POST("/imports/preview", m.handler.Preview)
		group.POST("/imports", m.handler.Commit)
		group.GET("/imports", m.handler.ListBatches)
		group.GET("/imports/:id", m.handler.GetBatch)
	}
}
