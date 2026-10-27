// Package memstore 是单机 / 测试用的内存实现。
//
// ⚠ 它**不是生产存储**：进程重启即丢失。CLI 默认用它，
// 但启动时会明确提示“会话历史只活在进程里”。
package memstore

import (
	"context"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
	"github.com/besa-ai/besa-agent/internal/tool"
)

// Checkpoints 实现 agent.CheckpointStore。
type Checkpoints struct {
	mu   sync.Mutex
	rows map[string][]agent.Checkpoint
}

func NewCheckpoints() *Checkpoints {
	return &Checkpoints{rows: map[string][]agent.Checkpoint{}}
}

func (c *Checkpoints) Latest(_ context.Context, runID string) (*agent.Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.rows[runID]
	if len(list) == 0 {
		return nil, nil
	}
	cp := list[len(list)-1]
	return &cp, nil
}

func (c *Checkpoints) Save(_ context.Context, cp agent.Checkpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cp.FormatVersion == 0 {
		cp.FormatVersion = 1
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	c.rows[cp.RunID] = append(c.rows[cp.RunID], cp)
	return nil
}

// ToolFinder 把 tool.ExecutionRepo 适配成 agent.ToolFinder。
type ToolFinder struct {
	Repo tool.ExecutionRepo
	Now  func() time.Time
}

func (f ToolFinder) Find(ctx context.Context, scope, idemKey string, lease time.Duration) (*agent.ToolExecutionView, error) {
	if f.Repo == nil {
		return nil, nil
	}
	view, err := f.Repo.Find(ctx, scope, idemKey, lease)
	if err != nil || view == nil {
		return nil, err
	}
	out := &agent.ToolExecutionView{ToolName: view.ToolName, State: view.State, Uncertain: view.Uncertain()}
	if f.Now != nil && view.State == "in_flight" && f.Now().Sub(view.StartedAt) > lease {
		out.State = "uncertain"
		out.Uncertain = true
	}
	return out, nil
}

// Sessions 是进程内会话历史。
type Sessions struct {
	mu       sync.Mutex
	messages map[string][]MessageRow
}

type MessageRow struct {
	MessageID  string
	SessionID  string
	Seq        int
	Role       domain.Role
	Content    string
	Name       string
	ToolCallID string
	ToolCalls  []domain.ToolCall
	CreatedAt  time.Time
}

func NewSessions() *Sessions { return &Sessions{messages: map[string][]MessageRow{}} }

func (s *Sessions) Ensure(_ context.Context, sessionID, caller, title string) error { return nil }

func (s *Sessions) Append(_ context.Context, sessionID string, msg domain.Message) (MessageRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.messages[sessionID]
	row := MessageRow{MessageID: ids.New("msg"), SessionID: sessionID, Seq: len(rows) + 1,
		Role: msg.Role, Content: msg.Content, Name: msg.Name, ToolCallID: msg.ToolCallID,
		ToolCalls: msg.ToolCalls, CreatedAt: time.Now().UTC()}
	s.messages[sessionID] = append(rows, row)
	return row, nil
}

func (s *Sessions) History(_ context.Context, sessionID string, limit int) ([]MessageRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.messages[sessionID]
	if limit > 0 && len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	out := append([]MessageRow{}, rows...)
	return out, nil
}
