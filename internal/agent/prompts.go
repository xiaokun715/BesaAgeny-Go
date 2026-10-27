package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// StageOrder 是内置七个阶段的固定顺序。
var StageOrder = []string{
	"requirement", "test_design", "test_case", "environment", "execution", "analysis", "defect",
}

type stageTemplate struct {
	description string
	system      string
	keywords    []string
}

var builtinTemplates = map[string]stageTemplate{
	"requirement": {
		description: "需求分析",
		system:      "你是需求分析专家。梳理业务规则、验收标准、前置条件与影响面；指出歧义与遗漏。输出结构化、可被测试设计直接使用。",
		keywords:    []string{"需求", "需求分析", "业务规则", "验收标准", "需求评审", "影响面"},
	},
	"test_design": {
		description: "测试设计",
		system:      "你是测试设计专家。使用等价类、边界值、判定表、状态迁移等方法，输出可执行的测试策略与覆盖矩阵。",
		keywords:    []string{"测试设计", "用例设计", "等价类", "边界值", "判定表", "状态迁移", "覆盖矩阵", "测试策略"},
	},
	"test_case": {
		description: "用例编写",
		system:      "你是测试用例编写专家。输出包含前置条件、步骤、预期结果、优先级与需求追踪关系的用例；格式必须合法。",
		keywords:    []string{"用例", "测试用例", "写用例", "用例编写", "test_case", "case"},
	},
	"environment": {
		description: "环境准备",
		system:      "你是测试环境专家。检查依赖、中间件、测试数据与配置，给出可验证的就绪条件；环境未就绪时不要假装成功。",
		keywords:    []string{"环境", "环境准备", "部署", "依赖", "中间件", "测试数据", "配置", "搭建"},
	},
	"execution": {
		description: "测试执行",
		system:      "你是测试执行专家。按用例执行并记录真实结果；失败时保留原始日志与证据，不要猜测。",
		keywords:    []string{"执行", "跑用例", "运行测试", "测试执行", "执行结果", "pytest", "跑一遍"},
	},
	"analysis": {
		description: "结果分析",
		system:      "你是测试结果分析专家。基于日志与证据定位失败原因，区分产品缺陷、环境问题与用例问题；结论必须可回指证据。",
		keywords:    []string{"分析", "结果分析", "失败原因", "定位", "缺陷分析", "日志分析", "根因"},
	},
	"defect": {
		description: "缺陷管理",
		system:      "你是缺陷管理专家。输出可复现步骤、实际/预期结果、严重度与影响范围；复现不了的要明说。",
		keywords:    []string{"缺陷", "报缺陷", "提单", "bug", "缺陷管理", "复现步骤", "严重度"},
	},
}

func BuiltinStages() []StageAgent {
	out := make([]StageAgent, 0, len(StageOrder))
	for _, name := range StageOrder {
		t := builtinTemplates[name]
		out = append(out, StageAgent{Stage: name, Description: t.description, SystemPrompt: t.system, Keywords: t.keywords})
	}
	return out
}

type StageRegistry struct {
	mu     sync.RWMutex
	agents map[string]StageAgent
	order  []string
}

func NewStageRegistry() *StageRegistry {
	return &StageRegistry{agents: map[string]StageAgent{}}
}

func (r *StageRegistry) Register(a StageAgent) error {
	if strings.TrimSpace(a.Stage) == "" {
		return fmt.Errorf("阶段必须有名字")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.agents[a.Stage]; exists {
		return fmt.Errorf("阶段重名：%s", a.Stage)
	}
	r.agents[a.Stage] = a
	r.order = append(r.order, a.Stage)
	return nil
}

func (r *StageRegistry) Get(stage string) (StageAgent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[stage]
	if !ok {
		return StageAgent{}, fmt.Errorf("未知阶段 %q（可用：%s）", stage, strings.Join(r.Names(), ", "))
	}
	return a, nil
}

func (r *StageRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rank := map[string]int{}
	for i, name := range StageOrder {
		rank[name] = i
	}
	out := append([]string{}, r.order...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := rank[out[i]]
		rj, jok := rank[out[j]]
		if iok && jok {
			return ri < rj
		}
		if iok {
			return true
		}
		if jok {
			return false
		}
		return out[i] < out[j]
	})
	return out
}

func (r *StageRegistry) All() []StageAgent {
	names := r.Names()
	out := make([]StageAgent, 0, len(names))
	for _, name := range names {
		r.mu.RLock()
		out = append(out, r.agents[name])
		r.mu.RUnlock()
	}
	return out
}

// BuildStages：内置七个阶段全开，配置可覆盖；允许配置中出现内置之外的阶段（数据驱动）。
// 阶段声明了未注册工具名只告警，不阻断启动。
func BuildStages(cfg map[string]any, knownTools []string) (*StageRegistry, []string) {
	reg := NewStageRegistry()
	var warnings []string
	known := map[string]bool{}
	for _, t := range knownTools {
		known[t] = true
	}
	seen := map[string]bool{}
	for _, a := range BuiltinStages() {
		raw, _ := cfg[a.Stage].(map[string]any)
		merged, warn := applyStageConfig(a, raw, known)
		warnings = append(warnings, warn...)
		if merged != nil {
			_ = reg.Register(*merged)
		}
		seen[a.Stage] = true
	}
	extra := make([]string, 0)
	for name := range cfg {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		raw, _ := cfg[name].(map[string]any)
		base := StageAgent{Stage: name, Description: name, SystemPrompt: "你是测试流程中的一个阶段，请按任务完成工作并说明依据。"}
		merged, warn := applyStageConfig(base, raw, known)
		warnings = append(warnings, warn...)
		if merged != nil {
			_ = reg.Register(*merged)
		}
	}
	return reg, warnings
}

func applyStageConfig(base StageAgent, raw map[string]any, known map[string]bool) (*StageAgent, []string) {
	var warnings []string
	if raw == nil {
		return &base, nil
	}
	if enabled, ok := raw["enabled"].(bool); ok && !enabled {
		return nil, nil
	}
	if desc, ok := raw["description"].(string); ok && desc != "" {
		base.Description = desc
	}
	if prompt, ok := raw["prompt"].(string); ok && prompt != "" {
		base.SystemPrompt = prompt
	}
	if kws, ok := raw["keywords"].([]any); ok {
		base.Keywords = nil
		for _, kw := range kws {
			base.Keywords = append(base.Keywords, fmt.Sprint(kw))
		}
	}
	if tools, ok := raw["tools"].([]any); ok {
		base.Tools = nil
		for _, t := range tools {
			name := fmt.Sprint(t)
			base.Tools = append(base.Tools, name)
			if !known[name] {
				warnings = append(warnings, fmt.Sprintf("阶段 %s 声明了未注册的工具 %q", base.Stage, name))
			}
		}
	}
	if strings.TrimSpace(base.SystemPrompt) == "" {
		warnings = append(warnings, fmt.Sprintf("阶段 %s 有启用但没有提示词", base.Stage))
		return nil, warnings
	}
	return &base, warnings
}
