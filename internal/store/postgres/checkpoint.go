package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/besa-ai/besa-agent/internal/agent"
)

type CheckpointRepo struct{ DB *DB }

func (r *CheckpointRepo) Latest(ctx context.Context, runID string) (*agent.Checkpoint, error) {
	row := r.DB.Pool.QueryRow(ctx, `
		SELECT run_id, format_version, step_index, COALESCE(node_id,''), status,
		       state, artifacts, COALESCE(session_id,''), COALESCE(caller,''), created_at
		FROM checkpoints WHERE run_id=$1
		ORDER BY created_at DESC, id DESC LIMIT 1`, runID)
	return scanCheckpoint(row)
}

func (r *CheckpointRepo) Save(ctx context.Context, cp agent.Checkpoint) error {
	if cp.FormatVersion == 0 {
		cp.FormatVersion = 1
	}
	state, _ := json.Marshal(cp.State)
	artifacts, _ := json.Marshal(cp.Artifacts)
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO checkpoints(run_id, format_version, step_index, node_id, status, state, artifacts, session_id, caller, created_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6::jsonb,$7::jsonb,NULLIF($8,''),NULLIF($9,''),COALESCE($10, now()))`,
		cp.RunID, cp.FormatVersion, cp.StepIndex, cp.NodeID, string(cp.Status), string(state), string(artifacts), cp.SessionID, cp.Caller, cp.CreatedAt)
	return err
}

// Unfinished 必须同时包含 running 与 needs_review ——
// 不含后者的话，最需要人看的那一类运行恰恰查不到。
func (r *CheckpointRepo) Unfinished(ctx context.Context, limit int) ([]agent.Checkpoint, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.DB.Pool.Query(ctx, `
		SELECT DISTINCT ON (run_id) run_id, format_version, step_index, COALESCE(node_id,''), status,
		       state, artifacts, COALESCE(session_id,''), COALESCE(caller,''), created_at
		FROM checkpoints WHERE status IN ('running','needs_review')
		ORDER BY run_id, created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.Checkpoint
	for rows.Next() {
		cp, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *cp)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCheckpoint(row rowScanner) (*agent.Checkpoint, error) {
	var cp agent.Checkpoint
	var stateRaw, artifactsRaw []byte
	var status string
	if err := row.Scan(&cp.RunID, &cp.FormatVersion, &cp.StepIndex, &cp.NodeID, &status,
		&stateRaw, &artifactsRaw, &cp.SessionID, &cp.Caller, &cp.CreatedAt); err != nil {
		return nil, err
	}
	if cp.FormatVersion != 1 {
		return nil, fmt.Errorf("未知的检查点格式版本 %d（运行 %s）—— 不按旧格式猜", cp.FormatVersion, cp.RunID)
	}
	cp.Status = agent.RunStatus(status)
	_ = json.Unmarshal(stateRaw, &cp.State)
	_ = json.Unmarshal(artifactsRaw, &cp.Artifacts)
	return &cp, nil
}
