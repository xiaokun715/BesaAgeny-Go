package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/foundation/clock"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
	"github.com/besa-ai/besa-agent/internal/provider"
)

const (
	EventCallStarted    = "gateway.call.started"
	EventCallSucceeded  = "gateway.call.succeeded"
	EventCallFailed     = "gateway.call.failed"
	EventCallRetried    = "gateway.call.retried"
	EventCallDegraded   = "gateway.call.degraded"
	EventModelSkipped   = "gateway.model.skipped"
	EventCircuitOpened  = "gateway.circuit.opened"
	EventQuotaExhausted = "gateway.quota.exhausted"
)

type Options struct {
	Retry    RetryPolicy
	Fallback FallbackPolicy
	Health   *HealthRegistry
	Limiter  Limiter
	Ledger   *UsageLedger
	Cost     *CostSheet
	Events   event.Emitter
	Clock    clock.Clock
	Deadline time.Duration
}

type Gateway struct {
	registry *Registry
	retry    RetryPolicy
	fallback FallbackPolicy
	health   *HealthRegistry
	limiter  Limiter
	ledger   *UsageLedger
	cost     *CostSheet
	events   event.Emitter
	clk      clock.Clock
	deadline time.Duration

	mu     sync.Mutex
	chats  map[string]provider.ChatModel
	embeds map[string]provider.EmbeddingModel
}

func New(registry *Registry, opts Options) *Gateway {
	if opts.Retry.TotalMaxAttempts == 0 {
		opts.Retry = DefaultRetryPolicy()
	}
	if opts.Health == nil {
		opts.Health = NewHealthRegistry(DefaultHealthPolicy(), opts.Clock)
	}
	if opts.Limiter == nil {
		opts.Limiter = NewLocalLimiter(RateLimitPolicy{}, opts.Clock)
	}
	if opts.Ledger == nil {
		opts.Ledger = NewUsageLedger(0)
	}
	if opts.Cost == nil {
		opts.Cost = &CostSheet{Currency: "CNY", Prices: map[string]Price{}}
	}
	if opts.Events == nil {
		opts.Events = event.Null{}
	}
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.Deadline <= 0 {
		opts.Deadline = 120 * time.Second
	}
	if !opts.Fallback.Enabled && !opts.Fallback.OnStreamStarted {
		opts.Fallback = DefaultFallbackPolicy()
	}
	return &Gateway{
		registry: registry,
		retry:    opts.Retry,
		fallback: opts.Fallback,
		health:   opts.Health,
		limiter:  opts.Limiter,
		ledger:   opts.Ledger,
		cost:     opts.Cost,
		events:   event.Safe(opts.Events),
		clk:      opts.Clock,
		deadline: opts.Deadline,
		chats:    map[string]provider.ChatModel{},
		embeds:   map[string]provider.EmbeddingModel{},
	}
}

type ChatOptions struct {
	TraceID        string
	SessionID      string
	Caller         string
	Deadline       time.Duration
	Temperature    *float64
	MaxTokens      *int
	Tools          []domain.ToolSpec
	ToolChoice     string
	ResponseFormat string
	JSONSchema     map[string]any
	Stop           []string
}

func (o ChatOptions) deadlineOr(def time.Duration) time.Duration {
	if o.Deadline > 0 {
		return o.Deadline
	}
	return def
}

// Chat 是对话补全的唯一入口。
func (g *Gateway) Chat(ctx context.Context, alias string, messages []domain.Message, opts ChatOptions) (domain.GatewayResult, error) {
	if opts.TraceID != "" {
		ctx = ids.WithTrace(ctx, opts.TraceID)
	}
	rctx := RoutingContext{
		Required:  RoutingForChat(false, len(opts.Tools) > 0, hasVision(messages), opts.ResponseFormat != "" && opts.ResponseFormat != "text"),
		SessionID: opts.SessionID, Caller: opts.Caller, CostOf: g.costLookup, Health: g.health, Describes: "对话",
	}
	budget := NewCallBudget(g.retry.TotalMaxAttempts, g.clk.Now().Add(opts.deadlineOr(g.deadline)))
	estimate := EstimateMessagesTokens(messages)
	chain, skipped, err := g.plan(alias, rctx)
	if err != nil {
		return domain.GatewayResult{}, err
	}
	if len(chain) == 0 {
		return domain.GatewayResult{}, &NoCapableModelError{Alias: alias, Missing: skipped}
	}
	g.emit(ctx, EventCallStarted, map[string]any{"alias": alias, "session_id": opts.SessionID, "candidates": keysOf(chain)})

	attempts := make([]domain.AttemptRecord, 0, len(chain))
	for index, spec := range chain {
		remaining := len(chain) - index - 1
		resp, ok := g.attempt(ctx, spec, alias, messages, budget, estimate, &attempts, opts, false)
		if ok {
			return g.finalize(resp, alias, spec, attempts, index, opts), nil
		}
		if err := ctx.Err(); err != nil {
			return domain.GatewayResult{}, err
		}
		decision := DecideFallback(g.fallback, budget, remaining, false)
		if decision.Proceed {
			g.emit(ctx, EventCallDegraded, map[string]any{"alias": alias, "session_id": opts.SessionID, "from": spec.Key, "reason": decision.Reason})
			continue
		}
		return domain.GatewayResult{}, g.terminal(decision.Reason, alias, attempts)
	}
	return domain.GatewayResult{}, g.terminal("no_more_candidates", alias, attempts)
}

// Embed 是向量化的唯一入口。
func (g *Gateway) Embed(ctx context.Context, alias string, texts []string, opts ChatOptions) (domain.GatewayResult, error) {
	if opts.TraceID != "" {
		ctx = ids.WithTrace(ctx, opts.TraceID)
	}
	rctx := RoutingContext{
		Required: []domain.Capability{domain.CapEmbedding}, SessionID: opts.SessionID,
		Caller: opts.Caller, CostOf: g.costLookup, Health: g.health, Describes: "向量化",
	}
	budget := NewCallBudget(g.retry.TotalMaxAttempts, g.clk.Now().Add(opts.deadlineOr(g.deadline)))
	estimate := 0
	for _, t := range texts {
		estimate += EstimateTextTokens(t)
	}
	chain, skipped, err := g.plan(alias, rctx)
	if err != nil {
		return domain.GatewayResult{}, err
	}
	if len(chain) == 0 {
		return domain.GatewayResult{}, &NoCapableModelError{Alias: alias, Missing: skipped}
	}
	g.emit(ctx, EventCallStarted, map[string]any{"alias": alias, "session_id": opts.SessionID, "candidates": keysOf(chain)})

	attempts := make([]domain.AttemptRecord, 0, len(chain))
	for index, spec := range chain {
		remaining := len(chain) - index - 1
		var emb domain.EmbeddingResult
		_, ok := g.attemptEmbed(ctx, spec, alias, texts, budget, estimate, &attempts, opts, &emb)
		if ok {
			result := g.finalize(domain.ChatResponse{Usage: emb.Usage, Model: emb.Model, TraceID: emb.TraceID}, alias, spec, attempts, index, opts)
			result.Embedding = &emb
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			return domain.GatewayResult{}, err
		}
		decision := DecideFallback(g.fallback, budget, remaining, false)
		if decision.Proceed {
			g.emit(ctx, EventCallDegraded, map[string]any{"alias": alias, "session_id": opts.SessionID, "from": spec.Key, "reason": decision.Reason})
			continue
		}
		return domain.GatewayResult{}, g.terminal(decision.Reason, alias, attempts)
	}
	return domain.GatewayResult{}, g.terminal("no_more_candidates", alias, attempts)
}

// StreamChat 同步完成 alias 解析与选链 —— 配置错误在调用瞬间抛出。
func (g *Gateway) StreamChat(ctx context.Context, alias string, messages []domain.Message, opts ChatOptions) (<-chan domain.StreamEvent, error) {
	if opts.TraceID != "" {
		ctx = ids.WithTrace(ctx, opts.TraceID)
	}
	rctx := RoutingContext{
		Required: RoutingForChat(true, false, hasVision(messages), false), SessionID: opts.SessionID,
		Caller: opts.Caller, CostOf: g.costLookup, Health: g.health, Describes: "流式对话",
	}
	chain, skipped, err := g.plan(alias, rctx)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return nil, &NoCapableModelError{Alias: alias, Missing: skipped}
	}
	budget := NewCallBudget(g.retry.TotalMaxAttempts, g.clk.Now().Add(opts.deadlineOr(g.deadline)))
	out := make(chan domain.StreamEvent, 16)
	go g.streamLoop(ctx, alias, chain, messages, budget, opts, out)
	return out, nil
}

func (g *Gateway) streamLoop(ctx context.Context, alias string, chain []ModelSpec, messages []domain.Message, budget *CallBudget, opts ChatOptions, out chan<- domain.StreamEvent) {
	defer close(out)
	emit := func(ev domain.StreamEvent) bool {
		select {
		case <-ctx.Done():
			return false
		case out <- ev:
			return true
		}
	}
	attempts := []domain.AttemptRecord{}
	estimate := EstimateMessagesTokens(messages)

	for index, spec := range chain {
		remaining := len(chain) - index - 1
		if !g.health.Allow(spec.Key) {
			attempts = append(attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "circuit_open"})
			continue
		}
		decision, err := g.limiter.Acquire(ctx, spec.Key, estimate, budget)
		if err != nil || !decision.Allowed {
			g.health.Release(spec.Key)
			attempts = append(attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "rate_limited:" + decision.Reason})
			continue
		}
		if !budget.TryAcquire() {
			g.health.Release(spec.Key)
			g.limiter.Release(spec.Key)
			attempts = append(attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "budget_exhausted"})
			continue
		}

		model, err := g.chatModel(spec)
		if err != nil {
			g.health.RecordFailure(spec.Key)
			g.limiter.Release(spec.Key)
			attempts = append(attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "failed", Error: err.Error()})
			if DecideFallback(g.fallback, budget, remaining, false).Proceed {
				continue
			}
			emit(domain.StreamEvent{Kind: domain.StreamKindFailed, Err: err, Attempts: attempts})
			return
		}
		stream, err := model.StreamChat(ctx, domain.ChatRequest{
			Messages: messages, Stream: true, Temperature: opts.Temperature, MaxTokens: opts.MaxTokens,
			Timeout: spec.Config.Timeout, TraceID: opts.TraceID,
		})
		if err != nil {
			g.health.RecordFailure(spec.Key)
			g.limiter.Release(spec.Key)
			attempts = append(attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "failed", Error: err.Error(), Retryable: provider.IsRetryable(err)})
			if DecideFallback(g.fallback, budget, remaining, false).Proceed {
				continue
			}
			emit(domain.StreamEvent{Kind: domain.StreamKindFailed, Err: err, Attempts: attempts})
			return
		}

		committed := false
		var text []byte
		var streamErr error
		for {
			chunk, err := stream.Recv()
			if err == nil {
				if chunk == "" {
					continue
				}
				committed = true
				text = append(text, chunk...)
				if !emit(domain.StreamEvent{Kind: domain.StreamKindChunk, Text: chunk}) {
					stream.Close()
					g.health.Release(spec.Key)
					g.limiter.Release(spec.Key)
					return
				}
				continue
			}
			if errors.Is(err, io.EOF) {
				break
			}
			streamErr = err
			break
		}
		_ = stream.Close()
		g.limiter.Release(spec.Key)

		if streamErr != nil {
			if ctx.Err() != nil {
				g.health.Release(spec.Key)
				return
			}
			g.health.RecordFailure(spec.Key)
			attempts = append(attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "failed", Error: streamErr.Error(), Retryable: provider.IsRetryable(streamErr)})
			// 只在**一个分片都没吐出去**时允许降级。
			if !committed && DecideFallback(g.fallback, budget, remaining, false).Proceed {
				continue
			}
			var terminal error = streamErr
			if committed {
				terminal = &StreamCommittedError{Alias: alias, Message: fmt.Sprintf("流式已输出 %d 字节后失败，无法重试或降级：%s", len(text), streamErr)}
			}
			emit(domain.StreamEvent{Kind: domain.StreamKindFailed, Err: terminal, Attempts: attempts})
			return
		}

		g.health.RecordSuccess(spec.Key)
		attempts = append(attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "success"})
		resp := domain.ChatResponse{Content: string(text), Model: spec.Model, FinishReason: "stop", TraceID: opts.TraceID}
		result := g.finalize(resp, alias, spec, attempts, index, opts)
		emit(domain.StreamEvent{Kind: domain.StreamKindDone, Result: &result})
		return
	}

	exhausted := budget.ExhaustedReason()
	var terminal error = &AllCandidatesFailedError{Alias: alias, Attempts: attempts, Reason: "no_more_candidates"}
	if exhausted != "" && len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		if last.Outcome == "skipped" && last.SkippedReason == "budget_exhausted" {
			terminal = &BudgetExhaustedError{Alias: alias, Reason: exhausted, Attempts: attempts}
		}
	}
	emit(domain.StreamEvent{Kind: domain.StreamKindFailed, Err: terminal, Attempts: attempts})
}

// attempt 执行一个候选（含重试）。成功返回 true。
func (g *Gateway) attempt(ctx context.Context, spec ModelSpec, alias string, messages []domain.Message, budget *CallBudget, estimate int, attempts *[]domain.AttemptRecord, opts ChatOptions, _ bool) (domain.ChatResponse, bool) {
	if !g.health.Allow(spec.Key) {
		*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "circuit_open"})
		g.emit(ctx, EventModelSkipped, map[string]any{"model": spec.Key, "session_id": opts.SessionID, "reason": "circuit_open"})
		return domain.ChatResponse{}, false
	}
	decision, err := g.limiter.Acquire(ctx, spec.Key, estimate, budget)
	if err != nil || !decision.Allowed {
		g.health.Release(spec.Key)
		reason := decision.Reason
		if reason == "" {
			reason = "cancelled"
		}
		*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "rate_limited:" + reason})
		g.emit(ctx, EventQuotaExhausted, map[string]any{"model": spec.Key, "session_id": opts.SessionID, "reason": reason})
		return domain.ChatResponse{}, false
	}
	probe := true
	defer func() {
		if probe {
			g.health.Release(spec.Key)
		}
		g.limiter.Release(spec.Key)
	}()

	retryIndex := 0
	for {
		if !budget.TryAcquire() {
			*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "budget_exhausted"})
			g.health.Release(spec.Key)
			probe = false
			return domain.ChatResponse{}, false
		}
		model, err := g.chatModel(spec)
		if err != nil {
			g.health.RecordFailure(spec.Key)
			probe = false
			*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "failed", Error: err.Error()})
			return domain.ChatResponse{}, false
		}
		started := g.clk.Now()
		callTimeout := spec.Config.Timeout
		if remaining := budget.RemainingSeconds(); remaining > 0 && time.Duration(remaining*float64(time.Second)) < callTimeout {
			callTimeout = time.Duration(remaining * float64(time.Second))
		}
		req := domain.ChatRequest{
			Messages: messages, Temperature: opts.Temperature, MaxTokens: opts.MaxTokens,
			ResponseFormat: opts.ResponseFormat, JSONSchema: opts.JSONSchema, Stop: opts.Stop,
			Tools: opts.Tools, ToolChoice: opts.ToolChoice, Timeout: callTimeout, TraceID: opts.TraceID,
		}
		resp, err := model.Chat(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				g.health.Release(spec.Key)
				probe = false
				return domain.ChatResponse{}, false
			}
			g.health.RecordFailure(spec.Key)
			probe = false
			retryable := provider.IsRetryable(err)
			*attempts = append(*attempts, domain.AttemptRecord{
				ModelKey: spec.Key, Provider: spec.Provider, Outcome: "failed",
				Error: err.Error(), Retryable: retryable, ElapsedS: g.clk.Since(started).Seconds(),
			})
			g.recordFailureUsage(spec, alias, opts)
			g.emit(ctx, EventCallFailed, map[string]any{"model": spec.Key, "session_id": opts.SessionID, "error": err.Error(), "retryable": retryable})
			if !retryable || retryIndex >= g.retry.MaxAttemptsPerCandidate-1 {
				return domain.ChatResponse{}, false
			}
			delay := Backoff(retryIndex, g.retry)
			if delay.Seconds() > budget.RemainingSeconds() {
				return domain.ChatResponse{}, false
			}
			retryIndex++
			g.emit(ctx, EventCallRetried, map[string]any{"model": spec.Key, "session_id": opts.SessionID, "attempt": retryIndex, "delay_s": delay.Seconds()})
			if err := g.clk.Sleep(ctx, delay); err != nil {
				return domain.ChatResponse{}, false
			}
			continue
		}
		g.health.RecordSuccess(spec.Key)
		probe = false
		actual := 0
		if total := resp.Usage.Total(); total != nil {
			actual = *total
		}
		g.limiter.Reconcile(spec.Key, estimate, &actual)
		*attempts = append(*attempts, domain.AttemptRecord{
			ModelKey: spec.Key, Provider: spec.Provider, Outcome: "success",
			ElapsedS: g.clk.Since(started).Seconds(),
		})
		return resp, true
	}
}

func (g *Gateway) attemptEmbed(ctx context.Context, spec ModelSpec, alias string, texts []string, budget *CallBudget, estimate int, attempts *[]domain.AttemptRecord, opts ChatOptions, out *domain.EmbeddingResult) (domain.ChatResponse, bool) {
	if !g.health.Allow(spec.Key) {
		*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "circuit_open"})
		return domain.ChatResponse{}, false
	}
	decision, err := g.limiter.Acquire(ctx, spec.Key, estimate, budget)
	if err != nil || !decision.Allowed {
		g.health.Release(spec.Key)
		*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "rate_limited:" + decision.Reason})
		return domain.ChatResponse{}, false
	}
	probe := true
	defer func() {
		if probe {
			g.health.Release(spec.Key)
		}
		g.limiter.Release(spec.Key)
	}()
	if !budget.TryAcquire() {
		g.health.Release(spec.Key)
		probe = false
		*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "skipped", SkippedReason: "budget_exhausted"})
		return domain.ChatResponse{}, false
	}
	model, err := g.embeddingModel(spec)
	if err != nil {
		g.health.RecordFailure(spec.Key)
		probe = false
		*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "failed", Error: err.Error()})
		return domain.ChatResponse{}, false
	}
	res, err := model.Embed(ctx, texts, 0, opts.TraceID)
	if err != nil {
		g.health.RecordFailure(spec.Key)
		probe = false
		*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "failed", Error: err.Error(), Retryable: provider.IsRetryable(err)})
		g.recordFailureUsage(spec, alias, opts)
		return domain.ChatResponse{}, false
	}
	g.health.RecordSuccess(spec.Key)
	probe = false
	actual := 0
	if total := res.Usage.Total(); total != nil {
		actual = *total
	}
	g.limiter.Reconcile(spec.Key, estimate, &actual)
	*attempts = append(*attempts, domain.AttemptRecord{ModelKey: spec.Key, Provider: spec.Provider, Outcome: "success"})
	*out = res
	return domain.ChatResponse{Usage: res.Usage, Model: res.Model, TraceID: res.TraceID}, true
}

func (g *Gateway) plan(alias string, rctx RoutingContext) ([]ModelSpec, map[string]string, error) {
	a, err := g.registry.Alias(alias)
	if err != nil {
		return nil, nil, err
	}
	candidates, err := g.registry.Candidates(alias)
	if err != nil {
		return nil, nil, err
	}
	chain, _ := Select(candidates, rctx, a.Strategy)
	skipped := map[string]string{}
	for _, c := range candidates {
		if !c.Available() {
			skipped[c.Key] = c.UnavailableReason
			continue
		}
		if missing := c.Capabilities.Missing(rctx.Required); len(missing) > 0 {
			names := make([]string, len(missing))
			for i, m := range missing {
				names[i] = string(m)
			}
			skipped[c.Key] = "缺少能力 " + join(names, ",")
		}
	}
	return chain, skipped, nil
}

func (g *Gateway) chatModel(spec ModelSpec) (provider.ChatModel, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m, ok := g.chats[spec.Key]; ok {
		return m, nil
	}
	p, err := g.registry.Provider(spec.Key)
	if err != nil {
		return nil, err
	}
	m, err := p.ChatModel()
	if err != nil {
		return nil, err
	}
	g.chats[spec.Key] = m
	return m, nil
}

func (g *Gateway) embeddingModel(spec ModelSpec) (provider.EmbeddingModel, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m, ok := g.embeds[spec.Key]; ok {
		return m, nil
	}
	p, err := g.registry.Provider(spec.Key)
	if err != nil {
		return nil, err
	}
	m, err := p.EmbeddingModel()
	if err != nil {
		return nil, err
	}
	g.embeds[spec.Key] = m
	return m, nil
}

func (g *Gateway) finalize(resp domain.ChatResponse, alias string, spec ModelSpec, attempts []domain.AttemptRecord, index int, opts ChatOptions) domain.GatewayResult {
	cost := g.cost.Calc(spec.Key, resp.Usage)
	degraded := index > 0
	g.ledger.Record(UsageRecord{
		TraceID: opts.TraceID, Alias: alias, ModelKey: spec.Key, Provider: spec.Provider, Model: spec.Model,
		InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens,
		CachedInputTokens: resp.Usage.CachedInputTokens, SessionID: opts.SessionID, Caller: opts.Caller,
		At: g.clk.Now().UTC(), Cost: cost, Degraded: degraded, AttemptIndex: len(attempts),
	})
	if degraded {
		slog.Warn("发生降级", "alias", alias, "model", spec.Key, "index", index+1, "trace_id", opts.TraceID)
	}
	g.emit(context.Background(), EventCallSucceeded, map[string]any{
		"alias": alias, "model": spec.Key, "trace_id": opts.TraceID, "session_id": opts.SessionID, "degraded": degraded, "cost": cost.String(),
	})
	return domain.GatewayResult{
		Response: resp, Alias: alias, ModelKey: spec.Key, Attempts: attempts,
		Degraded: degraded, Usage: resp.Usage, Cost: cost, TraceID: opts.TraceID,
	}
}

func (g *Gateway) recordFailureUsage(spec ModelSpec, alias string, opts ChatOptions) {
	g.ledger.Record(UsageRecord{
		TraceID: opts.TraceID, Alias: alias, ModelKey: spec.Key, Provider: spec.Provider, Model: spec.Model,
		SessionID: opts.SessionID, Caller: opts.Caller, At: g.clk.Now().UTC(),
		Cost: domain.Cost{Currency: g.cost.Currency}, Failed: true,
	})
}

func (g *Gateway) terminal(reason, alias string, attempts []domain.AttemptRecord) error {
	switch reason {
	case "attempts", "deadline":
		return &BudgetExhaustedError{Alias: alias, Reason: reason, Attempts: attempts}
	case "stream_committed":
		return &StreamCommittedError{Alias: alias}
	default:
		return &AllCandidatesFailedError{Alias: alias, Attempts: attempts, Reason: reason}
	}
}

func (g *Gateway) costLookup(modelKey string) (float64, bool) {
	p, ok := g.cost.PriceOf(modelKey)
	if !ok {
		return 0, false
	}
	f, _ := p.Input.Add(p.Output).Float64()
	return f, true
}

func (g *Gateway) ContextWindow(alias string) *int { return g.registry.ContextWindow(alias) }
func (g *Gateway) Ledger() *UsageLedger            { return g.ledger }
func (g *Gateway) Health() *HealthRegistry         { return g.health }
func (g *Gateway) Registry() *Registry             { return g.registry }
func (g *Gateway) Deadlines() time.Duration        { return g.deadline }

func (g *Gateway) Close() error {
	g.mu.Lock()
	g.chats = map[string]provider.ChatModel{}
	g.embeds = map[string]provider.EmbeddingModel{}
	g.mu.Unlock()
	return g.registry.Close()
}

func (g *Gateway) emit(ctx context.Context, name string, payload map[string]any) {
	ev := event.New(name)
	ev.TraceID = ids.Trace(ctx)
	ev.Payload = payload
	if sid, ok := payload["session_id"].(string); ok {
		ev.SessionID = sid
	}
	if alias, ok := payload["alias"].(string); ok {
		ev.Alias = alias
	}
	if model, ok := payload["model"].(string); ok {
		ev.Subject = model
	}
	g.events.Emit(ctx, ev)
}

func hasVision(messages []domain.Message) bool {
	for _, m := range messages {
		if m.HasImage() {
			return true
		}
	}
	return false
}

func keysOf(specs []ModelSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Key
	}
	return out
}
