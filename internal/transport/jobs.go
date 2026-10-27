package transport

import "github.com/besa-ai/besa-agent/internal/workflow"

// AgentJob 是 NATS 上的 agent 作业载荷。
type AgentJob struct {
	TaskID    string `json:"task_id"`
	Goal      string `json:"goal"`
	Stage     string `json:"stage"`
	SessionID string `json:"session_id"`
	Caller    string `json:"caller"`
	Strategy  string `json:"strategy,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Resume    bool   `json:"resume"`
}

// WorkflowJob 是 NATS 上的工作流作业载荷。
type WorkflowJob struct {
	TaskID    string             `json:"task_id"`
	Workflow  *workflow.Workflow `json:"workflow,omitempty"`
	Input     map[string]any     `json:"input,omitempty"`
	SessionID string             `json:"session_id"`
	Caller    string             `json:"caller"`
	Mode      string             `json:"mode"`
	Template  string             `json:"template"`
	Resume    bool               `json:"resume"`
	Goal      string             `json:"goal"`
}

// CancelJob 请求停止一次运行。
type CancelJob struct {
	RunID string `json:"run_id"`
}
