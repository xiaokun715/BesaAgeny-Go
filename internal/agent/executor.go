package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/foundation/clock"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/tool"
)

const (
	EventStepCompleted = "agent.step.completed"
)

// ToolCatalog 回答“这一轮该给模型看哪些工具”。按阶段类别筛：
// 给模型看见别的阶段的工具，它会去调用然后被拒。
type ToolCatalog interface {
	Select(categories, only []string) []domain.ToolDefinition
}

type ToolInvoker interface {
	Invoke(ctx context.Context, inv tool.Invocation) tool.Result
}

// PrepareHook 在每次调模型**之前**准备消息。它是参数而不是实例字段：
// AgentExecutor 是长生命周期对象，multiagent 会并发跑多个单 agent，
// 挂在实例上会让 A 的运行用上 B 的预算。
type PrepareHook func(ctx context.Context, messages []domain.Message, step int, remaining *time.Duration) ([]domain.Message, error)

type RunHooks struct {
	Prepare     PrepareHook
	Remaining   func() *time.Duration
	StopReason  func() string
	CallTimeout *time.Duration
	OnProgress  func(cost *decimal.Decimal, toolCalls int)
}

type AgentExecutor struct {
	Gateway     ChatClient
	Tools       ToolInvoker
	Catalog     ToolCatalog
	Alias       string
	MaxSteps    int
	Checkpoints CheckpointStore
	Clock       clock.Clock
}

func NewAgentExecutor(gw ChatClient, tools ToolInvoker, catalog ToolCatalog, alias string, maxSteps int, checkpoints CheckpointStore) *AgentExecutor {
	if alias == "" {
		alias = "runtime.default"
	}
	if maxSteps <= 0 {
		maxSteps = 20
	}
	return &AgentExecutor{Gateway: gw, Tools: tools, Catalog: catalog, Alias: alias, MaxSteps: maxSteps, Checkpoints: checkpoints, Clock: clock.System{}}
}

func (e *AgentExecutor) Run(ctx context.Context, agent StageAgent, task, runID, sessionID, caller string, messages []domain.Message, hooks RunHooks) AgentResult {
	ctx = tool.WithCallState(ctx)
	run := NewTaskRun(runID, agent.Stage, task, sessionID, caller)
	if messages == nil {
		messages = []domain.Message{domain.System(agent.SystemPrompt), domain.User(agent.BuildPrompt(task, nil))}
	}
	toolCalls := 0
	spent := decimal.Zero
	costKnown := true
	started := time.Now()
	traceID := ids.Trace(ctx)

	fail := func(msg string) AgentResult {
		run.Finish(StatusFailed)
		e.save(ctx, run, StatusFailed)
		var cost *decimal.Decimal
		if costKnown {
			c := spent
			cost = &c
		}
		return AgentResult{RunID: runID, Stage: agent.Stage, Status: StatusFailed, Steps: len(run.Steps), ToolCalls: toolCalls, Error: msg, Cost: cost, Elapsed: time.Since(started)}
	}

	for step := 0; step < e.MaxSteps; step++ {
		if hooks.StopReason != nil {
			if reason := hooks.StopReason(); reason != "" {
				return fail(reason)
			}
		}
		var remaining *time.Duration
		if hooks.Remaining != nil {
			remaining = hooks.Remaining()
			if remaining != nil && *remaining <= 0 {
				return fail(fmt.Sprintf("达到时间预算，在第 %d 步停止。要跑更久请调大 agent.limits.deadline_s —— 但先看看它是不是在原地打转。", step+1))
			}
		}
		if hooks.Prepare != nil {
			prepared, err := hooks.Prepare(ctx, messages, step, remaining)
			if err != nil {
				// 压缩是优化，不是依赖：失败就用原消息继续。
				slog.Warn("上下文准备钩子失败，本次用原消息继续", "step", step+1, "err", err)
			} else if prepared != nil {
				messages = prepared
			}
		}

		defs := e.Catalog.Select(agent.ToolCategories(), agent.Tools)
		specs := domain.ToolSpecsOf(defs)
		deadline := pickDeadline(remaining, hooks.CallTimeout)
		result, err := e.Gateway.Chat(ctx, e.Alias, messages, gateway.ChatOptions{
			TraceID: traceID, SessionID: sessionID, Caller: caller, Deadline: deadline, Tools: specs,
		})
		if err != nil {
			return fail(err.Error())
		}
		if result.Cost.Amount == nil {
			costKnown = false
		} else {
			spent = spent.Add(*result.Cost.Amount)
		}
		var delta *decimal.Decimal
		if costKnown {
			if result.Cost.Amount != nil {
				d := *result.Cost.Amount
				delta = &d
			}
		}

		if hooks.OnProgress != nil {
			hooks.OnProgress(delta, toolCalls)
		}

		calls := result.ToolCalls()
		if len(calls) == 0 {
			run.Finish(StatusDone)
			e.save(ctx, run, StatusDone)
			var cost *decimal.Decimal
			if costKnown {
				c := spent
				cost = &c
			}
			return AgentResult{RunID: runID, Stage: agent.Stage, Status: StatusDone, Answer: result.Content(), Steps: len(run.Steps), ToolCalls: toolCalls, Cost: cost, Elapsed: time.Since(started)}
		}

		// 先记模型的工具调用，再执行并回填结果 —— 顺序反了多数厂商会 400。
		messages = append(messages, domain.AssistantCalls(calls, result.Content()))
		var used []string
		var idemKeys []string
		for _, call := range calls {
			outcome := e.Tools.Invoke(ctx, tool.Invocation{
				ToolName: call.Name, Arguments: call.Arguments,
				// 作用域取 run_id：同一运行里同工具同参数 = 同一次调用。
				Scope: runID, TraceID: traceID, SessionID: sessionID, Caller: caller,
			})
			toolCalls++
			used = append(used, call.Name)
			if outcome.IdemKey != "" {
				idemKeys = append(idemKeys, outcome.IdemKey)
			}
			messages = append(messages, domain.ToolResult(call.ID, outcome.ToModelText()))
		}
		if hooks.OnProgress != nil {
			zero := decimal.Zero
			hooks.OnProgress(&zero, toolCalls)
		}
		run.Record(agent.Stage, "调用 "+strings.Join(used, ", "), used, idemKeys)
		e.save(ctx, run, StatusRunning)
	}

	return fail(fmt.Sprintf("达到步数上界（%d）仍未收敛。要调大请改 agent.limits.max_steps —— 但先看看它是不是在原地打转。", e.MaxSteps))
}

func (e *AgentExecutor) save(ctx context.Context, run *TaskRun, status RunStatus) {
	if e.Checkpoints == nil {
		return
	}
	cp := Checkpoint{
		RunID: run.RunID, FormatVersion: 1, StepIndex: run.NextIndex(), NodeID: run.Stage,
		Status: status, State: run.ToState(), SessionID: run.SessionID, Caller: run.Caller,
		CreatedAt: time.Now().UTC(),
	}
	if err := e.Checkpoints.Save(ctx, cp); err != nil {
		// 留痕是旁路，不是依赖；但必须出声。
		slog.Error("检查点写入失败。续跑时这条运行会从头开始 —— 不会重复副作用（幂等层兜底），但会白花一次钱。", "run_id", run.RunID, "err", err)
	}
}

func pickDeadline(remaining *time.Duration, callTimeout *time.Duration) time.Duration {
	var candidates []time.Duration
	if remaining != nil && *remaining > 0 {
		candidates = append(candidates, *remaining)
	}
	if callTimeout != nil && *callTimeout > 0 {
		candidates = append(candidates, *callTimeout)
	}
	if len(candidates) == 0 {
		return 0
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c < best {
			best = c
		}
	}
	return best
}
