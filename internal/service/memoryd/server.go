// Package memoryd 暴露长期记忆服务。
package memoryd

import (
	"context"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/memory"
	"github.com/besa-ai/besa-agent/internal/transport"
)

type Server struct {
	besav1.UnimplementedMemoryServiceServer
	Manager *memory.Manager
}

func New(manager *memory.Manager) *Server { return &Server{Manager: manager} }

func (s *Server) Remember(ctx context.Context, req *besav1.MemoryRememberRequest) (*besav1.MemoryRememberResponse, error) {
	turns := make([]ctxpkg.Turn, 0, len(req.Turns))
	for _, t := range req.Turns {
		turns = append(turns, ctxpkg.Turn{Role: t.Role, Content: t.Content})
	}
	items, err := s.Manager.Remember(ctx, turns, req.Source, transport.MemoryContextFromProto(req.Context))
	if err != nil {
		return nil, err
	}
	resp := &besav1.MemoryRememberResponse{}
	for _, item := range items {
		resp.Items = append(resp.Items, &besav1.MemoryItemMsg{Id: item.ID, Content: item.Content,
			Kind: string(item.Kind), Scope: string(item.Scope), Weight: item.Weight})
	}
	return resp, nil
}

func (s *Server) Recall(ctx context.Context, req *besav1.MemoryRecallRequest) (*besav1.MemoryRecallResponse, error) {
	kinds := make([]memory.Kind, 0, len(req.Kinds))
	for _, k := range req.Kinds {
		kinds = append(kinds, memory.Kind(k))
	}
	results, err := s.Manager.Recall(ctx, req.Query, transport.MemoryContextFromProto(req.Context), kinds, int(req.TopK))
	if err != nil {
		return nil, err
	}
	resp := &besav1.MemoryRecallResponse{}
	for _, r := range results {
		resp.Items = append(resp.Items, transport.MemoryItemToProto(r))
	}
	return resp, nil
}

func (s *Server) Maintain(ctx context.Context, req *besav1.MemoryMaintainRequest) (*besav1.MemoryMaintainResponse, error) {
	report, err := s.Manager.Maintain(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	return &besav1.MemoryMaintainResponse{Scanned: int32(report.Scanned), Tombstoned: int32(report.Tombstoned)}, nil
}
