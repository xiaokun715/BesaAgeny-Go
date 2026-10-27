package composition

import (
	"context"
	"time"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

type WorkflowBundle struct {
	Pool   *workflow.Pool
	Engine *workflow.Engine
}

func BuildWorkflow(base *Base, runner func(stage string) workflow.NodeRunner) *WorkflowBundle {
	cfg := base.Config
	pool := workflow.NewPool()
	for _, stage := range agent.StageOrder {
		capability := workflow.Capability{
			Tags: []string{stage}, Version: "1.0.0",
			Inputs:         map[string]workflow.FieldSpec{"task": {Type: "string", Required: false}},
			Outputs:        map[string]workflow.FieldSpec{stage: {Type: "string", Required: true}},
			Parallelizable: true, Retryable: true, EstLatencyS: 30, EstTokens: 4000,
		}
		_ = pool.Register(workflow.AgentSpec{Name: stage, Capability: capability, Runner: runner(stage)})
	}
	resources := map[string]int{}
	for name, raw := range cfg.Section("workflow.resources") {
		if m, ok := raw.(map[string]any); ok {
			if cap, ok := m["capacity"].(int); ok {
				resources[name] = cap
			} else if f, ok := m["capacity"].(float64); ok {
				resources[name] = int(f)
			}
		}
	}
	var repo workflow.Repo
	if base.DB != nil {
		repo = &postgres.WorkflowRepo{DB: base.DB}
	}
	engine := workflow.NewEngine(pool, BuildCheckpoints(base), repo, base.Events, workflow.EnginePolicy{
		MaxParallel:    cfg.Int("workflow.schedule.max_parallel", 4),
		RetryMax:       cfg.Int("workflow.schedule.retry_max", 1),
		MaxNodesPerRun: cfg.Int("workflow.schedule.max_nodes_per_run", 200),
		Deadline:       time.Duration(cfg.Float("workflow.schedule.deadline_s", 1800) * float64(time.Second)),
		OnNodeFailure:  cfg.String("workflow.schedule.on_node_failure", "classify"),
	}, resources)
	return &WorkflowBundle{Pool: pool, Engine: engine}
}

// LocalNodeRunner 用进程内 agent.Runtime 执行节点。
type LocalNodeRunner struct {
	Runtime   *agent.Runtime
	Stage     string
	SessionID string
	Caller    string
}

func (r LocalNodeRunner) RunNode(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	task := workflow.TaskFromInputs(req.Inputs, req.Node.ID)
	out := r.Runtime.RunOne(ctx, agent.RunRequest{Task: task, Stage: r.Stage, SessionID: r.SessionID, Caller: r.Caller})
	result := workflow.NodeResult{Status: workflow.StateSucceeded, Outputs: map[string]any{"answer": out.Answer, "answer_text": out.Answer},
		Elapsed: out.Elapsed, NeedsReview: out.NeedsReview()}
	if out.Cost != nil {
		c, _ := out.Cost.Float64()
		result.Cost = &c
	}
	switch out.Status {
	case agent.StatusDone:
	case agent.StatusNeedsReview:
		result.Status = workflow.StateFailed
		result.NeedsReview = true
		result.Error = out.Reason
	case agent.StatusCancelled:
		result.Status = workflow.StateFailed
		result.Error = "运行被取消"
	default:
		result.Status = workflow.StateFailed
		result.Error = out.Error
		result.Retryable = true
	}
	return result, nil
}
