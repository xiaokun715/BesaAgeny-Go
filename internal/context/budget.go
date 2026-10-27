package context

import (
	"fmt"
	"sort"

	"github.com/besa-ai/besa-agent/internal/domain"
)

// DynamicSegments 是参与配额分配的顺序。
var DynamicSegments = []Segment{SegmentWindow, SegmentTransition, SegmentSessionSummary, SegmentLongTerm}

func EstimateTokens(text string) int {
	n := 0
	for range text {
		n++
	}
	return (n + 3) / 4
}

func PlanBudget(policy BudgetPolicy, modelWindow int, demands map[Segment]int) BudgetPlan {
	if policy.Ratios == nil {
		policy = DefaultBudgetPolicy()
	}
	plan := BudgetPlan{
		ModelWindow: modelWindow, ReserveOutput: policy.ReserveOutput, SafetyMargin: policy.SafetyMargin,
		Allocated: map[Segment]int{}, Used: map[Segment]int{}, Dropped: map[Segment]int{},
	}
	available := modelWindow - policy.ReserveOutput - policy.SafetyMargin
	if available < 0 {
		available = 0
	}
	plan.AvailableInput = available

	fixed := demands[SegmentSystem] + demands[SegmentCurrent]
	plan.Used[SegmentSystem] = demands[SegmentSystem]
	plan.Used[SegmentCurrent] = demands[SegmentCurrent]
	if fixed > available {
		plan.Insufficient = true
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"固定段（system + 当前输入）需要 %d token，但可用输入只有 %d —— 不截断它们，本次预算不足", fixed, available))
		return plan
	}
	remain := available - fixed
	for _, seg := range DynamicSegments {
		quota := int(float64(remain) * policy.Ratios[seg])
		demand := demands[seg]
		alloc := min(quota, demand)
		plan.Allocated[seg] = alloc
		remain -= alloc
	}
	// 剩余按固定顺序补给仍未满足的段。
	for _, seg := range DynamicSegments {
		if remain <= 0 {
			break
		}
		demand := demands[seg]
		gap := demand - plan.Allocated[seg]
		if gap <= 0 {
			continue
		}
		add := min(gap, remain)
		plan.Allocated[seg] += add
		remain -= add
	}
	for _, seg := range DynamicSegments {
		demand := demands[seg]
		alloc := plan.Allocated[seg]
		floor := policy.Floors[seg]
		if demand > alloc && alloc < floor {
			plan.Used[seg] = 0
			plan.Dropped[seg] = demand
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s 段配额 %d 低于下限 %d —— 整段不注入", seg, alloc, floor))
			continue
		}
		plan.Used[seg] = min(demand, alloc)
		if demand > alloc {
			plan.Dropped[seg] = demand - alloc
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s 段被截到 %d token（需求 %d）", seg, alloc, demand))
		}
	}
	return plan
}

// groupBy 按 turn / step 分组。
//
// by_turn：一条 user 开始新的一轮；
// by_step：一条 assistant 开始新的一步 —— 一次运行内部全在同一轮，必须用 step。
func groupBy(messages []Message, unit string) [][]Message {
	if len(messages) == 0 {
		return nil
	}
	boundary := domain.RoleUser
	if unit == "step" {
		boundary = domain.RoleAssistant
	}
	var groups [][]Message
	var current []Message
	for _, m := range messages {
		if m.Role == boundary {
			if len(current) > 0 {
				groups = append(groups, current)
			}
			current = []Message{m}
			continue
		}
		current = append(current, m)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	// 开头非边界消息自成一组（保持原顺序）。
	if len(groups) > 1 && len(groups[0]) == 0 {
		groups = groups[1:]
	}
	return groups
}

func flatten(groups [][]Message) []Message {
	var out []Message
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func sortSegments(segs []Segment) []Segment {
	out := append([]Segment{}, segs...)
	sort.SliceStable(out, func(i, j int) bool { return segmentRank(out[i]) < segmentRank(out[j]) })
	return out
}

func segmentRank(s Segment) int {
	switch s {
	case SegmentWindow:
		return 0
	case SegmentTransition:
		return 1
	case SegmentLongTerm:
		return 2
	case SegmentSessionSummary:
		return 3
	}
	return 4
}
