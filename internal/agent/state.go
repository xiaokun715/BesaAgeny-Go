package agent

import (
	"encoding/json"
	"time"
)

type RunStatus string

const (
	StatusRunning     RunStatus = "running"
	StatusDone        RunStatus = "done"
	StatusFailed      RunStatus = "failed"
	StatusCancelled   RunStatus = "cancelled"
	StatusNeedsReview RunStatus = "needs_review"
)

func (s RunStatus) Finished() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled
}

type AgentStep struct {
	Index     int       `json:"index"`
	NodeID    string    `json:"node_id"`
	Summary   string    `json:"summary"`
	ToolsUsed []string  `json:"tools,omitempty"`
	IdemKeys  []string  `json:"idem_keys,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type TaskRun struct {
	RunID       string         `json:"run_id"`
	Stage       string         `json:"stage"`
	Goal        string         `json:"goal"`
	Status      RunStatus      `json:"status"`
	SessionID   string         `json:"session_id,omitempty"`
	Caller      string         `json:"caller,omitempty"`
	Steps       []AgentStep    `json:"steps,omitempty"`
	Artifacts   map[string]any `json:"artifacts,omitempty"`
	ResumedFrom int            `json:"resumed_from,omitempty"`
}

func NewTaskRun(runID, stage, goal, sessionID, caller string) *TaskRun {
	return &TaskRun{RunID: runID, Stage: stage, Goal: goal, Status: StatusRunning, SessionID: sessionID, Caller: caller, Artifacts: map[string]any{}}
}

func (r *TaskRun) NextIndex() int { return len(r.Steps) }

// Record 由这里分配索引 —— 调用方不该自己数。
func (r *TaskRun) Record(nodeID, summary string, tools, idemKeys []string) {
	r.Steps = append(r.Steps, AgentStep{
		Index: len(r.Steps), NodeID: nodeID, Summary: summary,
		ToolsUsed: tools, IdemKeys: idemKeys, CreatedAt: time.Now().UTC(),
	})
}

func (r *TaskRun) Finish(status RunStatus) { r.Status = status }

func (r TaskRun) ToState() map[string]any {
	raw, _ := json.Marshal(r)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// TaskRunFromState 恢复出的运行**一律是 running**，resumed_from = 已完成步数。
func TaskRunFromState(runID string, state map[string]any) *TaskRun {
	raw, _ := json.Marshal(state)
	var run TaskRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil
	}
	run.RunID = runID
	run.ResumedFrom = len(run.Steps)
	run.Status = StatusRunning
	if run.Artifacts == nil {
		run.Artifacts = map[string]any{}
	}
	return &run
}
