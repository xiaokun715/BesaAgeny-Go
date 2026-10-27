package composition

import (
	"time"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/store/memstore"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
)

func BuildStages(base *Base, knownTools []string) (*agent.StageRegistry, []string) {
	cfg := base.Config.Section("agent.stages")
	return agent.BuildStages(cfg, knownTools)
}

func BuildCheckpoints(base *Base) agent.CheckpointStore {
	if base.DB != nil {
		return &postgres.CheckpointRepo{DB: base.DB}
	}
	return memstore.NewCheckpoints()
}

func BuildAgent(
	base *Base,
	chat agent.ChatClient,
	tools agent.ToolInvoker,
	catalog agent.ToolCatalog,
	contextBuilder agent.ContextBuilder,
	checkpoints agent.CheckpointStore,
	finder agent.ToolFinder,
) (*agent.Runtime, error) {
	cfg := base.Config
	registry, warnings := BuildStages(base, nil)
	for _, w := range warnings {
		base.Logger.Warn(w)
	}
	router := agent.NewStageRouter(registry,
		cfg.String("agent.routing.on_unknown", "ask"),
		cfg.String("agent.routing.default_stage", "requirement"),
		cfg.Bool("agent.routing.use_llm", false),
		cfg.Float("agent.routing.rule_threshold", 0.5),
		cfg.Int("agent.routing.rule_saturation", 2),
		chat, cfg.String("agent.alias", "runtime.default"))

	policy := agent.DefaultPolicy()
	policy.Alias = cfg.String("agent.alias", "runtime.default")
	policy.Limits.MaxSteps = cfg.Int("agent.limits.max_steps", 20)
	policy.Limits.MaxToolCalls = cfg.Int("agent.limits.max_tool_calls", 100)
	if cfg.IsSet("agent.limits.deadline_s") && !cfg.IsNullLike("agent.limits.deadline_s") {
		d := time.Duration(cfg.Float("agent.limits.deadline_s", 600) * float64(time.Second))
		policy.Limits.Deadline = &d
	} else {
		policy.Limits.Deadline = nil
	}
	if cfg.IsSet("agent.limits.call_timeout_s") && !cfg.IsNullLike("agent.limits.call_timeout_s") {
		d := time.Duration(cfg.Float("agent.limits.call_timeout_s", 120) * float64(time.Second))
		policy.Limits.CallTimeout = &d
	}
	if cfg.IsSet("agent.limits.max_cost") && !cfg.IsNullLike("agent.limits.max_cost") {
		if v, ok := decimalFrom(cfg.GetOrNil("agent.limits.max_cost")); ok {
			policy.Limits.MaxCost = &v
		}
	}
	policy.Resume.OnUncertain = cfg.String("agent.resume.on_uncertain", "needs_review")
	policy.Resume.Lease = time.Duration(cfg.Float("agent.resume.lease_s", 120) * float64(time.Second))
	policy.Context.Enabled = cfg.Bool("agent.context.enabled", true)
	policy.Context.KeepRecentSteps = cfg.Int("agent.context.keep_recent_steps", 4)
	policy.Context.TargetRatio = cfg.Float("agent.context.target_ratio", 0.6)
	policy.EmitEvents = cfg.Bool("agent.emit_events", true)
	if policy.Context.KeepRecentSteps < 1 {
		policy.Context.KeepRecentSteps = 1
	}

	executor := agent.NewAgentExecutor(chat, tools, catalog, policy.Alias, policy.Limits.MaxSteps, checkpoints)
	return agent.NewRuntime(executor, registry, router, contextBuilder, checkpoints, finder, policy, base.Events), nil
}
