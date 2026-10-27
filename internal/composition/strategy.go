package composition

import (
	"github.com/besa-ai/besa-agent/internal/store/postgres"
	"github.com/besa-ai/besa-agent/internal/strategy"
)

// RegisterBuiltinStrategies 登记组件策略的**元数据**，不实现它们。
// 一期真实可用的组合见 base.yaml 的模板。
func RegisterBuiltinStrategies(reg *strategy.Registry) []error {
	var errs []error
	add := func(kind strategy.ComponentKind, name, version, iface string, scenarios, provides, inputs []string) {
		errs = append(errs, reg.Register(strategy.StrategySpec{
			Kind: kind, Name: name, Version: version, Interface: iface,
			Inputs: inputs, Provides: provides, Preconditions: []string{},
			Cost:      strategy.CostEstimate{Tokens: 0, LatencyMS: 10, ExternalCalls: 0},
			Scenarios: scenarios, Metrics: map[string]string{"quality": "score", "cost": "tokens"}, Available: true,
		}))
	}
	unavailable := func(kind strategy.ComponentKind, name, reason string, provides []string) {
		errs = append(errs, reg.Register(strategy.StrategySpec{
			Kind: kind, Name: name, Version: "0.0.0", Interface: string(kind),
			Provides: provides, Preconditions: []string{}, Cost: strategy.CostEstimate{},
			Scenarios: []string{}, Metrics: map[string]string{}, Available: false, UnavailableReason: reason,
		}))
	}

	add(strategy.CompContext, "linear", "1.0.0", "context.builder", []string{"长任务", "测试设计"}, []string{"context_window"}, []string{"history"})
	add(strategy.CompContext, "tiered", "1.0.0", "context.builder", []string{"跨会话", "故障诊断", "用例生成"}, []string{"context_window", "session_summary"}, []string{"history"})
	add(strategy.CompContext, "full", "1.0.0", "context.builder", []string{"短对话"}, []string{"context_window"}, []string{"history"})

	add(strategy.CompMemory, "stateless", "1.0.0", "memory", []string{"单次问答"}, nil, nil)
	add(strategy.CompMemory, "session", "1.0.0", "memory", []string{"连续调试", "用例生成"}, []string{"memory_content"}, nil)
	add(strategy.CompMemory, "structured", "1.0.0", "memory", []string{"历史故障复用", "故障诊断"}, []string{"memory_content", "case_experience"}, nil)

	add(strategy.CompRetrieval, "vector", "1.0.0", "retrieval", []string{"知识问答"}, nil, []string{"query"})
	add(strategy.CompRetrieval, "hybrid", "1.0.0", "retrieval", []string{"知识问答", "故障诊断"}, nil, []string{"query"})
	add(strategy.CompRetrieval, "keyword", "1.0.0", "retrieval", []string{"精确参数查询", "用例生成"}, nil, []string{"query"})
	add(strategy.CompRetrieval, "case_kb", "1.0.0", "retrieval", []string{"故障诊断", "历史案例"}, []string{"case_experience"}, []string{"query"})
	unavailable(strategy.CompRetrieval, "bm25", "未实现 BM25 检索（需要倒排索引服务）", nil)
	unavailable(strategy.CompRetrieval, "sql", "未实现 SQL 检索（需要指标库 schema）", nil)
	unavailable(strategy.CompRetrieval, "query_decomposition", "未实现查询分解（需要 LLM 多轮检索）", nil)

	add(strategy.CompToolSelection, "fixed", "1.0.0", "tool.catalog", []string{"单一操作"}, nil, []string{"tools"})
	add(strategy.CompToolSelection, "category", "1.0.0", "tool.catalog", []string{"用例生成", "测试执行"}, []string{"write_access"}, []string{"tools"})
	add(strategy.CompToolSelection, "dependency_based", "1.0.0", "tool.catalog", []string{"环境操作"}, []string{"write_access"}, []string{"tools"})

	add(strategy.CompPermission, "readonly", "1.0.0", "tool.permission", []string{"知识查询"}, nil, nil)
	add(strategy.CompPermission, "restricted", "1.0.0", "tool.permission", []string{"自动化测试", "环境操作"}, []string{"write_access"}, nil)
	add(strategy.CompPermission, "sandbox", "1.0.0", "tool.permission", []string{"自动化测试"}, []string{"write_access"}, nil)
	unavailable(strategy.CompPermission, "approval", "未实现审批流（需要外部工单系统）", nil)

	add(strategy.CompValidation, "format", "1.0.0", "validator", []string{"用例生成"}, nil, []string{"artifact"})
	add(strategy.CompValidation, "rules", "1.0.0", "validator", []string{"测试执行", "故障诊断"}, nil, []string{"artifact"})
	unavailable(strategy.CompValidation, "cross_validation", "multiagent 未启用 —— 会安静退化成一个 agent 的结论", []string{"multi_agent_evidence"})
	unavailable(strategy.CompValidation, "citation_check", "未实现引用校验", nil)
	unavailable(strategy.CompValidation, "constraint_check", "未实现约束校验", nil)
	unavailable(strategy.CompValidation, "env_state_check", "未实现环境状态校验", nil)
	unavailable(strategy.CompValidation, "log_evidence", "未实现日志证据校验", nil)

	add(strategy.CompExecution, "single", "1.0.0", "agent.runtime", []string{"简单任务"}, nil, []string{"task"})
	add(strategy.CompExecution, "retry", "1.0.0", "agent.runtime", []string{"易失败任务", "故障诊断"}, nil, []string{"task"})
	add(strategy.CompExecution, "compare", "1.0.0", "agent.runtime", []string{"复杂诊断"}, []string{"multiple_candidates"}, []string{"task"})
	unavailable(strategy.CompExecution, "backtrack", "未实现回溯（需要检查点 + 分支重放）", nil)
	return errs
}

type StrategyBundle struct {
	Registry  *strategy.Registry
	Selector  *strategy.Selector
	Templates []strategy.Template
	Issues    []string
	Errors    []error
}

func BuildStrategy(base *Base) *StrategyBundle {
	cfg := base.Config
	registry := strategy.NewRegistry()
	errs := RegisterBuiltinStrategies(registry)
	templates, issues := strategy.BuildTemplates(cfg.Section("strategy.templates"), registry)

	var eval strategy.EvalStore
	if base.DB != nil {
		eval = &postgres.StrategyEvalRepo{DB: base.DB}
	} else {
		eval = strategy.NewMemoryEvalStore()
	}
	policy := strategy.DefaultSelectionPolicy()
	policy.Enabled = cfg.Bool("strategy.enabled", true)
	policy.Rules = cfg.Bool("strategy.selection.rules", true)
	policy.Evaluated = cfg.Bool("strategy.selection.evaluated", true)
	policy.WeightCost = cfg.Float("strategy.selection.weights.cost", 0.3)
	policy.WeightLatency = cfg.Float("strategy.selection.weights.latency", 0.2)
	policy.MinSamples = cfg.Int("strategy.selection.min_samples", 20)
	policy.HighRiskRequiresValidated = cfg.Bool("strategy.guard.high_risk_requires_validated", true)
	policy.DefaultProfile = cfg.String("strategy.default_profile", strategy.BaselineName)
	policy.RuleSaturation = cfg.Int("strategy.selection.rule_saturation", 2)
	policy.BenchmarkSet = cfg.String("strategy.evaluation.benchmark_set", "default")

	selector := strategy.NewSelector(registry, templates, strategy.DefaultContracts(), policy, eval, base.Events)
	return &StrategyBundle{Registry: registry, Selector: selector, Templates: templates, Issues: issues, Errors: errs}
}
