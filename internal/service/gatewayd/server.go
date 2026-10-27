// Package gatewayd 暴露模型网关的 gRPC 接口。
package gatewayd

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/transport"
)

type Server struct {
	besav1.UnimplementedGatewayServiceServer
	Gateway *gateway.Gateway
}

func New(gw *gateway.Gateway) *Server { return &Server{Gateway: gw} }

func (s *Server) Chat(ctx context.Context, req *besav1.ChatRequest) (*besav1.GatewayResult, error) {
	result, err := s.Gateway.Chat(ctx, req.Alias, transport.MessagesFromProto(req.Messages), transport.ChatOptionsFromProto(req.Options))
	if err != nil {
		return nil, toStatus(err)
	}
	return transport.GatewayResultToProto(result), nil
}

func (s *Server) Embed(ctx context.Context, req *besav1.EmbedRequest) (*besav1.EmbedResponse, error) {
	result, err := s.Gateway.Embed(ctx, req.Alias, req.Texts, transport.ChatOptionsFromProto(req.Options))
	if err != nil {
		return nil, toStatus(err)
	}
	resp := &besav1.EmbedResponse{Model: "", Usage: transport.UsageToProto(result.Usage), Cost: transport.CostToProto(result.Cost), Meta: transport.GatewayResultToProto(result)}
	if result.Embedding != nil {
		resp.Model = result.Embedding.Model
		resp.Dimension = int32(result.Embedding.Dimension)
		for _, vec := range result.Embedding.Vectors {
			resp.Vectors = append(resp.Vectors, &besav1.FloatArray{Values: vec})
		}
	}
	return resp, nil
}

func (s *Server) StreamChat(req *besav1.ChatRequest, stream besav1.GatewayService_StreamChatServer) error {
	events, err := s.Gateway.StreamChat(stream.Context(), req.Alias, transport.MessagesFromProto(req.Messages), transport.ChatOptionsFromProto(req.Options))
	if err != nil {
		return toStatus(err)
	}
	for ev := range events {
		switch ev.Kind {
		case "chunk":
			if err := stream.Send(&besav1.StreamEvent{Event: &besav1.StreamEvent_Chunk{Chunk: ev.Text}}); err != nil {
				return err
			}
		case "done":
			if ev.Result != nil {
				if err := stream.Send(&besav1.StreamEvent{Event: &besav1.StreamEvent_Done{Done: transport.GatewayResultToProto(*ev.Result)}}); err != nil {
					return err
				}
			}
		case "failed":
			failure := &besav1.StreamFailure{Error: errString(ev.Err)}
			for _, a := range ev.Attempts {
				failure.Attempts = append(failure.Attempts, &besav1.AttemptRecord{ModelKey: a.ModelKey, Provider: a.Provider,
					Outcome: a.Outcome, Error: a.Error, Retryable: a.Retryable, SkippedReason: a.SkippedReason, ElapsedS: a.ElapsedS})
			}
			return stream.Send(&besav1.StreamEvent{Event: &besav1.StreamEvent_Failed{Failed: failure}})
		}
	}
	return nil
}

func (s *Server) ContextWindow(_ context.Context, req *besav1.AliasRequest) (*besav1.ContextWindowResponse, error) {
	window := s.Gateway.ContextWindow(req.Alias)
	if window == nil {
		return &besav1.ContextWindowResponse{Known: false}, nil
	}
	return &besav1.ContextWindowResponse{Known: true, Window: int32(*window)}, nil
}

func (s *Server) Health(_ context.Context, _ *besav1.Empty) (*besav1.HealthResponse, error) {
	out := &besav1.HealthResponse{Circuits: map[string]string{}}
	for key, state := range s.Gateway.Health().Snapshot() {
		out.Circuits[key] = string(state)
	}
	return out, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch gateway.CodeOf(err) {
	case gateway.CodeUnknownAlias:
		return status.Error(codes.NotFound, err.Error())
	case gateway.CodeNoCapable:
		return status.Error(codes.FailedPrecondition, err.Error())
	case gateway.CodeBudget:
		return status.Error(codes.ResourceExhausted, err.Error())
	case gateway.CodeAllFailed:
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
