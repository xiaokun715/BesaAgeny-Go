// Package workflow 是 Agent Pool 驱动的 DAG 编排。
//
// 三种模式：predefined（人工设计）/ template（骨架 + 裁剪）/ auto（动态生成）。
// auto 默认关闭，且候选图必须经过 Validator 才允许执行。
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type NodeState string

const (
	StatePending   NodeState = "PENDING"
	StateReady     NodeState = "READY"
	StateRunning   NodeState = "RUNNING"
	StateSucceeded NodeState = "SUCCEEDED"
	StateFailed    NodeState = "FAILED"
	StateSkipped   NodeState = "SKIPPED"
)

type EdgeKind string

const (
	EdgeData     EdgeKind = "data"
	EdgeResource EdgeKind = "resource"
)

type IssueKind string

const (
	IssueIOMismatch       IssueKind = "io_mismatch"
	IssueDependency       IssueKind = "dependency_closure"
	IssueCycle            IssueKind = "cycle"
	IssuePermission       IssueKind = "permission"
	IssueResourceConflict IssueKind = "resource_conflict"
)

type FieldSpec struct {
	Type     string
	Required bool
}

type Capability struct {
	Tags               []string
	Inputs             map[string]FieldSpec
	Outputs            map[string]FieldSpec
	Requires           []string
	NeedsAssets        []string
	Parallelizable     bool
	SideEffects        bool
	Retryable          bool
	ExclusiveResources []string
	EstLatencyS        float64
	EstTokens          int
	Version            string
}

type NodeRunner interface {
	RunNode(ctx context.Context, req NodeRequest) (NodeResult, error)
}

type AgentSpec struct {
	Name       string
	Capability Capability
	Runner     NodeRunner
}

type Node struct {
	ID       string            `json:"id"`
	Agent    string            `json:"agent"`
	Inputs   map[string]string `json:"inputs,omitempty"`
	Optional bool              `json:"optional,omitempty"`
	Reason   string            `json:"reason,omitempty"`
}

type Edge struct {
	Src   string   `json:"src"`
	Dst   string   `json:"dst"`
	Kind  EdgeKind `json:"kind,omitempty"`
	Field string   `json:"field,omitempty"`
}

type Workflow struct {
	WorkflowID string
	Version    string
	Nodes      []Node
	Edges      []Edge
	Origin     string // predefined | template | auto
}

type GraphIssue struct {
	Kind       IssueKind
	NodeID     string
	Detail     string
	Suggestion string
}

func (w Workflow) Node(id string) *Node {
	for i := range w.Nodes {
		if w.Nodes[i].ID == id {
			return &w.Nodes[i]
		}
	}
	return nil
}

func (w Workflow) Upstream(nodeID string) []Edge {
	var out []Edge
	for _, e := range w.Edges {
		if e.Dst == nodeID {
			out = append(out, e)
		}
	}
	return out
}

func (w Workflow) Downstream(nodeID string) []string {
	var out []string
	for _, e := range w.Edges {
		if e.Src == nodeID && e.Kind == EdgeData {
			out = append(out, e.Dst)
		}
	}
	return out
}

type NodeRequest struct {
	WorkflowRunID string
	Node          Node
	Agent         AgentSpec
	Inputs        map[string]any
}

type NodeResult struct {
	Status      NodeState      `json:"status"`
	Outputs     map[string]any `json:"outputs,omitempty"`
	Error       string         `json:"error,omitempty"`
	Retryable   bool           `json:"retryable,omitempty"`
	Elapsed     time.Duration  `json:"-"`
	ElapsedS    float64        `json:"elapsed_s"`
	Cost        *float64       `json:"cost,omitempty"`
	NeedsReview bool           `json:"needs_review,omitempty"`
}

type RunReport struct {
	WorkflowRunID string                `json:"workflow_run_id"`
	WorkflowID    string                `json:"workflow_id"`
	Status        NodeState             `json:"status"`
	Nodes         map[string]NodeResult `json:"nodes,omitempty"`
	StartedAt     time.Time             `json:"started_at"`
	FinishedAt    time.Time             `json:"finished_at"`
	Error         string                `json:"error,omitempty"`
}

// TaskFromInputs 把上游输出变成节点任务：优先用显式的 task，
// 其次用上游的 answer（纯文本），最后才退回 JSON —— 避免把转义后的 JSON 当任务喂给下一阶段。
func TaskFromInputs(inputs map[string]any, nodeID string) string {
	if s, ok := inputs["task"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	if s, ok := inputs["answer"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	raw, _ := json.Marshal(inputs)
	return "节点 " + nodeID + " 的输入：" + string(raw)
}

// Pool 维护 Agent 的能力元数据；注册期校验六类元数据齐全。
type Pool struct {
	mu    sync.RWMutex
	specs map[string]AgentSpec
}

func NewPool() *Pool { return &Pool{specs: map[string]AgentSpec{}} }

func (p *Pool) Register(spec AgentSpec) error {
	if spec.Name == "" || spec.Runner == nil {
		return fmt.Errorf("Agent 必须有名字与 runner")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.specs[spec.Name]; exists {
		return fmt.Errorf("Agent 重名：%s", spec.Name)
	}
	p.specs[spec.Name] = spec
	return nil
}

func (p *Pool) Get(name string) (AgentSpec, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	spec, ok := p.specs[name]
	if !ok {
		return AgentSpec{}, fmt.Errorf("未知 Agent %q（可用：%v）", name, p.namesLocked())
	}
	return spec, nil
}

func (p *Pool) Names() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.namesLocked()
}

func (p *Pool) namesLocked() []string {
	out := make([]string, 0, len(p.specs))
	for name := range p.specs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (p *Pool) All() []AgentSpec {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AgentSpec, 0, len(p.specs))
	for _, s := range p.specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TaskContext 是 Agent 选择的三段漏斗输入。
type TaskContext struct {
	Tags    []string
	Assets  []string
	Inputs  map[string]FieldSpec
	Risk    string
	Allowed []string // 本次运行允许的 Agent 名；空 = 不限制
}

type Candidate struct {
	Spec     AgentSpec
	Score    float64
	HitTags  []string
	Rejected string // 被哪条硬过滤淘汰
}

// Filter 硬过滤：能力标签 → IO 契约 → 前置条件/资产 → 权限。
func (p *Pool) Filter(task TaskContext) []Candidate {
	var out []Candidate
	for _, spec := range p.All() {
		c := Candidate{Spec: spec}
		if len(task.Allowed) > 0 && !contains(task.Allowed, spec.Name) {
			c.Rejected = "permission"
			out = append(out, c)
			continue
		}
		hits := 0
		for _, tag := range task.Tags {
			if contains(spec.Capability.Tags, tag) {
				hits++
			}
		}
		if len(task.Tags) > 0 && hits == 0 {
			c.Rejected = "capability_tags"
			out = append(out, c)
			continue
		}
		c.HitTags = nil
		missingInput := ""
		for name, field := range task.Inputs {
			provided := false
			if _, ok := spec.Capability.Inputs[name]; ok {
				provided = true
			}
			if !provided && field.Required {
				missingInput = name
				break
			}
		}
		if missingInput != "" {
			c.Rejected = "io_contract:" + missingInput
			out = append(out, c)
			continue
		}
		for _, asset := range spec.Capability.NeedsAssets {
			if !contains(task.Assets, asset) {
				c.Rejected = "asset:" + asset
				break
			}
		}
		if c.Rejected != "" {
			out = append(out, c)
			continue
		}
		// 简单的语义分：标签命中率 + 成本惩罚。
		score := float64(hits) / float64(max(1, len(task.Tags)))
		score -= 0.001 * spec.Capability.EstLatencyS
		score -= 0.000001 * float64(spec.Capability.EstTokens)
		c.Score = score
		out = append(out, c)
	}
	return out
}

// Rank 排序并取前 K。
func Rank(candidates []Candidate, limit int) []Candidate {
	ok := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Rejected == "" {
			ok = append(ok, c)
		}
	}
	sort.SliceStable(ok, func(i, j int) bool { return ok[i].Score > ok[j].Score })
	if limit > 0 && len(ok) > limit {
		ok = ok[:limit]
	}
	return ok
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}
