package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/gateway"
)

// UsageRepo 把账本里的记录与丢弃数落库。
// token 与成本列允许 NULL：**NULL 表示未知，不是 0**。
type UsageRepo struct{ DB *DB }

func (r *UsageRepo) Record(ctx context.Context, records []gateway.UsageRecord) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := r.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, rec := range records {
		var amount any
		currency := ""
		if rec.Cost.Amount != nil {
			amount = rec.Cost.Amount.String()
			currency = rec.Cost.Currency
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO usage(trace_id, alias, model_key, provider, model, input_tokens, output_tokens,
			                  cached_input_tokens, cost_amount, currency, degraded, failed, attempt_index,
			                  session_id, caller, occurred_at)
			VALUES (NULLIF($1,''),NULLIF($2,''),NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),
			        $6,$7,$8,$9::numeric,NULLIF($10,''),$11,$12,$13,NULLIF($14,''),NULLIF($15,''),$16)`,
			rec.TraceID, rec.Alias, rec.ModelKey, rec.Provider, rec.Model,
			rec.InputTokens, rec.OutputTokens, rec.CachedInputTokens, amount, currency,
			rec.Degraded, rec.Failed, rec.AttemptIndex, rec.SessionID, rec.Caller, rec.At); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *UsageRepo) RecordDrops(ctx context.Context, count int) error {
	if count <= 0 {
		return nil
	}
	_, err := r.DB.Pool.Exec(ctx, `INSERT INTO usage_drops(dropped_count, reason) VALUES ($1,'ledger_overflow')`, count)
	return err
}

// EventRepo 既是事件存档，也是 transactional outbox。
type EventRepo struct{ DB *DB }

func (r *EventRepo) SaveBatch(ctx context.Context, events []event.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := r.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, ev := range events {
		payload, _ := json.Marshal(ev.Payload)
		occurred := ev.OccurredAt
		if occurred.IsZero() {
			occurred = time.Now().UTC()
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO events(event_id, name, trace_id, session_id, caller, alias, subject, outcome,
			                   attempt_index, payload, occurred_at, partition_key)
			VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),
			        $9,$10::jsonb,$11,NULLIF($12,''))
			ON CONFLICT (event_id) DO NOTHING`,
			ev.ID, ev.Name, ev.TraceID, ev.SessionID, ev.Caller, ev.Alias, ev.Subject, ev.Outcome,
			ev.AttemptIdx, string(payload), occurred, ev.SessionID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type OutboxRow struct {
	ID      int64
	Event   event.Event
	Attempt int
}

// FetchUnpublished 用 FOR UPDATE SKIP LOCKED，允许多个 relay 副本并行。
func (r *EventRepo) FetchUnpublished(ctx context.Context, limit int) ([]OutboxRow, error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := r.DB.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT id, event_id, name, COALESCE(trace_id,''), COALESCE(session_id,''), COALESCE(caller,''),
		       COALESCE(alias,''), COALESCE(subject,''), COALESCE(outcome,''), attempt_index, payload,
		       occurred_at, publish_attempts
		FROM events WHERE published_at IS NULL
		ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	var out []OutboxRow
	for rows.Next() {
		var row OutboxRow
		var payload []byte
		ev := event.Event{}
		if err := rows.Scan(&row.ID, &ev.ID, &ev.Name, &ev.TraceID, &ev.SessionID, &ev.Caller,
			&ev.Alias, &ev.Subject, &ev.Outcome, &ev.AttemptIdx, &payload, &ev.OccurredAt, &row.Attempt); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(payload, &ev.Payload)
		row.Event = ev
		out = append(out, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *EventRepo) MarkPublished(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.DB.Pool.Exec(ctx, `UPDATE events SET published_at=now(), last_error=NULL WHERE id = ANY($1)`, ids)
	return err
}

func (r *EventRepo) MarkPublishFailed(ctx context.Context, ids []int64, errMsg string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE events SET publish_attempts=publish_attempts+1, last_error=$2 WHERE id = ANY($1)`, ids, errMsg)
	return err
}
