package importer

import (
	"bytes"
	"encoding/csv"
	"fmt"
)

// 台账迁移导入模板的列定义, 模板生成、表头校验与解析共用同一份定义, 保证三者始终一致。
var templateColumns = []string{
	"路灯编号", // 必填, 必须已存在于路灯台账
	"点位",   // 必填, 必须与台账登记的道路一致
	"故障类型", // 必填, 取值见故障字典
	"故障等级", // 选填, low/normal/high/urgent, 默认 normal
	"故障描述", // 必填
	"上报人",  // 选填
	"上报时间", // 必填, YYYY-MM-DD HH:mm:ss 或 YYYY-MM-DD, 不能晚于当前时间
	"闭环时间", // 选填, 填写表示该故障已闭环, 不能早于上报时间
	"维修人",  // 闭环时必填
	"维修班组", // 选填
	"维修费用", // 选填, 不小于 0 的数字, 仅闭环行可填
	"维修内容", // 选填
	"备注",   // 选填
}

// 列下标, 与 templateColumns 的顺序一一对应。
const (
	colLampCode = iota
	colLocation
	colFaultType
	colFaultLevel
	colDescription
	colReporter
	colReportedAt
	colClosedAt
	colRepairman
	colRepairTeam
	colCost
	colRepairContent
	colRemark
	colCount
)

// utf8BOM 写在模板文件开头, 让 Excel 打开 CSV 时按 UTF-8 识别中文。
const utf8BOM = "\xef\xbb\xbf"

// templateExample 模板中的示例行, 演示已闭环与未闭环两种填法。
var templateExample = [][]string{
	{"LD-00001", "中山路", "灯不亮", "normal", "整灯不亮, 更换驱动电源", "张三", "2026-08-01 09:30:00", "2026-08-01 16:20:00", "李师傅", "维修一班", "186.50", "更换驱动电源", "示例行: 已闭环故障"},
	{"LD-00002", "中山路", "灯光闪烁", "", "夜间间歇性闪烁", "王五", "2026-09-10 20:15:00", "", "", "", "", "", "示例行: 未闭环故障"},
}

// buildTemplate 生成导入模板 CSV(带 BOM, 含表头与两行示例)。
func buildTemplate() ([]byte, error) {
	buffer := &bytes.Buffer{}
	buffer.WriteString(utf8BOM)

	writer := csv.NewWriter(buffer)
	if err := writer.Write(templateColumns); err != nil {
		return nil, fmt.Errorf("生成模板表头失败: %w", err)
	}
	for _, row := range templateExample {
		if err := writer.Write(row); err != nil {
			return nil, fmt.Errorf("生成模板示例行失败: %w", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("生成模板失败: %w", err)
	}
	return buffer.Bytes(), nil
}
