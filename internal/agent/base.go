package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// StageAgent 是数据驱动的阶段定义：阶段之间的差别全在提示词与工具类别里。
type StageAgent struct {
	Stage        string
	Description  string
	SystemPrompt string
	Tools        []string
	Keywords     []string
}

func (a StageAgent) ToolCategories() []string {
	return []string{a.Stage, "infra"}
}

func (a StageAgent) BuildPrompt(task string, resumed *TaskRun) string {
	if resumed == nil || len(resumed.Steps) == 0 {
		return task
	}
	var b strings.Builder
	b.WriteString("这是一个**续跑**的任务。上次已经完成以下步骤：\n")
	for _, step := range resumed.Steps {
		fmt.Fprintf(&b, "- 第 %d 步：%s（工具：%s）\n", step.Index+1, step.Summary, strings.Join(step.ToolsUsed, ", "))
	}
	fmt.Fprintf(&b, "请从第 %d 步继续，不要重复已经完成的操作。\n\n任务：%s", resumed.NextIndex()+1, task)
	return b.String()
}

type AgentResult struct {
	RunID     string
	Stage     string
	Status    RunStatus
	Answer    string
	Steps     int
	ToolCalls int
	Error     string
	// nil = 价格未知（不是 0）
	Cost    *decimal.Decimal
	Elapsed time.Duration
}

func (r AgentResult) OK() bool { return r.Status == StatusDone }

// RunOutcome 自足：multiagent / workflow 不需要别的上下文就能理解它。
type RunOutcome struct {
	RunID     string           `json:"run_id"`
	Stage     string           `json:"stage"`
	Status    RunStatus        `json:"status"`
	Answer    string           `json:"answer"`
	Steps     int              `json:"steps"`
	ToolCalls int              `json:"tool_calls"`
	Elapsed   time.Duration    `json:"-"`
	Cost      *decimal.Decimal `json:"-"`
	Error     string           `json:"error,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	Routing   *RoutingDecision `json:"routing,omitempty"`
}

// MarshalJSON 把“耗时/成本”按人可读的形状输出；成本未知时是 "unknown" 而不是 0。
func (o RunOutcome) MarshalJSON() ([]byte, error) {
	type alias RunOutcome
	return json.Marshal(struct {
		alias
		ElapsedS float64 `json:"elapsed_s"`
		Cost     string  `json:"cost"`
	}{alias: alias(o), ElapsedS: o.Elapsed.Seconds(), Cost: o.CostText()})
}

func (o RunOutcome) OK() bool          { return o.Status == StatusDone }
func (o RunOutcome) NeedsReview() bool { return o.Status == StatusNeedsReview }

func (o RunOutcome) CostText() string {
	if o.Cost == nil {
		return "unknown"
	}
	return o.Cost.String()
}

// String 只用 ASCII 标记 —— 中文 Windows 控制台编不出 ✓ / ⚠。
func (o RunOutcome) String() string {
	mark := map[RunStatus]string{
		StatusDone: "OK", StatusFailed: "!!", StatusCancelled: "--", StatusNeedsReview: "??",
	}[o.Status]
	if mark == "" {
		mark = ".."
	}
	detail := o.Error
	if detail == "" {
		detail = o.Reason
	}
	if detail == "" {
		detail = firstRunes(o.Answer, 40)
	}
	if detail == "" {
		detail = "-"
	}
	return fmt.Sprintf("%s %s [%s] %d 步 / %d 次工具 / %.1fs / 花费 %s：%s",
		mark, o.Stage, o.Status, o.Steps, o.ToolCalls, o.Elapsed.Seconds(), o.CostText(), detail)
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
