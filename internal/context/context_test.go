package context_test

import (
	"context"
	"strings"
	"testing"

	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/domain"
)

type fakeSummarizer struct{ calls int }

func (f *fakeSummarizer) Complete(context.Context, string, string, int) (string, error) {
	f.calls++
	return "早期步骤摘要：读取了配置并确认了接口", nil
}

func TestBudgetInsufficientWhenFixedExceeds(t *testing.T) {
	policy := ctxpkg.DefaultBudgetPolicy()
	plan := ctxpkg.PlanBudget(policy, 4096, map[ctxpkg.Segment]int{
		ctxpkg.SegmentSystem: 5000, ctxpkg.SegmentCurrent: 5000,
	})
	if !plan.Insufficient {
		t.Fatalf("system+current 超过可用输入时应标记不足：%+v", plan)
	}
	if plan.Used[ctxpkg.SegmentSystem] != 5000 {
		t.Fatalf("固定段不应被截断：%+v", plan.Used)
	}
}

func TestCompactByStepPreservesPairs(t *testing.T) {
	longTool := strings.Repeat("x", 5000)
	messages := []ctxpkg.Message{
		{Role: domain.RoleUser, Content: "任务"},
		{Role: domain.RoleAssistant, ToolCalls: []ctxpkg.ToolCallRef{{ID: "c1", Name: "read"}}},
		{Role: domain.RoleTool, ToolCallID: "c1", Content: longTool},
		{Role: domain.RoleAssistant, ToolCalls: []ctxpkg.ToolCallRef{{ID: "c2", Name: "read"}}},
		{Role: domain.RoleTool, ToolCallID: "c2", Content: "short"},
		{Role: domain.RoleAssistant, Content: "最终答案"},
	}
	compactor := ctxpkg.NewCompactor(nil, "L1", 2000, 0, "v1")
	result := compactor.Compact(context.Background(), messages, ctxpkg.CompactOptions{
		TargetTokens: 10, KeepRecent: 1, Unit: "step", Segment: ctxpkg.SegmentWindow,
	})
	if result.LevelUsed != ctxpkg.LevelL1 {
		t.Fatalf("expected L1, got %s", result.LevelUsed)
	}
	// L1 不删除消息：配对完整性由“块数 == 消息数”保证。
	if len(result.Blocks) != len(messages) {
		t.Fatalf("L1 不应删除消息，blocks=%d messages=%d", len(result.Blocks), len(messages))
	}
	for _, b := range result.Blocks {
		if b.Message != nil && b.Message.Role == domain.RoleTool && b.Message.ToolCallID == "c1" {
			if len([]rune(b.Message.Content)) > 2100 {
				t.Fatalf("工具输出未被 L1 截断：%d", len([]rune(b.Message.Content)))
			}
			if !strings.Contains(b.Message.Content, "L1 截断") {
				t.Fatal("截断标记缺失")
			}
		}
	}
	// 重建后 assistant(tool_calls) 与 tool 结果仍然相邻。
	rebuilt := ctxpkg.Rebuild(result.Blocks)
	for i, m := range rebuilt {
		if m.Role == domain.RoleTool && m.ToolCallID == "c1" {
			if i == 0 || len(rebuilt[i-1].ToolCalls) == 0 {
				t.Fatal("重建后工具结果失去了配对")
			}
		}
	}
}

func TestCompactL2UsesSummary(t *testing.T) {
	sum := &fakeSummarizer{}
	messages := []ctxpkg.Message{
		{Role: domain.RoleUser, Content: strings.Repeat("历史 ", 200)},
		{Role: domain.RoleAssistant, Content: strings.Repeat("回复 ", 200)},
		{Role: domain.RoleUser, Content: "当前"},
		{Role: domain.RoleAssistant, Content: "答案"},
	}
	compactor := ctxpkg.NewCompactor(sum, "L2", 2000, 2, "v1")
	result := compactor.Compact(context.Background(), messages, ctxpkg.CompactOptions{
		TargetTokens: 1, KeepRecent: 1, Unit: "turn", Segment: ctxpkg.SegmentWindow,
	})
	if result.LevelUsed != ctxpkg.LevelL2 {
		t.Fatalf("expected L2, got %s (note=%s)", result.LevelUsed, result.Note)
	}
	if sum.calls == 0 {
		t.Fatal("L2 应调用摘要器")
	}
	foundSummary := false
	for _, b := range result.Blocks {
		if b.Level == ctxpkg.LevelL2 {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatal("缺少 L2 摘要块")
	}
}

func TestBuildMessagesKeepsCurrentLast(t *testing.T) {
	blocks := []ctxpkg.Block{
		{Content: "长期记忆", Role: domain.RoleUser, Segment: ctxpkg.SegmentLongTerm, Level: ctxpkg.LevelL0},
		{Content: "历史", Role: domain.RoleUser, Segment: ctxpkg.SegmentWindow, Level: ctxpkg.LevelL0,
			Message: &ctxpkg.Message{Role: domain.RoleUser, Content: "历史"}},
	}
	msgs := ctxpkg.BuildMessages("系统", "当前任务", blocks)
	if len(msgs) < 3 {
		t.Fatalf("消息过少：%+v", msgs)
	}
	if msgs[0].Role != domain.RoleSystem {
		t.Fatal("system 必须置顶")
	}
	if msgs[len(msgs)-1].Content != "当前任务" {
		t.Fatal("当前输入必须最后")
	}
}

func TestDedupePrefersOriginalOverSummary(t *testing.T) {
	text := strings.Repeat("这是一段足够长的重复内容 ", 10)
	blocks := []ctxpkg.Block{
		{Content: text, Segment: ctxpkg.SegmentSessionSummary, Level: ctxpkg.LevelL2},
		{Content: text, Segment: ctxpkg.SegmentWindow, Level: ctxpkg.LevelL0,
			Message: &ctxpkg.Message{Role: domain.RoleUser, Content: text}},
	}
	kept, dropped := ctxpkg.Dedupe(blocks)
	if len(kept) != 1 || len(dropped) != 1 {
		t.Fatalf("expected one kept one dropped, got %d/%d", len(kept), len(dropped))
	}
	if kept[0].Segment != ctxpkg.SegmentWindow {
		t.Fatalf("原文应优先于摘要，保留的是 %s", kept[0].Segment)
	}
}
