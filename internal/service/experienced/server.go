// Package experienced 暴露失败经验闭环服务。
package experienced

import (
	"context"

	besav1 "github.com/besa-ai/besa-agent/api/proto/v1"
	"github.com/besa-ai/besa-agent/internal/experience"
	"github.com/besa-ai/besa-agent/internal/transport"
)

type Server struct {
	besav1.UnimplementedExperienceServiceServer
	Manager *experience.Manager
}

func New(manager *experience.Manager) *Server { return &Server{Manager: manager} }

func (s *Server) OnFailure(ctx context.Context, req *besav1.FailureMsg) (*besav1.RecoveryDecisionMsg, error) {
	decision := s.Manager.OnFailure(ctx, transport.FailureFromProto(req))
	return &besav1.RecoveryDecisionMsg{Action: string(decision.Action), Reason: decision.Reason,
		MissingInputs: decision.MissingInputs}, nil
}

func (s *Server) Settle(ctx context.Context, req *besav1.SettleRequest) (*besav1.SettleResponse, error) {
	hypothesis := experience.Hypothesis{State: experience.HypothesisState(req.Hypothesis.GetState())}
	if req.Hypothesis != nil {
		hypothesis.Statement = req.Hypothesis.Statement
		hypothesis.SuspectedNode = req.Hypothesis.SuspectedNode
		hypothesis.Rationale = req.Hypothesis.Rationale
		for _, e := range req.Hypothesis.Evidence {
			hypothesis.Evidence = append(hypothesis.Evidence, transport.EvidenceFromProto(e))
		}
		if req.Hypothesis.ExperimentResult != "" {
			hypothesis.Experiment = &experience.ExperimentRecord{Result: req.Hypothesis.ExperimentResult, Reproduced: req.Hypothesis.ExperimentReproduced}
		}
	}
	settlement := experience.Settlement{
		Failure: transport.FailureFromProto(req.Failure), Hypothesis: hypothesis,
		Recovery:   req.Recovery,
		Validation: experience.ValidationStats{SuccessRate: req.SuccessRate, Samples: int(req.Samples), Reproducible: req.Reproducible},
		TenantID:   req.TenantId, Category: req.Category,
	}
	items, err := s.Manager.Settle(ctx, settlement)
	if err != nil {
		return nil, err
	}
	resp := &besav1.SettleResponse{}
	for _, exp := range items {
		resp.Experiences = append(resp.Experiences, transport.ExperienceToProto(exp))
	}
	return resp, nil
}

func (s *Server) Recall(ctx context.Context, req *besav1.ExperienceRecallRequest) (*besav1.ExperienceRecallResponse, error) {
	retrieved := s.Manager.Recall(ctx, experience.Query{
		TenantID: req.TenantId, Stage: req.Stage, ErrorType: req.ErrorType,
		Scenario: req.Scenario, Environment: req.Environment, TopK: int(req.TopK),
	})
	resp := &besav1.ExperienceRecallResponse{Conflicts: retrieved.Conflicts}
	for _, exp := range retrieved.Experiences {
		resp.Experiences = append(resp.Experiences, transport.ExperienceToProto(exp))
	}
	return resp, nil
}

func (s *Server) RecordApplication(ctx context.Context, req *besav1.RecordApplicationRequest) (*besav1.Empty, error) {
	s.Manager.RecordApplication(ctx, req.ExperienceId, req.TraceId, req.Outcome, req.MetricBefore, req.MetricAfter)
	return &besav1.Empty{}, nil
}

func (s *Server) Maintain(ctx context.Context, req *besav1.MaintainExperiencesRequest) (*besav1.MaintainExperiencesResponse, error) {
	changed, err := s.Manager.Maintain(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	return &besav1.MaintainExperiencesResponse{Changed: int32(changed)}, nil
}
