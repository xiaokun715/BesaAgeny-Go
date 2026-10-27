package tool

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Name()
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("工具必须有名字")
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("工具重名：%s（新增工具前先确认它真的不是另一个工具的别名）", name)
	}
	r.tools[name] = t
	return nil
}

func (r *Registry) Get(name string) (Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("未知工具 %q（可用：%s）", name, strings.Join(r.Names(), ", "))
	}
	return t, nil
}

func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Select 按类别与显式名单筛选。
// categories 为空表示不筛；only 非空时必须同时在 only 里。
func (r *Registry) Select(categories, only []string) []Definition {
	catSet := map[string]bool{}
	for _, c := range categories {
		catSet[c] = true
	}
	onlySet := map[string]bool{}
	for _, o := range only {
		onlySet[o] = true
	}
	var out []Definition
	for _, t := range r.All() {
		def := t.Definition()
		if len(catSet) > 0 && !catSet[def.Category] {
			continue
		}
		if len(onlySet) > 0 && !onlySet[def.Name] {
			continue
		}
		out = append(out, def)
	}
	return out
}
