package gateway

import (
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/domain"
)

// UsageRecord 是账本里的一行。失败调用也记账 ——
// “某个模型被打过一次”这个事实在账单对不上时是唯一线索。
type UsageRecord struct {
	TraceID           string
	Alias             string
	ModelKey          string
	Provider          string
	Model             string
	InputTokens       *int
	OutputTokens      *int
	CachedInputTokens *int
	SessionID         string
	Caller            string
	At                time.Time
	Cost              domain.Cost
	Degraded          bool
	AttemptIndex      int
	Failed            bool
}

// UsageLedger 是有界缓冲：溢出时丢最旧的记录，并累加 dropped。
// 丢弃必须可见 —— 否则“账少了”会被误认为“没花钱”。
type UsageLedger struct {
	mu      sync.Mutex
	records []UsageRecord
	max     int
	dropped int
}

func NewUsageLedger(maxRecords int) *UsageLedger {
	if maxRecords <= 0 {
		maxRecords = 10000
	}
	return &UsageLedger{max: maxRecords}
}

func (l *UsageLedger) Record(r UsageRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	if len(l.records) >= l.max {
		l.records = l.records[1:]
		l.dropped++
	}
	l.records = append(l.records, r)
}

func (l *UsageLedger) Drain() []UsageRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.records
	l.records = nil
	return out
}

func (l *UsageLedger) DrainDropped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.dropped
	l.dropped = 0
	return n
}

func (l *UsageLedger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

type Totals struct {
	InputTokens  int
	OutputTokens int
	Calls        int
	// CostKnown=false 表示至少有一条未知价格；已知部分仍累加。
	Cost      float64
	CostKnown bool
}

func (l *UsageLedger) Totals() Totals {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := Totals{CostKnown: true}
	for _, r := range l.records {
		t.Calls++
		if r.InputTokens != nil {
			t.InputTokens += *r.InputTokens
		}
		if r.OutputTokens != nil {
			t.OutputTokens += *r.OutputTokens
		}
		if r.Cost.Amount == nil {
			t.CostKnown = false
		} else {
			f, _ := r.Cost.Amount.Float64()
			t.Cost += f
		}
	}
	return t
}
