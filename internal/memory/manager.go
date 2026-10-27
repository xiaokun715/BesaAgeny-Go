package memory

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

type Embedder interface {
	Embed(ctx context.Context, texts []string, traceID string) ([][]float32, error)
}

type Manager struct {
	Store     Store
	Policy    Policy
	Extractor *Extractor
	Gates     Gates
	LLM       LLM
	Embedder  Embedder
	Events    event.Emitter
	Now       func() time.Time
}

func NewManager(store Store, policy Policy, extractor *Extractor, llm LLM, embedder Embedder, events event.Emitter) *Manager {
	if events == nil {
		events = event.Null{}
	}
	return &Manager{
		Store: store, Policy: policy, Extractor: extractor,
		Gates: Gates{Policy: policy, LLM: llm}, LLM: llm, Embedder: embedder,
		Events: event.Safe(events), Now: time.Now,
	}
}

// Remember 是写时判定：提取 → 三道闸门 → 查重 → 四分支。
// **任何失败都只降级留痕，不向会话抛异常。**
func (m *Manager) Remember(ctx context.Context, turns []ctxpkg.Turn, source string, mc Ctx) ([]Item, error) {
	if !m.Policy.Enabled || m.Store == nil {
		return nil, nil
	}
	now := m.Now().UTC()
	candidates := m.Extractor.Extract(ctx, turns, mc.TraceID)
	var saved []Item
	for _, candidate := range candidates {
		gate := m.Gates.Check(ctx, candidate.Content, candidate.Kind, mc.Scope)
		if !gate.Allowed {
			m.emit(ctx, "memory.candidate.rejected", map[string]any{"kind": candidate.Kind, "reason": gate.Reason})
			m.logOp(ctx, OpLog{MemoryID: "", Op: "reject", Reason: gate.Reason, Scope: mc.Scope, TenantID: mc.Tenant(), SourceTraceID: mc.TraceID})
			continue
		}
		content := gate.Content
		hash := ContentHash(mc.Scope, content)
		existing, err := m.Store.FindByHash(ctx, mc.Tenant(), mc.Scope, hash)
		if err != nil {
			slog.Warn("按内容哈希查重失败（旁路）", "err", err)
		}
		if existing != nil {
			// 同一内容再次出现 = 强化，不新建。
			if err := m.Store.Reinforce(ctx, existing.ID, SourceStrength[source], now); err != nil {
				slog.Warn("强化记忆失败", "err", err)
			}
			m.emit(ctx, "memory.reinforced", map[string]any{"memory_id": existing.ID, "reason": "内容哈希命中"})
			continue
		}

		item, action, target := m.resolve(ctx, candidate, content, hash, mc, now, source)
		switch action {
		case "reinforce":
			if err := m.Store.Reinforce(ctx, target.Item.ID, SourceStrength[source], now); err != nil {
				slog.Warn("强化记忆失败", "err", err)
			}
			m.emit(ctx, "memory.reinforced", map[string]any{"memory_id": target.Item.ID})
			continue
		case "conflict":
			m.emit(ctx, "memory.conflicted", map[string]any{"content": content})
			m.logOp(ctx, OpLog{Op: "conflict", Reason: "新记忆与旧记忆矛盾，保留双方待验证", Scope: mc.Scope, TenantID: mc.Tenant(), SourceTraceID: mc.TraceID})
			continue
		}

		if action == "update" && target != nil {
			item.Version = target.Item.Version + 1
			item.SupersedesID = target.Item.ID
		}
		vector := m.embedOne(ctx, item.Content, mc.TraceID)
		if err := m.Store.SaveWithVector(ctx, &item, vector); err != nil {
			slog.Warn("保存记忆失败（旁路）", "err", err)
			continue
		}
		if action == "update" && target != nil {
			_ = m.Store.Supersede(ctx, target.Item.ID, item.ID, now)
			m.emit(ctx, "memory.updated", map[string]any{"memory_id": item.ID, "supersedes": target.Item.ID})
		} else {
			m.emit(ctx, "memory.added", map[string]any{"memory_id": item.ID, "kind": item.Kind})
		}
		m.logOp(ctx, OpLog{MemoryID: item.ID, Op: actionToOp(action), AfterHash: item.ContentHash,
			Scope: mc.Scope, TenantID: mc.Tenant(), SourceTraceID: mc.TraceID})
		saved = append(saved, item)
	}
	return saved, nil
}

func (m *Manager) resolve(ctx context.Context, candidate Candidate, content, hash string, mc Ctx, now time.Time, source string) (Item, string, *Scored) {
	item := Item{
		ID: ids.New("mem"), TenantID: mc.Tenant(), Scope: mc.Scope, OwnerID: mc.OwnerID,
		SessionID: mc.SessionID, Content: content, ContentHash: hash, Kind: candidate.Kind,
		Tags: candidate.Tags, Confidence: candidate.Confidence, Importance: candidate.Importance,
		State: StateActive, Version: 1, SourceKind: source, SourceSessionID: mc.SessionID,
		SourceTraceID: mc.TraceID, Weight: 1, ValidFrom: now, CreatedAt: now, UpdatedAt: now,
	}
	vector := m.embedOne(ctx, content, mc.TraceID)
	opponents, err := m.Store.Search(ctx, SearchQuery{
		TenantID: mc.Tenant(), Scopes: []Scope{mc.Scope}, OwnerID: mc.OwnerID,
		SessionID: mc.SessionID, Query: content, Vector: vector, TopK: m.Policy.TopK,
		Bounded: false, LexicalWeight: m.Policy.LexicalWeight, Now: now,
	})
	if err != nil || len(opponents) == 0 {
		return item, "add", nil
	}
	best := opponents[0]
	if best.Score < m.Policy.SimilarityThreshold {
		return item, "add", nil
	}
	if m.LLM != nil {
		relation := m.judgeRelation(ctx, content, best.Item.Content)
		switch relation {
		case "equivalent", "corroborate":
			return item, "reinforce", &best
		case "contradict":
			if SourceStrength[source] >= SourceStrength[best.Item.SourceKind] || mc.SessionID == best.Item.SessionID {
				return item, "update", &best
			}
			return item, "conflict", &best
		case "unrelated":
			return item, "add", nil
		}
	}
	if best.Score >= m.Policy.MergeThreshold {
		return item, "reinforce", &best
	}
	return item, "reinforce", &best
}

func (m *Manager) judgeRelation(ctx context.Context, newContent, oldContent string) string {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"relation": map[string]any{"type": "string", "enum": []string{"equivalent", "corroborate", "contradict", "unrelated"}},
		},
		"required": []string{"relation"},
	}
	out, err := m.LLM.Complete(ctx,
		"判断两条记忆的关系。equivalent=同一件事；corroborate=互相印证；contradict=矛盾；unrelated=无关。",
		"旧："+oldContent+"\n新："+newContent, schema, 128)
	if err != nil {
		return "corroborate"
	}
	var parsed struct {
		Relation string `json:"relation"`
	}
	if json.Unmarshal([]byte(stripFence(out)), &parsed) != nil {
		return "corroborate"
	}
	return parsed.Relation
}

// Recall 先向量收窄，再用结构化条件过滤；失败返回空，不抛。
func (m *Manager) Recall(ctx context.Context, query string, mc Ctx, kinds []Kind, topK int) ([]Scored, error) {
	if !m.Policy.Enabled || m.Store == nil {
		return nil, nil
	}
	if topK <= 0 {
		topK = m.Policy.TopK
	}
	vector := m.embedOne(ctx, query, mc.TraceID)
	scopes := []Scope{mc.Scope}
	if mc.Scope == ScopeSession {
		// session 记忆之外也允许召回同 owner 的稳定偏好。
		scopes = append(scopes, ScopeUser)
	}
	results, err := m.Store.Search(ctx, SearchQuery{
		TenantID: mc.Tenant(), Scopes: scopes, OwnerID: mc.OwnerID, SessionID: mc.SessionID,
		Kinds: kinds, Query: query, Vector: vector, TopK: topK, Bounded: true,
		LexicalWeight: m.Policy.LexicalWeight, Now: m.Now().UTC(),
	})
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		idsToMark := make([]string, len(results))
		for i, r := range results {
			idsToMark[i] = r.Item.ID
		}
		_ = m.Store.MarkAccessed(ctx, idsToMark, m.Now().UTC())
	}
	return results, nil
}

type MaintenanceReport struct {
	Scanned    int
	Tombstoned int
}

func (m *Manager) Maintain(ctx context.Context, tenant string) (MaintenanceReport, error) {
	report := MaintenanceReport{}
	if m.Store == nil {
		return report, nil
	}
	items, err := m.Store.ListActive(ctx, tenant, time.Time{}, 5000)
	if err != nil {
		return report, err
	}
	now := m.Now().UTC()
	for _, item := range items {
		report.Scanned++
		if m.Policy.Decay.NextState(item, now) == StateTombstone {
			if err := m.Store.Tombstone(ctx, item.ID, now); err != nil {
				return report, err
			}
			report.Tombstoned++
			m.emit(ctx, "memory.decayed", map[string]any{"memory_id": item.ID})
		}
	}
	return report, nil
}

// ------------------------------ context.MemoryProvider ------------------------------

func (m *Manager) ContextRecall(ctx context.Context, query string, topK int, kinds []string, sessionID string) ([]ctxpkg.MemoryItem, error) {
	k := make([]Kind, 0, len(kinds))
	for _, kind := range kinds {
		k = append(k, Kind(kind))
	}
	results, err := m.Recall(ctx, query, Ctx{Scope: ScopeSession, SessionID: sessionID}, k, topK)
	if err != nil {
		return nil, err
	}
	out := make([]ctxpkg.MemoryItem, 0, len(results))
	for _, r := range results {
		out = append(out, ctxpkg.MemoryItem{Content: r.Item.ToModelText(), Score: r.Score, Kind: string(r.Item.Kind)})
	}
	return out, nil
}

func (m *Manager) ContextSettle(ctx context.Context, turns []ctxpkg.Turn, traceID, sessionID string) error {
	_, err := m.Remember(ctx, turns, "conversation", Ctx{Scope: ScopeSession, SessionID: sessionID, TraceID: traceID})
	return err
}

// ------------------------------ 辅助 ------------------------------

func (m *Manager) embedOne(ctx context.Context, text, traceID string) []float32 {
	if m.Embedder == nil || text == "" {
		return nil
	}
	vectors, err := m.Embedder.Embed(ctx, []string{text}, traceID)
	if err != nil || len(vectors) == 0 {
		m.emit(ctx, "memory.degraded", map[string]any{"reason": "向量化失败，退化为关键词检索"})
		return nil
	}
	return vectors[0]
}

func (m *Manager) logOp(ctx context.Context, op OpLog) {
	if op.OpID == "" {
		op.OpID = ids.New("memop")
	}
	if op.OccurredAt.IsZero() {
		op.OccurredAt = m.Now().UTC()
	}
	if op.TenantID == "" {
		op.TenantID = "default"
	}
	if err := m.Store.AppendOp(ctx, op); err != nil {
		slog.Debug("记忆审计日志写入失败", "err", err)
	}
}

func actionToOp(action string) string {
	switch action {
	case "update":
		return "update"
	case "merge":
		return "merge"
	default:
		return "add"
	}
}

func (m *Manager) emit(ctx context.Context, name string, payload map[string]any) {
	ev := event.New(name)
	ev.TraceID = ids.Trace(ctx)
	ev.Payload = payload
	if id, ok := payload["memory_id"].(string); ok {
		ev.Subject = id
	}
	m.Events.Emit(ctx, ev)
}
