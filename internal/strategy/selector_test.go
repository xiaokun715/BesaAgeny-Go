package strategy_test

import (
	"context"
	"testing"

	"github.com/besa-ai/besa-agent/internal/strategy"
)

func spec(kind strategy.ComponentKind, name string, provides ...string) strategy.StrategySpec {
	return strategy.StrategySpec{
		Kind: kind, Name: name, Version: "1.0.0", Interface: string(kind),
		Provides: provides, Preconditions: []string{}, Cost: strategy.CostEstimate{},
		Scenarios: []string{}, Metrics: map[string]string{}, Available: true,
	}
}

func testRegistry(t *testing.T) *strategy.Registry {
	t.Helper()
	reg := strategy.NewRegistry()
	specs := []strategy.StrategySpec{
		spec(strategy.CompContext, "tiered"),
		spec(strategy.CompMemory, "session", "memory_content"),
		spec(strategy.CompRetrieval, "keyword"),
		spec(strategy.CompToolSelection, "fixed"),
		spec(strategy.CompPermission, "restricted", "write_access"),
		spec(strategy.CompValidation, "rules"),
		spec(strategy.CompValidation, "format"),
		spec(strategy.CompExecution, "retry"),
		spec(strategy.CompExecution, "single"),
		spec(strategy.CompExecution, "compare", "multiple_candidates"),
		spec(strategy.CompToolSelection, "category", "write_access"),
		spec(strategy.CompPermission, "readonly"),
	}
	reg.MarkUnavailable(strategy.CompValidation, "cross_validation", "multiagent 未启用")
	for _, s := range specs {
		if err := reg.Register(s); err != nil {
			t.Fatalf("register %s: %v", s.Key(), err)
		}
	}
	return reg
}

func baseBindings() map[strategy.ComponentKind]string {
	return map[strategy.ComponentKind]string{
		strategy.CompContext: "tiered", strategy.CompMemory: "session", strategy.CompRetrieval: "keyword",
		strategy.CompToolSelection: "fixed", strategy.CompPermission: "restricted",
		strategy.CompValidation: "rules", strategy.CompExecution: "retry",
	}
}

func testTemplates() []strategy.Template {
	base := baseBindings()
	caseGen := map[strategy.ComponentKind]string{}
	for k, v := range base {
		caseGen[k] = v
	}
	caseGen[strategy.CompValidation] = "format"
	caseGen[strategy.CompToolSelection] = "category"
	return []strategy.Template{
		{Name: "baseline", Risk: strategy.RiskLow, Bindings: base, Trim: map[string][]strategy.ComponentKind{}},
		{Name: "case_generation", Risk: strategy.RiskLow, Bindings: caseGen, Trim: map[string][]strategy.ComponentKind{}},
	}
}

func TestTemplateSelection(t *testing.T) {
	selector := strategy.NewSelector(testRegistry(t), testTemplates(), nil, strategy.DefaultSelectionPolicy(), nil, nil)
	decision := selector.Select(context.Background(), strategy.TaskFeatures{TaskType: "case_generation", Risk: strategy.RiskLow})
	if decision.Refused || decision.Profile == nil {
		t.Fatalf("expected profile, got %+v", decision)
	}
	if decision.Layer != strategy.LayerTemplate {
		t.Fatalf("expected template layer, got %s (%s)", decision.Layer, decision.Reason)
	}
	if decision.Profile.StrategyOf(strategy.CompValidation) != "format" {
		t.Fatalf("template binding not applied: %+v", decision.Profile.Bindings)
	}
}

func TestHighRiskRefusedWithoutValidatedCombos(t *testing.T) {
	templates := []strategy.Template{testTemplates()[0]} // 只有 baseline
	selector := strategy.NewSelector(testRegistry(t), templates, nil, strategy.DefaultSelectionPolicy(), nil, nil)
	decision := selector.Select(context.Background(), strategy.TaskFeatures{TaskType: "unknown", Risk: strategy.RiskHigh})
	if !decision.Refused {
		t.Fatalf("高风险无已验证组合时必须拒绝，得到 %+v", decision)
	}
}

func TestContractsCatchReadonlyAndCompare(t *testing.T) {
	reg := testRegistry(t)
	bindings := baseBindings()
	bindings[strategy.CompPermission] = "readonly"
	bindings[strategy.CompToolSelection] = "category"
	profile, err := strategy.BuildProfile("bad", bindings, reg, strategy.LayerRules, "test", strategy.RiskLow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := strategy.Validate(profile, reg, strategy.DefaultContracts()); len(v) == 0 {
		t.Fatal("readonly + category 应触发契约违规")
	}

	bindings = baseBindings()
	bindings[strategy.CompExecution] = "compare"
	profile, _ = strategy.BuildProfile("cmp", bindings, reg, strategy.LayerRules, "test", strategy.RiskLow, nil)
	// registry 里 compare 自己 provides multiple_candidates，因此这条规则不应误报。
	for _, v := range strategy.Validate(profile, reg, strategy.DefaultContracts()) {
		if v.Rule == "compare_needs_multiple_candidates" {
			t.Fatalf("compare 自身提供 multiple_candidates，不应违规：%+v", v)
		}
	}
}

func TestEnumerateValid(t *testing.T) {
	valid, total := strategy.EnumerateValid(testRegistry(t), strategy.DefaultContracts())
	if total == 0 || len(valid) == 0 {
		t.Fatalf("expected some valid combos, valid=%d total=%d", len(valid), total)
	}
}
