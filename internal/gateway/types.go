// Package gateway 是模型调用的唯一入口：选谁、挂了怎么办、花多少、还活着吗。
//
// 编排是一个方法（Gateway.Chat / StreamChat），不是九层装饰器。
// 顺序是约束：health → rate_limit → retry（在 fallback 之内）。
package gateway

import (
	"time"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/provider"
)

type ModelSpec struct {
	Key               string
	Provider          string // vendor（日志与报表用）
	Model             string
	Capabilities      domain.CapabilitySet
	Priority          int
	Weight            float64
	ContextWindow     *int
	Config            provider.ModelConfig
	UnavailableReason string
}

func (m ModelSpec) Available() bool                   { return m.UnavailableReason == "" }
func (m ModelSpec) Supports(c domain.Capability) bool { return m.Capabilities[c] }

type AliasSpec struct {
	Alias      string
	Candidates []string
	Strategy   []string
}

type RoutingContext struct {
	Required  []domain.Capability
	SessionID string
	Caller    string
	CostOf    func(modelKey string) (float64, bool)
	Health    *HealthRegistry
	Describes string
}

func RoutingForChat(stream, tools, vision, jsonOutput bool) []domain.Capability {
	req := []domain.Capability{domain.CapRuntime}
	if stream {
		req = append(req, domain.CapStream)
	}
	if tools {
		req = append(req, domain.CapTools)
	}
	if vision {
		req = append(req, domain.CapVision)
	}
	if jsonOutput {
		req = append(req, domain.CapJSON)
	}
	return req
}

// RetryPolicy：单候选上限与跨候选累计上限**必须同时判**。
type RetryPolicy struct {
	MaxAttemptsPerCandidate int
	TotalMaxAttempts        int
	BackoffBase             time.Duration
	JitterRatio             float64
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttemptsPerCandidate: 2, TotalMaxAttempts: 4, BackoffBase: time.Second, JitterRatio: 0.3}
}

func (p RetryPolicy) Validate() error {
	if p.MaxAttemptsPerCandidate < 1 {
		return errInvalid("gateway.retry.max_attempts_per_candidate 必须 ≥ 1")
	}
	if p.TotalMaxAttempts < p.MaxAttemptsPerCandidate+1 {
		return errInvalid("gateway.retry.total_max_attempts 必须 ≥ max_attempts_per_candidate+1")
	}
	return nil
}

// CallBudget 是**唯一的上游调用计数点**。
// 任何地方都不得另起一个 attempt 计数器 —— 否则上界不再可知。
type CallBudget struct {
	MaxAttempts int
	Deadline    time.Time // 绝对时刻
	Used        int
}

func NewCallBudget(maxAttempts int, deadline time.Time) *CallBudget {
	return &CallBudget{MaxAttempts: maxAttempts, Deadline: deadline}
}

func (b *CallBudget) TryAcquire() bool {
	if b.Deadline.IsZero() || time.Now().Before(b.Deadline) {
		if b.Used < b.MaxAttempts {
			b.Used++
			return true
		}
	}
	return false
}

func (b *CallBudget) RemainingAttempts() int { return max(0, b.MaxAttempts-b.Used) }

func (b *CallBudget) RemainingSeconds() float64 {
	if b.Deadline.IsZero() {
		return 0
	}
	return time.Until(b.Deadline).Seconds()
}

func (b *CallBudget) ExhaustedReason() string {
	if !b.Deadline.IsZero() && !time.Now().Before(b.Deadline) {
		return "deadline"
	}
	if b.Used >= b.MaxAttempts {
		return "attempts"
	}
	return ""
}

type FallbackPolicy struct {
	Enabled         bool
	OnStreamStarted bool
}

func DefaultFallbackPolicy() FallbackPolicy {
	return FallbackPolicy{Enabled: true, OnStreamStarted: false}
}

type FallbackDecision struct {
	Proceed bool
	Reason  string
}

// Decide 的判定顺序是约束：stream_committed → disabled → no_more_candidates → budget。
// 第 3 条必须早于第 4 条，否则“全部候选失败”会被误报成“预算耗尽”。
func DecideFallback(policy FallbackPolicy, budget *CallBudget, remainingCandidates int, streamCommitted bool) FallbackDecision {
	if streamCommitted && !policy.OnStreamStarted {
		return FallbackDecision{Reason: "stream_committed"}
	}
	if !policy.Enabled {
		return FallbackDecision{Reason: "fallback_disabled"}
	}
	if remainingCandidates <= 0 {
		return FallbackDecision{Reason: "no_more_candidates"}
	}
	if reason := budget.ExhaustedReason(); reason != "" {
		return FallbackDecision{Reason: reason}
	}
	return FallbackDecision{Proceed: true, Reason: "proceed"}
}

type HealthPolicy struct {
	FailureThreshold int
	Cooldown         time.Duration
	HalfOpenProbes   int
}

func DefaultHealthPolicy() HealthPolicy {
	return HealthPolicy{FailureThreshold: 5, Cooldown: 30 * time.Second, HalfOpenProbes: 2}
}

type RateLimitPolicy struct {
	RPM            int
	TPM            int
	MaxConcurrency int
	OnExceed       string // wait | fallback
}

func (p RateLimitPolicy) Empty() bool {
	return p.RPM <= 0 && p.TPM <= 0 && p.MaxConcurrency <= 0
}
