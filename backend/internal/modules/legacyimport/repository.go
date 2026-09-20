package legacyimport

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"streetlight/internal/apperr"
	"streetlight/pkg/pagination"
)

// batchSortSpec 批次列表允许的排序字段。
var batchSortSpec = pagination.SortSpec{
	Allowed: map[string]string{
		"batch_no":   "batch_no",
		"created_at": "created_at",
		"file_name":  "file_name",
	},
	Default: "created_at",
}

// Repository 历史台账导入批次的数据访问。
type Repository struct {
	db *gorm.DB
}

// NewRepository 构造导入批次仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) session(ctx context.Context) *gorm.DB {
	return r.db.WithContext(ctx)
}

// Create 写入导入批次。
func (r *Repository) Create(ctx context.Context, entity *ImportBatch) error {
	if err := r.session(ctx).Create(entity).Error; err != nil {
		return fmt.Errorf("写入导入批次失败: %w", err)
	}
	return nil
}

// GetByID 按主键查询批次。
func (r *Repository) GetByID(ctx context.Context, id uint) (*ImportBatch, error) {
	var entity ImportBatch
	err := r.session(ctx).First(&entity, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apperr.NotFound("导入批次不存在: id=%d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("查询导入批次失败: %w", err)
	}
	return &entity, nil
}

// GetByContentHash 按内容指纹查询已成功导入的批次, 不存在时返回 nil。
func (r *Repository) GetByContentHash(ctx context.Context, hash string) (*ImportBatch, error) {
	var entity ImportBatch
	err := r.session(ctx).Where("content_hash = ?", hash).First(&entity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询导入批次失败: %w", err)
	}
	return &entity, nil
}

// List 分页查询导入批次。
func (r *Repository) List(ctx context.Context, page pagination.Query, keyword string) ([]ImportBatch, int64, error) {
	base := func() *gorm.DB {
		statement := r.session(ctx).Model(&ImportBatch{})
		if value := strings.TrimSpace(keyword); value != "" {
			like := "%" + value + "%"
			statement = statement.Where("batch_no LIKE ? OR file_name LIKE ? OR operator LIKE ?", like, like, like)
		}
		return statement
	}

	var total int64
	if err := base().Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计导入批次失败: %w", err)
	}
	items := make([]ImportBatch, 0)
	if err := base().Order(page.OrderClause()).Offset(page.Offset()).Limit(page.Limit()).Find(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("查询导入批次失败: %w", err)
	}
	return items, total, nil
}

// NextBatchNo 生成批次号 DR + YYYYMMDD + 4 位序号。
func (r *Repository) NextBatchNo(ctx context.Context, prefix string) (string, error) {
	var latest string
	err := r.session(ctx).Model(&ImportBatch{}).
		Where("batch_no LIKE ?", prefix+"%").
		Order("batch_no DESC").
		Limit(1).
		Pluck("batch_no", &latest).Error
	if err != nil {
		return "", fmt.Errorf("生成批次号失败: %w", err)
	}
	next := 1
	if latest != "" {
		if value, convErr := strconv.Atoi(strings.TrimPrefix(latest, prefix)); convErr == nil {
			next = value + 1
		}
	}
	return fmt.Sprintf("%s%04d", prefix, next), nil
}

// isUniqueViolation 兼容 sqlite 与 postgres 的唯一约束冲突判断。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") ||
		strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "unique violation")
}
