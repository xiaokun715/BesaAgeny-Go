package strategy

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/besa-ai/besa-agent/internal/event"
)

type SelectionPolicy struct {
	Enabled                   bool
	Rules                     bool
	Evaluated                 bool
	WeightCost                float64
	WeightLatency             float64
	MinSamples                int
	HighRiskRequiresValidated bool
	DefaultProfile            string
	RuleSaturation            int
	BenchmarkSet              string
}

func DefaultSelectionPolicy() SelectionPolicy {
	return SelectionPolicy{
		Enabled: true, Rules: true, Evaluated: true,
		WeightCost: 0.3, WeightLatency: 0.2, MinSamples: 20,
		HighRiskRequiresValidated: true, DefaultProfile: BaselineName,
		RuleSaturation: 2, BenchmarkSet: "default",
	}
}

type Selector struct {
	Registry  *Registry
	Templates []Template
	Contracts []Contract
	Policy    SelectionPolicy
	Eval      EvalStore
	Events    event.Emitter
}

func NewSelector(registry *Registry, templates []Template, contracts []Contract, policy SelectionPolicy, eval EvalStore, events event.Emitter) *Selector {
	if policy.RuleSaturation <= 0 {
		policy = DefaultSelectionPolicy()
	}
	if events == nil {
		events = event.Null{}
	}
	if contracts == nil {
		contracts = DefaultContracts()
	}
	return &Selector{Registry: registry, Templates: templates, Contracts: contracts, Policy: policy, Eval: eval, Events: event.Safe(events)}
}

func (s *Selector) Select(ctx context.Context, features TaskFeatures) SelectionDecision {
	decision := SelectionDecision{Features: &features}
	if !s.Policy.Enabled {
		profile, err := s.baselineProfile()
		if err != nil {
			decision.Refused = true
			decision.Reason = err.Error()
			return decision
		}
		decision.Profile = &profile
		decision.Layer = LayerDefault
		decision.Reason = "策略层被关闭，使用 baseline（启动期应告警）"
		decision.Degraded = true
		return decision
	}

	baseline, err := s.baselineProfile()
	if err != nil {
		decision.Refused = true
		decision.Reason = err.Error()
		s.emit(ctx, "strategy.high_risk_refused", map[string]any{"reason": decision.Reason})
		return decision
	}
	baselineViolations := Validate(baseline, s.Registry, s.Contracts)
	if len(baselineViolations) > 0 {
		decision.Violations = baselineViolations
		decision.Refused = true
		decision.Reason = "baseline 自身违反契约：" + baselineViolations[0].Detail
		s.emit(ctx, "strategy.combo_rejected", map[string]any{"profile": baseline.Name, "violations": len(baselineViolations)})
		return decision
	}

	// 高风险任务只在“合法 ∩ 已验证”的组合里选；空集直接拒绝，不降级。
	allowedTemplates := s.validatedTemplates(features)
	if features.Risk == RiskHigh && s.Policy.HighRiskRequiresValidated && len(allowedTemplates) == 0 {
		decision.Refused = true
		decision.Reason = "高风险任务没有任何通过契约校验的组合 —— 拒绝执行，不降级"
		s.emit(ctx, "strategy.high_risk_refused", map[string]any{"reason": decision.Reason})
		return decision
	}

	// ① 模板层：零评测查询。
	if t := s.matchTemplate(features, allowedTemplates); t != nil {
		if profile, violations := s.buildFromTemplate(*t, features, LayerTemplate); len(violations) == 0 {
			decision.Profile = &profile
			decision.Layer = LayerTemplate
			decision.Reason = "命中场景模板：" + t.Name + "（" + t.Description + "）"
			s.emit(ctx, "strategy.template_matched", map[string]any{"template": t.Name, "profile": profile.Name})
			return decision
		}
	}

	// ② 规则层：用各组件策略的 scenarios 与 task_type 匹配。
	if s.Policy.Rules {
		if profile, score, reason := s.byRules(features, allowedTemplates); profile != nil {
			decision.Profile = profile
			decision.Layer = LayerRules
			decision.Reason = reason
			decision.Candidates = []Candidate{{Profile: *profile, Score: score, Note: reason}}
			s.emit(ctx, "strategy.selected", map[string]any{"profile": profile.Name, "layer": "rules", "score": score})
			return decision
		}
	}

	// ③ 评测层：需要历史样本；版本不匹配视为没有数据。
	if s.Policy.Evaluated && s.Eval != nil {
		if profile, candidates, reason := s.byEvaluated(ctx, features, baseline, allowedTemplates); profile != nil {
			decision.Profile = profile
			decision.Layer = LayerEvaluated
			decision.Reason = reason
			decision.Candidates = candidates
			s.emit(ctx, "strategy.selected", map[string]any{"profile": profile.Name, "layer": "evaluated"})
			return decision
		}
	}

	// ④ 降级到 baseline —— 必须可见。
	decision.Profile = &baseline
	decision.Layer = LayerDefault
	decision.Degraded = true
	decision.Reason = "没有命中模板/规则/评测数据，降级到 baseline"
	s.emit(ctx, "strategy.degraded", map[string]any{"profile": baseline.Name, "reason": decision.Reason})
	return decision
}

func (s *Selector) baselineProfile() (AgentProfile, error) {
	t := FindTemplate(s.Templates, s.Policy.DefaultProfile)
	if t == nil {
		t = FindTemplate(s.Templates, BaselineName)
	}
	if t == nil {
		return AgentProfile{}, errorString("没有配置 default_profile/baseline 模板")
	}
	profile, err := BuildProfile(t.Name, t.Bindings, s.Registry, LayerDefault, "降级目标", t.Risk, nil)
	if err != nil {
		return AgentProfile{}, err
	}
	return profile, nil
}

func (s *Selector) validatedTemplates(features TaskFeatures) []Template {
	var out []Template
	for _, t := range s.Templates {
		if t.Name == BaselineName || t.Name == s.Policy.DefaultProfile {
			continue
		}
		profile, violations := s.buildFromTemplate(t, features, LayerTemplate)
		if len(violations) == 0 && profile.Name != "" {
			out = append(out, t)
		}
	}
	return out
}

func (s *Selector) buildFromTemplate(t Template, features TaskFeatures, layer Layer) (AgentProfile, []Violation) {
	baseline := FindTemplate(s.Templates, BaselineName)
	bindings, _ := Render(t, baseline, features.InputAssets)
	profile, err := BuildProfile(t.Name, bindings, s.Registry, layer, t.Description, maxRisk(t.Risk, features.Risk), nil)
	if err != nil {
		return AgentProfile{}, []Violation{{Rule: "metadata", Detail: err.Error()}}
	}
	return profile, Validate(profile, s.Registry, s.Contracts)
}

func (s *Selector) matchTemplate(features TaskFeatures, allowed []Template) *Template {
	key := features.TemplateKey()
	if key == "" {
		return nil
	}
	for i := range allowed {
		if strings.EqualFold(allowed[i].Name, key) {
			return &allowed[i]
		}
	}
	return nil
}

func (s *Selector) byRules(features TaskFeatures, allowed []Template) (*AgentProfile, float64, string) {
	type scored struct {
		template Template
		score    float64
		hits     int
		reason   string
	}
	var scores []scored
	for _, t := range allowed {
		hits := 0
		profile, violations := s.buildFromTemplate(t, features, LayerRules)
		if len(violations) > 0 {
			continue
		}
		for kind, name := range profile.Bindings {
			spec, err := s.Registry.Get(kind, name)
			if err != nil {
				continue
			}
			for _, scenario := range spec.Scenarios {
				if strings.Contains(strings.ToLower(scenario), strings.ToLower(features.TaskType)) ||
					strings.Contains(strings.ToLower(features.TaskType), strings.ToLower(scenario)) {
					hits++
					break
				}
			}
		}
		score := float64(hits) / float64(s.Policy.RuleSaturation)
		if score > 1 {
			score = 1
		}
		scores = append(scores, scored{template: t, score: score, hits: hits,
			reason: "规则层：场景命中 " + itoa(hits) + " 个"})
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
	if len(scores) == 0 || scores[0].score < 0.5 {
		return nil, 0, ""
	}
	if len(scores) > 1 && scores[1].score == scores[0].score {
		return nil, 0, "" // 并列 → 交给评测层
	}
	profile, violations := s.buildFromTemplate(scores[0].template, features, LayerRules)
	if len(violations) > 0 {
		return nil, 0, ""
	}
	return &profile, scores[0].score, scores[0].reason
}

func (s *Selector) byEvaluated(ctx context.Context, features TaskFeatures, baseline AgentProfile, allowed []Template) (*AgentProfile, []Candidate, string) {
	records, err := s.Eval.Records(ctx, s.Policy.BenchmarkSet)
	if err != nil || len(records) == 0 {
		return nil, nil, ""
	}
	var baseCost, baseLatency float64
	baseSamples := 0
	for _, r := range records {
		if r.Matches(baseline) && r.Samples >= s.Policy.MinSamples {
			baseCost = r.Cost
			baseLatency = r.Latency
			baseSamples = r.Samples
			break
		}
	}
	if baseSamples == 0 {
		return nil, nil, "" // 没有 baseline 评测数据 → 评测层不成立
	}
	var candidates []Candidate
	for _, t := range allowed {
		profile, violations := s.buildFromTemplate(t, features, LayerEvaluated)
		if len(violations) > 0 {
			continue
		}
		for _, r := range records {
			if !r.Matches(profile) || r.Samples < s.Policy.MinSamples {
				continue
			}
			score := r.Quality
			if baseCost > 0 {
				score -= s.Policy.WeightCost * (r.Cost / baseCost)
			}
			if baseLatency > 0 {
				score -= s.Policy.WeightLatency * (r.Latency / baseLatency)
			}
			candidates = append(candidates, Candidate{Profile: profile, Score: score, Quality: r.Quality, Cost: r.Cost, Latency: r.Latency, Note: "评测样本 " + itoa(r.Samples)})
			break
		}
	}
	if len(candidates) == 0 {
		return nil, nil, ""
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	best := candidates[0]
	if best.Score <= 0 {
		return nil, candidates, ""
	}
	return &best.Profile, candidates, "评测层：按 quality − λc·cost − λt·latency 选出最高分"
}

func (s *Selector) emit(ctx context.Context, name string, payload map[string]any) {
	ev := event.New(name)
	ev.Payload = payload
	s.Events.Emit(ctx, ev)
}

func maxRisk(a, b RiskLevel) RiskLevel {
	if a == RiskHigh || b == RiskHigh {
		return RiskHigh
	}
	return RiskLow
}

type errorString string

func (e errorString) Error() string { return string(e) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// EnumerateValid 枚举合法组合（供 doctor / 评测 worker）。
func EnumerateValid(registry *Registry, contracts []Contract) (valid []AgentProfile, total int) {
	bindings := map[ComponentKind][]string{}
	for _, kind := range ComponentKinds {
		for _, spec := range registry.Available(kind) {
			bindings[kind] = append(bindings[kind], spec.Name)
		}
	}
	var walk func(idx int, current map[ComponentKind]string)
	walk = func(idx int, current map[ComponentKind]string) {
		if idx == len(ComponentKinds) {
			total++
			profile, err := BuildProfile("enum", copyBindings(current), registry, LayerRules, "枚举", RiskLow, nil)
			if err != nil {
				return
			}
			if len(Validate(profile, registry, contracts)) == 0 {
				valid = append(valid, profile)
			}
			return
		}
		kind := ComponentKinds[idx]
		for _, name := range bindings[kind] {
			current[kind] = name
			walk(idx+1, current)
		}
		delete(current, kind)
	}
	walk(0, map[ComponentKind]string{})
	if len(valid) > 5000 {
		slog.Warn("合法组合数量过多，说明契约约束可能失效", "valid", len(valid), "total", total)
	}
	return valid, total
}

func copyBindings(m map[ComponentKind]string) map[ComponentKind]string {
	out := make(map[ComponentKind]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
