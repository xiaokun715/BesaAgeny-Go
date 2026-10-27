package experience_test

import (
	"context"
	"testing"

	"github.com/besa-ai/besa-agent/internal/experience"
)

func manager() *experience.Manager {
	return experience.NewManager(experience.NewMemoryStore(), experience.DefaultGatePolicy(), 1, false, nil)
}

func TestNeedsReviewIsNotFailure(t *testing.T) {
	m := manager()
	decision := m.OnFailure(context.Background(), experience.Failure{Stage: "execution", Status: "needs_review"})
	if decision.Action != experience.ActionAbort {
		t.Fatalf("needs_review 不应进入归因，得到 %+v", decision)
	}
}

func TestClassifyAndRecovery(t *testing.T) {
	category, recover := experience.Classify(experience.Failure{ErrorType: "environment_not_ready", Message: "environment initializing", Retryable: boolPtr(true)})
	if category != experience.CatEnvironment || recover != experience.RecoverAuto {
		t.Fatalf("unexpected classification: %s %s", category, recover)
	}
	decision := experience.Decide(experience.RecoveryInput{ErrorType: "environment_not_ready", RetryCount: 0, MaxRetries: 2, Risk: "low"})
	if decision.Action != experience.ActionWaitRecheck {
		t.Fatalf("环境未就绪应先等待复查，得到 %+v", decision)
	}
	high := experience.Decide(experience.RecoveryInput{ErrorType: "environment_not_ready", RetryCount: 0, MaxRetries: 2, Risk: "high"})
	if high.Action != experience.ActionRequestAuth {
		t.Fatalf("高风险需要确认，得到 %+v", high)
	}
}

func TestSettleRequiresEvidenceAndVerifiedHypothesis(t *testing.T) {
	m := manager()
	failure := experience.Failure{Stage: "environment", ErrorType: "environment_not_ready", Message: "环境仍在初始化", TraceID: "t1", Risk: "low"}
	unverified, err := m.Settle(context.Background(), experience.Settlement{
		Failure: failure,
		Hypothesis: experience.Hypothesis{Statement: "猜测", State: experience.HypothesisHypothesis,
			Evidence: []experience.Evidence{{Kind: "log_line", Source: "env.log", Detail: "initializing"}}},
		Recovery: "wait_and_recheck", TenantID: "default",
	})
	if err != nil || len(unverified) != 0 {
		t.Fatalf("未验证假设不得入库：%v %+v", err, unverified)
	}

	settled, err := m.Settle(context.Background(), experience.Settlement{
		Failure: failure,
		Hypothesis: experience.Hypothesis{Statement: "环境初始化未完成", State: experience.HypothesisVerified,
			Evidence: []experience.Evidence{{Kind: "state_query", Source: "env-status", Detail: "environment_status=initializing"}}},
		Recovery: "wait_and_recheck", TenantID: "default",
		Validation: experience.ValidationStats{SuccessRate: 0.95, Samples: 5, Reproducible: true},
	})
	if err != nil || len(settled) != 1 {
		t.Fatalf("verified 经验应入库：%v %+v", err, settled)
	}
	if settled[0].State != experience.ExpVerified {
		t.Fatalf("state=%s", settled[0].State)
	}
	retrieved := m.Recall(context.Background(), experience.Query{TenantID: "default", Stage: "environment", TopK: 5})
	if len(retrieved.Experiences) != 1 {
		t.Fatalf("应能检索到经验：%+v", retrieved)
	}
}

func TestGateRejectsMissingEvidence(t *testing.T) {
	m := manager()
	ok, reason := m.Gate(experience.Experience{State: experience.ExpVerified, Scope: map[string]string{"tenant": "default"}})
	if ok {
		t.Fatalf("无证据必须拒绝，reason=%s", reason)
	}
}

func TestRecordApplicationDegrades(t *testing.T) {
	m := manager()
	settled, _ := m.Settle(context.Background(), experience.Settlement{
		Failure: experience.Failure{Stage: "tool", ErrorType: "tool_timeout", Message: "timeout"},
		Hypothesis: experience.Hypothesis{Statement: "工具超时", State: experience.HypothesisVerified,
			Evidence: []experience.Evidence{{Kind: "exit_code", Source: "tool", Detail: "timeout"}}},
		Recovery: "retry", TenantID: "default",
	})
	if len(settled) == 0 {
		t.Fatal("setup failed")
	}
	id := settled[0].ID
	for i := 0; i < 3; i++ {
		m.RecordApplication(context.Background(), id, "trace", "failure", 1, 0)
	}
	exp, _ := m.Store.Get(context.Background(), id)
	if exp.State != experience.ExpDegraded {
		t.Fatalf("连续无效应用后应降级，得到 %s", exp.State)
	}
}

func boolPtr(v bool) *bool { return &v }
