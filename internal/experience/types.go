// Package experience 是失败驱动的经验闭环。
//
// 三条边界：
//   - needs_review 不算失败，不进归因；
//   - Critic 只能提出假设，**升格为 verified 必须经过证据校验**；
//   - 未验证经验不进检索结果，也不能影响恢复决策。
package experience

import (
	"time"
)

type Evidence struct {
	Kind   string // tool_return | exit_code | state_query | log_line | assertion
	Source string
	Detail string
	Weight float64 // 只有客观来源允许 >0
}

type VerificationResult struct {
	Stage          string
	Status         string // passed | failed | unknown
	ErrorType      string
	Evidence       []Evidence
	Retryable      *bool // nil = 不知道，不是 false
	RecoveryAction string
	RuleVersion    int
}

type Failure struct {
	Stage          string
	Status         string
	ErrorType      string
	Message        string
	Evidence       []Evidence
	Retryable      *bool
	RecoveryAction string
	TraceID        string
	RunID          string
	NodeID         string
	Risk           string
}

type ErrorCategory string

const (
	CatRequirement   ErrorCategory = "requirement"
	CatRetrieval     ErrorCategory = "retrieval"
	CatTestCase      ErrorCategory = "test_case"
	CatEnvironment   ErrorCategory = "environment"
	CatTool          ErrorCategory = "tool"
	CatTestFailure   ErrorCategory = "test_failure"
	CatDiagnosis     ErrorCategory = "diagnosis"
	CatOrchestration ErrorCategory = "orchestration"
)

type Recoverability string

const (
	RecoverAuto          Recoverability = "auto"
	RecoverExternal      Recoverability = "external_action"
	RecoverUnrecoverable Recoverability = "unrecoverable"
)

type RecoveryAction string

const (
	ActionRetry       RecoveryAction = "retry"
	ActionWaitRecheck RecoveryAction = "wait_recheck"
	ActionAdjust      RecoveryAction = "adjust"
	ActionRollback    RecoveryAction = "rollback"
	ActionSkip        RecoveryAction = "skip"
	ActionRequestAuth RecoveryAction = "request_auth"
	ActionAbort       RecoveryAction = "abort"
)

type RecoveryInput struct {
	ErrorType       string
	Evidence        []Evidence
	RetryCount      int
	MaxRetries      int
	RemainingBudget *float64
	Risk            string // low | medium | high
}

type RecoveryDecision struct {
	Action        RecoveryAction
	Reason        string
	MissingInputs []string
}

type HypothesisState string

const (
	HypothesisCorrelation HypothesisState = "correlation"
	HypothesisHypothesis  HypothesisState = "hypothesis"
	HypothesisVerified    HypothesisState = "verified"
)

type ExperimentRecord struct {
	Description string
	Result      string
	Reproduced  bool
	At          time.Time
}

type Hypothesis struct {
	Statement     string
	SuspectedNode string
	Rationale     string
	Evidence      []Evidence
	State         HypothesisState
	Experiment    *ExperimentRecord
}

type ValidationStats struct {
	Reproducible bool
	SuccessRate  float64
	Samples      int
}

type ExpiryCondition struct {
	Field string
	Op    string
	Value string
}

type ExperienceState string

const (
	ExpUnverified  ExperienceState = "unverified"
	ExpVerified    ExperienceState = "verified"
	ExpDegraded    ExperienceState = "degraded"
	ExpDeactivated ExperienceState = "deactivated"
)

type Experience struct {
	ID               string
	Category         string // fault_handling | successful_path | strategy_tuning
	Scenario         string
	Symptom          string
	Conditions       map[string]string
	Evidence         []Evidence
	RootCause        string
	CauseState       string // unverified | verified
	RecoveryStrategy string
	Scope            map[string]string
	Validation       ValidationStats
	State            ExperienceState
	ExpiresWhen      *ExpiryCondition
	CreatedAt        time.Time
	UpdatedAt        time.Time
	LastAppliedAt    *time.Time
	BadUses          int
}

type DocRef struct {
	ID      string
	Title   string
	Version string
	Content string
	Score   float64
}

type Retrieved struct {
	Knowledge   []DocRef
	Experiences []Experience
	Conflicts   []string
}

type Query struct {
	TenantID    string            `json:"tenant_id"`
	Stage       string            `json:"stage"`
	ErrorType   string            `json:"error_type"`
	Scenario    string            `json:"scenario"`
	Environment map[string]string `json:"environment"`
	TopK        int               `json:"top_k"`
}

type GatePolicy struct {
	DedupeSimilarity          float64
	RequireEvidence           bool
	RequireScope              bool
	MinSuccessRate            float64
	MinSamples                int
	RequireExpiryForVersioned bool
	DegradeAfterBadUses       int
}

func DefaultGatePolicy() GatePolicy {
	return GatePolicy{DedupeSimilarity: 0.9, RequireEvidence: true, RequireScope: true,
		MinSuccessRate: 0.6, MinSamples: 3, RequireExpiryForVersioned: true, DegradeAfterBadUses: 3}
}
