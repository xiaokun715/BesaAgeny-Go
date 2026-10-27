package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
	"github.com/besa-ai/besa-agent/internal/foundation/logx"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

const (
	EventStarted          = "tool.started"
	EventExecuted         = "tool.executed"
	EventExecutedDegraded = "tool.executed_degraded"
	EventReused           = "tool.reused"
	EventRefused          = "tool.refused"
	EventUncertain        = "tool.uncertain"
	EventSpilled          = "tool.spilled"
	EventLoopBlocked      = "tool.loop_blocked"
)

type Executor struct {
	Registry   *Registry
	Permission *PermissionPolicy
	Guard      *IdempotencyGuard
	Inspector  InjectionPolicy
	Results    *ResultProcessor
	Breaker    *RetryBreaker
	Events     event.Emitter
	KeyField   string // 工具实参里承载显式幂等键的字段名
	Limits     Limits

	schemaMu sync.Mutex
	schemas  map[string]*jsonschema.Schema
}

func NewExecutor(registry *Registry, permission *PermissionPolicy, guard *IdempotencyGuard, events event.Emitter, limits Limits) *Executor {
	if limits.MaxOutputBytes == 0 {
		limits = DefaultLimits()
	}
	return &Executor{
		Registry: registry, Permission: permission, Guard: guard,
		Inspector: DefaultInjectionPolicy(),
		Results:   NewResultProcessor(limits, true),
		Breaker:   NewRetryBreaker(3),
		Events:    event.Safe(events),
		KeyField:  "idempotency_key",
		Limits:    limits,
		schemas:   map[string]*jsonschema.Schema{},
	}
}

// Invoke 是工具层唯一入口。顺序是约束：
// 找工具 → 参数 → 注入 → 权限 → 循环 → 无效重试 → 幂等登记 → 执行 → 收尾。
func (e *Executor) Invoke(ctx context.Context, inv Invocation) Result {
	started := time.Now()
	ctx = WithCallState(ctx)
	if inv.TraceID != "" {
		ctx = ids.WithTrace(ctx, inv.TraceID)
	}

	tool, err := e.Registry.Get(inv.ToolName)
	if err != nil {
		return e.refuse(ctx, inv, SideEffectRead, err.Error(), "未知工具")
	}
	def := tool.Definition()
	effect := SideEffect(def.SideEffect)
	if effect == "" {
		effect = SideEffectRead
	}

	args, invalid := e.prepareArguments(def, inv)
	if invalid != "" {
		return e.refuse(ctx, inv, effect, invalid, "参数不合法")
	}

	tc := e.Permission.ContextFor(def, inv, e.Limits)

	// 注入检测**在权限之前**：权限假定参数格式合法。
	if verdict := e.Inspector.Check(args, tc.AllowedPaths); !verdict.Safe() {
		return e.refuse(ctx, inv, effect, verdict.Reason(), "注入检测")
	}
	if decision := e.Permission.Authorize(def); !decision.Allowed {
		return e.refuse(ctx, inv, effect, decision.Reason, "未授权")
	}

	state := stateFrom(ctx)
	if blocked := state.check(inv.ToolName, e.Limits.MaxCallDepth, e.Limits.MaxCallsPerRun); blocked != "" {
		e.emit(ctx, EventLoopBlocked, inv, effect, map[string]any{"outcome": OutcomeRefused, "reason": "循环"})
		return e.refuse(ctx, inv, effect, blocked, "循环")
	}

	fingerprint := ArgsDigest(inv)
	if e.Breaker.ShouldRefuse(fingerprint) {
		return e.refuse(ctx, inv, effect, fmt.Sprintf(
			"同一份参数已经连续失败 %d 次。**换个做法** —— 重复提交同样的参数不会有不同结果。",
			e.Breaker.Count(fingerprint)), "无效重试")
	}

	claim := e.Guard.Claim(ctx, inv, effect)
	if !claim.CanExecute {
		outcome := claim.Outcome
		name := EventRefused
		if outcome == OutcomeReused {
			name = EventReused
		} else if outcome == OutcomeUncertain {
			name = EventUncertain
		}
		e.emit(ctx, name, inv, effect, map[string]any{"outcome": outcome, "reason": claim.Reason})
		return claim.AsResult()
	}
	if claim.Degraded() {
		e.emit(ctx, EventExecutedDegraded, inv, effect, map[string]any{"outcome": OutcomeExecutedDegraded, "reason": claim.Reason})
	}

	e.emit(ctx, EventStarted, inv, effect, map[string]any{"outcome": "started"})
	state.enter(inv.ToolName)
	result := e.runTool(ctx, tool, args, tc)
	state.exit()

	return e.finish(ctx, claim, inv, tc, fingerprint, result, started)
}

func (e *Executor) runTool(ctx context.Context, tool Tool, args map[string]any, tc *Context) (result Result) {
	timeout := tc.Timeout
	if timeout <= 0 {
		timeout = DefaultLimits().Timeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("工具执行时 panic", "tool", tool.Name(), "panic", rec)
			result = Result{Outcome: OutcomeFailed, ToolName: tool.Name(), Error: fmt.Sprintf("panic: %v", rec), ErrorKind: ErrorUnknown, Recovery: RecoveryReport}
		}
	}()
	result = tool.Run(runCtx, args, tc)
	if runCtx.Err() == context.DeadlineExceeded && !result.OK() {
		result.Outcome = OutcomeFailed
		result.ErrorKind = ErrorTransient
		result.Recovery = RecoveryAdvise
		if result.Error == "" {
			result.Error = fmt.Sprintf("工具执行超时（%s），已终止。注意：超时前它可能已经产生了副作用。", timeout)
		}
	}
	return result
}

func (e *Executor) finish(ctx context.Context, claim Claim, inv Invocation, tc *Context, fingerprint string, result Result, started time.Time) Result {
	effect := claim.SideEffect
	if result.OK() {
		e.Breaker.Clear(fingerprint)
		// **先结果处置，再登记完成**：脱敏不可逆，不能先存后脱。
		result = e.Results.Process(ctx, result, tc, claim.IdemKey)
	} else {
		e.Breaker.RecordFailure(fingerprint)
	}

	elapsed := time.Since(started)
	if result.ToolName == "" {
		result.ToolName = inv.ToolName
	}
	if result.ErrorKind == "" {
		result.ErrorKind = ErrorUnknown
	}
	if result.Recovery == "" {
		result.Recovery = RecoveryReport
	}
	if claim.Degraded() && result.Outcome == OutcomeExecuted {
		result.Outcome = OutcomeExecutedDegraded
	}
	result.Elapsed = elapsed
	result.IdemKey = claim.IdemKey

	if result.Outcome == OutcomeExecuted || result.Outcome == OutcomeExecutedDegraded {
		if err := e.Guard.MarkDone(ctx, claim, result); err != nil {
			slog.Warn("登记完成状态失败（去重可能在下次失效）", "err", err, "tool", inv.ToolName)
		}
	} else {
		e.Guard.MarkFailed(ctx, claim, result.Error)
	}

	if result.SpilledPath != "" {
		e.emit(ctx, EventSpilled, inv, effect, map[string]any{"outcome": result.Outcome, "path": result.SpilledPath})
	}
	name := EventRefused
	payload := map[string]any{
		"outcome": result.Outcome, "elapsed_s": elapsed.Seconds(),
		"truncated": result.Truncated, "redacted": result.Redacted,
	}
	if result.OK() {
		name = EventExecuted
		if result.Outcome == OutcomeExecutedDegraded {
			name = EventExecutedDegraded
		}
	}
	e.emit(ctx, name, inv, effect, payload)
	return result
}

func (e *Executor) prepareArguments(def Definition, inv Invocation) (map[string]any, string) {
	args := make(map[string]any, len(inv.Arguments))
	for k, v := range inv.Arguments {
		if e.KeyField != "" && k == e.KeyField {
			continue // 幂等键是执行层概念，不是工具入参
		}
		args[k] = v
	}
	if len(def.Parameters) == 0 {
		return args, ""
	}
	schema, err := e.compileSchema(def)
	if err != nil {
		return args, fmt.Sprintf("工具 %s 的 schema 本身有问题：%v", inv.ToolName, err)
	}
	if err := schema.Validate(args); err != nil {
		path := "（根）"
		message := err.Error()
		if ve, ok := err.(*jsonschema.ValidationError); ok {
			if ve.InstanceLocation != "" {
				path = ve.InstanceLocation
			}
			message = ve.Message
			for len(ve.Causes) == 1 {
				ve = ve.Causes[0]
				message = ve.Message
				if ve.InstanceLocation != "" {
					path = ve.InstanceLocation
				}
			}
		}
		return args, fmt.Sprintf(
			"参数不符合 %s 的 schema：%s（位置：%s）\n参数校验失败时**不会执行** —— 带着错参数执行会产生更难查的副作用。\n该工具的必填参数：%v",
			inv.ToolName, message, path, def.Parameters["required"])
	}
	return args, ""
}

func (e *Executor) compileSchema(def Definition) (*jsonschema.Schema, error) {
	e.schemaMu.Lock()
	defer e.schemaMu.Unlock()
	if s, ok := e.schemas[def.Name]; ok {
		return s, nil
	}
	raw, err := json.Marshal(def.Parameters)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema://"+def.Name, bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile("schema://" + def.Name)
	if err != nil {
		return nil, err
	}
	e.schemas[def.Name] = schema
	return schema, nil
}

func (e *Executor) refuse(ctx context.Context, inv Invocation, effect SideEffect, reason, kind string) Result {
	e.emit(ctx, EventRefused, inv, effect, map[string]any{"outcome": OutcomeRefused, "reason": kind})
	return Result{Outcome: OutcomeRefused, ToolName: inv.ToolName, Error: reason, ErrorKind: ErrorFatal, Recovery: RecoveryReport}
}

func (e *Executor) emit(ctx context.Context, name string, inv Invocation, effect SideEffect, extra map[string]any) {
	ev := event.New(name)
	ev.TraceID = firstNonEmpty(inv.TraceID, ids.Trace(ctx))
	ev.SessionID = inv.SessionID
	ev.Caller = inv.Caller
	ev.Subject = inv.ToolName
	payload := map[string]any{"tool_name": inv.ToolName, "side_effect": string(effect), "trace_id": ev.TraceID}
	for k, v := range extra {
		payload[k] = v
	}
	if outcome, ok := extra["outcome"].(Outcome); ok {
		ev.Outcome = string(outcome)
	}
	ev.Payload = payload
	e.Events.Emit(ctx, ev)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var _ = logx.New
