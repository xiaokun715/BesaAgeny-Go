package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/besa-ai/besa-agent/internal/workflow"
)

type WorkflowRepo struct{ DB *DB }

func (r *WorkflowRepo) CreateRun(ctx context.Context, report workflow.RunReport, w workflow.Workflow, input map[string]any) error {
	raw, _ := json.Marshal(input)
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO workflow_runs(workflow_run_id, workflow_id, version, origin, status, input)
		VALUES ($1,$2,NULLIF($3,''),$4,'running',$5::jsonb)
		ON CONFLICT (workflow_run_id) DO NOTHING`,
		report.WorkflowRunID, w.WorkflowID, w.Version, w.Origin, string(raw))
	return err
}

func (r *WorkflowRepo) SaveNode(ctx context.Context, workflowRunID, nodeID string, status workflow.NodeState, outputs map[string]any, errMsg string) error {
	raw, _ := json.Marshal(outputs)
	var finished any
	if status == workflow.StateSucceeded || status == workflow.StateFailed || status == workflow.StateSkipped {
		finished = time.Now().UTC()
	}
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO workflow_nodes(workflow_run_id, node_id, agent, status, outputs, finished_at)
		VALUES ($1,$2,'',$3,$4::jsonb,$5)
		ON CONFLICT (workflow_run_id, node_id) DO UPDATE
		SET status=EXCLUDED.status, outputs=EXCLUDED.outputs, finished_at=EXCLUDED.finished_at`,
		workflowRunID, nodeID, string(status), string(raw), finished)
	return err
}

func (r *WorkflowRepo) FinishRun(ctx context.Context, report workflow.RunReport) error {
	raw, _ := json.Marshal(report.Nodes)
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE workflow_runs SET status=$2, result=$3::jsonb, finished_at=$4 WHERE workflow_run_id=$1`,
		report.WorkflowRunID, string(report.Status), string(raw), report.FinishedAt)
	return err
}
