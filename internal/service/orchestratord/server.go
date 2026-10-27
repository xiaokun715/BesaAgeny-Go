// Package orchestratord 暴露多 agent 编排服务。
package orchestratord

import (
	"context"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/multiagent"
)

type Server struct {
	besav1.UnimplementedOrchestratorServiceServer
	Orchestrator *multiagent.Orchestrator
}

func New(o *multiagent.Orchestrator) *Server { return &Server{Orchestrator: o} }

func (s *Server) Run(ctx context.Context, req *besav1.OrchestrateRequest) (*besav1.OrchestrateResponse, error) {
	outcome, err := s.Orchestrator.Run(ctx, multiagent.Request{
		Task: req.Task, Strategy: req.Strategy, Participants: req.Participants,
		Rounds: int(req.Rounds), SessionID: req.SessionId, Caller: req.Caller, Risk: req.Risk,
	})
	if err != nil {
		return nil, err
	}
	resp := &besav1.OrchestrateResponse{Strategy: outcome.Strategy, Answer: outcome.Answer,
		Consensus: outcome.Consensus, Reason: outcome.Reason}
	for _, run := range outcome.Runs {
		resp.Runs = append(resp.Runs, &besav1.ParticipantRunMsg{Name: run.Name, Stage: run.Stage,
			Status: string(run.Status), Answer: run.Answer, Error: run.Error, Cost: run.Cost, NeedsReview: run.NeedsReview})
	}
	return resp, nil
}
