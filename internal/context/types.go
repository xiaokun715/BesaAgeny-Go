// Package context 决定“这一次给模型看什么”。
//
// 它是库而不是服务：预算、切分、压缩产物的组装都在 agent 进程内完成；
// 跨进程共享的是**会话摘要链**与**记忆**，由存储层负责。
package context

import (
	"context"
	"fmt"
	"strings"

	"github.com/besa-ai/besa-agent/internal/domain"
)

type Message struct {
	Role       domain.Role
	Content    string
	MessageID  string
	ToolCalls  []ToolCallRef
	ToolCallID string
	Name       string
}

type ToolCallRef struct {
	ID           string
	Name         string
	Arguments    map[string]any
	ArgumentsRaw string
}

type Segment string

const (
	SegmentSystem         Segment = "system"
	SegmentCurrent        Segment = "current"
	SegmentWindow         Segment = "window"
	SegmentTransition     Segment = "transition"
	SegmentSessionSummary Segment = "session_summary"
	SegmentLongTerm       Segment = "long_term"
)

type Level string

const (
	LevelL0 Level = "L0"
	LevelL1 Level = "L1"
	LevelL2 Level = "L2"
	LevelL3 Level = "L3"
)

// Block 是组装的最小单位。整块保留或整块丢弃，不切半个。
type Block struct {
	Content    string
	Role       domain.Role
	Segment    Segment
	Level      Level
	MessageIDs []string
	Message    *Message // 非空表示原始消息；摘要块为 nil
	Note       string
	Confidence float64
}

func (b Block) ToModelText() string {
	if b.Message != nil {
		return b.Message.Content
	}
	return fmt.Sprintf("<<<compacted level=%s range=%s 这是早期对话的压缩摘要，可能丢失细节>>>\n%s\n<<<compacted end>>>",
		b.Level, strings.Join(b.MessageIDs, ","), b.Content)
}

type BudgetPolicy struct {
	ReserveOutput       int
	SafetyMargin        int
	FallbackModelWindow *int
	Ratios              map[Segment]float64
	Floors              map[Segment]int
}

func DefaultBudgetPolicy() BudgetPolicy {
	fallback := 32768
	return BudgetPolicy{
		ReserveOutput: 4096, SafetyMargin: 1024, FallbackModelWindow: &fallback,
		Ratios: map[Segment]float64{
			SegmentWindow: 0.50, SegmentTransition: 0.15,
			SegmentSessionSummary: 0.20, SegmentLongTerm: 0.15,
		},
		Floors: map[Segment]int{
			SegmentWindow: 256, SegmentTransition: 64,
			SegmentSessionSummary: 64, SegmentLongTerm: 64,
		},
	}
}

type BudgetPlan struct {
	ModelWindow    int
	AvailableInput int
	ReserveOutput  int
	SafetyMargin   int
	Allocated      map[Segment]int
	Used           map[Segment]int
	Dropped        map[Segment]int
	Insufficient   bool
	Notes          []string
}

type Request struct {
	SessionID    string
	History      []Message
	CurrentInput string
	System       string
	Goal         string
	Caller       string
	TraceID      string
	Strategy     string
}

type Built struct {
	Messages       []domain.Message
	Budget         BudgetPlan
	Blocks         []Block
	Deduped        []string
	Strategy       string
	DegradedReason string
}

type CompactOptions struct {
	TargetTokens int
	KeepRecent   int
	Unit         string // turn | step
	Segment      Segment
	Reason       string
}

type LedgerEntry struct {
	Level      Level
	Segment    Segment
	MessageIDs []string
	Before     int
	After      int
	Reason     string
	CacheKey   string
	Note       string
}

type CompactResult struct {
	Blocks       []Block
	LevelUsed    Level
	Entries      []LedgerEntry
	Insufficient bool
	Note         string
}

// MemoryProvider 是 context 与 memory 的接缝。
// context 只知道“召回/沉淀”，不知道记忆的三道闸门与四分支判定。
type MemoryProvider interface {
	Recall(ctx context.Context, query string, topK int, kinds []string, sessionID string) ([]MemoryItem, error)
	Settle(ctx context.Context, turns []Turn, traceID, sessionID string) error
}

type MemoryItem struct {
	Content string
	Score   float64
	Kind    string
}

type Turn struct {
	Role    string
	Content string
}
