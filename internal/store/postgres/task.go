package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

type Task struct {
	TaskID     string
	SessionID  string
	Caller     string
	Kind       string // agent | workflow
	Stage      string
	Goal       string
	Status     string // queued | running | done | failed | cancelled | needs_review
	RunID      string
	WorkflowID string
	Result     map[string]any
	Error      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type TaskRepo struct{ DB *DB }

func (r *TaskRepo) Create(ctx context.Context, task Task) (Task, error) {
	if task.TaskID == "" {
		task.TaskID = ids.New("task")
	}
	if task.Kind == "" {
		task.Kind = "agent"
	}
	if task.Status == "" {
		task.Status = "queued"
	}
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO tasks(task_id, session_id, caller, kind, stage, goal, status)
		VALUES ($1,NULLIF($2,''),NULLIF($3,''),$4,NULLIF($5,''),$6,$7)`,
		task.TaskID, task.SessionID, task.Caller, task.Kind, task.Stage, task.Goal, task.Status)
	if err != nil {
		return task, err
	}
	return r.Get(ctx, task.TaskID)
}

func (r *TaskRepo) Get(ctx context.Context, taskID string) (Task, error) {
	var t Task
	var result []byte
	err := r.DB.Pool.QueryRow(ctx, `
		SELECT task_id, COALESCE(session_id,''), COALESCE(caller,''), kind, COALESCE(stage,''), goal,
		       status, COALESCE(run_id,''), COALESCE(workflow_id,''), result, COALESCE(error,''), created_at, updated_at
		FROM tasks WHERE task_id=$1`, taskID).
		Scan(&t.TaskID, &t.SessionID, &t.Caller, &t.Kind, &t.Stage, &t.Goal,
			&t.Status, &t.RunID, &t.WorkflowID, &result, &t.Error, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return t, err
	}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &t.Result)
	}
	return t, nil
}

func (r *TaskRepo) UpdateStatus(ctx context.Context, taskID, status, runID, errMsg string) error {
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE tasks SET status=$2, run_id=COALESCE(NULLIF($3,''), run_id), error=NULLIF($4,''), updated_at=now()
		WHERE task_id=$1`, taskID, status, runID, errMsg)
	return err
}

func (r *TaskRepo) Complete(ctx context.Context, taskID, status string, result map[string]any, errMsg string) error {
	raw, _ := json.Marshal(result)
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE tasks SET status=$2, result=$3::jsonb, error=NULLIF($4,''), updated_at=now()
		WHERE task_id=$1`, taskID, status, string(raw), errMsg)
	return err
}

func (r *TaskRepo) ListBySession(ctx context.Context, sessionID string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.DB.Pool.Query(ctx, `
		SELECT task_id FROM tasks WHERE session_id=$1 ORDER BY created_at DESC LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		t, err := r.Get(ctx, id)
		if err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}
