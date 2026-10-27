// Package multiagent 把单 agent 的 run_one 当零件编排。
//
// 它**不把协作逻辑下沉到单 agent**：单 agent 自足、不知道自己在被编排。
package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

type Runner interface {
	RunOne(ctx context.Context, req agent.RunRequest) agent.RunOutcome
}

type LLM interface {
	Complete(ctx context.Context, system, prompt string, schema map[string]any, maxTokens int) (string, error)
}

type Member struct {
	Name    string
	Stage   string
	Runtime Runner
}

type Orchestrator struct {
	members map[string]Member
	LLM     LLM
	Events  event.Emitter
}

func NewOrchestrator(members []Member, llm LLM, events event.Emitter) *Orchestrator {
	m := map[string]Member{}
	for _, member := range members {
		m[member.Name] = member
	}
	if events == nil {
		events = event.Null{}
	}
	return &Orchestrator{members: m, LLM: llm, Events: event.Safe(events)}
}

type Request struct {
	Task         string
	Strategy     string // parallel | committee | debate | hierarchical
	Participants []string
	Rounds       int
	SessionID    string
	Caller       string
	Risk         string
}

type ParticipantRun struct {
	Name        string
	Stage       string
	Status      agent.RunStatus
	Answer      string
	Error       string
	Cost        string
	NeedsReview bool
}

type Outcome struct {
	Strategy  string
	Answer    string
	Runs      []ParticipantRun
	Consensus float64
	Reason    string
}

func (o *Orchestrator) Run(ctx context.Context, req Request) (Outcome, error) {
	members, err := o.resolve(req.Participants)
	if err != nil {
		return Outcome{}, err
	}
	strategy := req.Strategy
	if strategy == "" {
		strategy = "parallel"
	}
	if req.Rounds <= 0 {
		req.Rounds = 2
	}
	switch strategy {
	case "parallel":
		runs := o.parallel(ctx, members, req.Task, req.SessionID, req.Caller)
		return Outcome{Strategy: strategy, Answer: o.aggregate(ctx, req.Task, runs), Runs: runs, Reason: "并行扇出后汇总"}, nil
	case "committee":
		runs := o.parallel(ctx, members, req.Task, req.SessionID, req.Caller)
		answer, consensus, reason := o.vote(ctx, req.Task, runs)
		return Outcome{Strategy: strategy, Answer: answer, Runs: runs, Consensus: consensus, Reason: reason}, nil
	case "debate":
		return o.debate(ctx, members, req), nil
	case "hierarchical":
		return o.hierarchical(ctx, members, req), nil
	default:
		return Outcome{}, fmt.Errorf("未知的多 agent 策略 %q", strategy)
	}
}

func (o *Orchestrator) resolve(names []string) ([]Member, error) {
	if len(names) == 0 {
		for _, m := range o.members {
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("没有可用的 Agent")
	}
	var out []Member
	for _, name := range names {
		m, ok := o.members[name]
		if !ok {
			return nil, fmt.Errorf("未知 Agent %q", name)
		}
		out = append(out, m)
	}
	return out, nil
}

func (o *Orchestrator) parallel(ctx context.Context, members []Member, task, sessionID, caller string) []ParticipantRun {
	runs := make([]ParticipantRun, len(members))
	var wg sync.WaitGroup
	for i, member := range members {
		wg.Add(1)
		go func(i int, member Member) {
			defer wg.Done()
			out := member.Runtime.RunOne(ctx, agent.RunRequest{Task: task, Stage: member.Stage, SessionID: sessionID, Caller: caller})
			runs[i] = ParticipantRun{Name: member.Name, Stage: out.Stage, Status: out.Status,
				Answer: out.Answer, Error: out.Error, Cost: out.CostText(), NeedsReview: out.NeedsReview()}
			o.emit(ctx, "multiagent.participant.finished", map[string]any{"agent": member.Name, "status": out.Status})
		}(i, member)
	}
	wg.Wait()
	return runs
}

func (o *Orchestrator) aggregate(ctx context.Context, task string, runs []ParticipantRun) string {
	var ok []ParticipantRun
	for _, r := range runs {
		if r.Status == agent.StatusDone && strings.TrimSpace(r.Answer) != "" {
			ok = append(ok, r)
		}
	}
	if len(ok) == 0 {
		return ""
	}
	if len(ok) == 1 || o.LLM == nil {
		return joinAnswers(ok)
	}
	var b strings.Builder
	for _, r := range ok {
		fmt.Fprintf(&b, "【%s】\n%s\n\n", r.Name, r.Answer)
	}
	out, err := o.LLM.Complete(ctx,
		"你是汇总器。合并多个专家的答案：保留一致结论，显式列出分歧，不要编造。",
		"任务："+task+"\n\n"+b.String(), nil, 2048)
	if err != nil || strings.TrimSpace(out) == "" {
		return joinAnswers(ok)
	}
	return out
}

func (o *Orchestrator) vote(ctx context.Context, task string, runs []ParticipantRun) (string, float64, string) {
	var ok []ParticipantRun
	for _, r := range runs {
		if r.Status == agent.StatusDone && strings.TrimSpace(r.Answer) != "" {
			ok = append(ok, r)
		}
	}
	if len(ok) == 0 {
		return "", 0, "没有成功的参与者"
	}
	if len(ok) == 1 {
		return ok[0].Answer, 1, "只有一个成功参与者"
	}
	// 先看是否有完全一致的答案（确定性多数）。
	counts := map[string]int{}
	for _, r := range ok {
		counts[strings.TrimSpace(r.Answer)]++
	}
	best, bestCount := "", 0
	for answer, n := range counts {
		if n > bestCount {
			best, bestCount = answer, n
		}
	}
	if bestCount > len(ok)/2 {
		return best, float64(bestCount) / float64(len(ok)), "多数票一致"
	}
	if o.LLM != nil {
		var b strings.Builder
		for i, r := range ok {
			fmt.Fprintf(&b, "[%d] %s：%s\n", i+1, r.Name, r.Answer)
		}
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"choice": map[string]any{"type": "integer"},
				"reason": map[string]any{"type": "string"},
			},
			"required": []string{"choice", "reason"},
		}
		out, err := o.LLM.Complete(ctx, "你是评委。选出最符合任务要求、证据最充分的答案。", task+"\n\n"+b.String(), schema, 512)
		if err == nil {
			var parsed struct {
				Choice int    `json:"choice"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal([]byte(stripFence(out)), &parsed) == nil && parsed.Choice >= 1 && parsed.Choice <= len(ok) {
				return ok[parsed.Choice-1].Answer, 1.0 / float64(len(ok)), "评委选择：" + parsed.Reason
			}
		}
	}
	return joinAnswers(ok), float64(bestCount) / float64(len(ok)), "无多数票，合并答案"
}

func (o *Orchestrator) debate(ctx context.Context, members []Member, req Request) Outcome {
	if len(members) < 2 {
		runs := o.parallel(ctx, members, req.Task, req.SessionID, req.Caller)
		return Outcome{Strategy: "debate", Answer: o.aggregate(ctx, req.Task, runs), Runs: runs, Reason: "参与者不足 2，退化为并行"}
	}
	var all []ParticipantRun
	previous := ""
	for round := 1; round <= req.Rounds; round++ {
		var wg sync.WaitGroup
		runs := make([]ParticipantRun, len(members))
		for i, member := range members {
			wg.Add(1)
			go func(i int, member Member) {
				defer wg.Done()
				task := req.Task
				if previous != "" {
					task += "\n\n其他参与者的上一轮观点：\n" + previous + "\n请指出分歧并修正你的结论。"
				}
				out := member.Runtime.RunOne(ctx, agent.RunRequest{Task: task, Stage: member.Stage, SessionID: req.SessionID, Caller: req.Caller})
				runs[i] = ParticipantRun{Name: member.Name, Stage: out.Stage, Status: out.Status, Answer: out.Answer, Error: out.Error, Cost: out.CostText()}
			}(i, member)
		}
		wg.Wait()
		all = append(all, runs...)
		var b strings.Builder
		for _, r := range runs {
			fmt.Fprintf(&b, "【%s】%s\n", r.Name, r.Answer)
		}
		previous = b.String()
	}
	answer, consensus, reason := o.vote(ctx, req.Task, all[len(all)-len(members):])
	return Outcome{Strategy: "debate", Answer: answer, Runs: all, Consensus: consensus, Reason: reason}
}

func (o *Orchestrator) hierarchical(ctx context.Context, members []Member, req Request) Outcome {
	if len(members) < 2 || o.LLM == nil {
		runs := o.parallel(ctx, members, req.Task, req.SessionID, req.Caller)
		return Outcome{Strategy: "hierarchical", Answer: o.aggregate(ctx, req.Task, runs), Runs: runs, Reason: "没有管理者或 LLM，退化为并行"}
	}
	workers := members[1:]
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subtasks": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"subtasks"},
	}
	out, err := o.LLM.Complete(ctx, "你是管理者。把任务拆成可由下属独立完成的子任务，返回 JSON。", req.Task, schema, 1024)
	var subtasks []string
	if err == nil {
		var parsed struct {
			Subtasks []string `json:"subtasks"`
		}
		_ = json.Unmarshal([]byte(stripFence(out)), &parsed)
		subtasks = parsed.Subtasks
	}
	if len(subtasks) == 0 {
		subtasks = []string{req.Task}
	}
	var wg sync.WaitGroup
	results := make([]ParticipantRun, 0, len(subtasks))
	var mu sync.Mutex
	for i, sub := range subtasks {
		member := workers[i%len(workers)]
		wg.Add(1)
		go func(sub string, member Member) {
			defer wg.Done()
			res := member.Runtime.RunOne(ctx, agent.RunRequest{Task: sub, Stage: member.Stage, SessionID: req.SessionID, Caller: req.Caller})
			mu.Lock()
			results = append(results, ParticipantRun{Name: member.Name, Stage: res.Stage, Status: res.Status, Answer: res.Answer, Error: res.Error, Cost: res.CostText()})
			mu.Unlock()
		}(sub, member)
	}
	wg.Wait()
	answer := o.aggregate(ctx, req.Task, results)
	return Outcome{Strategy: "hierarchical", Answer: answer, Runs: results, Reason: fmt.Sprintf("管理者拆分 %d 个子任务", len(subtasks))}
}

func (o *Orchestrator) emit(ctx context.Context, name string, payload map[string]any) {
	ev := event.New(name)
	ev.Payload = payload
	o.Events.Emit(ctx, ev)
}

func joinAnswers(runs []ParticipantRun) string {
	var b strings.Builder
	for i, r := range runs {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		b.WriteString(r.Answer)
	}
	return b.String()
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// WorkflowRunner 把 multiagent 的成员适配成 workflow.NodeRunner。
type WorkflowRunner struct {
	Member Member
}

func (r WorkflowRunner) RunNode(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	task := workflow.TaskFromInputs(req.Inputs, req.Node.ID)
	out := r.Member.Runtime.RunOne(ctx, agent.RunRequest{Task: task, Stage: r.Member.Stage})
	outputs := map[string]any{"answer": out.Answer, "answer_text": out.Answer}
	result := workflow.NodeResult{
		Status: workflow.StateSucceeded, Outputs: outputs,
		Elapsed: out.Elapsed, NeedsReview: out.NeedsReview(),
	}
	if out.Cost != nil {
		cost, _ := out.Cost.Float64()
		result.Cost = &cost
	}
	switch out.Status {
	case agent.StatusDone:
		result.Status = workflow.StateSucceeded
	case agent.StatusNeedsReview:
		result.Status = workflow.StateFailed
		result.Error = out.Reason
		result.NeedsReview = true
	case agent.StatusCancelled:
		result.Status = workflow.StateFailed
		result.Error = "运行被取消"
	default:
		result.Status = workflow.StateFailed
		result.Error = out.Error
		result.Retryable = true
	}
	o := NewOrchestrator(nil, nil, nil)
	o.emit(ctx, "workflow.node.agent.finished", map[string]any{"node_id": req.Node.ID, "agent": r.Member.Name, "status": out.Status})
	return result, nil
}

var _ = slog.Warn
