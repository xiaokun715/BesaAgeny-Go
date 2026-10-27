package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

type EnginePolicy struct {
	MaxParallel    int
	RetryMax       int
	MaxNodesPerRun int
	Deadline       time.Duration
	OnNodeFailure  string // classify | fail_fast | continue
}

func DefaultEnginePolicy() EnginePolicy {
	return EnginePolicy{MaxParallel: 4, RetryMax: 1, MaxNodesPerRun: 200, Deadline: 30 * time.Minute, OnNodeFailure: "classify"}
}

type Repo interface {
	CreateRun(ctx context.Context, report RunReport, w Workflow, input map[string]any) error
	SaveNode(ctx context.Context, workflowRunID, nodeID string, status NodeState, outputs map[string]any, errMsg string) error
	FinishRun(ctx context.Context, report RunReport) error
}

type Engine struct {
	Pool             *Pool
	Checkpoints      agent.CheckpointStore
	Events           event.Emitter
	Repo             Repo
	Policy           EnginePolicy
	ResourceCapacity map[string]int
}

func NewEngine(pool *Pool, checkpoints agent.CheckpointStore, repo Repo, events event.Emitter, policy EnginePolicy, resources map[string]int) *Engine {
	if policy.MaxParallel <= 0 {
		policy = DefaultEnginePolicy()
	}
	if events == nil {
		events = event.Null{}
	}
	return &Engine{Pool: pool, Checkpoints: checkpoints, Repo: repo, Events: event.Safe(events), Policy: policy, ResourceCapacity: resources}
}

func (e *Engine) Validate(w Workflow, taskInputs map[string]FieldSpec, assets []string) []GraphIssue {
	return Validate(w, ValidationContext{Pool: e.Pool, ResourceCapacity: e.ResourceCapacity, TaskInputs: taskInputs, Assets: assets})
}

// Run 默认必须先校验；任一 issue → 一个节点都不跑。
func (e *Engine) Run(ctx context.Context, w Workflow, input map[string]any, resume bool) (RunReport, error) {
	report := RunReport{
		WorkflowRunID: ids.New("wf"), WorkflowID: w.WorkflowID,
		StartedAt: time.Now().UTC(), Nodes: map[string]NodeResult{},
	}
	if len(w.Nodes) > e.Policy.MaxNodesPerRun {
		return report, fmt.Errorf("图节点数 %d 超过单次运行上界 %d", len(w.Nodes), e.Policy.MaxNodesPerRun)
	}
	taskInputs := map[string]FieldSpec{}
	assets := []string{}
	for k := range input {
		taskInputs[k] = FieldSpec{Type: "", Required: true}
		assets = append(assets, k)
	}
	issues := e.Validate(w, taskInputs, assets)
	if len(issues) > 0 {
		report.Status = StateFailed
		report.Error = fmt.Sprintf("图校验失败（%d 项），未执行任何节点：%s", len(issues), issues[0].Detail)
		report.FinishedAt = time.Now().UTC()
		e.emit(ctx, "workflow.planned.rejected", map[string]any{"workflow_id": w.WorkflowID, "issues": len(issues)})
		return report, fmt.Errorf("%s", report.Error)
	}
	if e.Repo != nil {
		_ = e.Repo.CreateRun(ctx, report, w, input)
	}

	runCtx := ctx
	if e.Policy.Deadline > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, e.Policy.Deadline)
		defer cancel()
	}

	states := map[string]NodeState{}
	attempts := map[string]int{}
	outputs := map[string]map[string]any{}
	errs := map[string]string{}
	resources := map[string]int{}
	running := map[string]bool{}
	for _, n := range w.Nodes {
		states[n.ID] = StatePending
	}

	capacity := func(r string) int {
		if e.ResourceCapacity != nil {
			if c, ok := e.ResourceCapacity[r]; ok {
				return c
			}
		}
		return 1
	}
	canAcquire := func(spec AgentSpec) bool {
		for _, r := range spec.Capability.ExclusiveResources {
			if resources[r] >= capacity(r) {
				return false
			}
		}
		return true
	}
	release := func(spec AgentSpec) {
		for _, r := range spec.Capability.ExclusiveResources {
			if resources[r] > 0 {
				resources[r]--
			}
		}
	}

	// 传播 SKIPPED：上游失败/跳过且本节点依赖它。
	var markSkipped func(nodeID, reason string)
	markSkipped = func(nodeID, reason string) {
		if states[nodeID] != StatePending {
			return
		}
		states[nodeID] = StateSkipped
		e.emit(ctx, "workflow.node.skipped", map[string]any{"workflow_run_id": report.WorkflowRunID, "node_id": nodeID, "reason": reason})
		e.saveNode(ctx, report.WorkflowRunID, w.WorkflowID, nodeID, StateSkipped, nil, reason)
	}

	for {
		if err := runCtx.Err(); err != nil {
			report.Status = StateFailed
			report.Error = "运行超时或取消：" + err.Error()
			break
		}
		// 1) 跳过传播
		for _, n := range w.Nodes {
			if states[n.ID] != StatePending {
				continue
			}
			for _, edge := range w.Upstream(n.ID) {
				if edge.Kind != EdgeData {
					continue
				}
				up := states[edge.Src]
				if up == StateFailed || up == StateSkipped {
					markSkipped(n.ID, fmt.Sprintf("上游 %s 状态为 %s", edge.Src, up))
					break
				}
			}
		}

		// 2) 找就绪节点
		var ready []Node
		for _, n := range w.Nodes {
			if states[n.ID] != StatePending || running[n.ID] {
				continue
			}
			spec, err := e.Pool.Get(n.Agent)
			if err != nil {
				states[n.ID] = StateFailed
				errs[n.ID] = err.Error()
				continue
			}
			if !canAcquire(spec) {
				continue
			}
			ok := true
			for _, edge := range w.Upstream(n.ID) {
				if edge.Kind == EdgeData && states[edge.Src] != StateSucceeded {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, n)
			}
		}
		sort.SliceStable(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
		slots := e.Policy.MaxParallel - len(running)
		if slots > len(ready) {
			slots = len(ready)
		}
		if slots <= 0 {
			if len(running) == 0 {
				break
			}
			// 等任意一个完成需要结果通道；这里的批次执行保证不会走到。
			break
		}
		ready = ready[:slots]

		// 3) 执行这一批
		type outcome struct {
			node   Node
			spec   AgentSpec
			result NodeResult
		}
		results := make(chan outcome, len(ready))
		var wg sync.WaitGroup
		for _, n := range ready {
			spec, _ := e.Pool.Get(n.Agent)
			for _, r := range spec.Capability.ExclusiveResources {
				resources[r]++
			}
			states[n.ID] = StateRunning
			running[n.ID] = true
			inputs := e.resolveInputs(w, n, input, outputs)
			if e.Repo != nil {
				_ = e.Repo.SaveNode(ctx, report.WorkflowRunID, n.ID, StateRunning, nil, "")
			}
			e.emit(ctx, "workflow.node.started", map[string]any{"workflow_run_id": report.WorkflowRunID, "node_id": n.ID, "agent": n.Agent, "attempt": attempts[n.ID] + 1})
			wg.Add(1)
			go func(n Node, spec AgentSpec) {
				defer wg.Done()
				res, err := spec.Runner.RunNode(runCtx, NodeRequest{WorkflowRunID: report.WorkflowRunID, Node: n, Agent: spec, Inputs: inputs})
				if err != nil {
					res = NodeResult{Status: StateFailed, Error: err.Error()}
				}
				results <- outcome{node: n, spec: spec, result: res}
			}(n, spec)
		}
		wg.Wait()
		close(results)

		// 4) 收敛结果
		for out := range results {
			n := out.node
			spec := out.spec
			delete(running, n.ID)
			release(spec)
			res := out.result
			res.ElapsedS = res.Elapsed.Seconds()
			report.Nodes[n.ID] = res
			switch res.Status {
			case StateSucceeded:
				states[n.ID] = StateSucceeded
				outputs[n.ID] = res.Outputs
				e.emit(ctx, "workflow.node.finished", map[string]any{"workflow_run_id": report.WorkflowRunID, "node_id": n.ID, "status": "succeeded"})
				e.saveNode(ctx, report.WorkflowRunID, w.WorkflowID, n.ID, StateSucceeded, res.Outputs, "")
			default:
				if res.NeedsReview {
					errs[n.ID] = "needs_review:" + res.Error
				} else {
					errs[n.ID] = res.Error
				}
				retryable := res.Retryable || e.Policy.OnNodeFailure == "classify"
				if retryable && attempts[n.ID] < e.Policy.RetryMax {
					attempts[n.ID]++
					states[n.ID] = StatePending
					e.emit(ctx, "workflow.node.retried", map[string]any{"workflow_run_id": report.WorkflowRunID, "node_id": n.ID, "attempt": attempts[n.ID]})
					continue
				}
				if n.Optional {
					states[n.ID] = StateSkipped
					e.emit(ctx, "workflow.node.skipped", map[string]any{"workflow_run_id": report.WorkflowRunID, "node_id": n.ID, "reason": "optional 节点失败"})
					e.saveNode(ctx, report.WorkflowRunID, w.WorkflowID, n.ID, StateSkipped, nil, res.Error)
					continue
				}
				states[n.ID] = StateFailed
				e.emit(ctx, "workflow.node.finished", map[string]any{"workflow_run_id": report.WorkflowRunID, "node_id": n.ID, "status": "failed", "error": res.Error})
				e.saveNode(ctx, report.WorkflowRunID, w.WorkflowID, n.ID, StateFailed, res.Outputs, res.Error)
			}
		}
	}

	// 终态判定
	report.FinishedAt = time.Now().UTC()
	report.Status = StateSucceeded
	for _, n := range w.Nodes {
		st := states[n.ID]
		if st == StateFailed {
			report.Status = StateFailed
			if report.Error == "" {
				report.Error = fmt.Sprintf("节点 %s 失败：%s", n.ID, errs[n.ID])
			}
		}
		if st == StatePending || st == StateRunning {
			report.Status = StateFailed
			if report.Error == "" {
				report.Error = fmt.Sprintf("节点 %s 未能完成（状态 %s）", n.ID, st)
			}
		}
	}
	if e.Repo != nil {
		_ = e.Repo.FinishRun(ctx, report)
	}
	e.emit(ctx, "workflow.finished", map[string]any{
		"workflow_run_id": report.WorkflowRunID, "workflow_id": w.WorkflowID,
		"status": report.Status, "nodes": len(w.Nodes),
	})
	return report, nil
}

func (e *Engine) resolveInputs(w Workflow, node Node, taskInput map[string]any, outputs map[string]map[string]any) map[string]any {
	inputs := map[string]any{}
	for _, edge := range w.Upstream(node.ID) {
		if edge.Kind != EdgeData {
			continue
		}
		srcOut := outputs[edge.Src]
		if edge.Field != "" {
			if v, ok := srcOut[edge.Field]; ok {
				inputs[edge.Field] = v
			}
			continue
		}
		for k, v := range srcOut {
			inputs[k] = v
		}
	}
	for field, source := range node.Inputs {
		if out, ok := outputs[source]; ok {
			if v, exists := out[field]; exists {
				inputs[field] = v
			} else {
				for k, v := range out {
					inputs[k] = v
				}
			}
			continue
		}
		if v, ok := taskInput[source]; ok {
			inputs[field] = v
		}
	}
	for k, v := range taskInput {
		if _, ok := inputs[k]; !ok {
			inputs[k] = v
		}
	}
	return inputs
}

func (e *Engine) saveNode(ctx context.Context, wfRunID, wfID, nodeID string, status NodeState, outputs map[string]any, errMsg string) {
	if e.Checkpoints == nil {
		return
	}
	runStatus := agent.StatusDone
	switch status {
	case StateFailed:
		runStatus = agent.StatusFailed
	case StateSkipped:
		runStatus = agent.StatusCancelled
	case StateRunning:
		runStatus = agent.StatusRunning
	}
	state := map[string]any{"workflow_id": wfID, "node_id": nodeID, "status": string(status), "error": errMsg}
	if outputs != nil {
		state["outputs"] = outputs
	}
	cp := agent.Checkpoint{
		RunID: wfRunID, FormatVersion: 1, StepIndex: 0,
		NodeID: wfRunID + ":" + nodeID, Status: runStatus, State: state, CreatedAt: time.Now().UTC(),
	}
	if err := e.Checkpoints.Save(ctx, cp); err != nil {
		slog.Warn("图级检查点写入失败（续跑会重跑该节点）", "node", nodeID, "err", err)
	}
}

func (e *Engine) emit(ctx context.Context, name string, payload map[string]any) {
	ev := event.New(name)
	ev.Payload = payload
	if id, ok := payload["workflow_run_id"].(string); ok {
		ev.Subject = id
	}
	e.Events.Emit(ctx, ev)
}
