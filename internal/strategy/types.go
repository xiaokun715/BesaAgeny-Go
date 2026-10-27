// Package strategy 是“这次任务该用哪套做法”的选择层。
//
// 它**不实现任何策略**：只做登记 / 校验 / 选择 / 装配。
// 策略的实现归各自的家（context / memory / tool / agent）。
package strategy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type ComponentKind string

const (
	CompContext       ComponentKind = "context"
	CompMemory        ComponentKind = "memory"
	CompRetrieval     ComponentKind = "retrieval"
	CompToolSelection ComponentKind = "tool_selection"
	CompPermission    ComponentKind = "permission"
	CompValidation    ComponentKind = "validation"
	CompExecution     ComponentKind = "execution"
)

var ComponentKinds = []ComponentKind{
	CompContext, CompMemory, CompRetrieval, CompToolSelection,
	CompPermission, CompValidation, CompExecution,
}

type RiskLevel string

const (
	RiskLow  RiskLevel = "low"
	RiskHigh RiskLevel = "high"
)

type Layer string

const (
	LayerTemplate  Layer = "template"
	LayerRules     Layer = "rules"
	LayerEvaluated Layer = "evaluated"
	LayerDefault   Layer = "default"
)

type CostEstimate struct {
	Tokens        int
	LatencyMS     int
	ExternalCalls int
}

// StrategySpec 的六要素缺一不可（接口、版本、前置条件、成本、场景、指标）。
type StrategySpec struct {
	Kind              ComponentKind
	Name              string
	Version           string
	Interface         string
	Inputs            []string
	Provides          []string
	Preconditions     []string
	Cost              CostEstimate
	Scenarios         []string
	Metrics           map[string]string
	Available         bool
	UnavailableReason string
}

func (s StrategySpec) Key() string { return string(s.Kind) + "." + s.Name }

func (s StrategySpec) Validate() error {
	if s.Kind == "" || s.Name == "" || s.Version == "" || s.Interface == "" {
		return fmt.Errorf("策略 %s.%s 缺少必需元数据（version/interface）", s.Kind, s.Name)
	}
	if s.Preconditions == nil || s.Scenarios == nil || s.Metrics == nil {
		return fmt.Errorf("策略 %s 缺少前置条件/场景/指标", s.Key())
	}
	if !s.Available && s.UnavailableReason == "" {
		return fmt.Errorf("策略 %s 标记不可用但没有给出原因", s.Key())
	}
	return nil
}

type TaskFeatures struct {
	TaskType        string    `json:"task_type"`
	Risk            RiskLevel `json:"risk"`
	BudgetTokens    *int      `json:"budget_tokens,omitempty"`
	BudgetLatencyMS *int      `json:"budget_latency_ms,omitempty"`
	BudgetCost      *float64  `json:"budget_cost,omitempty"`
	Environment     string    `json:"environment"`
	InputAssets     []string  `json:"input_assets"`
	Template        string    `json:"template"`
}

func (f TaskFeatures) TemplateKey() string {
	if f.Template != "" {
		return f.Template
	}
	return f.TaskType
}

func (f TaskFeatures) Fingerprint() string {
	raw := fmt.Sprintf("%s|%s|%s|%v|%v|%s", f.TaskType, f.Risk, f.Template, f.InputAssets, f.BudgetTokens, f.Environment)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

type AgentProfile struct {
	Name     string
	Bindings map[ComponentKind]string
	Versions map[string]string // "kind.name" -> version
	Params   map[string]any
	Layer    Layer
	Reason   string
	Risk     RiskLevel
}

func (p AgentProfile) StrategyOf(kind ComponentKind) string { return p.Bindings[kind] }
func (p AgentProfile) VersionOf(kind ComponentKind) string {
	return p.Versions[string(kind)+"."+p.Bindings[kind]]
}

func (p AgentProfile) ProfileKey() string {
	keys := make([]string, 0, len(p.Bindings))
	for k, v := range p.Bindings {
		keys = append(keys, string(k)+"="+v)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}

type Violation struct {
	Rule       string
	Components []ComponentKind
	Detail     string
	Suggestion string
}

type Candidate struct {
	Profile AgentProfile
	Score   float64
	Quality float64
	Cost    float64
	Latency float64
	Note    string
}

type SelectionDecision struct {
	Profile    *AgentProfile
	Layer      Layer
	Reason     string
	Candidates []Candidate
	Violations []Violation
	Degraded   bool
	Refused    bool
	Features   *TaskFeatures
}

func (d SelectionDecision) OK() bool { return d.Profile != nil && !d.Refused }
