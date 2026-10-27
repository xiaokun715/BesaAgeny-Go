// Package strategyd 暴露组件策略选择服务。
package strategyd

import (
	"context"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/strategy"
	"github.com/besa-ai/besa-agent/internal/transport"
)

type Server struct {
	besav1.UnimplementedStrategyServiceServer
	Selector *strategy.Selector
	Registry *strategy.Registry
}

func New(selector *strategy.Selector, registry *strategy.Registry) *Server {
	return &Server{Selector: selector, Registry: registry}
}

func (s *Server) Select(ctx context.Context, req *besav1.TaskFeaturesMsg) (*besav1.SelectionDecisionMsg, error) {
	decision := s.Selector.Select(ctx, transport.TaskFeaturesFromProto(req))
	return transport.SelectionDecisionToProto(decision), nil
}

func (s *Server) List(_ context.Context, _ *besav1.Empty) (*besav1.ListStrategiesResponse, error) {
	resp := &besav1.ListStrategiesResponse{}
	for _, spec := range s.Registry.All() {
		resp.Strategies = append(resp.Strategies, &besav1.StrategySpecMsg{
			Kind: string(spec.Kind), Name: spec.Name, Version: spec.Version,
			Available: spec.Available, UnavailableReason: spec.UnavailableReason, Scenarios: spec.Scenarios,
		})
	}
	return resp, nil
}
