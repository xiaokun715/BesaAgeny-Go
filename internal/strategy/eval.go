package strategy

import (
	"context"
	"sync"
	"time"
)

type EvalRecord struct {
	ProfileKey   string
	Versions     map[string]string
	Quality      float64
	Cost         float64
	Latency      float64
	Samples      int
	BenchmarkSet string
	At           time.Time
}

// Matches：任一版本对不上即视为没有数据 —— 策略升版后不能沿用旧结论。
func (r EvalRecord) Matches(p AgentProfile) bool {
	if r.ProfileKey != p.ProfileKey() {
		return false
	}
	if len(r.Versions) != len(p.Versions) {
		return false
	}
	for k, v := range p.Versions {
		if r.Versions[k] != v {
			return false
		}
	}
	for k := range r.Versions {
		if _, ok := p.Versions[k]; !ok {
			return false
		}
	}
	return true
}

type EvalStore interface {
	Records(ctx context.Context, benchmark string) ([]EvalRecord, error)
	Save(ctx context.Context, record EvalRecord) error
}

type MemoryEvalStore struct {
	mu      sync.Mutex
	records []EvalRecord
}

func NewMemoryEvalStore() *MemoryEvalStore { return &MemoryEvalStore{} }

func (s *MemoryEvalStore) Records(_ context.Context, benchmark string) ([]EvalRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []EvalRecord
	for _, r := range s.records {
		if benchmark == "" || r.BenchmarkSet == benchmark {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *MemoryEvalStore) Save(_ context.Context, record EvalRecord) error {
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	if record.Samples <= 0 {
		record.Samples = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}
