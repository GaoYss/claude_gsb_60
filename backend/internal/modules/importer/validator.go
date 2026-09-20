package importer

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
)

// validRow 是一行通过校验、等待落库的数据。
type validRow struct {
	lineNo      int
	lamp        *lamp.Lamp
	level       string
	faultType   string
	description string
	reporter    string
	reportedAt  time.Time
	closed      bool
	closedAt    time.Time
	repairman   string
	repairTeam  string
	cost        float64
	content     string
	remark      string
}

// validateRows 逐行校验解析结果, 返回可落库的数据与全部行级错误。
// lampsByCode 为台账中存在的路灯, openByLamp 为系统中已存在的未闭环故障(按路灯 ID)。
func validateRows(rows []rawRow, lampsByCode map[string]*lamp.Lamp, openByLamp map[uint]*fault.Fault, now time.Time) ([]validRow, []RowError) {
	valid := make([]validRow, 0, len(rows))
	rowErrors := make([]RowError, 0)

	// 文件内未闭环故障按路灯去重, 记录首次出现的行号用于报错定位。
	openLineByLamp := make(map[uint]int)

	for _, row := range rows {
		item, reasons := validateRow(row, lampsByCode, now)

		// 未闭环行(闭环时间为空): 与系统存量未闭环故障、文件内其他未闭环行去重。
		// 即使行内还有其他字段错误也一并报出, 让用户一次改完再重传。
		device := lampsByCode[cell(row.Cells, colLampCode)]
		if device != nil && cell(row.Cells, colClosedAt) == "" {
			if existing, ok := openByLamp[device.ID]; ok {
				reasons = append(reasons, fmt.Sprintf(
					"该路灯在系统中已存在未闭环故障 %s, 同一盏灯不允许重复登记未闭环故障",
					existing.FaultNo,
				))
			}
			if firstLine, ok := openLineByLamp[device.ID]; ok {
				reasons = append(reasons, fmt.Sprintf(
					"与第 %d 行重复: 同一盏灯在同一批中只能有一条未闭环故障",
					firstLine,
				))
			} else {
				openLineByLamp[device.ID] = row.LineNo
			}
		}

		if len(reasons) > 0 {
			rowErrors = append(rowErrors, RowError{
				LineNo:   row.LineNo,
				LampCode: cell(row.Cells, colLampCode),
				Reasons:  reasons,
			})
			continue
		}
		valid = append(valid, *item)
	}
	return valid, rowErrors
}

// validateRow 校验单行字段, 返回规范化后的数据与全部失败原因(一次性收集, 便于用户改完重传)。
func validateRow(row rawRow, lampsByCode map[string]*lamp.Lamp, now time.Time) (*validRow, []string) {
	cells := row.Cells
	reasons := make([]string, 0)

	// 路灯编号与点位: 编号必须存在于台账, 点位必须与台账登记的道路一致。
	var device *lamp.Lamp
	code := cell(cells, colLampCode)
	if code == "" {
		reasons = append(reasons, "路灯编号不能为空")
	} else if found, ok := lampsByCode[code]; ok {
		device = found
	} else {
		reasons = append(reasons, fmt.Sprintf("路灯编号不存在: %s", code))
	}

	location := cell(cells, colLocation)
	if location == "" {
		reasons = append(reasons, "点位不能为空")
	} else if device != nil && location != device.RoadName {
		reasons = append(reasons, fmt.Sprintf("点位与台账不符: 该灯登记位置为「%s」", device.RoadName))
	}

	// 故障类型与等级。
	faultType := cell(cells, colFaultType)
	if faultType == "" {
		reasons = append(reasons, "故障类型不能为空")
	} else if !isValidFaultType(faultType) {
		reasons = append(reasons, fmt.Sprintf("非法的故障类型: %s(可选: %s)", faultType, strings.Join(fault.FaultTypes(), "/")))
	}

	level := cell(cells, colFaultLevel)
	if level == "" {
		level = fault.LevelNormal
	} else if !isValidLevel(level) {
		reasons = append(reasons, fmt.Sprintf("非法的故障等级: %s(可选: %s)", level, strings.Join(fault.Levels(), "/")))
	}

	description := cell(cells, colDescription)
	if description == "" {
		reasons = append(reasons, "故障描述不能为空")
	} else if len(description) > 512 {
		reasons = append(reasons, "故障描述超出 512 字限制")
	}

	// 上报时间与闭环时间的合理性。
	reportedAtText := cell(cells, colReportedAt)
	var reportedAt time.Time
	if reportedAtText == "" {
		reasons = append(reasons, "上报时间不能为空")
	} else if parsed, err := parseDateTime(reportedAtText); err != nil {
		reasons = append(reasons, fmt.Sprintf("上报时间格式不正确, 应为 YYYY-MM-DD HH:mm:ss: %s", reportedAtText))
	} else if parsed.After(now) {
		reasons = append(reasons, "上报时间不能晚于当前时间")
	} else {
		reportedAt = parsed
	}

	closed := false
	var closedAt time.Time
	closedAtText := cell(cells, colClosedAt)
	if closedAtText != "" {
		if parsed, err := parseDateTime(closedAtText); err != nil {
			reasons = append(reasons, fmt.Sprintf("闭环时间格式不正确, 应为 YYYY-MM-DD HH:mm:ss: %s", closedAtText))
		} else {
			if !reportedAt.IsZero() && parsed.Before(reportedAt) {
				reasons = append(reasons, "闭环时间不能早于上报时间")
			}
			if parsed.After(now) {
				reasons = append(reasons, "闭环时间不能晚于当前时间")
			}
			closed = true
			closedAt = parsed
		}
	}

	// 维修信息仅允许已闭环行填写, 已闭环行必须填写维修人。
	repairman := cell(cells, colRepairman)
	repairTeam := cell(cells, colRepairTeam)
	costText := cell(cells, colCost)
	content := cell(cells, colRepairContent)
	if closed {
		if repairman == "" {
			reasons = append(reasons, "已闭环的故障必须填写维修人")
		}
	} else if repairman != "" || repairTeam != "" || costText != "" || content != "" {
		reasons = append(reasons, "未闭环的故障不应填写维修信息(维修人/维修班组/维修费用/维修内容), 如需记录请先填写闭环时间")
	}

	cost := 0.0
	if costText != "" {
		if value, err := parseMoney(costText); err != nil {
			reasons = append(reasons, fmt.Sprintf("维修费用必须是数字: %s", costText))
		} else if value < 0 {
			reasons = append(reasons, "维修费用不能为负数")
		} else {
			cost = value
		}
	}

	// 长度限制与模型字段保持一致。
	if value := cell(cells, colReporter); len(value) > 64 {
		reasons = append(reasons, "上报人超出 64 字限制")
	}
	if len(repairman) > 64 {
		reasons = append(reasons, "维修人超出 64 字限制")
	}
	if len(repairTeam) > 64 {
		reasons = append(reasons, "维修班组超出 64 字限制")
	}
	if len(content) > 512 {
		reasons = append(reasons, "维修内容超出 512 字限制")
	}
	if value := cell(cells, colRemark); len(value) > 255 {
		reasons = append(reasons, "备注超出 255 字限制")
	}

	if len(reasons) > 0 || device == nil {
		return nil, reasons
	}
	return &validRow{
		lineNo:      row.LineNo,
		lamp:        device,
		level:       level,
		faultType:   faultType,
		description: description,
		reporter:    cell(cells, colReporter),
		reportedAt:  reportedAt,
		closed:      closed,
		closedAt:    closedAt,
		repairman:   repairman,
		repairTeam:  repairTeam,
		cost:        cost,
		content:     content,
		remark:      cell(cells, colRemark),
	}, nil
}

// cell 安全地读取单元格内容。
func cell(cells []string, index int) string {
	if index < 0 || index >= len(cells) {
		return ""
	}
	return cells[index]
}

// parseDateTime 解析台账中的时间, 兼容常见的中式日期时间写法。
func parseDateTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	layouts := []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02 15:04:05",
		"2006/01/02 15:04",
		"2006/01/02",
	}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析的时间: %s", value)
}

// parseMoney 解析金额, 兼容千分位与「¥/元」等常见写法。
func parseMoney(value string) (float64, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "¥")
	value = strings.TrimPrefix(value, "￥")
	value = strings.TrimSuffix(value, "元")
	value = strings.ReplaceAll(value, ",", "")
	return strconv.ParseFloat(strings.TrimSpace(value), 64)
}

// isValidFaultType 校验故障类型是否在故障字典内。
func isValidFaultType(value string) bool {
	for _, item := range fault.FaultTypes() {
		if item == value {
			return true
		}
	}
	return false
}

// isValidLevel 校验故障等级是否在故障字典内。
func isValidLevel(value string) bool {
	for _, item := range fault.Levels() {
		if item == value {
			return true
		}
	}
	return false
}
