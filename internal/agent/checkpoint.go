package agent

import (
	"context"
	"time"
)

// Checkpoint 是 append-only 快照。FormatVersion=1；未知版本必须报错，
// 不能按旧格式猜。
type Checkpoint struct {
	RunID         string
	FormatVersion int
	StepIndex     int
	NodeID        string
	Status        RunStatus
	State         map[string]any
	Artifacts     map[string]any
	SessionID     string
	Caller        string
	CreatedAt     time.Time
}

type CheckpointStore interface {
	Latest(ctx context.Context, runID string) (*Checkpoint, error)
	Save(ctx context.Context, cp Checkpoint) error
}

// ToolFinder 用于续跑判定：查“这一步用过的工具调用里，哪些不确定”。
// 判定由工具层做（按租约把超时 in_flight 判成 uncertain），这里不重新实现。
type ToolFinder interface {
	Find(ctx context.Context, scope, idemKey string, lease time.Duration) (*ToolExecutionView, error)
}

type ToolExecutionView struct {
	ToolName  string
	State     string
	Uncertain bool
}

// EventEmitter 与 gateway/tool 的事件出口同形。
type Emitter interface {
	Emit(ctx context.Context, name string, payload map[string]any)
}
