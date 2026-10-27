package memory

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

type SearchQuery struct {
	TenantID      string
	Scopes        []Scope
	OwnerID       string
	SessionID     string
	Kinds         []Kind
	Query         string
	Vector        []float32
	TopK          int
	Bounded       bool
	LexicalWeight float64
	Now           time.Time
}

// Store 是记忆的权威存储。SaveWithVector 必须在**同一个事务**里
// 写元数据与向量 —— 否则会出现“有记忆但检索不到”。
type Store interface {
	FindByHash(ctx context.Context, tenant string, scope Scope, hash string) (*Item, error)
	SaveWithVector(ctx context.Context, item *Item, vector []float32) error
	Reinforce(ctx context.Context, id string, strength float64, now time.Time) error
	MarkAccessed(ctx context.Context, ids []string, now time.Time) error
	Supersede(ctx context.Context, oldID, newID string, now time.Time) error
	Merge(ctx context.Context, ids []string, into string, now time.Time) error
	Tombstone(ctx context.Context, id string, now time.Time) error
	Search(ctx context.Context, q SearchQuery) ([]Scored, error)
	ListActive(ctx context.Context, tenant string, before time.Time, limit int) ([]Item, error)
	AppendOp(ctx context.Context, op OpLog) error
}

type OpLog struct {
	OpID          string
	MemoryID      string
	Op            string
	BeforeHash    string
	AfterHash     string
	Reason        string
	Actor         string
	SourceTraceID string
	Scope         Scope
	TenantID      string
	Payload       map[string]any
	OccurredAt    time.Time
}

// ------------------------------ 内存实现（测试 / 单机） ------------------------------

type MemoryStore struct {
	mu      sync.RWMutex
	items   map[string]*Item
	vectors map[string][]float32
	ops     []OpLog
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: map[string]*Item{}, vectors: map[string][]float32{}}
}

func (s *MemoryStore) FindByHash(_ context.Context, tenant string, scope Scope, hash string) (*Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.items {
		if item.TenantID == tenant && item.Scope == scope && item.ContentHash == hash && item.State == StateActive {
			copy := *item
			return &copy, nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) SaveWithVector(_ context.Context, item *Item, vector []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *item
	s.items[item.ID] = &copy
	if len(vector) > 0 {
		s.vectors[item.ID] = vector
	}
	return nil
}

func (s *MemoryStore) Reinforce(_ context.Context, id string, strength float64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		return nil
	}
	item.EvidenceCount++
	item.Weight = SaturatingWeight(item.Weight+strength, 5)
	item.UpdatedAt = now
	return nil
}

func (s *MemoryStore) MarkAccessed(_ context.Context, ids []string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if item, ok := s.items[id]; ok {
			item.AccessCount++
			item.UsedCount++
			item.LastAccessedAt = &now
		}
	}
	return nil
}

func (s *MemoryStore) Supersede(_ context.Context, oldID, newID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.items[oldID]; ok {
		old.State = StateSuperseded
		old.SupersededBy = newID
		old.ValidTo = &now
		old.UpdatedAt = now
	}
	return nil
}

func (s *MemoryStore) Merge(_ context.Context, ids []string, into string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if id == into {
			continue
		}
		if item, ok := s.items[id]; ok {
			item.State = StateMerged
			item.MergedInto = into
			item.UpdatedAt = now
		}
	}
	return nil
}

func (s *MemoryStore) Tombstone(_ context.Context, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item, ok := s.items[id]; ok {
		item.State = StateTombstone
		item.UpdatedAt = now
	}
	return nil
}

func (s *MemoryStore) Search(_ context.Context, q SearchQuery) ([]Scored, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}
	weight := q.LexicalWeight
	if weight < 0 || weight > 1 {
		weight = 0.3
	}
	var out []Scored
	for id, item := range s.items {
		if item.State != StateActive || item.TenantID != q.TenantID {
			continue
		}
		if item.ValidTo != nil && item.ValidTo.Before(now) {
			continue
		}
		if !scopeAllowed(q.Scopes, item.Scope) || !kindAllowed(q.Kinds, item.Kind) {
			continue
		}
		if q.OwnerID != "" && item.OwnerID != "" && item.OwnerID != q.OwnerID {
			continue
		}
		if q.SessionID != "" && item.Scope == ScopeSession && item.SessionID != q.SessionID {
			continue
		}
		lexical := LexicalOverlap(q.Query, item.Content)
		var vecScore float64
		if len(q.Vector) > 0 {
			if vec, ok := s.vectors[id]; ok {
				vecScore = Cosine(q.Vector, vec)
				if vecScore < 0 {
					vecScore = 0
				}
			}
		}
		score := (1-weight)*vecScore + weight*lexical
		if score <= 0 {
			continue
		}
		copy := *item
		out = append(out, Scored{Item: copy, Score: score, MatchReason: "vector+lexical"})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if q.TopK > 0 && len(out) > q.TopK {
		out = out[:q.TopK]
	}
	return out, nil
}

func (s *MemoryStore) ListActive(_ context.Context, tenant string, before time.Time, limit int) ([]Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Item
	for _, item := range s.items {
		if item.TenantID != tenant || item.State != StateActive {
			continue
		}
		if !before.IsZero() && item.UpdatedAt.After(before) {
			continue
		}
		out = append(out, *item)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) AppendOp(_ context.Context, op OpLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
	return nil
}

func scopeAllowed(allowed []Scope, scope Scope) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, s := range allowed {
		if s == scope {
			return true
		}
	}
	return false
}

func kindAllowed(allowed []Kind, kind Kind) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, k := range allowed {
		if k == kind {
			return true
		}
	}
	return false
}

func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
