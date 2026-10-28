package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/composition"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/experience"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
	"github.com/besa-ai/besa-agent/internal/service/controld"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
	redisstore "github.com/besa-ai/besa-agent/internal/store/redis"
	"github.com/besa-ai/besa-agent/internal/transport"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

type Worker struct {
	Base       *composition.Base
	Runtime    *agent.Runtime
	Sessions   composition.SessionStore
	Tasks      controld.TaskStore
	Redis      *redisstore.Client
	Engine     *workflow.Engine
	Pool       *workflow.Pool
	Experience *transport.ExperienceClient
	Runs       *postgres.RunRepo
}

// StartAgent 消费 agent.run 与 agent.cancel。
func (w *Worker) StartAgent(ctx context.Context) error {
	if w.Base.NATS == nil {
		return nil
	}
	go func() {
		_ = w.Base.NATS.ConsumeJob(ctx, "agent.cancel", "agent-cancel", func(ctx context.Context, payload []byte) error {
			var job transport.CancelJob
			if err := json.Unmarshal(payload, &job); err != nil {
				return nil
			}
			w.Runtime.Cancel(job.RunID)
			if w.Redis != nil {
				_ = w.Redis.RequestCancel(ctx, job.RunID, 30*time.Minute)
			}
			return nil
		})
	}()
	return w.Base.NATS.ConsumeJob(ctx, "agent.run", "agent-workers", w.handleAgent)
}

func (w *Worker) handleAgent(ctx context.Context, payload []byte) error {
	var job transport.AgentJob
	if err := json.Unmarshal(payload, &job); err != nil {
		w.Base.Logger.Warn("agent 作业无法解析", "err", err)
		return nil // 坏消息不重投
	}
	if w.Tasks != nil && job.TaskID != "" {
		_ = w.Tasks.UpdateStatus(ctx, job.TaskID, "running", "", "")
	}
	if job.SessionID != "" {
		_ = w.Sessions.Ensure(ctx, job.SessionID, job.Caller, truncate(job.Goal, 80))
	}
	histRows, _ := w.Sessions.History(ctx, job.SessionID, 200)
	historyMsgs := composition.ToContextMessages(histRows)
	_, _ = w.Sessions.Append(ctx, job.SessionID, domain.User(job.Goal))

	runID := job.RunID
	if runID == "" {
		runID = ids.New("run")
	}
	if w.Tasks != nil && job.TaskID != "" {
		// 任务顶层 run_id 必须可见，否则查询任务时拿不到运行 id。
		_ = w.Tasks.UpdateStatus(ctx, job.TaskID, "running", runID, "")
	}
	if w.Runs != nil {
		_ = w.Runs.Create(ctx, runID, job.SessionID, job.Caller, job.Stage, job.Goal)
	}
	// 跨进程取消：轮询 Redis，让 runtime 在下一个检查点退出。
	if w.Redis != nil {
		watchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-watchCtx.Done():
					return
				case <-ticker.C:
					if cancelled, err := w.Redis.IsCancelled(watchCtx, runID); err == nil && cancelled {
						w.Runtime.Cancel(runID)
						return
					}
				}
			}
		}()
	}

	outcome := w.Runtime.RunOne(ctx, agent.RunRequest{
		Task: job.Goal, Stage: job.Stage, SessionID: job.SessionID, Caller: job.Caller,
		RunID: runID, Resume: job.Resume, History: historyMsgs,
	})
	if outcome.Answer != "" {
		_, _ = w.Sessions.Append(ctx, job.SessionID, domain.Message{Role: domain.RoleAssistant, Content: outcome.Answer})
	}
	result := map[string]any{
		"run_id": outcome.RunID, "status": outcome.Status, "answer": outcome.Answer,
		"steps": outcome.Steps, "tool_calls": outcome.ToolCalls, "elapsed_s": outcome.Elapsed.Seconds(),
		"cost": outcome.CostText(), "error": outcome.Error, "reason": outcome.Reason,
	}
	// 失败 → 经验服务做分类与定向恢复决策。**旁路**：失败不阻断任务收尾。
	if w.Experience != nil && w.Base.Config.Bool("experience.enabled", false) && outcome.Status == agent.StatusFailed {
		decision, err := w.Experience.OnFailure(ctx, experience.Failure{
			Stage: outcome.Stage, Status: string(outcome.Status), ErrorType: outcome.Error,
			Message: outcome.Error, TraceID: outcome.RunID, RunID: outcome.RunID, Risk: "low",
		})
		if err == nil {
			result["recovery_action"] = string(decision.Action)
			result["recovery_reason"] = decision.Reason
		}
	}
	if w.Runs != nil {
		currency := w.Base.Config.String("gateway.cost.currency", "CNY")
		_ = w.Runs.Finish(ctx, runID, outcome.Status, outcome.Cost, currency)
	}
	if w.Tasks != nil && job.TaskID != "" {
		_ = w.Tasks.Complete(ctx, job.TaskID, string(outcome.Status), result, outcome.Error)
	}
	return nil
}

// StartWorkflow 消费 workflow.run。
func (w *Worker) StartWorkflow(ctx context.Context) error {
	if w.Base.NATS == nil || w.Engine == nil {
		return nil
	}
	return w.Base.NATS.ConsumeJob(ctx, "workflow.run", "workflow-workers", w.handleWorkflow)
}

func (w *Worker) handleWorkflow(ctx context.Context, payload []byte) error {
	var job transport.WorkflowJob
	if err := json.Unmarshal(payload, &job); err != nil {
		return nil
	}
	if w.Tasks != nil && job.TaskID != "" {
		_ = w.Tasks.UpdateStatus(ctx, job.TaskID, "running", "", "")
	}
	def := job.Workflow
	if def == nil {
		if job.Mode != "" && job.Mode != "auto" && job.Mode != "choose" {
			_ = w.Tasks.Complete(ctx, job.TaskID, "failed", nil, "该模式需要提供 workflow 定义")
			return nil
		}
		if !w.Base.Config.Bool("workflow.auto.allow", false) {
			_ = w.Tasks.Complete(ctx, job.TaskID, "failed", nil, "workflow.auto.allow=false：自动生成流程图默认关闭")
			return nil
		}
		plan, err := workflow.PlanAuto(workflow.TaskContext{
			Tags:   deriveTags(job.Goal),
			Inputs: map[string]workflow.FieldSpec{},
		}, w.Pool, workflow.DefaultPlannerConfig())
		if err != nil {
			_ = w.Tasks.Complete(ctx, job.TaskID, "failed", nil, "自动规划失败："+err.Error())
			return nil
		}
		if len(plan.Issues) > 0 {
			_ = w.Tasks.Complete(ctx, job.TaskID, "failed", nil, "自动生成的图未通过校验："+plan.Issues[0].Detail)
			return nil
		}
		def = &plan.Workflow
	}
	report, err := w.Engine.Run(ctx, *def, job.Input, job.Resume)
	result := map[string]any{"workflow_run_id": report.WorkflowRunID, "status": report.Status, "error": report.Error}
	status := "done"
	if err != nil || report.Status == workflow.StateFailed {
		status = "failed"
	}
	errMsg := report.Error
	if err != nil {
		errMsg = err.Error()
	}
	if w.Tasks != nil && job.TaskID != "" {
		_ = w.Tasks.UpdateStatus(ctx, job.TaskID, "running", report.WorkflowRunID, "")
		_ = w.Tasks.Complete(ctx, job.TaskID, status, result, errMsg)
	}
	return nil
}

func deriveTags(goal string) []string {
	lower := strings.ToLower(goal)
	var tags []string
	for _, stage := range agent.BuiltinStages() {
		for _, kw := range stage.Keywords {
			if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
				tags = append(tags, stage.Stage)
				break
			}
		}
	}
	return tags
}
