package strategy

import (
	"fmt"
	"sort"
	"sync"
)

type Registry struct {
	mu    sync.RWMutex
	specs map[string]StrategySpec
}

func NewRegistry() *Registry { return &Registry{specs: map[string]StrategySpec{}} }

func (r *Registry) Register(spec StrategySpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := spec.Key()
	if _, exists := r.specs[key]; exists {
		return fmt.Errorf("策略重名：%s（新增前先确认它不是同一策略的另一个名字）", key)
	}
	r.specs[key] = spec
	return nil
}

func (r *Registry) MarkUnavailable(kind ComponentKind, name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := string(kind) + "." + name
	spec, ok := r.specs[key]
	if !ok {
		return
	}
	spec.Available = false
	spec.UnavailableReason = reason
	r.specs[key] = spec
}

func (r *Registry) Get(kind ComponentKind, name string) (StrategySpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.specs[string(kind)+"."+name]
	if !ok {
		return StrategySpec{}, fmt.Errorf("未知策略 %s.%s（该类别可用：%v）", kind, name, r.namesLocked(kind))
	}
	return spec, nil
}

func (r *Registry) Has(kind ComponentKind, name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.specs[string(kind)+"."+name]
	return ok
}

func (r *Registry) ByKind(kind ComponentKind) []StrategySpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []StrategySpec
	for _, s := range r.specs {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Available(kind ComponentKind) []StrategySpec {
	var out []StrategySpec
	for _, s := range r.ByKind(kind) {
		if s.Available {
			out = append(out, s)
		}
	}
	return out
}

func (r *Registry) All() []StrategySpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]StrategySpec, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

func (r *Registry) namesLocked(kind ComponentKind) []string {
	var out []string
	for _, s := range r.specs {
		if s.Kind == kind {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}

// BuildProfile 从注册表补齐版本。未知策略硬失败。
func BuildProfile(name string, bindings map[ComponentKind]string, registry *Registry, layer Layer, reason string, risk RiskLevel, params map[string]any) (AgentProfile, error) {
	versions := map[string]string{}
	for kind, strategyName := range bindings {
		spec, err := registry.Get(kind, strategyName)
		if err != nil {
			return AgentProfile{}, err
		}
		if !spec.Available {
			return AgentProfile{}, fmt.Errorf("策略 %s 不可用：%s", spec.Key(), spec.UnavailableReason)
		}
		versions[spec.Key()] = spec.Version
	}
	if params == nil {
		params = map[string]any{}
	}
	return AgentProfile{Name: name, Bindings: bindings, Versions: versions, Params: params, Layer: layer, Reason: reason, Risk: risk}, nil
}
