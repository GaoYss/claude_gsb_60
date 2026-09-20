package importer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/repair"
	"streetlight/pkg/pagination"
)

// Repository 负责导入批次的数据访问, 以及导入结果的事务内核对。
type Repository struct {
	db *gorm.DB
}

// NewRepository 构造台账迁移仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) session(ctx context.Context) *gorm.DB {
	return r.db.WithContext(ctx)
}

// WithTx 返回绑定到指定事务的仓储。
func (r *Repository) WithTx(tx *gorm.DB) *Repository {
	return &Repository{db: tx}
}

// Create 新增导入批次。
func (r *Repository) Create(ctx context.Context, entity *ImportBatch) error {
	if err := r.session(ctx).Create(entity).Error; err != nil {
		return fmt.Errorf("创建导入批次失败: %w", err)
	}
	return nil
}

// NextBatchSequence 返回指定前缀下可用的下一个批次流水号。
func (r *Repository) NextBatchSequence(ctx context.Context, prefix string) (int, error) {
	var latest string
	err := r.session(ctx).Model(&ImportBatch{}).
		Where("batch_no LIKE ?", prefix+"%").
		Order("batch_no DESC").
		Limit(1).
		Pluck("batch_no", &latest).Error
	if err != nil {
		return 0, fmt.Errorf("生成导入批次号失败: %w", err)
	}
	if latest == "" {
		return 1, nil
	}
	value, convErr := strconv.Atoi(strings.TrimPrefix(latest, prefix))
	if convErr != nil {
		return 1, nil
	}
	return value + 1, nil
}

// GetByHash 按文件哈希查询导入批次, 不存在时返回 nil, 用于识别重复导入。
func (r *Repository) GetByHash(ctx context.Context, hash string) (*ImportBatch, error) {
	var entity ImportBatch
	err := r.session(ctx).Where("file_hash = ?", hash).First(&entity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询导入批次失败: %w", err)
	}
	return &entity, nil
}

// GetByID 按主键查询导入批次。
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

// List 分页查询导入批次。
func (r *Repository) List(ctx context.Context, keyword string, page pagination.Query) ([]ImportBatch, int64, error) {
	base := func() *gorm.DB {
		statement := r.session(ctx).Model(&ImportBatch{})
		if value := strings.TrimSpace(keyword); value != "" {
			like := "%" + value + "%"
			statement = statement.Where("batch_no LIKE ? OR file_name LIKE ?", like, like)
		}
		return statement
	}

	var total int64
	if err := base().Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计导入批次失败: %w", err)
	}

	entities := make([]ImportBatch, 0)
	if err := base().Order(page.OrderClause()).Offset(page.Offset()).Limit(page.Limit()).Find(&entities).Error; err != nil {
		return nil, 0, fmt.Errorf("查询导入批次失败: %w", err)
	}
	return entities, total, nil
}

// CountFaultsByBatch 统计批次实际写入的故障条数, 用于提交前核对没有漏项。
func (r *Repository) CountFaultsByBatch(ctx context.Context, batchID uint) (int64, error) {
	var total int64
	err := r.session(ctx).Model(&fault.Fault{}).
		Where("import_batch_id = ?", batchID).
		Count(&total).Error
	if err != nil {
		return 0, fmt.Errorf("核对导入故障条数失败: %w", err)
	}
	return total, nil
}

// ReconcileRepairsByBatch 统计批次实际写入的维修条数与费用合计, 用于提交前核对没有漏项。
func (r *Repository) ReconcileRepairsByBatch(ctx context.Context, batchID uint) (int64, float64, error) {
	type summary struct {
		Total int64
		Cost  float64
	}
	var row summary
	err := r.session(ctx).Model(&repair.Repair{}).
		Select("COUNT(*) AS total, COALESCE(SUM(cost), 0) AS cost").
		Where("import_batch_id = ?", batchID).
		Scan(&row).Error
	if err != nil {
		return 0, 0, fmt.Errorf("核对导入维修记录失败: %w", err)
	}
	return row.Total, row.Cost, nil
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
