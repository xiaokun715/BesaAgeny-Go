package transport

import (
	"context"
	"time"

	"google.golang.org/grpc"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/agent"
	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/experience"
	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/memory"
	"github.com/besa-ai/besa-agent/internal/multiagent"
	"github.com/besa-ai/besa-agent/internal/strategy"
	"github.com/besa-ai/besa-agent/internal/tool"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

// RunRequestJSON 是控制面 REST 的同步运行请求。
type RunRequestJSON struct {
	Task      string `json:"task"`
	Stage     string `json:"stage"`
	Hint      string `json:"hint"`
	SessionID string `json:"session_id"`
	Caller    string `json:"caller"`
	RunID     string `json:"run_id"`
	Resume    bool   `json:"resume"`
}

func timeDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// ------------------------------ Gateway ------------------------------

type GatewayClient struct {
	Client     besav1.GatewayServiceClient
	ChatAlias  string
	EmbedAlias string
}

func NewGatewayClient(conn *grpc.ClientConn) *GatewayClient {
	return &GatewayClient{Client: besav1.NewGatewayServiceClient(conn)}
}

func (g *GatewayClient) Chat(ctx context.Context, alias string, messages []domain.Message, opts gateway.ChatOptions) (domain.GatewayResult, error) {
	resp, err := g.Client.Chat(ctx, &besav1.ChatRequest{Alias: alias, Messages: MessagesToProto(messages), Options: ChatOptionsToProto(opts)})
	if err != nil {
		return domain.GatewayResult{}, err
	}
	return GatewayResultFromProto(resp), nil
}

func (g *GatewayClient) Embed(ctx context.Context, texts []string, traceID string) ([][]float32, error) {
	alias := g.EmbedAlias
	if alias == "" {
		alias = "emb.default"
	}
	resp, err := g.Client.Embed(ctx, &besav1.EmbedRequest{Alias: alias, Texts: texts, Options: &besav1.ChatOptions{TraceId: traceID}})
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(resp.Vectors))
	for i, v := range resp.Vectors {
		out[i] = v.Values
	}
	return out, nil
}

// EmbedWithAlias 供 memory/strategy 使用指定逻辑名。
func (g *GatewayClient) EmbedWithAlias(ctx context.Context, alias string, texts []string, traceID string) ([]domain.EmbeddingResult, error) {
	resp, err := g.Client.Embed(ctx, &besav1.EmbedRequest{Alias: alias, Texts: texts, Options: &besav1.ChatOptions{TraceId: traceID}})
	if err != nil {
		return nil, err
	}
	out := make([]domain.EmbeddingResult, len(resp.Vectors))
	for i, v := range resp.Vectors {
		out[i] = domain.EmbeddingResult{Vectors: [][]float32{v.Values}, Dimension: len(v.Values), Model: resp.Model}
	}
	return out, nil
}

func (g *GatewayClient) Complete(ctx context.Context, system, prompt string, schema map[string]any, maxTokens int) (string, error) {
	opts := gateway.ChatOptions{MaxTokens: &maxTokens}
	if schema != nil {
		opts.ResponseFormat = "json"
		opts.JSONSchema = schema
	}
	alias := g.ChatAlias
	if alias == "" {
		alias = "runtime.default"
	}
	result, err := g.Chat(ctx, alias, []domain.Message{domain.System(system), domain.User(prompt)}, opts)
	if err != nil {
		return "", err
	}
	return result.Content(), nil
}

// ------------------------------ Tool ------------------------------

type ToolClient struct {
	Client besav1.ToolServiceClient
}

func NewToolClient(conn *grpc.ClientConn) *ToolClient {
	return &ToolClient{Client: besav1.NewToolServiceClient(conn)}
}

func (t *ToolClient) Invoke(ctx context.Context, inv tool.Invocation) tool.Result {
	resp, err := t.Client.Invoke(ctx, InvocationToProto(inv))
	if err != nil {
		return tool.Result{Outcome: tool.OutcomeFailed, ToolName: inv.ToolName, Error: "工具服务调用失败：" + err.Error(), ErrorKind: tool.ErrorTransient, Recovery: tool.RecoveryReport}
	}
	return ToolResultFromProto(resp)
}

// ------------------------------ Agent ------------------------------

type AgentClient struct {
	Client besav1.AgentServiceClient
}

func NewAgentClient(conn *grpc.ClientConn) *AgentClient {
	return &AgentClient{Client: besav1.NewAgentServiceClient(conn)}
}

func (a *AgentClient) RunOne(ctx context.Context, req agent.RunRequest) agent.RunOutcome {
	resp, err := a.Client.RunOne(ctx, RunRequestToProto(req))
	if err != nil {
		return agent.RunOutcome{RunID: req.RunID, Status: agent.StatusFailed, Error: "agent 服务调用失败：" + err.Error()}
	}
	return RunOutcomeFromProto(resp)
}

func (a *AgentClient) Cancel(ctx context.Context, runID string) error {
	_, err := a.Client.Cancel(ctx, &besav1.CancelRequest{RunId: runID})
	return err
}

// NodeRunner 把 workflow 节点映射到一次 agent 运行。
type NodeRunner struct {
	Client    *AgentClient
	Stage     string
	SessionID string
	Caller    string
}

func (n NodeRunner) RunNode(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	task := workflow.TaskFromInputs(req.Inputs, req.Node.ID)
	out := n.Client.RunOne(ctx, agent.RunRequest{Task: task, Stage: n.Stage, SessionID: n.SessionID, Caller: n.Caller})
	result := workflow.NodeResult{
		Status: workflow.StateSucceeded, Outputs: map[string]any{"answer": out.Answer},
		Elapsed: out.Elapsed, NeedsReview: out.NeedsReview(),
	}
	if out.Cost != nil {
		c, _ := out.Cost.Float64()
		result.Cost = &c
	}
	switch out.Status {
	case agent.StatusDone:
		result.Status = workflow.StateSucceeded
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

// ------------------------------ Memory ------------------------------

type MemoryClient struct {
	Client besav1.MemoryServiceClient
}

func NewMemoryClient(conn *grpc.ClientConn) *MemoryClient {
	return &MemoryClient{Client: besav1.NewMemoryServiceClient(conn)}
}

func (m *MemoryClient) Remember(ctx context.Context, turns []ctxpkg.Turn, source string, mc memory.Ctx) ([]memory.Item, error) {
	req := &besav1.MemoryRememberRequest{Source: source, Context: MemoryContextToProto(mc)}
	for _, t := range turns {
		req.Turns = append(req.Turns, &besav1.MemoryTurn{Role: t.Role, Content: t.Content})
	}
	resp, err := m.Client.Remember(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]memory.Item, 0, len(resp.Items))
	for _, item := range resp.Items {
		out = append(out, memory.Item{ID: item.Id, Content: item.Content, Kind: memory.Kind(item.Kind), Scope: memory.Scope(item.Scope), Weight: item.Weight})
	}
	return out, nil
}

func (m *MemoryClient) Recall(ctx context.Context, query string, topK int, kinds []string, sessionID string) ([]ctxpkg.MemoryItem, error) {
	resp, err := m.Client.Recall(ctx, &besav1.MemoryRecallRequest{
		Query: query, Kinds: kinds, TopK: int32(topK),
		Context: &besav1.MemoryContext{Scope: string(memory.ScopeSession), SessionId: sessionID},
	})
	if err != nil {
		return nil, err
	}
	out := make([]ctxpkg.MemoryItem, 0, len(resp.Items))
	for _, item := range resp.Items {
		out = append(out, ctxpkg.MemoryItem{Content: item.Content, Score: item.Score, Kind: item.Kind})
	}
	return out, nil
}

func (m *MemoryClient) Settle(ctx context.Context, turns []ctxpkg.Turn, traceID, sessionID string) error {
	req := &besav1.MemoryRememberRequest{Source: "conversation", Context: &besav1.MemoryContext{
		Scope: string(memory.ScopeSession), SessionId: sessionID, TraceId: traceID,
	}}
	for _, t := range turns {
		req.Turns = append(req.Turns, &besav1.MemoryTurn{Role: t.Role, Content: t.Content})
	}
	_, err := m.Client.Remember(ctx, req)
	return err
}

func (m *MemoryClient) Maintain(ctx context.Context, tenant string) (int32, int32, error) {
	resp, err := m.Client.Maintain(ctx, &besav1.MemoryMaintainRequest{TenantId: tenant})
	if err != nil {
		return 0, 0, err
	}
	return resp.Scanned, resp.Tombstoned, nil
}

// ------------------------------ Strategy ------------------------------

type StrategyClient struct {
	Client besav1.StrategyServiceClient
}

func NewStrategyClient(conn *grpc.ClientConn) *StrategyClient {
	return &StrategyClient{Client: besav1.NewStrategyServiceClient(conn)}
}

func (s *StrategyClient) Select(ctx context.Context, features strategy.TaskFeatures) (strategy.SelectionDecision, error) {
	resp, err := s.Client.Select(ctx, TaskFeaturesToProto(features))
	if err != nil {
		return strategy.SelectionDecision{}, err
	}
	decision := strategy.SelectionDecision{
		Layer: strategy.Layer(resp.Layer), Reason: resp.Reason, Degraded: resp.Degraded, Refused: resp.Refused,
	}
	if resp.HasProfile {
		bindings := map[strategy.ComponentKind]string{}
		for k, v := range resp.Bindings {
			bindings[strategy.ComponentKind(k)] = v
		}
		decision.Profile = &strategy.AgentProfile{Name: resp.ProfileName, Bindings: bindings, Versions: resp.Versions}
	}
	for _, v := range resp.Violations {
		decision.Violations = append(decision.Violations, strategy.Violation{Rule: v.Rule, Detail: v.Detail, Suggestion: v.Suggestion})
	}
	return decision, nil
}

// ------------------------------ Experience ------------------------------

type ExperienceClient struct {
	Client besav1.ExperienceServiceClient
}

func NewExperienceClient(conn *grpc.ClientConn) *ExperienceClient {
	return &ExperienceClient{Client: besav1.NewExperienceServiceClient(conn)}
}

func (e *ExperienceClient) OnFailure(ctx context.Context, f experience.Failure) (experience.RecoveryDecision, error) {
	resp, err := e.Client.OnFailure(ctx, FailureToProto(f))
	if err != nil {
		return experience.RecoveryDecision{}, err
	}
	return experience.RecoveryDecision{Action: experience.RecoveryAction(resp.Action), Reason: resp.Reason, MissingInputs: resp.MissingInputs}, nil
}

func (e *ExperienceClient) Recall(ctx context.Context, q experience.Query) (experience.Retrieved, error) {
	resp, err := e.Client.Recall(ctx, &besav1.ExperienceRecallRequest{
		TenantId: q.TenantID, Stage: q.Stage, ErrorType: q.ErrorType, Scenario: q.Scenario,
		Environment: q.Environment, TopK: int32(q.TopK),
	})
	if err != nil {
		return experience.Retrieved{}, err
	}
	var out experience.Retrieved
	for _, item := range resp.Experiences {
		exp := experience.Experience{ID: item.Id, Category: item.Category, Scenario: item.Scenario,
			Symptom: item.Symptom, Conditions: item.Conditions, RootCause: item.RootCause,
			RecoveryStrategy: item.RecoveryStrategy, Scope: item.Scope,
			Validation: experience.ValidationStats{SuccessRate: item.SuccessRate, Samples: int(item.Samples)},
			State:      experience.ExperienceState(item.State)}
		out.Experiences = append(out.Experiences, exp)
	}
	out.Conflicts = resp.Conflicts
	return out, nil
}

// ------------------------------ Orchestrator / Workflow ------------------------------

type OrchestratorClient struct {
	Client besav1.OrchestratorServiceClient
}

func NewOrchestratorClient(conn *grpc.ClientConn) *OrchestratorClient {
	return &OrchestratorClient{Client: besav1.NewOrchestratorServiceClient(conn)}
}

func (o *OrchestratorClient) Run(ctx context.Context, req multiagent.Request) (multiagent.Outcome, error) {
	resp, err := o.Client.Run(ctx, &besav1.OrchestrateRequest{Task: req.Task, Strategy: req.Strategy,
		Participants: req.Participants, Rounds: int32(req.Rounds), SessionId: req.SessionID,
		Caller: req.Caller, Risk: req.Risk})
	if err != nil {
		return multiagent.Outcome{}, err
	}
	out := multiagent.Outcome{Strategy: resp.Strategy, Answer: resp.Answer, Consensus: resp.Consensus, Reason: resp.Reason}
	for _, r := range resp.Runs {
		out.Runs = append(out.Runs, multiagent.ParticipantRun{Name: r.Name, Stage: r.Stage,
			Status: agent.RunStatus(r.Status), Answer: r.Answer, Error: r.Error, Cost: r.Cost, NeedsReview: r.NeedsReview})
	}
	return out, nil
}

type WorkflowClient struct {
	Client besav1.WorkflowServiceClient
}

func NewWorkflowClient(conn *grpc.ClientConn) *WorkflowClient {
	return &WorkflowClient{Client: besav1.NewWorkflowServiceClient(conn)}
}

func (w *WorkflowClient) Validate(ctx context.Context, def workflow.Workflow) ([]workflow.GraphIssue, error) {
	resp, err := w.Client.Validate(ctx, WorkflowToProto(def))
	if err != nil {
		return nil, err
	}
	var out []workflow.GraphIssue
	for _, issue := range resp.Issues {
		out = append(out, workflow.GraphIssue{Kind: workflow.IssueKind(issue.Kind), NodeID: issue.NodeId, Detail: issue.Detail, Suggestion: issue.Suggestion})
	}
	return out, nil
}

func (w *WorkflowClient) Run(ctx context.Context, def workflow.Workflow, input map[string]any) (workflow.RunReport, error) {
	resp, err := w.Client.Run(ctx, &besav1.WorkflowRunRequest{Workflow: WorkflowToProto(def), Input: MapToStruct(input)})
	if err != nil {
		return workflow.RunReport{}, err
	}
	report := workflow.RunReport{WorkflowRunID: resp.WorkflowRunId, Status: workflow.NodeState(resp.Status), Error: resp.Error, Nodes: map[string]workflow.NodeResult{}}
	for _, n := range resp.Nodes {
		outputs := StructToMap(n.Outputs)
		report.Nodes[n.NodeId] = workflow.NodeResult{Status: workflow.NodeState(n.Status), Error: n.Error, Elapsed: timeDuration(n.ElapsedS), NeedsReview: n.NeedsReview, Outputs: outputs}
	}
	return report, nil
}
