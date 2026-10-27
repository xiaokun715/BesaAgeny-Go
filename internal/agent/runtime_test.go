package agent_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/store/memstore"
	"github.com/besa-ai/besa-agent/internal/tool"
)

type fakeChat struct {
	calls []domain.ChatRequest
	reply func(n int, req domain.ChatRequest) domain.GatewayResult
}

func (f *fakeChat) Chat(_ context.Context, _ string, messages []domain.Message, opts gateway.ChatOptions) (domain.GatewayResult, error) {
	f.calls = append(f.calls, domain.ChatRequest{Messages: messages, Tools: opts.Tools})
	return f.reply(len(f.calls), f.calls[len(f.calls)-1]), nil
}

type fakeTools struct{ calls int }

func (f *fakeTools) Invoke(_ context.Context, _ tool.Invocation) tool.Result {
	f.calls++
	return tool.Result{Outcome: tool.OutcomeExecuted, ToolName: "read", Output: "file-content", IdemKey: "idem-1"}
}

type fakeCatalog struct{}

func (fakeCatalog) Select(_, _ []string) []domain.ToolDefinition {
	return []domain.ToolDefinition{{Name: "read", Category: "infra", SideEffect: "read", Parameters: map[string]any{"type": "object"}}}
}

func buildRuntime(chat agent.ChatClient, tools agent.ToolInvoker, checkpoints agent.CheckpointStore, finder agent.ToolFinder, limits agent.Limits, resume agent.ResumePolicy) *agent.Runtime {
	stages, _ := agent.BuildStages(nil, nil)
	router := agent.NewStageRouter(stages, "ask", "requirement", false, 0.5, 2, chat, "runtime.default")
	policy := agent.DefaultPolicy()
	policy.Limits = limits
	policy.Resume = resume
	policy.Context.Enabled = false
	executor := agent.NewAgentExecutor(chat, tools, fakeCatalog{}, "runtime.default", limits.MaxSteps, checkpoints)
	return agent.NewRuntime(executor, stages, router, nil, checkpoints, finder, policy, nil)
}

func noDeadline() agent.Limits {
	return agent.Limits{MaxSteps: 4, MaxToolCalls: 10}
}

func TestRouterExplicitRulesAndAsk(t *testing.T) {
	stages, _ := agent.BuildStages(nil, nil)
	router := agent.NewStageRouter(stages, "ask", "", false, 0.5, 2, nil, "runtime.default")

	decision, err := router.Route(context.Background(), "帮我为登录接口写测试用例", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Stage != "test_case" {
		t.Fatalf("expected test_case, got %+v", decision)
	}
	if decision.Confidence < 0.5 || decision.Reason == "" {
		t.Fatalf("rule routing must carry reason/confidence: %+v", decision)
	}

	explicit, err := router.Route(context.Background(), "任何东西", "analysis", "")
	if err != nil || explicit.Stage != "analysis" || explicit.Confidence != 1 {
		t.Fatalf("explicit stage must win: %+v %v", explicit, err)
	}

	unknown, err := router.Route(context.Background(), "今天天气不错", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !unknown.NeedsInput {
		t.Fatalf("on_unknown=ask 应返回 needs_input，得到 %+v", unknown)
	}
}

func TestRuntimeLoopExecutesToolThenAnswers(t *testing.T) {
	chat := &fakeChat{reply: func(n int, req domain.ChatRequest) domain.GatewayResult {
		if n == 1 {
			return domain.GatewayResult{Response: domain.ChatResponse{Content: "先读文件", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.txt"}}}}}
		}
		return domain.GatewayResult{Response: domain.ChatResponse{Content: "完成"}}
	}}
	tools := &fakeTools{}
	checkpoints := memstore.NewCheckpoints()
	rt := buildRuntime(chat, tools, checkpoints, nil, noDeadline(), agent.ResumePolicy{OnUncertain: "needs_review", Lease: time.Minute})

	out := rt.RunOne(context.Background(), agent.RunRequest{Task: "读文件", Stage: "test_case", SessionID: "s1"})
	if out.Status != agent.StatusDone || out.Answer != "完成" {
		t.Fatalf("expected done, got %+v", out)
	}
	if tools.calls != 1 || out.ToolCalls != 1 {
		t.Fatalf("expected one tool call, got %d/%d", tools.calls, out.ToolCalls)
	}
	cp, _ := checkpoints.Latest(context.Background(), out.RunID)
	if cp == nil || cp.Status != agent.StatusDone {
		t.Fatalf("terminal checkpoint missing: %+v", cp)
	}
}

func TestRuntimeMaxStepsFails(t *testing.T) {
	chat := &fakeChat{reply: func(int, domain.ChatRequest) domain.GatewayResult {
		return domain.GatewayResult{Response: domain.ChatResponse{ToolCalls: []domain.ToolCall{{ID: "c", Name: "read", Arguments: map[string]any{}}}}}
	}}
	limits := agent.Limits{MaxSteps: 2, MaxToolCalls: 10}
	rt := buildRuntime(chat, &fakeTools{}, memstore.NewCheckpoints(), nil, limits, agent.ResumePolicy{OnUncertain: "needs_review", Lease: time.Minute})
	out := rt.RunOne(context.Background(), agent.RunRequest{Task: "x", Stage: "test_case"})
	if out.Status != agent.StatusFailed || !strings.Contains(out.Error, "步数上界") {
		t.Fatalf("expected step limit failure, got %+v", out)
	}
}

type uncertainFinder struct{}

func (uncertainFinder) Find(context.Context, string, string, time.Duration) (*agent.ToolExecutionView, error) {
	return &agent.ToolExecutionView{ToolName: "write", State: "uncertain", Uncertain: true}, nil
}

func TestResumeWithUncertainNeedsReview(t *testing.T) {
	checkpoints := memstore.NewCheckpoints()
	run := agent.NewTaskRun("run-x", "test_case", "goal", "s1", "")
	run.Record("test_case", "调用 write", []string{"write"}, []string{"idem-1"})
	if err := checkpoints.Save(context.Background(), agent.Checkpoint{
		RunID: "run-x", FormatVersion: 1, StepIndex: 1, NodeID: "test_case", Status: agent.StatusRunning,
		State: run.ToState(), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	chat := &fakeChat{reply: func(int, domain.ChatRequest) domain.GatewayResult {
		return domain.GatewayResult{Response: domain.ChatResponse{Content: "不应执行"}}
	}}
	rt := buildRuntime(chat, &fakeTools{}, checkpoints, uncertainFinder{}, noDeadline(), agent.ResumePolicy{OnUncertain: "needs_review", Lease: time.Minute})
	out := rt.RunOne(context.Background(), agent.RunRequest{Task: "goal", Stage: "test_case", RunID: "run-x", Resume: true})
	if out.Status != agent.StatusNeedsReview {
		t.Fatalf("expected needs_review, got %+v", out)
	}
	if len(chat.calls) != 0 {
		t.Fatal("needs_review 判定后不得调用模型")
	}
}

func TestResumeAbortPolicy(t *testing.T) {
	checkpoints := memstore.NewCheckpoints()
	run := agent.NewTaskRun("run-y", "test_case", "goal", "", "")
	run.Record("test_case", "调用 write", []string{"write"}, []string{"idem-1"})
	_ = checkpoints.Save(context.Background(), agent.Checkpoint{
		RunID: "run-y", FormatVersion: 1, StepIndex: 1, NodeID: "test_case", Status: agent.StatusRunning,
		State: run.ToState(), CreatedAt: time.Now(),
	})
	rt := buildRuntime(&fakeChat{reply: func(int, domain.ChatRequest) domain.GatewayResult {
		return domain.GatewayResult{Response: domain.ChatResponse{Content: "x"}}
	}}, &fakeTools{}, checkpoints, uncertainFinder{}, noDeadline(), agent.ResumePolicy{OnUncertain: "abort", Lease: time.Minute})
	out := rt.RunOne(context.Background(), agent.RunRequest{Task: "goal", Stage: "test_case", RunID: "run-y", Resume: true})
	if out.Status != agent.StatusFailed {
		t.Fatalf("abort 策略应失败，得到 %+v", out)
	}
}
