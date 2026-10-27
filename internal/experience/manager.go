package experience

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

type Store interface {
	Save(ctx context.Context, exp Experience) error
	Get(ctx context.Context, id string) (*Experience, error)
	Update(ctx context.Context, exp Experience) error
	Search(ctx context.Context, q Query) ([]Experience, error)
	FindByScenario(ctx context.Context, tenant, scenario, errorType string) (*Experience, error)
	List(ctx context.Context, tenant string, limit int) ([]Experience, error)
	RecordApplication(ctx context.Context, id, traceID, outcome string, before, after float64) error
}

type Manager struct {
	Store           Store
	Gates           GatePolicy
	MaxRetries      int
	AllowHypothesis bool
	Events          event.Emitter
	Now             func() time.Time
}

func NewManager(store Store, gates GatePolicy, maxRetries int, allowHypothesis bool, events event.Emitter) *Manager {
	if events == nil {
		events = event.Null{}
	}
	return &Manager{Store: store, Gates: gates, MaxRetries: maxRetries, AllowHypothesis: allowHypothesis, Events: event.Safe(events), Now: time.Now}
}

// OnFailure 检测 → 分类 → 定向恢复。**旁路**：失败只降级留痕，不阻断主流程。
func (m *Manager) OnFailure(ctx context.Context, f Failure) RecoveryDecision {
	// needs_review 不是失败，不得进入归因。
	if f.Status == "needs_review" {
		return RecoveryDecision{Action: ActionAbort, Reason: "needs_review 需要人处理，不进入失败归因"}
	}
	category, _ := Classify(f)
	m.emit(ctx, "experience.failure.detected", map[string]any{"stage": f.Stage, "error_type": f.ErrorType, "category": category, "trace_id": f.TraceID})
	m.emit(ctx, "experience.classified", map[string]any{"category": category, "error_type": f.ErrorType})
	decision := Decide(RecoveryInput{
		ErrorType: f.ErrorType, Evidence: f.Evidence, RetryCount: 0, MaxRetries: m.MaxRetries,
		RemainingBudget: nil, Risk: f.Risk,
	})
	m.emit(ctx, "experience.recovery.decided", map[string]any{"action": decision.Action, "reason": decision.Reason})
	return decision
}

type Settlement struct {
	Failure    Failure
	Hypothesis Hypothesis
	Recovery   string
	Validation ValidationStats
	TenantID   string
	Category   string
}

// Settle：只有 verified 假设才能沉淀为可检索经验。
func (m *Manager) Settle(ctx context.Context, s Settlement) ([]Experience, error) {
	if s.Hypothesis.State != HypothesisVerified {
		if !m.AllowHypothesis {
			m.emit(ctx, "experience.hypothesis.dropped", map[string]any{"reason": "未验证的假设不进入经验库"})
			return nil, nil
		}
	}
	if len(s.Hypothesis.Evidence) == 0 {
		m.emit(ctx, "experience.gate.rejected", map[string]any{"reason": "没有客观证据"})
		return nil, nil
	}
	tenant := s.TenantID
	if tenant == "" {
		tenant = "default"
	}
	scenario := describeScenario(s.Failure)
	conditions := map[string]string{
		"stage": s.Failure.Stage, "error_type": s.Failure.ErrorType,
		"tenant": tenant, "risk": s.Failure.Risk,
	}
	existing, _ := m.Store.FindByScenario(ctx, tenant, scenario, s.Failure.ErrorType)
	if existing != nil {
		existing.Evidence = mergeEvidence(existing.Evidence, s.Hypothesis.Evidence)
		existing.Validation = mergeStats(existing.Validation, s.Validation)
		existing.UpdatedAt = m.Now().UTC()
		existing.RootCause = s.Hypothesis.Statement
		existing.RecoveryStrategy = s.Recovery
		if s.Hypothesis.State == HypothesisVerified {
			existing.CauseState = "verified"
			existing.State = ExpVerified
		}
		if err := m.Store.Update(ctx, *existing); err != nil {
			return nil, err
		}
		m.emit(ctx, "experience.settled", map[string]any{"experience_id": existing.ID, "reinforced": true})
		return []Experience{*existing}, nil
	}

	exp := Experience{
		ID: ids.New("exp"), Category: s.Category, Scenario: scenario, Symptom: s.Failure.Message,
		Conditions: conditions, Evidence: s.Hypothesis.Evidence, RootCause: s.Hypothesis.Statement,
		CauseState: "verified", RecoveryStrategy: s.Recovery, Scope: conditions,
		Validation: s.Validation, State: ExpVerified, CreatedAt: m.Now().UTC(), UpdatedAt: m.Now().UTC(),
	}
	if exp.Category == "" {
		category, _ := Classify(s.Failure)
		exp.Category = string(category)
	}
	if exp.Validation.Samples == 0 {
		exp.Validation = ValidationStats{Reproducible: true, SuccessRate: 1, Samples: 1}
	}
	if ok, reason := m.Gate(exp); !ok {
		m.emit(ctx, "experience.gate.rejected", map[string]any{"reason": reason})
		return nil, nil
	}
	if err := m.Store.Save(ctx, exp); err != nil {
		return nil, err
	}
	m.emit(ctx, "experience.settled", map[string]any{"experience_id": exp.ID})
	return []Experience{exp}, nil
}

func (m *Manager) Gate(exp Experience) (bool, string) {
	if m.Gates.RequireEvidence {
		objective := false
		for _, ev := range exp.Evidence {
			if ev.Source != "" && ev.Detail != "" {
				objective = true
				break
			}
		}
		if !objective {
			return false, "证据必须可回指（source + detail），不能是模型自述"
		}
	}
	if m.Gates.RequireScope && len(exp.Scope) == 0 {
		return false, "缺少适用范围"
	}
	if m.Gates.RequireExpiryForVersioned {
		if v := exp.Scope["environment_version"]; v != "" && exp.ExpiresWhen == nil {
			return false, "版本相关经验必须有失效条件"
		}
	}
	if exp.Validation.Samples >= m.Gates.MinSamples && exp.Validation.SuccessRate < m.Gates.MinSuccessRate {
		return false, "成功率低于门槛"
	}
	return true, ""
}

// Recall：只返回 verified；规范优先（这里没有规范库时返回经验）。
func (m *Manager) Recall(ctx context.Context, q Query) Retrieved {
	if q.TopK <= 0 {
		q.TopK = 10
	}
	items, err := m.Store.Search(ctx, q)
	if err != nil {
		slog.Warn("经验检索失败", "err", err)
		return Retrieved{}
	}
	var out Retrieved
	for _, exp := range items {
		if exp.State != ExpVerified {
			continue
		}
		if !scopeMatches(exp.Scope, q.Environment) {
			continue
		}
		out.Experiences = append(out.Experiences, exp)
		if len(out.Experiences) >= q.TopK {
			break
		}
	}
	m.emit(ctx, "experience.recalled", map[string]any{"count": len(out.Experiences)})
	return out
}

func (m *Manager) RecordApplication(ctx context.Context, id, traceID, outcome string, before, after float64) {
	exp, err := m.Store.Get(ctx, id)
	if err != nil || exp == nil {
		return
	}
	_ = m.Store.RecordApplication(ctx, id, traceID, outcome, before, after)
	exp.Validation.Samples++
	if strings.EqualFold(outcome, "success") || after > before {
		exp.Validation.SuccessRate = (exp.Validation.SuccessRate*float64(exp.Validation.Samples-1) + 1) / float64(exp.Validation.Samples)
		exp.BadUses = 0
		exp.LastAppliedAt = ptrTime(m.Now().UTC())
	} else {
		exp.Validation.SuccessRate = (exp.Validation.SuccessRate * float64(exp.Validation.Samples-1)) / float64(exp.Validation.Samples)
		exp.BadUses++
		if m.Gates.DegradeAfterBadUses > 0 && exp.BadUses >= m.Gates.DegradeAfterBadUses {
			exp.State = ExpDegraded
			m.emit(ctx, "experience.degraded", map[string]any{"experience_id": exp.ID, "bad_uses": exp.BadUses})
		}
	}
	exp.UpdatedAt = m.Now().UTC()
	_ = m.Store.Update(ctx, *exp)
	m.emit(ctx, "experience.applied", map[string]any{"experience_id": id, "outcome": outcome})
}

func (m *Manager) Maintain(ctx context.Context, tenant string) (int, error) {
	items, err := m.Store.List(ctx, tenant, 5000)
	if err != nil {
		return 0, err
	}
	now := m.Now().UTC()
	changed := 0
	for _, exp := range items {
		if exp.ExpiresWhen != nil && expired(exp, now) {
			exp.State = ExpDeactivated
			exp.UpdatedAt = now
			if err := m.Store.Update(ctx, exp); err != nil {
				return changed, err
			}
			changed++
		}
	}
	return changed, nil
}

func (m *Manager) emit(ctx context.Context, name string, payload map[string]any) {
	ev := event.New(name)
	ev.Payload = payload
	ev.TraceID = stringOf(payload["trace_id"])
	if id, ok := payload["experience_id"].(string); ok {
		ev.Subject = id
	}
	m.Events.Emit(ctx, ev)
}

func describeScenario(f Failure) string {
	parts := []string{}
	if f.Stage != "" {
		parts = append(parts, f.Stage)
	}
	if f.ErrorType != "" {
		parts = append(parts, f.ErrorType)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ":")
}

func mergeEvidence(a, b []Evidence) []Evidence {
	seen := map[string]bool{}
	var out []Evidence
	for _, ev := range append(a, b...) {
		key := ev.Kind + "|" + ev.Source + "|" + ev.Detail
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ev)
	}
	return out
}

func mergeStats(a, b ValidationStats) ValidationStats {
	total := a.Samples + b.Samples
	if total == 0 {
		return b
	}
	return ValidationStats{
		Reproducible: a.Reproducible || b.Reproducible,
		SuccessRate:  (a.SuccessRate*float64(a.Samples) + b.SuccessRate*float64(b.Samples)) / float64(total),
		Samples:      total,
	}
}

func scopeMatches(scope, env map[string]string) bool {
	for k, v := range env {
		if sv, ok := scope[k]; ok && sv != "" && sv != v {
			return false
		}
	}
	return true
}

func expired(exp Experience, now time.Time) bool {
	if exp.ExpiresWhen == nil {
		return false
	}
	current := exp.Scope[exp.ExpiresWhen.Field]
	switch exp.ExpiresWhen.Op {
	case "!=":
		return current != exp.ExpiresWhen.Value
	case "==", "=":
		return current == exp.ExpiresWhen.Value
	case "<":
		return current < exp.ExpiresWhen.Value
	}
	return false
}

func ptrTime(t time.Time) *time.Time { return &t }

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

var _ = math.Max
