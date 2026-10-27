package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/provider"
	"github.com/shopspring/decimal"
)

type fakeChat struct {
	fn func(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error)
}

func (f fakeChat) Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	return f.fn(ctx, req)
}
func (f fakeChat) StreamChat(context.Context, domain.ChatRequest) (provider.Stream, error) {
	return nil, errors.New("not implemented")
}

type fakeProvider struct {
	caps domain.CapabilitySet
	chat provider.ChatModel
}

func (p fakeProvider) Name() string                       { return "fake" }
func (p fakeProvider) Capabilities() domain.CapabilitySet { return p.caps }
func (p fakeProvider) ChatModel() (provider.ChatModel, error) {
	return p.chat, nil
}
func (p fakeProvider) EmbeddingModel() (provider.EmbeddingModel, error) {
	return nil, errors.New("no embedding")
}
func (p fakeProvider) Close() error { return nil }

func testGateway(t *testing.T, chats map[string]func(context.Context, domain.ChatRequest) (domain.ChatResponse, error), order []string) *Gateway {
	t.Helper()
	caps := domain.CapabilitySet{domain.CapRuntime: true}
	models := map[string]ModelSpec{}
	for _, key := range order {
		models[key] = ModelSpec{Key: key, Provider: "fake", Model: key, Capabilities: caps,
			Config: provider.ModelConfig{Key: key, Timeout: time.Second}}
	}
	aliases := map[string]AliasSpec{"runtime.default": {Alias: "runtime.default", Candidates: order, Strategy: []string{"capability", "priority"}}}
	reg := NewRegistry(models, aliases, func(adapter string, cfg provider.ModelConfig) provider.Provider {
		return fakeProvider{caps: caps, chat: fakeChat{fn: chats[cfg.Key]}}
	})
	return New(reg, Options{
		Retry:  RetryPolicy{MaxAttemptsPerCandidate: 1, TotalMaxAttempts: 2, BackoffBase: 0},
		Ledger: NewUsageLedger(100),
		Cost:   &CostSheet{Currency: "CNY", Prices: map[string]Price{}},
	})
}

func TestFallbackMarksDegraded(t *testing.T) {
	g := testGateway(t, map[string]func(context.Context, domain.ChatRequest) (domain.ChatResponse, error){
		"m1": func(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{}, &provider.Error{Kind: "http", StatusCode: 503, Retryable: false, Message: "down"}
		},
		"m2": func(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{Content: "备选答", Usage: domain.Usage{InputTokens: domain.IntPtr(10), OutputTokens: domain.IntPtr(2)}}, nil
		},
	}, []string{"m1", "m2"})

	result, err := g.Chat(context.Background(), "runtime.default", []domain.Message{domain.User("hi")}, ChatOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content() != "备选答" || result.ModelKey != "m2" || !result.Degraded {
		t.Fatalf("expected degraded fallback to m2, got %+v", result)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].Outcome != "failed" || result.Attempts[1].Outcome != "success" {
		t.Fatalf("attempts not recorded correctly: %+v", result.Attempts)
	}
	// 没配价格 → 成本未知，不是 0
	if result.Cost.Amount != nil {
		t.Fatalf("cost should be unknown, got %v", result.Cost.Amount)
	}
}

func TestUnknownAliasListsAvailable(t *testing.T) {
	g := testGateway(t, map[string]func(context.Context, domain.ChatRequest) (domain.ChatResponse, error){}, []string{"m1"})
	_, err := g.Chat(context.Background(), "runtime.defualt", nil, ChatOptions{})
	var unknown *UnknownAliasError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected UnknownAliasError, got %v", err)
	}
	if len(unknown.Available) != 1 || unknown.Available[0] != "runtime.default" {
		t.Fatalf("available aliases not listed: %+v", unknown.Available)
	}
}

func TestCircuitBreakerSkipsDeadModel(t *testing.T) {
	m1Calls := 0
	g := testGateway(t, map[string]func(context.Context, domain.ChatRequest) (domain.ChatResponse, error){
		"m1": func(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
			m1Calls++
			return domain.ChatResponse{}, &provider.Error{Kind: "http", StatusCode: 500, Retryable: false, Message: "down"}
		},
		"m2": func(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{Content: "ok"}, nil
		},
	}, []string{"m1", "m2"})
	g.health = NewHealthRegistry(HealthPolicy{FailureThreshold: 1, Cooldown: time.Hour, HalfOpenProbes: 1}, nil)

	_, _ = g.Chat(context.Background(), "runtime.default", []domain.Message{domain.User("hi")}, ChatOptions{})
	_, err := g.Chat(context.Background(), "runtime.default", []domain.Message{domain.User("hi")}, ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m1Calls != 1 {
		t.Fatalf("熔断打开后不应再访问上游，实际调用 %d 次", m1Calls)
	}
}

func TestRetryOnlyRetryable(t *testing.T) {
	calls := 0
	g := testGateway(t, map[string]func(context.Context, domain.ChatRequest) (domain.ChatResponse, error){
		"m1": func(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
			calls++
			if calls == 1 {
				return domain.ChatResponse{}, &provider.Error{Kind: "http", StatusCode: 503, Retryable: true, Message: "temporary"}
			}
			return domain.ChatResponse{Content: "恢复"}, nil
		},
	}, []string{"m1"})
	g.retry = RetryPolicy{MaxAttemptsPerCandidate: 2, TotalMaxAttempts: 4, BackoffBase: 0}

	result, err := g.Chat(context.Background(), "runtime.default", []domain.Message{domain.User("hi")}, ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content() != "恢复" || calls != 2 {
		t.Fatalf("expected one retry, calls=%d result=%+v", calls, result)
	}
}

func TestCostCalculation(t *testing.T) {
	g := testGateway(t, map[string]func(context.Context, domain.ChatRequest) (domain.ChatResponse, error){
		"m1": func(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{Content: "x", Usage: domain.Usage{InputTokens: domain.IntPtr(1000), OutputTokens: domain.IntPtr(1000)}}, nil
		},
	}, []string{"m1"})
	g.cost = &CostSheet{Currency: "CNY", Prices: map[string]Price{
		"m1": {Input: mustDecimal("0.001"), Output: mustDecimal("0.002")},
	}}
	result, err := g.Chat(context.Background(), "runtime.default", []domain.Message{domain.User("hi")}, ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cost.Amount == nil || result.Cost.Amount.String() != "0.003" {
		t.Fatalf("expected 0.003 CNY, got %v", result.Cost.Amount)
	}
}

func mustDecimal(s string) decimal.Decimal { return decimal.RequireFromString(s) }
