package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

type MessageRow struct {
	MessageID  string
	SessionID  string
	Seq        int
	Role       domain.Role
	Content    string
	Name       string
	ToolCallID string
	ToolCalls  []domain.ToolCall
	CreatedAt  time.Time
}

type SessionRepo struct{ DB *DB }

func (r *SessionRepo) Ensure(ctx context.Context, sessionID, caller, title string) error {
	if sessionID == "" {
		return nil
	}
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO sessions(session_id, caller, title) VALUES ($1, NULLIF($2,''), NULLIF($3,''))
		ON CONFLICT (session_id) DO UPDATE SET updated_at=now()`, sessionID, caller, title)
	return err
}

// Append 在事务里分配 seq。advisory lock 按 session 串行化，
// 避免两个副本同时插入同一 seq。
func (r *SessionRepo) Append(ctx context.Context, sessionID string, msg domain.Message) (MessageRow, error) {
	row := MessageRow{MessageID: ids.New("msg"), SessionID: sessionID, Role: msg.Role, Content: msg.Content, Name: msg.Name, ToolCallID: msg.ToolCallID, ToolCalls: msg.ToolCalls}
	tx, err := r.DB.Pool.Begin(ctx)
	if err != nil {
		return row, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, sessionID); err != nil {
		return row, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions(session_id) VALUES ($1) ON CONFLICT DO NOTHING`, sessionID); err != nil {
		return row, err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE session_id=$1`, sessionID).Scan(&row.Seq); err != nil {
		return row, err
	}
	var calls any
	if len(msg.ToolCalls) > 0 {
		raw, _ := json.Marshal(msg.ToolCalls)
		calls = raw
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO messages(message_id, session_id, seq, role, content, name, tool_call_id, tool_calls)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8::jsonb)`,
		row.MessageID, sessionID, row.Seq, string(msg.Role), msg.Content, msg.Name, msg.ToolCallID, jsonValue(calls)); err != nil {
		return row, err
	}
	row.CreatedAt = time.Now().UTC()
	return row, tx.Commit(ctx)
}

func (r *SessionRepo) History(ctx context.Context, sessionID string, limit int) ([]MessageRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.DB.Pool.Query(ctx, `
		SELECT message_id, session_id, seq, role, content, COALESCE(name,''), COALESCE(tool_call_id,''), tool_calls, created_at
		FROM messages WHERE session_id=$1 ORDER BY seq DESC LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		var role string
		var calls []byte
		if err := rows.Scan(&m.MessageID, &m.SessionID, &m.Seq, &role, &m.Content, &m.Name, &m.ToolCallID, &calls, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Role = domain.Role(role)
		if len(calls) > 0 {
			_ = json.Unmarshal(calls, &m.ToolCalls)
		}
		out = append(out, m)
	}
	// 返回正序。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
