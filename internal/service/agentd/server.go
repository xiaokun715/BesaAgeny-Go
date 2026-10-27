// Package agentd 暴露单 agent 运行时。
package agentd

import (
	"context"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/agent"
	"github.com/besa-ai/besa-agent/internal/transport"
)

type Server struct {
	besav1.UnimplementedAgentServiceServer
	Runtime *agent.Runtime
}

func New(runtime *agent.Runtime) *Server { return &Server{Runtime: runtime} }

func (s *Server) RunOne(ctx context.Context, req *besav1.RunRequest) (*besav1.RunOutcomeMsg, error) {
	outcome := s.Runtime.RunOne(ctx, transport.RunRequestFromProto(req))
	return transport.RunOutcomeToProto(outcome), nil
}

func (s *Server) Cancel(_ context.Context, req *besav1.CancelRequest) (*besav1.CancelResponse, error) {
	s.Runtime.Cancel(req.GetRunId())
	return &besav1.CancelResponse{Ok: true}, nil
}
