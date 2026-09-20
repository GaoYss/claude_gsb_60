package legacyimport

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// rowData 是一行通过格式解析后的结构化数据, 时间为零值表示未填写。
type rowData struct {
	Line int

	LampCode      string
	RoadName      string
	Lamp          *lamp.Lamp // 校验阶段按编号查到的路灯
	FaultType     string
	FaultLevel    string
	Source        string
	Description   string
	Reporter      string
	ReporterPhone string
	ReportedAt    time.Time

	Status      string // pending/processing/repaired/closed
	ClosedAt    time.Time
	CloseRemark string

	HasRepair    bool
	Repairman    string
	RepairTeam   string
	ContactPhone string
	StartedAt    time.Time
	FinishedAt   time.Time
	Result       string
	Content      string
	Materials    string
	Cost         float64
	Remark       string
}

// RowIssue 是一条数据行的校验结果, 供前端逐行展示可读原因。
type RowIssue struct {
	Row      int      `json:"row"`
	LampCode string   `json:"lamp_code"`
	Messages []string `json:"messages"`
}

// PreviewSummary 是预检/导入结果中的条数与金额合计。
type PreviewSummary struct {
	FaultCount  int     `json:"fault_count"`
	RepairCount int     `json:"repair_count"`
	TotalCost   float64 `json:"total_cost"`
	LampCount   int     `json:"lamp_count"`
}

// validationResult 汇总校验通过的行与逐行错误。
type validationResult struct {
	Rows    []rowData
	Issues  []RowIssue
	Summary PreviewSummary
}

// validateRows 逐行校验并做跨行/与台账之间的联动校验。
//
// 分三遍:
//  1. 单行格式与业务合理性(编号/点位/枚举/时间/闭环与维修信息的一致性);
//  2. 文件内同一路灯的未闭环故障不得重复;
//  3. 不得与台账中既有未闭环故障冲突。
func validateRows(ctx context.Context, lampRepo *lamp.Repository, faultRepo *fault.Repository, rows []rawRow) (*validationResult, error) {
	result := &validationResult{Rows: make([]rowData, 0, len(rows))}

	// 预取本文件涉及的路灯, 按编号缓存, 避免逐行查库。
	lampByCode, err := loadLamps(ctx, lampRepo, rows)
	if err != nil {
		return nil, err
	}

	// 第一遍: 单行校验。
	for _, raw := range rows {
		data := rowData{Line: raw.Line, FaultLevel: fault.LevelNormal, Source: fault.SourceInspection}
		messages := make([]string, 0)
		get := func(header string) string { return strings.TrimSpace(raw.Cells[header]) }

		messages = validateIdentity(&data, get, lampByCode, messages)
		messages = validateFaultFields(&data, get, messages)
		messages = validateTimesAndStatus(&data, get, messages)
		messages = validateRepairFields(&data, get, messages)

		if len(messages) > 0 {
			result.Issues = append(result.Issues, RowIssue{Row: raw.Line, LampCode: data.LampCode, Messages: messages})
			continue
		}
		result.Rows = append(result.Rows, data)
	}

	// 第二遍: 文件内同一盏灯只允许一条未闭环故障, 命中的灯其所有未闭环行全部退回修改。
	openRowsByLamp := make(map[string][]int) // lampCode -> 行在 result.Rows 中的下标
	for i, data := range result.Rows {
		if fault.IsOpen(data.Status) {
			openRowsByLamp[data.LampCode] = append(openRowsByLamp[data.LampCode], i)
		}
	}
	dropIndexes := map[int]string{} // 被剔除的行下标 -> 原因
	for code, indexes := range openRowsByLamp {
		if len(indexes) <= 1 {
			continue
		}
		for _, idx := range indexes {
			dropIndexes[idx] = fmt.Sprintf("路灯 %s 在本文件中存在 %d 条未闭环故障, 同一盏灯同时只允许一条", code, len(indexes))
		}
	}

	// 第三遍: 与台账既有未闭环故障冲突。
	for i, data := range result.Rows {
		if _, dropped := dropIndexes[i]; dropped || !fault.IsOpen(data.Status) || data.Lamp == nil {
			continue
		}
		count, err := faultRepo.CountOpenByLamp(ctx, data.Lamp.ID)
		if err != nil {
			return nil, err
		}
		if count > 0 {
			dropIndexes[i] = fmt.Sprintf("路灯 %s 在现有台账中已有 %d 条未闭环故障, 请先闭环或核对后再导入", data.LampCode, count)
		}
	}

	if len(dropIndexes) > 0 {
		kept := make([]rowData, 0, len(result.Rows)-len(dropIndexes))
		for i, data := range result.Rows {
			if reason, dropped := dropIndexes[i]; dropped {
				result.Issues = append(result.Issues, RowIssue{
					Row: data.Line, LampCode: data.LampCode, Messages: []string{reason},
				})
				continue
			}
			kept = append(kept, data)
		}
		result.Rows = kept
	}

	sort.SliceStable(result.Issues, func(i, j int) bool { return result.Issues[i].Row < result.Issues[j].Row })
	result.Summary = summarize(result.Rows)
	return result, nil
}

// validateIdentity 校验路灯编号与点位。
func validateIdentity(data *rowData, get func(string) string, lampByCode map[string]*lamp.Lamp, messages []string) []string {
	data.LampCode = get(colLampCode)
	data.RoadName = get(colRoadName)
	if data.LampCode == "" {
		messages = append(messages, "路灯编号不能为空")
	} else if device, ok := lampByCode[data.LampCode]; ok {
		data.Lamp = device
	} else {
		messages = append(messages, fmt.Sprintf("路灯编号 %s 不存在, 请先在路灯台账中建档", data.LampCode))
	}
	if data.RoadName == "" {
		messages = append(messages, "点位(道路名称)不能为空")
	} else if data.Lamp != nil && data.RoadName != data.Lamp.RoadName {
		messages = append(messages, fmt.Sprintf("点位与编号不一致: %s 实际位于「%s」, 不能填写「%s」",
			data.LampCode, data.Lamp.RoadName, data.RoadName))
	}
	return messages
}

// validateFaultFields 校验故障类型、等级、来源、描述等字段。
func validateFaultFields(data *rowData, get func(string) string, messages []string) []string {
	data.FaultType = get(colFaultType)
	if data.FaultType == "" {
		messages = append(messages, "故障类型不能为空")
	} else if !contains(fault.FaultTypes(), data.FaultType) {
		messages = append(messages, fmt.Sprintf("故障类型不合法: %s(可取值: %s)", data.FaultType, strings.Join(fault.FaultTypes(), "/")))
	}

	if value := get(colFaultLevel); value != "" {
		if mapped, ok := levelFromCN[value]; ok {
			data.FaultLevel = mapped
		} else {
			messages = append(messages, fmt.Sprintf("紧急程度不合法: %s(可填: %s)", value, strings.Join(levelOptions, "/")))
		}
	}
	if value := get(colSource); value != "" {
		if mapped, ok := sourceFromCN[value]; ok {
			data.Source = mapped
		} else {
			messages = append(messages, fmt.Sprintf("故障来源不合法: %s(可填: %s)", value, strings.Join(sourceOptions, "/")))
		}
	}

	data.Description = get(colDescription)
	if data.Description == "" {
		messages = append(messages, "故障描述不能为空")
	}
	data.Reporter = get(colReporter)
	data.ReporterPhone = get(colReporterPhone)
	data.CloseRemark = get(colCloseRemark)
	data.RepairTeam = get(colRepairTeam)
	data.ContactPhone = get(colContactPhone)
	data.Content = get(colContent)
	data.Materials = get(colMaterials)
	data.Remark = get(colRemark)
	return messages
}

// validateTimesAndStatus 校验上报/闭环时间与闭环状态的合理性。
func validateTimesAndStatus(data *rowData, get func(string) string, messages []string) []string {
	if value := get(colReportedAt); value == "" {
		messages = append(messages, "上报时间不能为空")
	} else if parsed, ok := parseTimeCell(value); ok {
		data.ReportedAt = parsed
	} else {
		messages = append(messages, fmt.Sprintf("上报时间格式不正确: %s(建议 2026-08-01 20:15:00)", value))
	}

	if value := get(colClosedAt); value != "" {
		if parsed, ok := parseTimeCell(value); ok {
			data.ClosedAt = parsed
		} else {
			messages = append(messages, fmt.Sprintf("闭环时间格式不正确: %s", value))
		}
	}

	if value := get(colCloseStatus); value == "" {
		messages = append(messages, "闭环状态不能为空")
	} else if mapped, ok := statusFromCN[value]; ok {
		data.Status = mapped
	} else {
		messages = append(messages, fmt.Sprintf("闭环状态不合法: %s(可填: %s)", value, strings.Join(statusOptions, "/")))
	}

	if !data.ReportedAt.IsZero() && !data.ClosedAt.IsZero() && data.ClosedAt.Before(data.ReportedAt) {
		messages = append(messages, fmt.Sprintf("闭环时间(%s)不能早于上报时间(%s)",
			data.ClosedAt.Format("2006-01-02 15:04"), data.ReportedAt.Format("2006-01-02 15:04")))
	}

	switch data.Status {
	case fault.StatusPending, fault.StatusProcessing:
		if !data.ClosedAt.IsZero() {
			messages = append(messages, fmt.Sprintf("闭环状态为「%s」属于未闭环, 不能填写闭环时间", fault.StatusLabel(data.Status)))
		}
	case fault.StatusRepaired, fault.StatusClosed:
		if data.ClosedAt.IsZero() {
			messages = append(messages, fmt.Sprintf("闭环状态为「%s」时必须填写闭环时间", fault.StatusLabel(data.Status)))
		}
	}
	return messages
}

// validateRepairFields 校验维修信息、时间顺序、维修结果与费用。
//
// 分组规则:
//   - 维修人与开工时间必须同时出现(表示登记过维修过程);
//   - 完工时间与维修结果必须同时出现(表示维修已完成), 且必须先有开工信息;
//   - 只做了故障登记、没有维修过程时全部留空。
func validateRepairFields(data *rowData, get func(string) string, messages []string) []string {
	data.Repairman = get(colRepairman)
	startedText := get(colStartedAt)
	finishedText := get(colFinishedAt)
	resultText := get(colRepairResult)

	startFilled := data.Repairman != "" || startedText != ""
	finishFilled := finishedText != "" || resultText != ""

	if data.Repairman == "" && startedText != "" {
		messages = append(messages, "填写了开工时间时必须填写维修人")
	}
	if data.Repairman != "" && startedText == "" {
		messages = append(messages, "填写了维修人时必须填写开工时间")
	}
	if finishedText != "" && resultText == "" {
		messages = append(messages, "填写了完工时间时必须填写维修结果")
	}
	if resultText != "" && finishedText == "" {
		messages = append(messages, "填写了维修结果时必须填写完工时间")
	}
	if finishFilled && !startFilled {
		messages = append(messages, "维修已完工却缺少开工信息, 请补齐维修人与开工时间")
	}
	if !startFilled && !finishFilled {
		// 没有维修过程时, 状态不允许停在维修中; 已修复必须有结果为已修复的完工维修记录。
		if data.Status == fault.StatusProcessing {
			messages = append(messages, "闭环状态为「维修中」时必须填写维修人与开工时间")
		}
		if data.Status == fault.StatusRepaired {
			messages = append(messages, "闭环状态为「已修复」时, 必须填写维修记录且维修结果为「已修复」")
		}
	}
	if startFilled {
		data.HasRepair = true
		if parsed, ok := parseTimeCell(startedText); ok {
			data.StartedAt = parsed
		} else {
			messages = append(messages, fmt.Sprintf("开工时间格式不正确: %s", startedText))
		}
		if resultText != "" {
			if mapped, ok := resultFromCN[resultText]; ok {
				data.Result = mapped
			} else {
				messages = append(messages, fmt.Sprintf("维修结果不合法: %s(可填: %s)", resultText, strings.Join(resultOptions, "/")))
			}
		}
		if finishedText != "" {
			if parsed, ok := parseTimeCell(finishedText); ok {
				data.FinishedAt = parsed
			} else {
				messages = append(messages, fmt.Sprintf("完工时间格式不正确: %s", finishedText))
			}
		} else if data.Result != "" {
			messages = append(messages, "维修结果需与完工时间同时填写")
		}

		if !data.StartedAt.IsZero() && !data.ReportedAt.IsZero() && data.StartedAt.Before(data.ReportedAt) {
			messages = append(messages, "开工时间不能早于故障上报时间")
		}
		if !data.StartedAt.IsZero() && !data.FinishedAt.IsZero() && data.FinishedAt.Before(data.StartedAt) {
			messages = append(messages, "完工时间不能早于开工时间")
		}
		if !data.FinishedAt.IsZero() && !data.ClosedAt.IsZero() && data.ClosedAt.Before(data.FinishedAt) {
			messages = append(messages, "闭环时间不能早于完工时间")
		}
		if data.Status == fault.StatusPending {
			messages = append(messages, "存在维修记录时闭环状态不能为「待处理」, 应为维修中/已修复/已关闭")
		}
		if data.Status == fault.StatusRepaired && data.Result != repair.ResultFixed {
			messages = append(messages, "闭环状态为「已修复」时, 维修结果必须为「已修复」")
		}
	}

	if value := get(colCost); value != "" {
		amount, _, err := formatCost(value)
		if err != nil {
			messages = append(messages, err.Error())
		} else {
			if amount > 0 && !startFilled {
				messages = append(messages, "填写了维修费用但缺少维修信息, 请补齐维修人与开工时间, 或删除费用")
			}
			data.Cost = amount
		}
	}
	return messages
}

// loadLamps 批量预取文件中出现的路灯; 不存在的编号不进缓存, 由单行校验报出。
func loadLamps(ctx context.Context, lampRepo *lamp.Repository, rows []rawRow) (map[string]*lamp.Lamp, error) {
	codes := make(map[string]struct{})
	for _, row := range rows {
		if code := strings.TrimSpace(row.Cells[colLampCode]); code != "" {
			codes[code] = struct{}{}
		}
	}
	result := make(map[string]*lamp.Lamp, len(codes))
	for code := range codes {
		device, err := lampRepo.GetByCode(ctx, code)
		if err != nil {
			if businessErr, ok := apperr.As(err); ok && businessErr.Status == http.StatusNotFound {
				continue
			}
			return nil, err
		}
		result[code] = device
	}
	return result, nil
}

// summarize 统计可导入行的故障条数、维修条数与费用合计。
func summarize(rows []rowData) PreviewSummary {
	summary := PreviewSummary{FaultCount: len(rows)}
	lamps := make(map[string]struct{})
	cost := 0.0
	for _, data := range rows {
		lamps[data.LampCode] = struct{}{}
		if !data.HasRepair {
			continue
		}
		summary.RepairCount++
		cost += data.Cost
	}
	summary.LampCount = len(lamps)
	summary.TotalCost = math.Round(cost*100) / 100
	return summary
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
