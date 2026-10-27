package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/besa-ai/besa-agent/internal/agent"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

type RunRepo struct{ DB *DB }

func (r *RunRepo) Create(ctx context.Context, runID, sessionID, caller, stage, goal string) error {
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO runs(run_id, session_id, caller, stage, goal, status)
		VALUES ($1,NULLIF($2,''),NULLIF($3,''),NULLIF($4,''),$5,'running')
		ON CONFLICT (run_id) DO NOTHING`, runID, sessionID, caller, stage, goal)
	return err
}

func (r *RunRepo) Finish(ctx context.Context, runID string, status agent.RunStatus, cost *decimal.Decimal, currency string) error {
	var amount any
	if cost != nil {
		amount = cost.String()
	}
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE runs SET status=$2, finished_at=now(), cost_amount=$3::numeric,
		                cost_currency=NULLIF($4,'')
		WHERE run_id=$1`, runID, string(status), amount, currency)
	return err
}

func (r *RunRepo) SaveSteps(ctx context.Context, run *agent.TaskRun) error {
	if run == nil {
		return nil
	}
	tx, err := r.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, step := range run.Steps {
		if _, err := tx.Exec(ctx, `
			INSERT INTO run_steps(run_id, step_index, node_id, summary, tools_used, idem_keys, status)
			VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,$7)
			ON CONFLICT (run_id, step_index) DO UPDATE
			SET node_id=EXCLUDED.node_id, summary=EXCLUDED.summary,
			    tools_used=EXCLUDED.tools_used, idem_keys=EXCLUDED.idem_keys, status=EXCLUDED.status`,
			run.RunID, step.Index, step.NodeID, step.Summary, step.ToolsUsed, step.IdemKeys, string(run.Status)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type RunView struct {
	RunID      string
	Status     string
	Stage      string
	Goal       string
	StartedAt  time.Time
	FinishedAt *time.Time
}

func (r *RunRepo) Get(ctx context.Context, runID string) (*RunView, error) {
	var v RunView
	err := r.DB.Pool.QueryRow(ctx, `
		SELECT run_id, status, COALESCE(stage,''), goal, started_at, finished_at
		FROM runs WHERE run_id=$1`, runID).Scan(&v.RunID, &v.Status, &v.Stage, &v.Goal, &v.StartedAt, &v.FinishedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}
