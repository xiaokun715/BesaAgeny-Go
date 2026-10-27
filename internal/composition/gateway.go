package composition

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/store/redis"
)

func BuildGateway(base *Base) (*gateway.Gateway, error) {
	cfg := base.Config
	registry, err := gateway.FromConfig(cfg, gateway.RegistryOptions{Env: cfg.EnvVars()})
	if err != nil {
		return nil, err
	}
	retry := gateway.RetryPolicy{
		MaxAttemptsPerCandidate: cfg.Int("gateway.retry.max_attempts_per_candidate", 2),
		TotalMaxAttempts:        cfg.Int("gateway.retry.total_max_attempts", 4),
		BackoffBase:             time.Duration(cfg.Float("gateway.retry.backoff_base_s", 1.0) * float64(time.Second)),
		JitterRatio:             cfg.Float("gateway.retry.jitter_ratio", 0.3),
	}
	if err := retry.Validate(); err != nil {
		return nil, err
	}
	health := gateway.NewHealthRegistry(gateway.HealthPolicy{
		FailureThreshold: cfg.Int("gateway.health.failure_threshold", 5),
		Cooldown:         time.Duration(cfg.Float("gateway.health.cooldown_s", 30) * float64(time.Second)),
		HalfOpenProbes:   cfg.Int("gateway.health.half_open_probes", 2),
	}, base.Clock)

	limitPolicy := gateway.RateLimitPolicy{
		RPM:            cfg.Int("gateway.rate_limit.defaults.rpm", 0),
		TPM:            cfg.Int("gateway.rate_limit.defaults.tpm", 0),
		MaxConcurrency: cfg.Int("gateway.rate_limit.defaults.max_concurrency", 0),
		OnExceed:       cfg.String("gateway.rate_limit.on_exceed", "wait"),
	}
	var limiter gateway.Limiter
	if !cfg.Bool("gateway.rate_limit.enabled", true) {
		limiter = gateway.NewLocalLimiter(gateway.RateLimitPolicy{}, base.Clock)
	} else if base.Redis != nil && cfg.String("gateway.rate_limit.backend", "local") == "redis" {
		limiter = redis.NewLimiter(base.Redis, limitPolicy)
	} else {
		limiter = gateway.NewLocalLimiter(limitPolicy, base.Clock)
	}

	cost := &gateway.CostSheet{Currency: cfg.String("gateway.cost.currency", "CNY"), Prices: map[string]gateway.Price{}}
	for modelKey, raw := range cfg.Section("gateway.cost.prices") {
		priceMap, _ := raw.(map[string]any)
		price := gateway.Price{}
		if v, ok := decimalFrom(priceMap["input"]); ok {
			price.Input = v
		}
		if v, ok := decimalFrom(priceMap["output"]); ok {
			price.Output = v
		}
		if v, ok := decimalFrom(priceMap["cached_input"]); ok {
			price.CachedInput = v
			price.HasCached = true
		}
		cost.Prices[modelKey] = price
	}

	gw := gateway.New(registry, gateway.Options{
		Retry: retry,
		Fallback: gateway.FallbackPolicy{
			Enabled:         cfg.Bool("gateway.fallback.enabled", true),
			OnStreamStarted: cfg.Bool("gateway.fallback.on_stream_started", false),
		},
		Health: health, Limiter: limiter,
		Ledger:   gateway.NewUsageLedger(cfg.Int("gateway.usage.max_records", 10000)),
		Cost:     cost,
		Events:   base.Events,
		Clock:    base.Clock,
		Deadline: time.Duration(cfg.Float("gateway.deadline.default_s", 120) * float64(time.Second)),
	})
	return gw, nil
}

func decimalFrom(v any) (decimal.Decimal, bool) {
	switch n := v.(type) {
	case float64:
		return decimal.NewFromFloat(n), true
	case int:
		return decimal.NewFromInt(int64(n)), true
	case string:
		d, err := decimal.NewFromString(n)
		return d, err == nil
	}
	return decimal.Zero, false
}

// LocalLLM 把 gateway 适配成 context.Summarizer / memory.LLM。
type LocalLLM struct {
	Gateway *gateway.Gateway
	Alias   string
}

func (l *LocalLLM) Complete(ctx context.Context, system, prompt string, schema map[string]any, maxTokens int) (string, error) {
	opts := gateway.ChatOptions{MaxTokens: &maxTokens}
	if schema != nil {
		opts.ResponseFormat = "json"
		opts.JSONSchema = schema
	}
	alias := l.Alias
	if alias == "" {
		alias = "runtime.default"
	}
	result, err := l.Gateway.Chat(ctx, alias, []domain.Message{domain.System(system), domain.User(prompt)}, opts)
	if err != nil {
		return "", err
	}
	return result.Content(), nil
}

// LocalEmbedder 把 gateway 适配成 memory.Embedder。
type LocalEmbedder struct {
	Gateway *gateway.Gateway
	Alias   string
}

func (l *LocalEmbedder) Embed(ctx context.Context, texts []string, traceID string) ([][]float32, error) {
	alias := l.Alias
	if alias == "" {
		alias = "emb.default"
	}
	result, err := l.Gateway.Embed(ctx, alias, texts, gateway.ChatOptions{TraceID: traceID})
	if err != nil {
		return nil, err
	}
	if result.Embedding == nil {
		return nil, fmt.Errorf("向量化结果为空")
	}
	return result.Embedding.Vectors, nil
}
