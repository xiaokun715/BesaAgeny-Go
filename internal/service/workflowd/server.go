// Package workflowd 暴露 DAG 编排服务。
package workflowd

import (
	"context"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/transport"
	"github.com/besa-ai/besa-agent/internal/workflow"
)

type Server struct {
	besav1.UnimplementedWorkflowServiceServer
	Engine *workflow.Engine
}

func New(engine *workflow.Engine) *Server { return &Server{Engine: engine} }

func (s *Server) Validate(_ context.Context, req *besav1.WorkflowDef) (*besav1.ValidateResponse, error) {
	def := transport.WorkflowFromProto(req)
	issues := s.Engine.Validate(def, nil, nil)
	resp := &besav1.ValidateResponse{}
	for _, issue := range issues {
		resp.Issues = append(resp.Issues, &besav1.WorkflowIssueMsg{Kind: string(issue.Kind), NodeId: issue.NodeID,
			Detail: issue.Detail, Suggestion: issue.Suggestion})
	}
	return resp, nil
}

func (s *Server) Run(ctx context.Context, req *besav1.WorkflowRunRequest) (*besav1.WorkflowRunResponse, error) {
	def := transport.WorkflowFromProto(req.Workflow)
	input := transport.StructToMap(req.Input)
	report, err := s.Engine.Run(ctx, def, input, req.Resume)
	resp := &besav1.WorkflowRunResponse{
		WorkflowRunId: report.WorkflowRunID, Status: string(report.Status), Error: report.Error,
	}
	for id, node := range report.Nodes {
		resp.Nodes = append(resp.Nodes, &besav1.WorkflowNodeResultMsg{
			NodeId: id, Status: string(node.Status), Error: node.Error, ElapsedS: node.Elapsed.Seconds(),
			NeedsReview: node.NeedsReview, Outputs: transport.MapToStruct(node.Outputs),
		})
	}
	return resp, err
}
