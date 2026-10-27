package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/besa-ai/besa-agent/internal/experience"
)

type ExperienceRepo struct{ DB *DB }

func (r *ExperienceRepo) Save(ctx context.Context, exp experience.Experience) error {
	conditions, _ := json.Marshal(exp.Conditions)
	evidence, _ := json.Marshal(exp.Evidence)
	scope, _ := json.Marshal(exp.Scope)
	validation, _ := json.Marshal(exp.Validation)
	var expires any
	if exp.ExpiresWhen != nil {
		raw, _ := json.Marshal(exp.ExpiresWhen)
		expires = raw
	}
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO experiences(experience_id, category, scenario, symptom, conditions, evidence, root_cause,
		                        cause_state, recovery_strategy, scope, validation, state, expires_when, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10::jsonb,$11::jsonb,$12,$13::jsonb,$14,$15)
		ON CONFLICT (experience_id) DO UPDATE
		SET category=EXCLUDED.category, scenario=EXCLUDED.scenario, symptom=EXCLUDED.symptom,
		    conditions=EXCLUDED.conditions, evidence=EXCLUDED.evidence, root_cause=EXCLUDED.root_cause,
		    cause_state=EXCLUDED.cause_state, recovery_strategy=EXCLUDED.recovery_strategy,
		    scope=EXCLUDED.scope, validation=EXCLUDED.validation, state=EXCLUDED.state,
		    expires_when=EXCLUDED.expires_when, updated_at=EXCLUDED.updated_at`,
		exp.ID, exp.Category, exp.Scenario, exp.Symptom, string(conditions), string(evidence), exp.RootCause,
		exp.CauseState, exp.RecoveryStrategy, string(scope), string(validation), string(exp.State), jsonValue(expires),
		exp.CreatedAt, exp.UpdatedAt)
	return err
}

func (r *ExperienceRepo) Get(ctx context.Context, id string) (*experience.Experience, error) {
	row := r.DB.Pool.QueryRow(ctx, `SELECT `+expColumns+` FROM experiences WHERE experience_id=$1`, id)
	return scanExperience(row)
}

func (r *ExperienceRepo) Update(ctx context.Context, exp experience.Experience) error {
	return r.Save(ctx, exp)
}

func (r *ExperienceRepo) FindByScenario(ctx context.Context, tenant, scenario, errorType string) (*experience.Experience, error) {
	row := r.DB.Pool.QueryRow(ctx, `
		SELECT `+expColumns+` FROM experiences
		WHERE scope->>'tenant'=$1 AND scenario=$2 AND conditions->>'error_type'=$3
		ORDER BY updated_at DESC LIMIT 1`, tenant, scenario, errorType)
	return scanExperience(row)
}

func (r *ExperienceRepo) Search(ctx context.Context, q experience.Query) ([]experience.Experience, error) {
	query := `SELECT ` + expColumns + ` FROM experiences WHERE state='verified'`
	args := []any{}
	next := 1
	if q.TenantID != "" {
		query += ` AND scope->>'tenant'=$1`
		args = append(args, q.TenantID)
		next++
	}
	if q.Stage != "" {
		query += ` AND conditions->>'stage'=$` + strconv.Itoa(next)
		args = append(args, q.Stage)
		next++
	}
	if q.ErrorType != "" {
		query += ` AND conditions->>'error_type'=$` + strconv.Itoa(next)
		args = append(args, q.ErrorType)
		next++
	}
	if q.Scenario != "" {
		query += ` AND scenario ILIKE '%'||$` + strconv.Itoa(next) + `||'%'`
		args = append(args, q.Scenario)
		next++
	}
	limit := q.TopK
	if limit <= 0 {
		limit = 10
	}
	query += ` ORDER BY (validation->>'success_rate')::float8 DESC LIMIT ` + strconv.Itoa(limit)
	rows, err := r.DB.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []experience.Experience
	for rows.Next() {
		exp, err := scanExperience(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *exp)
	}
	return out, rows.Err()
}

func (r *ExperienceRepo) List(ctx context.Context, tenant string, limit int) ([]experience.Experience, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := r.DB.Pool.Query(ctx, `SELECT `+expColumns+` FROM experiences
		WHERE ($1='' OR scope->>'tenant'=$1) ORDER BY updated_at DESC LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []experience.Experience
	for rows.Next() {
		exp, err := scanExperience(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *exp)
	}
	return out, rows.Err()
}

func (r *ExperienceRepo) RecordApplication(ctx context.Context, id, traceID, outcome string, before, after float64) error {
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO experience_applications(experience_id, trace_id, outcome, metric_before, metric_after)
		VALUES ($1,NULLIF($2,''),$3,$4,$5)`, id, traceID, outcome, before, after)
	return err
}

const expColumns = `experience_id, category, scenario, symptom, conditions, evidence, COALESCE(root_cause,''),
	cause_state, COALESCE(recovery_strategy,''), scope, validation, state, expires_when, created_at, updated_at, last_applied_at`

func scanExperience(row rowScanner) (*experience.Experience, error) {
	var exp experience.Experience
	var state string
	var conditions, evidence, scope, validation, expires []byte
	var lastApplied *time.Time
	err := row.Scan(&exp.ID, &exp.Category, &exp.Scenario, &exp.Symptom, &conditions, &evidence,
		&exp.RootCause, &exp.CauseState, &exp.RecoveryStrategy, &scope, &validation, &state,
		&expires, &exp.CreatedAt, &exp.UpdatedAt, &lastApplied)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	exp.State = experience.ExperienceState(state)
	exp.LastAppliedAt = lastApplied
	_ = json.Unmarshal(conditions, &exp.Conditions)
	_ = json.Unmarshal(evidence, &exp.Evidence)
	_ = json.Unmarshal(scope, &exp.Scope)
	_ = json.Unmarshal(validation, &exp.Validation)
	if len(expires) > 0 {
		var cond experience.ExpiryCondition
		if json.Unmarshal(expires, &cond) == nil {
			exp.ExpiresWhen = &cond
		}
	}
	return &exp, nil
}
