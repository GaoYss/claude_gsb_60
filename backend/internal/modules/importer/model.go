package importer

import "time"

// ImportBatch 台账迁移导入批次。
// 记录一次批量导入的来源文件与核对合计, 导入的故障/维修记录通过 import_batch_id 回指本批次。
type ImportBatch struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	BatchNo  string `gorm:"size:64;uniqueIndex;not null" json:"batch_no"`
	FileName string `gorm:"size:255;not null" json:"file_name"`
	// FileHash 是文件内容的 SHA-256, 用于识别同一份文件的重复导入。
	FileHash string `gorm:"size:64;uniqueIndex;not null" json:"file_hash"`
	// TotalRows 文件中的数据行数(不含表头与空行)。
	TotalRows int `gorm:"not null;default:0" json:"total_rows"`
	// FaultCount 实际导入的故障条数, 导入后与数据行数逐条核对。
	FaultCount int `gorm:"not null;default:0" json:"fault_count"`
	// OpenFaultCount 其中未闭环(无闭环时间)的故障条数。
	OpenFaultCount int `gorm:"not null;default:0" json:"open_fault_count"`
	// RepairCount 实际导入的维修记录条数(已闭环行各产生一条)。
	RepairCount int `gorm:"not null;default:0" json:"repair_count"`
	// TotalCost 维修费用合计, 用于与旧台账按金额核对。
	TotalCost float64   `gorm:"not null;default:0" json:"total_cost"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName 指定表名。
func (ImportBatch) TableName() string { return "import_batch" }
