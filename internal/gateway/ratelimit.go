package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/foundation/clock"
)

type LimitDecision struct {
	Allowed bool
	Reason  string
}

// Limiter 是限流契约。分布式部署由 Redis 实现，单机由 LocalLimiter 实现。
type Limiter interface {
	Acquire(ctx context.Context, key string, estimatedTokens int, budget *CallBudget) (LimitDecision, error)
	Release(key string)
	Reconcile(key string, estimated int, actual *int)
}

type LocalLimiter struct {
	mu     sync.Mutex
	policy RateLimitPolicy
	clock  clock.Clock
	states map[string]*limitState
}

type limitState struct {
	requests []time.Time
	tokens   []tokenSample
	inflight int
}

type tokenSample struct {
	at    time.Time
	count int
}

func NewLocalLimiter(policy RateLimitPolicy, clk clock.Clock) *LocalLimiter {
	if clk == nil {
		clk = clock.System{}
	}
	return &LocalLimiter{policy: policy, clock: clk, states: map[string]*limitState{}}
}

func (l *LocalLimiter) state(key string) *limitState {
	s, ok := l.states[key]
	if !ok {
		s = &limitState{}
		l.states[key] = s
	}
	return s
}

func (l *LocalLimiter) Acquire(ctx context.Context, key string, estimated int, budget *CallBudget) (LimitDecision, error) {
	if l.policy.Empty() {
		return LimitDecision{Allowed: true}, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return LimitDecision{Reason: "cancelled"}, err
		}
		l.mu.Lock()
		s := l.state(key)
		now := l.clock.Now()
		cutoff := now.Add(-time.Minute)
		for len(s.requests) > 0 && s.requests[0].Before(cutoff) {
			s.requests = s.requests[1:]
		}
		for len(s.tokens) > 0 && s.tokens[0].at.Before(cutoff) {
			s.tokens = s.tokens[1:]
		}
		reason := ""
		if l.policy.MaxConcurrency > 0 && s.inflight >= l.policy.MaxConcurrency {
			reason = "concurrency"
		}
		if reason == "" && l.policy.RPM > 0 && len(s.requests) >= l.policy.RPM {
			reason = "rpm"
		}
		if reason == "" && l.policy.TPM > 0 {
			used := 0
			for _, t := range s.tokens {
				used += t.count
			}
			if used+estimated > l.policy.TPM {
				reason = "tpm"
			}
		}
		if reason == "" {
			s.inflight++
			s.requests = append(s.requests, now)
			if l.policy.TPM > 0 {
				s.tokens = append(s.tokens, tokenSample{at: now, count: estimated})
			}
			l.mu.Unlock()
			return LimitDecision{Allowed: true}, nil
		}
		onExceed := l.policy.OnExceed
		l.mu.Unlock()
		if onExceed != "wait" {
			return LimitDecision{Allowed: false, Reason: reason}, nil
		}
		// 等到额度或预算耗尽；恰好等到 deadline 也视为无用，改判 fallback。
		// 等不到额度就不要等：预算已归零时继续排队只会拖到外层超时。
		if remaining := budget.RemainingSeconds(); remaining <= 0.05 {
			return LimitDecision{Allowed: false, Reason: reason}, nil
		}
		if err := l.clock.Sleep(ctx, 50*time.Millisecond); err != nil {
			return LimitDecision{Reason: "cancelled"}, err
		}
	}
}

func (l *LocalLimiter) Release(key string) {
	if l.policy.Empty() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state(key)
	if s.inflight > 0 {
		s.inflight--
	}
}

func (l *LocalLimiter) Reconcile(key string, estimated int, actual *int) {
	if actual == nil || l.policy.TPM <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state(key)
	for i := len(s.tokens) - 1; i >= 0; i-- {
		if s.tokens[i].count == estimated {
			s.tokens[i].count = *actual
			return
		}
	}
}
