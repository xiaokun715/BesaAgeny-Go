package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/besa-ai/besa-agent/internal/strategy"
)

type StrategyEvalRepo struct{ DB *DB }

func (r *StrategyEvalRepo) Records(ctx context.Context, benchmark string) ([]strategy.EvalRecord, error) {
	if benchmark == "" {
		benchmark = "default"
	}
	rows, err := r.DB.Pool.Query(ctx, `
		SELECT profile_key, versions, quality, cost, latency, samples, benchmark_set, measured_at
		FROM strategy_eval WHERE benchmark_set=$1 ORDER BY measured_at DESC`, benchmark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []strategy.EvalRecord
	for rows.Next() {
		var rec strategy.EvalRecord
		var versions []byte
		if err := rows.Scan(&rec.ProfileKey, &versions, &rec.Quality, &rec.Cost, &rec.Latency, &rec.Samples, &rec.BenchmarkSet, &rec.At); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(versions, &rec.Versions)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *StrategyEvalRepo) Save(ctx context.Context, rec strategy.EvalRecord) error {
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	versions, _ := json.Marshal(rec.Versions)
	if rec.Samples <= 0 {
		rec.Samples = 1
	}
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO strategy_eval(profile_key, versions, quality, cost, latency, samples, benchmark_set, measured_at)
		VALUES ($1,$2::jsonb,$3,$4,$5,$6,$7,$8)`,
		rec.ProfileKey, string(versions), rec.Quality, rec.Cost, rec.Latency, rec.Samples, rec.BenchmarkSet, rec.At)
	return err
}
