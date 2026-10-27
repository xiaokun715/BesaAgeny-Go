package workflow

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type PlannerConfig struct {
	MaxNodes            int
	DataDependency      string // schema | name | off
	OnAmbiguousProvider string // reject | priority
}

func DefaultPlannerConfig() PlannerConfig {
	return PlannerConfig{MaxNodes: 20, DataDependency: "schema", OnAmbiguousProvider: "reject"}
}

type PlanResult struct {
	Workflow Workflow
	Issues   []GraphIssue
	Notes    []string
}

// PlanAuto 依任务与 Agent 能力生成候选 DAG。**它无执行权**，只产候选。
func PlanAuto(task TaskContext, pool *Pool, cfg PlannerConfig) (PlanResult, error) {
	candidates := Rank(pool.Filter(task), cfg.MaxNodes)
	if len(candidates) == 0 {
		return PlanResult{}, fmt.Errorf("没有任何 Agent 满足任务能力标签 %v", task.Tags)
	}
	w := Workflow{WorkflowID: "auto", Origin: "auto"}
	producedBy := map[string]string{} // 字段 -> 节点 id
	for i, c := range candidates {
		nodeID := fmt.Sprintf("n%d", i+1)
		node := Node{ID: nodeID, Agent: c.Spec.Name, Inputs: map[string]string{},
			Reason: fmt.Sprintf("命中标签 %v，成本分 %.3f", c.HitTags, c.Score)}
		for field, fs := range c.Spec.Capability.Inputs {
			if !fs.Required {
				continue
			}
			if src, ok := producedBy[field]; ok {
				node.Inputs[field] = src
				w.Edges = append(w.Edges, Edge{Src: src, Dst: nodeID, Kind: EdgeData, Field: field})
				continue
			}
			if _, ok := task.Inputs[field]; ok {
				node.Inputs[field] = field
				continue
			}
			// 必需输入凑不齐 → 这个 Agent 不该进候选图。
			node.Reason = ""
		}
		if node.Reason == "" {
			continue
		}
		for field := range c.Spec.Capability.Outputs {
			if prev, exists := producedBy[field]; exists && cfg.OnAmbiguousProvider == "reject" {
				return PlanResult{}, fmt.Errorf("字段 %q 有多个提供者（%s / %s）", field, prev, nodeID)
			}
			producedBy[field] = nodeID
		}
		w.Nodes = append(w.Nodes, node)
	}
	w.WorkflowID = "auto-" + taskKey(task)
	result := PlanResult{Workflow: w}
	result.Issues = Validate(w, ValidationContext{Pool: pool, TaskInputs: task.Inputs, Assets: task.Assets})
	return result, nil
}

// PlanTemplate 使用预定义骨架，并记录裁剪依据。
func PlanTemplate(template Workflow, task TaskContext) PlanResult {
	w := template
	w.Origin = "template"
	result := PlanResult{Workflow: w}
	if len(task.Assets) > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf("输入资产 %v 可用于裁剪（预定义模板不自动删节点，避免丢证据）", task.Assets))
	}
	return result
}

// LoadTemplate 从 YAML 读取预定义/模板流程。
type yamlWorkflow struct {
	ID      string `yaml:"id"`
	Version string `yaml:"version"`
	Origin  string `yaml:"origin"`
	Nodes   []struct {
		ID       string            `yaml:"id"`
		Agent    string            `yaml:"agent"`
		Inputs   map[string]string `yaml:"inputs"`
		Optional bool              `yaml:"optional"`
		Reason   string            `yaml:"reason"`
	} `yaml:"nodes"`
	Edges []struct {
		Src   string `yaml:"src"`
		Dst   string `yaml:"dst"`
		Kind  string `yaml:"kind"`
		Field string `yaml:"field"`
	} `yaml:"edges"`
}

func LoadTemplate(path string) (Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Workflow{}, err
	}
	var y yamlWorkflow
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return Workflow{}, err
	}
	w := Workflow{WorkflowID: y.ID, Version: y.Version, Origin: y.Origin}
	if w.Origin == "" {
		w.Origin = "predefined"
	}
	for _, n := range y.Nodes {
		inputs := n.Inputs
		if inputs == nil {
			inputs = map[string]string{}
		}
		w.Nodes = append(w.Nodes, Node{ID: n.ID, Agent: n.Agent, Inputs: inputs, Optional: n.Optional, Reason: n.Reason})
	}
	for _, e := range y.Edges {
		kind := EdgeData
		if EdgeKind(e.Kind) == EdgeResource {
			kind = EdgeResource
		}
		w.Edges = append(w.Edges, Edge{Src: e.Src, Dst: e.Dst, Kind: kind, Field: e.Field})
	}
	if w.WorkflowID == "" {
		w.WorkflowID = strings.TrimSuffix(path, ".yaml")
	}
	return w, nil
}

func taskKey(task TaskContext) string {
	tags := append([]string{}, task.Tags...)
	sort.Strings(tags)
	return strings.Join(tags, "-")
}
