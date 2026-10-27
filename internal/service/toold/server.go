// Package toold 暴露工具执行服务。
//
// 工具执行必须在服务端重新校验 schema / 权限 / 注入 ——
// agent 侧的“已授权”声明不可信。
package toold

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/tool"
	"github.com/besa-ai/besa-agent/internal/transport"
)

type Server struct {
	besav1.UnimplementedToolServiceServer
	Executor *tool.Executor
	Registry *tool.Registry
}

func New(executor *tool.Executor, registry *tool.Registry) *Server {
	return &Server{Executor: executor, Registry: registry}
}

func (s *Server) Invoke(ctx context.Context, req *besav1.ToolInvocation) (*besav1.ToolResultMsg, error) {
	if req.GetToolName() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_name 不能为空")
	}
	result := s.Executor.Invoke(ctx, transport.InvocationFromProto(req))
	return transport.ToolResultToProto(result), nil
}

func (s *Server) ListTools(_ context.Context, req *besav1.ListToolsRequest) (*besav1.ListToolsResponse, error) {
	defs := s.Registry.Select(req.Categories, req.Only)
	resp := &besav1.ListToolsResponse{}
	for _, d := range defs {
		resp.Tools = append(resp.Tools, transport.ToolDefinitionToProto(d))
	}
	return resp, nil
}
