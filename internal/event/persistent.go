package event

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// BatchWriter 是事件存档的窄接口（Postgres events 表）。
type BatchWriter interface {
	SaveBatch(ctx context.Context, events []Event) error
}

// Persistent 把事件先放进有界缓冲，再批量写库。
//
// 事件是观测，不能阻塞调用；但丢弃必须可见 —— 与 UsageLedger 同一条纪律。
type Persistent struct {
	writer     BatchWriter
	ch         chan Event
	done       chan struct{}
	once       sync.Once
	dropped    atomic.Int64
	flushEvery time.Duration
	batchSize  int
}

func NewPersistent(writer BatchWriter, buffer int) *Persistent {
	if buffer <= 0 {
		buffer = 4096
	}
	return &Persistent{writer: writer, ch: make(chan Event, buffer), done: make(chan struct{}), flushEvery: 500 * time.Millisecond, batchSize: 200}
}

func (p *Persistent) Start(ctx context.Context) {
	go p.loop(ctx)
}

func (p *Persistent) Emit(_ context.Context, ev Event) {
	select {
	case p.ch <- ev:
	default:
		p.dropped.Add(1)
	}
}

func (p *Persistent) Dropped() int64 { return p.dropped.Load() }

func (p *Persistent) loop(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(p.flushEvery)
	defer ticker.Stop()
	batch := make([]Event, 0, p.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		writeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := p.writer.SaveBatch(writeCtx, batch)
		cancel()
		if err != nil {
			slog.Warn("事件批量落库失败（观测旁路）", "count", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			// drain：把已经缓冲的事件尽量写完。
			for {
				select {
				case ev := <-p.ch:
					batch = append(batch, ev)
					if len(batch) >= p.batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case <-ticker.C:
			flush()
		case ev := <-p.ch:
			batch = append(batch, ev)
			if len(batch) >= p.batchSize {
				flush()
			}
		}
	}
}

// Close 等待缓冲写完（最多 timeout）。
func (p *Persistent) Close(timeout time.Duration) {
	p.once.Do(func() {
		// ctx 由 Start 的调用方取消；这里只等 done。
		select {
		case <-p.done:
		case <-time.After(timeout):
		}
	})
}

// Multi 把事件同时送到多个出口（内存 / 持久化 / NATS）。
type Multi struct{ emitters []Emitter }

func NewMulti(emitters ...Emitter) *Multi { return &Multi{emitters: emitters} }

func (m *Multi) Emit(ctx context.Context, ev Event) {
	for _, e := range m.emitters {
		if e != nil {
			e.Emit(ctx, ev)
		}
	}
}
