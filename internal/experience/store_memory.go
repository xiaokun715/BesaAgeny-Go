package experience

import (
	"context"
	"sort"
	"sync"
	"time"
)

type MemoryStore struct {
	mu    sync.RWMutex
	items map[string]Experience
	apps  []Application
}

type Application struct {
	ExperienceID string
	TraceID      string
	Outcome      string
	MetricBefore float64
	MetricAfter  float64
	At           time.Time
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{items: map[string]Experience{}} }

func (s *MemoryStore) Save(_ context.Context, exp Experience) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[exp.ID] = exp
	return nil
}

func (s *MemoryStore) Get(_ context.Context, id string) (*Experience, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	exp, ok := s.items[id]
	if !ok {
		return nil, nil
	}
	return &exp, nil
}

func (s *MemoryStore) Update(_ context.Context, exp Experience) error {
	return s.Save(context.Background(), exp)
}

func (s *MemoryStore) FindByScenario(_ context.Context, tenant, scenario, errorType string) (*Experience, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, exp := range s.items {
		if exp.Scope["tenant"] == tenant && exp.Scenario == scenario && exp.Conditions["error_type"] == errorType {
			copy := exp
			return &copy, nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) Search(_ context.Context, q Query) ([]Experience, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Experience
	for _, exp := range s.items {
		if q.TenantID != "" && exp.Scope["tenant"] != q.TenantID {
			continue
		}
		if q.Stage != "" && exp.Conditions["stage"] != q.Stage {
			continue
		}
		if q.ErrorType != "" && exp.Conditions["error_type"] != q.ErrorType {
			continue
		}
		if q.Scenario != "" && !stringsContainsFold(exp.Scenario, q.Scenario) {
			continue
		}
		if exp.State != ExpVerified {
			continue
		}
		copy := exp
		out = append(out, copy)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Validation.SuccessRate > out[j].Validation.SuccessRate })
	if q.TopK > 0 && len(out) > q.TopK {
		out = out[:q.TopK]
	}
	return out, nil
}

func (s *MemoryStore) List(_ context.Context, tenant string, limit int) ([]Experience, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Experience
	for _, exp := range s.items {
		if tenant != "" && exp.Scope["tenant"] != tenant {
			continue
		}
		out = append(out, exp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) RecordApplication(_ context.Context, id, traceID, outcome string, before, after float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apps = append(s.apps, Application{ExperienceID: id, TraceID: traceID, Outcome: outcome, MetricBefore: before, MetricAfter: after, At: time.Now().UTC()})
	return nil
}

func stringsContainsFold(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) && containsFold(haystack, needle)
}

func containsFold(s, sub string) bool {
	ls, lsub := lower(s), lower(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return true
		}
	}
	return false
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
