package legacyimport

import "time"

// ImportBatch 记录一次历史台账文件导入的核对信息, 同时承担重复文件识别。
type ImportBatch struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// BatchNo 批次号, DR + YYYYMMDD + 4 位序号。
	BatchNo string `gorm:"size:64;uniqueIndex;not null" json:"batch_no"`
	// FileName 上传时的原始文件名, 仅用于展示与追溯。
	FileName string `gorm:"size:255;not null;default:''" json:"file_name"`
	// FileHash 原始文件字节的 SHA256, 辅助排查。
	FileHash string `gorm:"size:64;not null;default:''" json:"file_hash"`
	// ContentHash 规范化后逐行内容的 SHA256, 作为重复导入判定依据(唯一)。
	ContentHash string `gorm:"size:64;uniqueIndex;not null" json:"content_hash"`

	// TotalRows 文件中参与解析的数据行数(不含表头、空行与模板示例行)。
	TotalRows int `gorm:"not null;default:0" json:"total_rows"`
	// FaultCount / RepairCount 实际写入的故障与维修记录条数。
	FaultCount  int `gorm:"not null;default:0" json:"fault_count"`
	RepairCount int `gorm:"not null;default:0" json:"repair_count"`
	// TotalCost 实际写入维修记录的费用合计, 与文件内金额逐分核对。
	TotalCost float64 `gorm:"not null;default:0" json:"total_cost"`
	// Operator 执行导入的操作人(可选)。
	Operator string `gorm:"size:64;not null;default:''" json:"operator"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定表名。
func (ImportBatch) TableName() string { return "legacy_import_batch" }
