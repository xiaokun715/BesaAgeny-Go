package composition

import (
	"context"
	"time"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
	"github.com/besa-ai/besa-agent/internal/tool"
	"github.com/besa-ai/besa-agent/internal/tool/builtin"
)

type ToolsBundle struct {
	Registry *tool.Registry
	Executor *tool.Executor
	Repo     tool.ExecutionRepo
}

func BuildTools(base *Base, spill tool.SpillStore) (*ToolsBundle, error) {
	cfg := base.Config
	registry := tool.NewRegistry()
	enabled := cfg.StringSlice("tool.enabled")
	if enabled == nil {
		enabled = []string{"read", "grep", "write"}
	}
	enabledSet := map[string]bool{}
	for _, name := range enabled {
		enabledSet[name] = true
	}
	if enabledSet["read"] {
		_ = registry.Register(builtin.NewRead())
	}
	if enabledSet["grep"] {
		_ = registry.Register(builtin.NewGrep())
	}
	if enabledSet["write"] {
		_ = registry.Register(builtin.NewWrite())
	}
	if enabledSet["bash"] {
		_ = registry.Register(builtin.NewBash())
	}

	levels := cfg.StringSlice("tool.permission.levels")
	if levels == nil {
		levels = []string{"read"}
	}
	scopeCfg := cfg.Section("tool.permission.scope")
	scopes := map[string][]string{}
	for level, raw := range scopeCfg {
		scopes[level] = toStringSlice(raw)
	}
	permission := tool.NewPermissionPolicy(levels, scopes)

	limits := tool.Limits{
		MaxOutputBytes: cfg.Int("tool.limits.max_output_bytes", 262144),
		MaxOutputLines: cfg.Int("tool.limits.max_output_lines", 2000),
		Timeout:        time.Duration(cfg.Float("tool.limits.timeout_s", 60) * float64(time.Second)),
		MaxCallsPerRun: cfg.Int("tool.limits.max_calls_per_run", 200),
		MaxCallDepth:   cfg.Int("tool.limits.max_call_depth", 8),
		SpillThreshold: cfg.Int("tool.result.spill_threshold_bytes", 65536),
		SpillQuotaMB:   cfg.Int("tool.result.spill_total_quota_mb", 512),
		SpillDir:       cfg.String("tool.result.spill_dir", ""),
		SpillTTL:       time.Duration(cfg.Float("tool.result.spill_ttl_hours", 24) * float64(time.Hour)),
	}

	// 幂等的权威记录：有数据库就用 Postgres，没有就退到进程内（仍能防并发重复）。
	var repo tool.ExecutionRepo
	if base.DB != nil {
		repo = &postgres.ToolExecutionRepo{DB: base.DB}
	} else {
		repo = tool.NewMemoryExecutionRepo()
	}
	var fast tool.FastStore
	if base.Redis != nil {
		fast = base.Redis
	}
	onUnavailable := map[tool.SideEffect]string{}
	if raw, ok := cfg.Get("idempotency.on_unavailable"); ok {
		if m, ok := raw.(map[string]any); ok {
			for level, action := range m {
				onUnavailable[tool.SideEffect(level)] = asString(action)
			}
		}
	}
	guard := tool.NewIdempotencyGuard(repo, fast,
		time.Duration(cfg.Float("idempotency.lease_s", 120)*float64(time.Second)),
		time.Duration(cfg.Float("idempotency.result_ttl_s", 3600)*float64(time.Second)),
		cfg.Int("idempotency.max_cached_result_bytes", 65536), onUnavailable)

	results := tool.NewResultProcessor(limits, cfg.Bool("tool.output.redact", true))
	results.Store = spill

	executor := tool.NewExecutor(registry, permission, guard, base.Events, limits)
	executor.Inspector = tool.InjectionPolicy{
		CheckPathTraversal:   cfg.Bool("tool.injection.check_path_traversal", true),
		URLAllowlist:         cfg.StringSlice("tool.injection.url_allowlist"),
		BlockPrivateNetworks: cfg.Bool("tool.injection.block_private_networks", true),
		DeniedPaths:          cfg.StringSlice("tool.output.deny_paths"),
	}
	executor.Results = results
	executor.KeyField = cfg.String("idempotency.key_field", "idempotency_key")
	return &ToolsBundle{Registry: registry, Executor: executor, Repo: repo}, nil
}

// CatalogAdapter 把注册表的筛选结果交给 agent。
type CatalogAdapter struct {
	Registry *tool.Registry
}

func (c CatalogAdapter) Select(categories, only []string) []tool.Definition {
	return c.Registry.Select(categories, only)
}

// ToolFinderAdapter 把工具执行记录适配成 agent.ToolFinder。
type ToolFinderAdapter struct {
	Repo tool.ExecutionRepo
}

func (a ToolFinderAdapter) Find(ctx context.Context, scope, idemKey string, lease time.Duration) (*agent.ToolExecutionView, error) {
	if a.Repo == nil {
		return nil, nil
	}
	view, err := a.Repo.Find(ctx, scope, idemKey, lease)
	if err != nil || view == nil {
		return nil, err
	}
	return &agent.ToolExecutionView{ToolName: view.ToolName, State: view.State, Uncertain: view.Uncertain()}, nil
}

func toStringSlice(v any) []string {
	switch list := v.(type) {
	case []any:
		out := make([]string, len(list))
		for i, item := range list {
			out[i] = asString(item)
		}
		return out
	case []string:
		return list
	case string:
		return []string{list}
	}
	return nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
