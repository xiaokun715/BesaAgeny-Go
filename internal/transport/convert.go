// Package transport 负责领域类型 ↔ protobuf 的转换与 gRPC 客户端。
package transport

import (
	"github.com/shopspring/decimal"
	"google.golang.org/protobuf/types/known/structpb"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/experience"
	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/memory"
	"github.com/besa-ai/besa-agent/internal/strategy"
	"github.com/besa-ai/besa-agent/internal/tool"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

func MapToStruct(m map[string]any) *structpb.Struct {
	if m == nil {
		return nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

func StructToMap(s *structpb.Struct) map[string]any {
	if s == nil {
		return nil
	}
	return s.AsMap()
}

// ------------------------------ 消息 ------------------------------

func MessageToProto(m domain.Message) *besav1.Message {
	out := &besav1.Message{
		Role: string(m.Role), Content: m.Content, Name: m.Name, ToolCallId: m.ToolCallID,
	}
	for _, p := range m.Parts {
		out.Parts = append(out.Parts, &besav1.ContentPart{
			Text: p.Text, ImageUrl: p.ImageURL, ImageData: p.ImageData, MediaType: p.MediaType, Detail: p.Detail,
		})
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCallToProto(tc))
	}
	return out
}

func MessageFromProto(m *besav1.Message) domain.Message {
	if m == nil {
		return domain.Message{}
	}
	out := domain.Message{Role: domain.Role(m.Role), Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallId}
	for _, p := range m.Parts {
		out.Parts = append(out.Parts, domain.ContentPart{
			Text: p.Text, ImageURL: p.ImageUrl, ImageData: p.ImageData, MediaType: p.MediaType, Detail: p.Detail,
		})
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCallFromProto(tc))
	}
	return out
}

func MessagesToProto(msgs []domain.Message) []*besav1.Message {
	out := make([]*besav1.Message, len(msgs))
	for i, m := range msgs {
		out[i] = MessageToProto(m)
	}
	return out
}

func MessagesFromProto(msgs []*besav1.Message) []domain.Message {
	out := make([]domain.Message, len(msgs))
	for i, m := range msgs {
		out[i] = MessageFromProto(m)
	}
	return out
}

func ToolCallToProto(tc domain.ToolCall) *besav1.ToolCall {
	return &besav1.ToolCall{Id: tc.ID, Name: tc.Name, ArgumentsRaw: tc.ArgumentsRaw, Arguments: MapToStruct(tc.Arguments)}
}

func ToolCallFromProto(tc *besav1.ToolCall) domain.ToolCall {
	return domain.ToolCall{ID: tc.Id, Name: tc.Name, ArgumentsRaw: tc.ArgumentsRaw, Arguments: StructToMap(tc.Arguments)}
}

func ToolSpecToProto(spec domain.ToolSpec) *besav1.ToolSpec {
	return &besav1.ToolSpec{Name: spec.Name, Description: spec.Description, Parameters: MapToStruct(spec.Parameters)}
}

func ToolSpecFromProto(spec *besav1.ToolSpec) domain.ToolSpec {
	return domain.ToolSpec{Name: spec.Name, Description: spec.Description, Parameters: StructToMap(spec.Parameters)}
}

func UsageToProto(u domain.Usage) *besav1.Usage {
	out := &besav1.Usage{}
	if u.InputTokens != nil {
		v := int32(*u.InputTokens)
		out.InputTokens = &v
	}
	if u.OutputTokens != nil {
		v := int32(*u.OutputTokens)
		out.OutputTokens = &v
	}
	if u.CachedInputTokens != nil {
		v := int32(*u.CachedInputTokens)
		out.CachedInputTokens = &v
	}
	return out
}

func UsageFromProto(u *besav1.Usage) domain.Usage {
	if u == nil {
		return domain.Usage{}
	}
	var out domain.Usage
	if u.InputTokens != nil {
		v := int(*u.InputTokens)
		out.InputTokens = &v
	}
	if u.OutputTokens != nil {
		v := int(*u.OutputTokens)
		out.OutputTokens = &v
	}
	if u.CachedInputTokens != nil {
		v := int(*u.CachedInputTokens)
		out.CachedInputTokens = &v
	}
	return out
}

func CostToProto(c domain.Cost) *besav1.Cost {
	out := &besav1.Cost{Currency: c.Currency}
	if c.Amount != nil {
		out.Amount = c.Amount.String()
		out.AmountKnown = true
	}
	return out
}

func CostFromProto(c *besav1.Cost) domain.Cost {
	if c == nil {
		return domain.Cost{}
	}
	out := domain.Cost{Currency: c.Currency}
	if c.AmountKnown && c.Amount != "" {
		if d, err := decimal.NewFromString(c.Amount); err == nil {
			out.Amount = &d
		}
	}
	return out
}

func ChatOptionsToProto(o gateway.ChatOptions) *besav1.ChatOptions {
	out := &besav1.ChatOptions{
		TraceId: o.TraceID, SessionId: o.SessionID, Caller: o.Caller,
		DeadlineS: o.Deadline.Seconds(), ToolChoice: o.ToolChoice,
		ResponseFormat: o.ResponseFormat, JsonSchema: MapToStruct(o.JSONSchema), Stop: o.Stop,
	}
	if o.Temperature != nil {
		v := *o.Temperature
		out.Temperature = &v
	}
	if o.MaxTokens != nil {
		v := int32(*o.MaxTokens)
		out.MaxTokens = &v
	}
	for _, spec := range o.Tools {
		out.Tools = append(out.Tools, ToolSpecToProto(spec))
	}
	return out
}

func ChatOptionsFromProto(o *besav1.ChatOptions) gateway.ChatOptions {
	if o == nil {
		return gateway.ChatOptions{}
	}
	out := gateway.ChatOptions{
		TraceID: o.TraceId, SessionID: o.SessionId, Caller: o.Caller,
		Deadline: timeDuration(o.DeadlineS), ToolChoice: o.ToolChoice,
		ResponseFormat: o.ResponseFormat, JSONSchema: StructToMap(o.JsonSchema), Stop: o.Stop,
	}
	if o.Temperature != nil {
		v := *o.Temperature
		out.Temperature = &v
	}
	if o.MaxTokens != nil {
		v := int(*o.MaxTokens)
		out.MaxTokens = &v
	}
	for _, spec := range o.Tools {
		out.Tools = append(out.Tools, ToolSpecFromProto(spec))
	}
	return out
}

func GatewayResultToProto(r domain.GatewayResult) *besav1.GatewayResult {
	out := &besav1.GatewayResult{
		Alias: r.Alias, ModelKey: r.ModelKey, Degraded: r.Degraded,
		Usage: UsageToProto(r.Usage), Cost: CostToProto(r.Cost), TraceId: r.TraceID,
		Content: r.Response.Content, Model: r.Response.Model, FinishReason: r.Response.FinishReason,
		Reasoning: r.Response.Reasoning, StructuredNative: r.Response.StructuredNative,
	}
	for _, a := range r.Attempts {
		out.Attempts = append(out.Attempts, &besav1.AttemptRecord{
			ModelKey: a.ModelKey, Provider: a.Provider, Outcome: a.Outcome, Error: a.Error,
			Retryable: a.Retryable, SkippedReason: a.SkippedReason, ElapsedS: a.ElapsedS,
		})
	}
	for _, tc := range r.Response.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCallToProto(tc))
	}
	return out
}

func GatewayResultFromProto(p *besav1.GatewayResult) domain.GatewayResult {
	if p == nil {
		return domain.GatewayResult{}
	}
	out := domain.GatewayResult{
		Alias: p.Alias, ModelKey: p.ModelKey, Degraded: p.Degraded,
		Usage: UsageFromProto(p.Usage), Cost: CostFromProto(p.Cost), TraceID: p.TraceId,
		Response: domain.ChatResponse{
			Content: p.Content, Model: p.Model, FinishReason: p.FinishReason,
			Reasoning: p.Reasoning, StructuredNative: p.StructuredNative, TraceID: p.TraceId,
			Usage: UsageFromProto(p.Usage),
		},
	}
	for _, a := range p.Attempts {
		out.Attempts = append(out.Attempts, domain.AttemptRecord{
			ModelKey: a.ModelKey, Provider: a.Provider, Outcome: a.Outcome, Error: a.Error,
			Retryable: a.Retryable, SkippedReason: a.SkippedReason, ElapsedS: a.ElapsedS,
		})
	}
	for _, tc := range p.ToolCalls {
		out.Response.ToolCalls = append(out.Response.ToolCalls, ToolCallFromProto(tc))
	}
	return out
}

// ------------------------------ 工具 ------------------------------

func InvocationToProto(inv tool.Invocation) *besav1.ToolInvocation {
	return &besav1.ToolInvocation{
		ToolName: inv.ToolName, Arguments: MapToStruct(inv.Arguments), Scope: inv.Scope,
		IdempotencyKey: inv.IdempotencyKey, TraceId: inv.TraceID, SessionId: inv.SessionID, Caller: inv.Caller,
	}
}

func InvocationFromProto(p *besav1.ToolInvocation) tool.Invocation {
	return tool.Invocation{
		ToolName: p.ToolName, Arguments: StructToMap(p.Arguments), Scope: p.Scope,
		IdempotencyKey: p.IdempotencyKey, TraceID: p.TraceId, SessionID: p.SessionId, Caller: p.Caller,
	}
}

func ToolResultToProto(r tool.Result) *besav1.ToolResultMsg {
	return &besav1.ToolResultMsg{
		Outcome: string(r.Outcome), ToolName: r.ToolName, Output: r.Output, Error: r.Error,
		Truncated: r.Truncated, ElapsedS: r.Elapsed.Seconds(), IdemKey: r.IdemKey,
		SpilledPath: r.SpilledPath, Redacted: r.Redacted, ErrorKind: string(r.ErrorKind),
		Recovery: string(r.Recovery), Hint: r.Hint,
	}
}

func ToolResultFromProto(p *besav1.ToolResultMsg) tool.Result {
	return tool.Result{
		Outcome: tool.Outcome(p.Outcome), ToolName: p.ToolName, Output: p.Output, Error: p.Error,
		Truncated: p.Truncated, Elapsed: timeDuration(p.ElapsedS), IdemKey: p.IdemKey,
		SpilledPath: p.SpilledPath, Redacted: p.Redacted, ErrorKind: tool.ErrorKind(p.ErrorKind),
		Recovery: tool.RecoveryAction(p.Recovery), Hint: p.Hint,
	}
}

func ToolDefinitionToProto(d tool.Definition) *besav1.ToolDefinitionMsg {
	return &besav1.ToolDefinitionMsg{Name: d.Name, Description: d.Description,
		Parameters: MapToStruct(d.Parameters), SideEffect: d.SideEffect, Category: d.Category}
}

// ------------------------------ Agent ------------------------------

func RunRequestToProto(r agent.RunRequest) *besav1.RunRequest {
	out := &besav1.RunRequest{
		Task: r.Task, Stage: r.Stage, Hint: r.Hint, SessionId: r.SessionID,
		Caller: r.Caller, RunId: r.RunID, Resume: r.Resume,
	}
	for _, m := range r.History {
		out.History = append(out.History, MessageToProto(domain.Message{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}))
	}
	return out
}

func RunRequestFromProto(p *besav1.RunRequest) agent.RunRequest {
	out := agent.RunRequest{Task: p.Task, Stage: p.Stage, Hint: p.Hint, SessionID: p.SessionId,
		Caller: p.Caller, RunID: p.RunId, Resume: p.Resume}
	for _, m := range p.History {
		out.History = append(out.History, context.FromDomain(MessageFromProto(m)))
	}
	return out
}

func RunOutcomeToProto(o agent.RunOutcome) *besav1.RunOutcomeMsg {
	out := &besav1.RunOutcomeMsg{
		RunId: o.RunID, Stage: o.Stage, Status: string(o.Status), Answer: o.Answer,
		Steps: int32(o.Steps), ToolCalls: int32(o.ToolCalls), ElapsedS: o.Elapsed.Seconds(),
		Error: o.Error, Reason: o.Reason,
	}
	if o.Cost != nil {
		out.Cost = o.Cost.String()
		out.CostKnown = true
	}
	if o.Routing != nil {
		out.Routing = &besav1.RoutingDecisionMsg{
			Stage: o.Routing.Stage, Reason: o.Routing.Reason, Confidence: o.Routing.Confidence,
			Candidates: o.Routing.Candidates, NeedsInput: o.Routing.NeedsInput,
		}
	}
	return out
}

func RunOutcomeFromProto(p *besav1.RunOutcomeMsg) agent.RunOutcome {
	out := agent.RunOutcome{
		RunID: p.RunId, Stage: p.Stage, Status: agent.RunStatus(p.Status), Answer: p.Answer,
		Steps: int(p.Steps), ToolCalls: int(p.ToolCalls), Elapsed: timeDuration(p.ElapsedS),
		Error: p.Error, Reason: p.Reason,
	}
	if p.CostKnown && p.Cost != "" {
		if d, err := decimal.NewFromString(p.Cost); err == nil {
			out.Cost = &d
		}
	}
	if p.Routing != nil {
		out.Routing = &agent.RoutingDecision{Stage: p.Routing.Stage, Reason: p.Routing.Reason,
			Confidence: p.Routing.Confidence, Candidates: p.Routing.Candidates, NeedsInput: p.Routing.NeedsInput}
	}
	return out
}

// ------------------------------ Memory / Strategy / Experience ------------------------------

func MemoryContextToProto(c memory.Ctx) *besav1.MemoryContext {
	return &besav1.MemoryContext{TenantId: c.TenantID, Scope: string(c.Scope), OwnerId: c.OwnerID,
		SessionId: c.SessionID, Caller: c.Caller, TraceId: c.TraceID}
}

func MemoryContextFromProto(p *besav1.MemoryContext) memory.Ctx {
	if p == nil {
		return memory.Ctx{}
	}
	return memory.Ctx{TenantID: p.TenantId, Scope: memory.Scope(p.Scope), OwnerID: p.OwnerId,
		SessionID: p.SessionId, Caller: p.Caller, TraceID: p.TraceId}
}

func MemoryItemToProto(s memory.Scored) *besav1.MemoryItemMsg {
	return &besav1.MemoryItemMsg{Id: s.Item.ID, Content: s.Item.Content, Kind: string(s.Item.Kind),
		Scope: string(s.Item.Scope), Score: s.Score, Weight: s.Item.Weight, MatchReason: s.MatchReason}
}

func TaskFeaturesToProto(f strategy.TaskFeatures) *besav1.TaskFeaturesMsg {
	out := &besav1.TaskFeaturesMsg{TaskType: f.TaskType, Risk: string(f.Risk), Environment: f.Environment,
		InputAssets: f.InputAssets, Template: f.Template}
	if f.BudgetTokens != nil {
		v := int32(*f.BudgetTokens)
		out.BudgetTokens = &v
	}
	if f.BudgetLatencyMS != nil {
		v := int32(*f.BudgetLatencyMS)
		out.BudgetLatencyMs = &v
	}
	if f.BudgetCost != nil {
		v := *f.BudgetCost
		out.BudgetCost = &v
	}
	return out
}

func TaskFeaturesFromProto(p *besav1.TaskFeaturesMsg) strategy.TaskFeatures {
	out := strategy.TaskFeatures{TaskType: p.TaskType, Risk: strategy.RiskLevel(p.Risk),
		Environment: p.Environment, InputAssets: p.InputAssets, Template: p.Template}
	if p.BudgetTokens != nil {
		v := int(*p.BudgetTokens)
		out.BudgetTokens = &v
	}
	if p.BudgetLatencyMs != nil {
		v := int(*p.BudgetLatencyMs)
		out.BudgetLatencyMS = &v
	}
	if p.BudgetCost != nil {
		v := *p.BudgetCost
		out.BudgetCost = &v
	}
	return out
}

func SelectionDecisionToProto(d strategy.SelectionDecision) *besav1.SelectionDecisionMsg {
	out := &besav1.SelectionDecisionMsg{Layer: string(d.Layer), Reason: d.Reason, Degraded: d.Degraded, Refused: d.Refused}
	if d.Profile != nil {
		out.HasProfile = true
		out.ProfileName = d.Profile.Name
		out.ProfileKey = d.Profile.ProfileKey()
		out.Bindings = map[string]string{}
		for k, v := range d.Profile.Bindings {
			out.Bindings[string(k)] = v
		}
		out.Versions = d.Profile.Versions
	}
	for _, v := range d.Violations {
		out.Violations = append(out.Violations, &besav1.ViolationMsg{Rule: v.Rule, Detail: v.Detail, Suggestion: v.Suggestion})
	}
	return out
}

func EvidenceToProto(e experience.Evidence) *besav1.EvidenceMsg {
	return &besav1.EvidenceMsg{Kind: e.Kind, Source: e.Source, Detail: e.Detail, Weight: e.Weight}
}

func EvidenceFromProto(e *besav1.EvidenceMsg) experience.Evidence {
	return experience.Evidence{Kind: e.Kind, Source: e.Source, Detail: e.Detail, Weight: e.Weight}
}

func FailureToProto(f experience.Failure) *besav1.FailureMsg {
	out := &besav1.FailureMsg{Stage: f.Stage, Status: f.Status, ErrorType: f.ErrorType, Message: f.Message,
		RecoveryAction: f.RecoveryAction, TraceId: f.TraceID, RunId: f.RunID, NodeId: f.NodeID, Risk: f.Risk}
	out.Retryable = f.Retryable
	for _, e := range f.Evidence {
		out.Evidence = append(out.Evidence, EvidenceToProto(e))
	}
	return out
}

func FailureFromProto(p *besav1.FailureMsg) experience.Failure {
	out := experience.Failure{Stage: p.Stage, Status: p.Status, ErrorType: p.ErrorType, Message: p.Message,
		RecoveryAction: p.RecoveryAction, TraceID: p.TraceId, RunID: p.RunId, NodeID: p.NodeId, Risk: p.Risk,
		Retryable: p.Retryable}
	for _, e := range p.Evidence {
		out.Evidence = append(out.Evidence, EvidenceFromProto(e))
	}
	return out
}

func ExperienceToProto(exp experience.Experience) *besav1.ExperienceMsg {
	out := &besav1.ExperienceMsg{Id: exp.ID, Category: exp.Category, Scenario: exp.Scenario, Symptom: exp.Symptom,
		Conditions: exp.Conditions, RootCause: exp.RootCause, RecoveryStrategy: exp.RecoveryStrategy,
		Scope: exp.Scope, SuccessRate: exp.Validation.SuccessRate, Samples: int32(exp.Validation.Samples),
		State: string(exp.State)}
	for _, e := range exp.Evidence {
		out.Evidence = append(out.Evidence, EvidenceToProto(e))
	}
	return out
}

func WorkflowToProto(w workflow.Workflow) *besav1.WorkflowDef {
	out := &besav1.WorkflowDef{WorkflowId: w.WorkflowID, Version: w.Version, Origin: w.Origin}
	for _, n := range w.Nodes {
		out.Nodes = append(out.Nodes, &besav1.NodeDef{Id: n.ID, Agent: n.Agent, Inputs: n.Inputs, Optional: n.Optional, Reason: n.Reason})
	}
	for _, e := range w.Edges {
		out.Edges = append(out.Edges, &besav1.EdgeDef{Src: e.Src, Dst: e.Dst, Kind: string(e.Kind), Field: e.Field})
	}
	return out
}

func WorkflowFromProto(p *besav1.WorkflowDef) workflow.Workflow {
	w := workflow.Workflow{WorkflowID: p.WorkflowId, Version: p.Version, Origin: p.Origin}
	for _, n := range p.Nodes {
		inputs := n.Inputs
		if inputs == nil {
			inputs = map[string]string{}
		}
		w.Nodes = append(w.Nodes, workflow.Node{ID: n.Id, Agent: n.Agent, Inputs: inputs, Optional: n.Optional, Reason: n.Reason})
	}
	for _, e := range p.Edges {
		kind := workflow.EdgeData
		if workflow.EdgeKind(e.Kind) == workflow.EdgeResource {
			kind = workflow.EdgeResource
		}
		w.Edges = append(w.Edges, workflow.Edge{Src: e.Src, Dst: e.Dst, Kind: kind, Field: e.Field})
	}
	return w
}
