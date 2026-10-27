package tool

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/besa-ai/besa-agent/internal/foundation/clock"
)

// PermissionPolicy 的默认值是**拒绝**：等级未启用、或该等级没有范围，都不许访问。
type PermissionPolicy struct {
	Enabled map[SideEffect]bool
	Scopes  map[SideEffect][]string
}

func NewPermissionPolicy(enabled []string, scopes map[string][]string) *PermissionPolicy {
	p := &PermissionPolicy{Enabled: map[SideEffect]bool{}, Scopes: map[SideEffect][]string{}}
	for _, e := range enabled {
		p.Enabled[SideEffect(e)] = true
	}
	for level, paths := range scopes {
		resolved := make([]string, 0, len(paths))
		for _, path := range paths {
			resolved = append(resolved, ResolvePath(path))
		}
		p.Scopes[SideEffect(level)] = resolved
	}
	// read 是只读；只配了 write 却不配 read 是常见疏忽，这里不自动推导 ——
	// 推导会让“忘了配”看起来像“配过了”。
	return p
}

type PermissionDecision struct {
	Allowed bool
	Reason  string
}

func (p *PermissionPolicy) Authorize(def Definition) PermissionDecision {
	effect := SideEffect(def.SideEffect)
	if effect == "" {
		effect = SideEffectRead
	}
	if !p.Enabled[effect] {
		levels := make([]string, 0, len(p.Enabled))
		for level, on := range p.Enabled {
			if on {
				levels = append(levels, string(level))
			}
		}
		sort.Strings(levels)
		return PermissionDecision{Reason: fmt.Sprintf(
			"工具 %s 的副作用等级是 %q，未授权（已授权：%s）。要开启请改 tool.permission.levels。",
			def.Name, effect, strings.Join(levels, ", "))}
	}
	if len(p.Scopes[effect]) == 0 {
		return PermissionDecision{Reason: fmt.Sprintf(
			"副作用等级 %q 没有配置可访问范围 —— **默认拒绝**（没配就是不许访问）。请配置 tool.permission.scope.%s。",
			effect, effect)}
	}
	return PermissionDecision{Allowed: true}
}

// ContextFor 构造工具执行上下文。范围取该等级的已解析路径。
func (p *PermissionPolicy) ContextFor(def Definition, inv Invocation, limits Limits) *Context {
	effect := SideEffect(def.SideEffect)
	if effect == "" {
		effect = SideEffectRead
	}
	return &Context{
		Scope:          inv.Scope,
		AllowedPaths:   append([]string{}, p.Scopes[effect]...),
		TraceID:        inv.TraceID,
		SessionID:      inv.SessionID,
		Caller:         inv.Caller,
		MaxOutputBytes: limits.MaxOutputBytes,
		MaxOutputLines: limits.MaxOutputLines,
		Timeout:        limits.Timeout,
		Clock:          clock.System{},
	}
}

type Limits struct {
	MaxOutputBytes int
	MaxOutputLines int
	Timeout        time.Duration
	MaxCallsPerRun int
	MaxCallDepth   int
	SpillThreshold int
	SpillQuotaMB   int
	SpillDir       string
	SpillTTL       time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxOutputBytes: 262144, MaxOutputLines: 2000, Timeout: 60 * time.Second,
		MaxCallsPerRun: 200, MaxCallDepth: 8,
		SpillThreshold: 65536, SpillQuotaMB: 512, SpillTTL: 24 * time.Hour,
	}
}
