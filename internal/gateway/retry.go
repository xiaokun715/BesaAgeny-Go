package gateway

import (
	"math/rand"
	"time"

	"github.com/besa-ai/besa-agent/internal/provider"
)

// Backoff = base * 2^attempt * (1 ± jitter)。抖动是必需的：
// 多 agent 并发时同步退避会形成脉冲。
func Backoff(attempt int, policy RetryPolicy) time.Duration {
	d := policy.BackoffBase
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	if d <= 0 {
		return 0
	}
	if policy.JitterRatio > 0 {
		factor := 1 + (rand.Float64()*2-1)*policy.JitterRatio
		d = time.Duration(float64(d) * factor)
	}
	return d
}

// ShouldRetry 判“错误可重试”且“全局预算还有余量”。
func ShouldRetry(err error, budget *CallBudget) bool {
	return provider.IsRetryable(err) && budget.ExhaustedReason() == ""
}
