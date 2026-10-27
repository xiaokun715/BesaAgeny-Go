package gateway

import (
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/foundation/clock"
)

type CircuitState string

const (
	StateClosed   CircuitState = "closed"
	StateOpen     CircuitState = "open"
	StateHalfOpen CircuitState = "half_open"
)

type breaker struct {
	state          CircuitState
	failures       int
	successes      int
	openedAt       time.Time
	probesInFlight int
}

// HealthRegistry 是进程内的熔断表。
//
// 分布式部署下每个 gateway 副本各自维护 —— 这是有意的：
// 共享熔断需要写竞争，而“某个副本认为模型挂了”已经足够保护它自己。
type HealthRegistry struct {
	mu       sync.Mutex
	policy   HealthPolicy
	clock    clock.Clock
	breakers map[string]*breaker
}

func NewHealthRegistry(policy HealthPolicy, clk clock.Clock) *HealthRegistry {
	if clk == nil {
		clk = clock.System{}
	}
	if policy.FailureThreshold <= 0 {
		policy = DefaultHealthPolicy()
	}
	return &HealthRegistry{policy: policy, clock: clk, breakers: map[string]*breaker{}}
}

func (h *HealthRegistry) get(key string) *breaker {
	b, ok := h.breakers[key]
	if !ok {
		b = &breaker{state: StateClosed}
		h.breakers[key] = b
	}
	return b
}

// Allow 在读取时惰性推进 OPEN → HALF_OPEN，并在半开期限流探测并发数。
// 放行后**无论走哪条出路都必须 Release**（记成功、记失败或直接归还）。
func (h *HealthRegistry) Allow(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.get(key)
	now := h.clock.Now()
	if b.state == StateOpen {
		if now.Sub(b.openedAt) < h.policy.Cooldown {
			return false
		}
		b.state = StateHalfOpen
		b.successes = 0
		b.probesInFlight = 0
	}
	if b.state == StateHalfOpen {
		if b.probesInFlight >= max(1, h.policy.HalfOpenProbes) {
			return false
		}
		b.probesInFlight++
	}
	return true
}

func (h *HealthRegistry) Release(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.get(key)
	if b.probesInFlight > 0 {
		b.probesInFlight--
	}
}

func (h *HealthRegistry) RecordSuccess(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.get(key)
	b.failures = 0
	if b.probesInFlight > 0 {
		b.probesInFlight--
	}
	switch b.state {
	case StateHalfOpen:
		b.successes++
		if b.successes >= max(1, h.policy.HalfOpenProbes) {
			b.state = StateClosed
			b.successes = 0
		}
	case StateClosed:
		b.state = StateClosed
	}
}

func (h *HealthRegistry) RecordFailure(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.get(key)
	if b.probesInFlight > 0 {
		b.probesInFlight--
	}
	if b.state == StateHalfOpen {
		// 半开期任一失败即回 OPEN，不算失败率（样本量太小）。
		b.state = StateOpen
		b.openedAt = h.clock.Now()
		b.successes = 0
		return
	}
	b.failures++
	if b.state == StateClosed && b.failures >= h.policy.FailureThreshold {
		b.state = StateOpen
		b.openedAt = h.clock.Now()
	}
}

func (h *HealthRegistry) State(key string) CircuitState {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.get(key)
	if b.state == StateOpen && h.clock.Now().Sub(b.openedAt) >= h.policy.Cooldown {
		return StateHalfOpen
	}
	return b.state
}

// Snapshot 供 doctor / metrics 使用。
func (h *HealthRegistry) Snapshot() map[string]CircuitState {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]CircuitState, len(h.breakers))
	for k, b := range h.breakers {
		out[k] = b.state
	}
	return out
}
