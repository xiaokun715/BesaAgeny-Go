package agent

import (
	stdctx "context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/foundation/clock"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

const (
	EventRunStarted       = "agent.run.started"
	EventStageRouted      = "agent.stage.routed"
	EventResumeDecided    = "agent.resume.decided"
	EventContextCompacted = "agent.context.compacted"
	EventBudgetExhausted  = "agent.budget.exhausted"
	EventRunFinished      = "agent.run.finished"
	EventDegraded         = "agent.degraded"
)

type Limits struct {
	MaxSteps     int
	Deadline     *time.Duration
	MaxCost      *decimal.Decimal
	CallTimeout  *time.Duration
	MaxToolCalls int
}

func DefaultLimits() Limits {
	d := 600 * time.Second
	return Limits{MaxSteps: 20, Deadline: &d, MaxToolCalls: 100}
}

type ResumePolicy struct {
	OnUncertain string // needs_review | abort | retry_uncertain
	Lease       time.Duration
}

type ContextRunPolicy struct {
	Enabled         bool
	KeepRecentSteps int
	TargetRatio     float64
}

type Policy struct {
	Alias      string
	Limits     Limits
	Resume     ResumePolicy
	Context    ContextRunPolicy
	EmitEvents bool
}

func DefaultPolicy() Policy {
	return Policy{
		Alias: "runtime.default", Limits: DefaultLimits(),
		Resume:     ResumePolicy{OnUncertain: "needs_review", Lease: 120 * time.Second},
		Context:    ContextRunPolicy{Enabled: true, KeepRecentSteps: 4, TargetRatio: 0.6},
		EmitEvents: true,
	}
}

type ContextBuilder interface {
	Build(ctx stdctx.Context, req ctxpkg.Request) ctxpkg.Built
	CompactTranscript(ctx stdctx.Context, messages []ctxpkg.Message, opts ctxpkg.CompactOptions) ctxpkg.CompactResult
	Settle(ctx stdctx.Context, req ctxpkg.Request) bool
	ModelWindow() int
}

type Runtime struct {
	Executor    *AgentExecutor
	Stages      *StageRegistry
	Router      *StageRouter
	Context     ContextBuilder
	Checkpoints CheckpointStore
	ToolFinder  ToolFinder
	Policy      Policy
	Clock       clock.Clock
	Events      event.Emitter

	mu        sync.Mutex
	cancelled map[string]bool
}

type RunRequest struct {
	Task      string
	Stage     string
	Hint      string
	SessionID string
	Caller    string
	RunID     string
	Resume    bool
	History   []ctxpkg.Message
}

func NewRuntime(executor *AgentExecutor, stages *StageRegistry, router *StageRouter, contextBuilder ContextBuilder, checkpoints CheckpointStore, finder ToolFinder, policy Policy, events event.Emitter) *Runtime {
	if policy.Alias == "" {
		policy = DefaultPolicy()
	}
	if policy.Resume.OnUncertain == "" {
		policy.Resume = ResumePolicy{OnUncertain: "needs_review", Lease: 120 * time.Second}
	}
	if policy.Context.KeepRecentSteps < 1 {
		policy.Context.KeepRecentSteps = 4
	}
	if events == nil {
		events = event.Null{}
	}
	return &Runtime{
		Executor: executor, Stages: stages, Router: router, Context: contextBuilder,
		Checkpoints: checkpoints, ToolFinder: finder, Policy: policy,
		Clock: clock.System{}, Events: event.Safe(events), cancelled: map[string]bool{},
	}
}

// RunOne 跑一个阶段到一个终态。自足 —— 它不知道自己在单跑还是被编排。
func (r *Runtime) RunOne(ctx stdctx.Context, req RunRequest) RunOutcome {
	runID := req.RunID
	if runID == "" {
		runID = ids.New("run")
	}
	if req.Task == "" {
		return RunOutcome{RunID: runID, Status: StatusFailed, Error: "任务不能为空"}
	}
	ctx, _ = ids.EnsureTrace(ctx)
	started := r.Clock.Now()
	b := &runBudget{limits: r.Policy.Limits, started: started, clock: r.Clock, cancelled: func() bool { return r.isCancelled(runID) }}
	b.start()

	r.emit(ctx, EventRunStarted, map[string]any{"run_id": runID, "session_id": req.SessionID, "resume": req.Resume})

	decision, err := r.Router.Route(ctx, req.Task, req.Stage, req.Hint)
	if err != nil {
		reason := err.Error()
		r.saveTerminal(ctx, runID, StatusFailed, req, "", reason)
		r.emit(ctx, EventRunFinished, map[string]any{"run_id": runID, "status": StatusFailed, "error": reason})
		return RunOutcome{RunID: runID, Status: StatusFailed, Error: reason, Elapsed: b.elapsed()}
	}
	r.emit(ctx, EventStageRouted, map[string]any{
		"run_id": runID, "stage": decision.Stage, "reason": decision.Reason,
		"confidence": decision.Confidence, "needs_input": decision.NeedsInput, "candidates": decision.Candidates,
	})
	if decision.NeedsInput {
		out := RunOutcome{RunID: runID, Status: StatusNeedsReview, Reason: decision.Reason, Routing: &decision, Elapsed: b.elapsed()}
		r.saveTerminal(ctx, runID, StatusNeedsReview, req, "", decision.Reason)
		r.emit(ctx, EventRunFinished, map[string]any{"run_id": runID, "status": StatusNeedsReview, "reason": decision.Reason})
		return out
	}
	agentDef, err := r.Stages.Get(decision.Stage)
	if err != nil {
		reason := err.Error()
		r.saveTerminal(ctx, runID, StatusFailed, req, "", reason)
		return RunOutcome{RunID: runID, Status: StatusFailed, Error: reason, Routing: &decision, Elapsed: b.elapsed()}
	}

	resumed, kind, reason := r.decideResume(ctx, runID, req.Resume)
	r.emit(ctx, EventResumeDecided, map[string]any{"run_id": runID, "decision": kind, "reason": reason})
	switch kind {
	case "needs_review":
		out := RunOutcome{RunID: runID, Stage: agentDef.Stage, Status: StatusNeedsReview, Reason: reason, Routing: &decision, Elapsed: b.elapsed()}
		r.saveTerminal(ctx, runID, StatusNeedsReview, req, agentDef.Stage, reason)
		r.emit(ctx, EventRunFinished, map[string]any{"run_id": runID, "stage": agentDef.Stage, "status": StatusNeedsReview, "reason": reason})
		return out
	case "abort":
		out := RunOutcome{RunID: runID, Stage: agentDef.Stage, Status: StatusFailed, Error: reason, Routing: &decision, Elapsed: b.elapsed()}
		r.saveTerminal(ctx, runID, StatusFailed, req, agentDef.Stage, reason)
		r.emit(ctx, EventRunFinished, map[string]any{"run_id": runID, "stage": agentDef.Stage, "status": StatusFailed, "error": reason})
		return out
	}

	current := agentDef.BuildPrompt(req.Task, resumed)
	var messages []domain.Message
	var contextReq ctxpkg.Request
	if r.Context != nil {
		contextReq = ctxpkg.Request{
			SessionID: req.SessionID, History: req.History, CurrentInput: current,
			System: agentDef.SystemPrompt, Goal: req.Task, Caller: req.Caller, TraceID: ids.Trace(ctx),
		}
		built := r.Context.Build(ctx, contextReq)
		messages = built.Messages
		if built.DegradedReason != "" {
			r.emit(ctx, EventDegraded, map[string]any{"run_id": runID, "phase": "context", "reason": built.DegradedReason})
		}
	} else {
		messages = []domain.Message{domain.System(agentDef.SystemPrompt), domain.User(current)}
	}

	hooks := RunHooks{
		Remaining:   b.remaining,
		StopReason:  b.stopReason,
		CallTimeout: r.Policy.Limits.CallTimeout,
		OnProgress: func(cost *decimal.Decimal, toolCalls int) {
			b.addCost(cost)
			b.toolCalls = toolCalls
		},
	}
	if r.Context != nil && r.Policy.Context.Enabled {
		hooks.Prepare = func(ctx stdctx.Context, msgs []domain.Message, step int, remaining *time.Duration) ([]domain.Message, error) {
			return r.prepare(ctx, runID, msgs, step)
		}
	}

	result := r.Executor.Run(ctx, agentDef, req.Task, runID, req.SessionID, req.Caller, messages, hooks)

	status := result.Status
	if r.isCancelled(runID) && status == StatusFailed {
		status = StatusCancelled
	}
	if ctx.Err() != nil && status == StatusFailed {
		status = StatusCancelled
	}
	if status == StatusFailed && result.Error != "" {
		r.emit(ctx, EventBudgetExhausted, map[string]any{"run_id": runID, "reason": result.Error})
	}
	cost := result.Cost
	if cost == nil {
		// 只有预算里确实累加过成本时才用它兜底；否则**未知仍然是未知**。
		if total := b.totalCost(); total != nil && b.cost.IsPositive() {
			cost = total
		}
	}
	outcome := RunOutcome{
		RunID: runID, Stage: result.Stage, Status: status, Answer: result.Answer,
		Steps: result.Steps, ToolCalls: result.ToolCalls, Error: result.Error,
		Cost: cost, Elapsed: result.Elapsed, Routing: &decision,
	}
	if outcome.Elapsed <= 0 {
		outcome.Elapsed = b.elapsed()
	}
	r.finish(ctx, outcome, req, contextReq)
	return outcome
}

func (r *Runtime) prepare(ctx stdctx.Context, runID string, messages []domain.Message, step int) ([]domain.Message, error) {
	policy := r.Policy.Context
	if r.Context == nil || !policy.Enabled {
		return messages, nil
	}
	converted := ctxpkg.FromDomainAll(messages)
	var preamble []ctxpkg.Message
	rest := converted
	for len(rest) > 0 && rest[0].Role != domain.RoleAssistant {
		preamble = append(preamble, rest[0])
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return messages, nil
	}
	window := r.Context.ModelWindow()
	available := 0
	if window > 0 {
		available = window - 4096 - 1024
	}
	target := int(float64(available) * policy.TargetRatio)
	if target < 64 {
		target = 64
	}
	result := r.Context.CompactTranscript(ctx, rest, ctxpkg.CompactOptions{
		TargetTokens: target, KeepRecent: policy.KeepRecentSteps, Unit: "step",
		Segment: ctxpkg.SegmentWindow, Reason: fmt.Sprintf("运行内压缩（第 %d 步之前）", step+1),
	})
	saved := 0
	for _, e := range result.Entries {
		saved += e.Before - e.After
	}
	r.emit(ctx, EventContextCompacted, map[string]any{
		"run_id": runID, "step": step + 1, "level": result.LevelUsed,
		"saved_tokens": saved, "blocks": len(result.Blocks), "note": result.Note,
	})
	blocks := make([]ctxpkg.Block, 0, len(preamble)+len(result.Blocks))
	for _, m := range preamble {
		copied := m
		blocks = append(blocks, ctxpkg.Block{Content: m.Content, Role: m.Role, Segment: ctxpkg.SegmentWindow, Level: ctxpkg.LevelL0, Message: &copied})
	}
	blocks = append(blocks, result.Blocks...)
	return ctxpkg.Rebuild(blocks), nil
}

func (r *Runtime) decideResume(ctx stdctx.Context, runID string, resume bool) (*TaskRun, string, string) {
	if !resume {
		return nil, "start", "未要求续跑"
	}
	if r.Checkpoints == nil {
		return nil, "start", "没有配置检查点存储，无法查快照 —— 从头跑"
	}
	cp, err := r.Checkpoints.Latest(ctx, runID)
	if err != nil {
		slog.Warn("读取检查点失败，从头跑", "run_id", runID, "err", err)
		return nil, "start", "读取检查点失败 —— 从头跑"
	}
	if cp == nil {
		return nil, "start", "没有找到这个 run_id 的快照 —— 从头跑"
	}
	if cp.Status == StatusNeedsReview {
		return nil, "needs_review", "这个运行上次停在 needs_review，还没被人处理过。先处理它，再决定要不要续跑 —— 直接重跑会盖掉那个待决的问题。"
	}
	if cp.Status.Finished() {
		return nil, "start", fmt.Sprintf("上一次已经 %s，这次是重跑", cp.Status)
	}
	run := TaskRunFromState(runID, cp.State)
	if run == nil {
		return nil, "start", "快照无法还原 —— 从头跑"
	}
	uncertain := r.uncertainCalls(ctx, run)
	if len(uncertain) == 0 {
		return run, "resume", fmt.Sprintf("从第 %d 步接着跑（无不确定的工具执行）", run.NextIndex()+1)
	}
	detail := uncertain[0]
	if len(uncertain) > 1 {
		detail = fmt.Sprintf("%s 等 %d 个", detail, len(uncertain))
	}
	switch r.Policy.Resume.OnUncertain {
	case "retry_uncertain":
		slog.Warn("续跑：有工具执行结果不明，按配置**冒险重做**，可能重复产生副作用", "run_id", runID, "detail", detail)
		return run, "resume", "**冒险重做**：" + detail
	case "abort":
		return nil, "abort", fmt.Sprintf("有工具执行结果不明（%s），按配置放弃续跑。", detail)
	default:
		return nil, "needs_review", fmt.Sprintf(
			"上一次停在第 %d 步，而这一步有工具调用**执行过、但结果没回填**（%s）。接着跑可能重复产生副作用，重来可能白做 —— 这个取舍不替你做。",
			run.NextIndex()+1, detail)
	}
}

func (r *Runtime) uncertainCalls(ctx stdctx.Context, run *TaskRun) []string {
	if r.ToolFinder == nil {
		return nil
	}
	lease := r.Policy.Resume.Lease
	if lease <= 0 {
		lease = 120 * time.Second
	}
	var uncertain []string
	for _, step := range run.Steps {
		for _, key := range step.IdemKeys {
			view, err := r.ToolFinder.Find(ctx, run.RunID, key, lease)
			if err != nil || view == nil {
				continue
			}
			if view.Uncertain {
				prefix := key
				if len(prefix) > 12 {
					prefix = prefix[:12]
				}
				uncertain = append(uncertain, fmt.Sprintf("%s(%s)", view.ToolName, prefix))
			}
		}
	}
	return uncertain
}

func (r *Runtime) finish(ctx stdctx.Context, outcome RunOutcome, req RunRequest, contextReq ctxpkg.Request) {
	// 收尾四件旁路事：终态快照、事件、记忆沉淀。**任何一件失败都不得让运行失败。**
	r.saveTerminal(ctx, outcome.RunID, outcome.Status, req, outcome.Stage, outcome.Reason)
	r.emit(ctx, EventRunFinished, map[string]any{
		"run_id": outcome.RunID, "stage": outcome.Stage, "status": outcome.Status,
		"steps": outcome.Steps, "tool_calls": outcome.ToolCalls,
		"elapsed_s": outcome.Elapsed.Seconds(), "cost": outcome.CostText(),
		"error": outcome.Error, "reason": outcome.Reason,
	})
	if r.Context != nil && outcome.OK() {
		history := append([]ctxpkg.Message{}, contextReq.History...)
		history = append(history,
			ctxpkg.Message{Role: domain.RoleUser, Content: req.Task},
			ctxpkg.Message{Role: domain.RoleAssistant, Content: outcome.Answer},
		)
		_ = r.Context.Settle(ctx, ctxpkg.Request{
			SessionID: req.SessionID, History: history, CurrentInput: req.Task,
			Goal: req.Task, Caller: req.Caller, TraceID: ids.Trace(ctx),
		})
	}
}

func (r *Runtime) saveTerminal(ctx stdctx.Context, runID string, status RunStatus, req RunRequest, stage, reason string) {
	if r.Checkpoints == nil {
		return
	}
	cp := Checkpoint{
		RunID: runID, FormatVersion: 1, StepIndex: 0, NodeID: stage, Status: status,
		State:     map[string]any{"stage": stage, "status": string(status), "reason": reason},
		SessionID: req.SessionID, Caller: req.Caller, CreatedAt: time.Now().UTC(),
	}
	if err := r.Checkpoints.Save(ctx, cp); err != nil {
		slog.Error("终态检查点写入失败（续跑时会从头开始，但不会重复副作用）", "run_id", runID, "status", status, "err", err)
	}
}

// Cancel 是“另一个协程在喊停”，不是立刻终止 —— 循环会在下一轮检查点退出。
func (r *Runtime) Cancel(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelled[runID] = true
}

func (r *Runtime) isCancelled(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled[runID]
}

func (r *Runtime) emit(ctx stdctx.Context, name string, payload map[string]any) {
	if !r.Policy.EmitEvents {
		return
	}
	ev := event.New(name)
	ev.TraceID = ids.Trace(ctx)
	if runID, ok := payload["run_id"].(string); ok {
		ev.Subject = runID
	}
	if sessionID, ok := payload["session_id"].(string); ok {
		ev.SessionID = sessionID
	}
	ev.Payload = payload
	r.Events.Emit(ctx, ev)
}

// ------------------------------ 预算 ------------------------------

type runBudget struct {
	limits    Limits
	clock     clock.Clock
	started   time.Time
	cost      decimal.Decimal
	costKnown bool
	toolCalls int
	cancelled func() bool
}

func (b *runBudget) start() {
	b.started = b.clock.Now()
	b.costKnown = true
}

func (b *runBudget) elapsed() time.Duration {
	return max(0, b.clock.Since(b.started))
}

func (b *runBudget) remaining() *time.Duration {
	if b.limits.Deadline == nil {
		return nil
	}
	rem := *b.limits.Deadline - b.elapsed()
	return &rem
}

func (b *runBudget) addCost(amount *decimal.Decimal) {
	if amount == nil {
		b.costKnown = false
		return
	}
	b.cost = b.cost.Add(*amount)
}

func (b *runBudget) totalCost() *decimal.Decimal {
	if !b.costKnown {
		return nil
	}
	c := b.cost
	return &c
}

// stopReason 返回非空就是停下的理由，并写明撞的是哪一个上界。
func (b *runBudget) stopReason() string {
	if b.cancelled != nil && b.cancelled() {
		return "运行被取消（外部要求停下）"
	}
	if rem := b.remaining(); rem != nil && *rem <= 0 {
		return fmt.Sprintf("达到时间预算（%.0f 秒）", b.limits.Deadline.Seconds())
	}
	if b.limits.MaxToolCalls > 0 && b.toolCalls >= b.limits.MaxToolCalls {
		return fmt.Sprintf("达到工具调用次数上界（%d）", b.limits.MaxToolCalls)
	}
	if b.limits.MaxCost != nil && b.costKnown && b.cost.GreaterThanOrEqual(*b.limits.MaxCost) {
		return fmt.Sprintf("达到成本上界（%s）", b.limits.MaxCost.String())
	}
	return ""
}
