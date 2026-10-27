package strategy

import (
	"fmt"
	"sort"
	"strings"
)

const BaselineName = "baseline"

type Template struct {
	Name        string
	Risk        RiskLevel
	Bindings    map[ComponentKind]string
	Trim        map[string][]ComponentKind
	Description string
}

// BuildTemplates：模板引用未注册策略时告警，不阻断启动。
func BuildTemplates(cfg map[string]any, registry *Registry) ([]Template, []string) {
	var issues []string
	var out []Template
	for _, name := range sortedKeys(cfg) {
		raw, _ := cfg[name].(map[string]any)
		t := Template{Name: name, Risk: RiskLow, Bindings: map[ComponentKind]string{}, Trim: map[string][]ComponentKind{}}
		if risk, ok := raw["risk"].(string); ok && RiskLevel(risk) == RiskHigh {
			t.Risk = RiskHigh
		}
		if desc, ok := raw["description"].(string); ok {
			t.Description = desc
		}
		if trim, ok := raw["trim"].(map[string]any); ok {
			for asset, comps := range trim {
				for _, c := range toStrings(comps) {
					t.Trim[asset] = append(t.Trim[asset], ComponentKind(c))
				}
			}
		}
		for _, kind := range ComponentKinds {
			if v, ok := raw[string(kind)].(string); ok && v != "" {
				t.Bindings[kind] = v
				if !registry.Has(kind, v) {
					issues = append(issues, fmt.Sprintf("模板 %s 引用了未注册的策略 %s.%s", name, kind, v))
				}
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, issues
}

func FindTemplate(templates []Template, name string) *Template {
	for i := range templates {
		if templates[i].Name == name {
			return &templates[i]
		}
	}
	return nil
}

// Render：baseline 打底，模板覆盖，trim 命中资产时回退到 baseline。
func Render(t Template, baseline *Template, assets []string) (map[ComponentKind]string, []string) {
	bindings := map[ComponentKind]string{}
	if baseline != nil {
		for k, v := range baseline.Bindings {
			bindings[k] = v
		}
	}
	for k, v := range t.Bindings {
		bindings[k] = v
	}
	var notes []string
	for _, asset := range assets {
		for _, kind := range t.Trim[asset] {
			original := t.Bindings[kind]
			if baseline != nil {
				if b, ok := baseline.Bindings[kind]; ok {
					bindings[kind] = b
					notes = append(notes, fmt.Sprintf("输入资产 %s 命中裁剪：%s 回退 %s→%s", asset, kind, original, b))
				}
			}
		}
	}
	return bindings, notes
}

func toStrings(v any) []string {
	switch list := v.(type) {
	case []any:
		out := make([]string, len(list))
		for i, item := range list {
			out[i] = fmt.Sprint(item)
		}
		return out
	case []string:
		return list
	case string:
		return strings.Split(list, ",")
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
