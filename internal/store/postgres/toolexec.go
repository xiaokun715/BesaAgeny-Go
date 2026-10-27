package postgres

import (
	"context"
	"time"

	"github.com/besa-ai/besa-agent/internal/tool"
)

type ToolExecutionRepo struct{ DB *DB }

func (r *ToolExecutionRepo) Find(ctx context.Context, scope, idemKey string, lease time.Duration) (*tool.ExecutionView, error) {
	row := r.DB.Pool.QueryRow(ctx, `
		SELECT scope, idem_key, tool_name, state, COALESCE(owner,''), started_at, finished_at,
		       COALESCE(result,''), result_truncated, COALESCE(error,''), output_bytes
		FROM tool_executions WHERE scope=$1 AND idem_key=$2`, scope, idemKey)
	var v tool.ExecutionView
	var finished *time.Time
	if err := row.Scan(&v.Scope, &v.IdemKey, &v.ToolName, &v.State, &v.Owner, &v.StartedAt, &finished,
		&v.Result, &v.ResultTruncated, &v.Error, &v.OutputBytes); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	v.FinishedAt = finished
	// uncertain 在读取时判定，**不写回** —— 写回会破坏“读取时判定”的语义。
	if v.State == "in_flight" && time.Since(v.StartedAt) > lease {
		v.State = "uncertain"
	}
	return &v, nil
}

func (r *ToolExecutionRepo) Register(ctx context.Context, reg tool.Registration) (bool, error) {
	tag, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO tool_executions(scope, idem_key, tool_name, side_effect, args_digest, state, owner, trace_id, session_id, caller)
		VALUES ($1,$2,$3,$4,$5,'in_flight',$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''))
		ON CONFLICT (scope, idem_key) DO NOTHING`,
		reg.Scope, reg.IdemKey, reg.ToolName, string(reg.SideEffect), reg.ArgsDigest, reg.Owner, reg.TraceID, reg.SessionID, reg.Caller)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *ToolExecutionRepo) MarkDone(ctx context.Context, scope, idemKey string, result *string, truncated bool, outputBytes int) error {
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE tool_executions
		SET state='done', result=$3, result_truncated=$4, output_bytes=$5, error=NULL, finished_at=now(),
		    owner=NULL
		WHERE scope=$1 AND idem_key=$2`, scope, idemKey, result, truncated, outputBytes)
	return err
}

func (r *ToolExecutionRepo) MarkFailed(ctx context.Context, scope, idemKey, errMsg string) error {
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE tool_executions
		SET state='failed', result=NULL, result_truncated=false, error=$3, finished_at=now(), owner=NULL
		WHERE scope=$1 AND idem_key=$2`, scope, idemKey, errMsg)
	return err
}

func (r *ToolExecutionRepo) Reopen(ctx context.Context, scope, idemKey, owner string) (bool, error) {
	tag, err := r.DB.Pool.Exec(ctx, `
		UPDATE tool_executions
		SET state='in_flight', owner=$3, result=NULL, error=NULL, finished_at=NULL, started_at=now()
		WHERE scope=$1 AND idem_key=$2 AND state='failed'`, scope, idemKey, owner)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
