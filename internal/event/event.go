// Package event 定义统一的事件形状与出口。
//
// 事件是观测：**任何事件出口失败都不得影响主流程**。各模块只通过 Emitter
// 发事件，不直接依赖具体总线（NATS / 内存）。
package event

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

type Event struct {
	ID         string         `json:"event_id"`
	Name       string         `json:"name"`
	TraceID    string         `json:"trace_id,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Caller     string         `json:"caller,omitempty"`
	Alias      string         `json:"alias,omitempty"`
	Subject    string         `json:"subject,omitempty"`
	Outcome    string         `json:"outcome,omitempty"`
	AttemptIdx int            `json:"attempt_index,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

func New(name string) Event {
	return Event{ID: ids.Token(), Name: name, OccurredAt: time.Now().UTC()}
}

type Emitter interface {
	Emit(ctx context.Context, ev Event)
}

type Null struct{}

func (Null) Emit(context.Context, Event) {}

// Func 把函数适配成 Emitter（测试与组合根用）。
type Func func(context.Context, Event)

func (f Func) Emit(ctx context.Context, ev Event) { f(ctx, ev) }

// Safe 包裹另一个 Emitter，吞掉 panic —— 观测失败不能拖垮调用。
func Safe(next Emitter) Emitter {
	if next == nil {
		return Null{}
	}
	return Func(func(ctx context.Context, ev Event) {
		defer func() { _ = recover() }()
		next.Emit(ctx, ev)
	})
}

// InMemory 是进程内总线，供单机模式与测试使用。
type InMemory struct {
	mu         sync.RWMutex
	subs       map[string][]chan Event
	all        []chan Event
	history    []Event
	maxHistory int
}

func NewInMemory() *InMemory {
	return &InMemory{subs: map[string][]chan Event{}, maxHistory: 20000}
}

func (b *InMemory) Emit(_ context.Context, ev Event) {
	b.mu.Lock()
	if len(ev.ID) == 0 {
		ev.ID = ids.Token()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	if b.maxHistory > 0 {
		b.history = append(b.history, ev)
		if len(b.history) > b.maxHistory {
			b.history = b.history[len(b.history)-b.maxHistory:]
		}
	}
	subs := append([]chan Event{}, b.subs[ev.Name]...)
	all := append([]chan Event{}, b.all...)
	b.mu.Unlock()
	for _, ch := range append(subs, all...) {
		select {
		case ch <- ev:
		default: // 慢消费者不阻塞生产者
		}
	}
}

// Subscribe 订阅某个事件名；name 为空表示全部。
func (b *InMemory) Subscribe(name string, buffer int) <-chan Event {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)
	b.mu.Lock()
	defer b.mu.Unlock()
	if name == "" {
		b.all = append(b.all, ch)
	} else {
		b.subs[name] = append(b.subs[name], ch)
	}
	return ch
}

func (b *InMemory) History() []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Event{}, b.history...)
}

// Marshal 是落库/发送前的标准编码。
func (e Event) Marshal() ([]byte, error) { return json.Marshal(e) }
