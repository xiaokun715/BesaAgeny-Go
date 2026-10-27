package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/besa-ai/besa-agent/internal/memory"
)

type MemoryRepo struct {
	DB       *DB
	Space    string
	Dim      int
	ModelKey string
}

func (r *MemoryRepo) EnsureSpace(ctx context.Context) error {
	if r.Space == "" {
		r.Space = "default"
	}
	if r.Dim != 768 && r.Dim != 1024 {
		return fmt.Errorf("向量维度 %d 不受支持（只支持 768/1024；4096 建不出 pgvector HNSW 索引）", r.Dim)
	}
	var dim int
	var metric string
	err := r.DB.Pool.QueryRow(ctx, `SELECT dim, metric FROM vector_spaces WHERE space=$1`, r.Space).Scan(&dim, &metric)
	if err == nil {
		if dim != r.Dim {
			return fmt.Errorf("向量空间 %s 维度不一致：库中 %d，配置 %d", r.Space, dim, r.Dim)
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	_, err = r.DB.Pool.Exec(ctx, `
		INSERT INTO vector_spaces(space, dim, metric, model_key) VALUES ($1,$2,'cosine',$3)
		ON CONFLICT (space) DO NOTHING`, r.Space, r.Dim, r.ModelKey)
	return err
}

func (r *MemoryRepo) embeddingsTable() string {
	return fmt.Sprintf("embeddings_%d", r.Dim)
}

func (r *MemoryRepo) FindByHash(ctx context.Context, tenant string, scope memory.Scope, hash string) (*memory.Item, error) {
	row := r.DB.Pool.QueryRow(ctx, `
		SELECT `+itemColumns+` FROM memory
		WHERE tenant_id=$1 AND scope=$2 AND content_hash=$3 AND state='active'
		ORDER BY updated_at DESC LIMIT 1`, tenant, string(scope), hash)
	return scanMemoryItem(row)
}

func (r *MemoryRepo) SaveWithVector(ctx context.Context, item *memory.Item, vector []float32) error {
	tx, err := r.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory(memory_id, tenant_id, scope, owner_id, session_id, content, content_hash, summary,
		                  kind, category, tags, confidence, importance, state, version, supersedes_id,
		                  source_kind, source_session_id, source_trace_id, evidence_count, weight,
		                  valid_from, valid_to, created_at, updated_at, meta)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7,NULLIF($8,''),$9,NULLIF($10,''),$11::jsonb,
		        $12,$13,$14,$15,NULLIF($16,''),$17,NULLIF($18,''),NULLIF($19,''),$20,$21,
		        $22,$23,$24,$25,'{}'::jsonb)`,
		item.ID, item.TenantID, string(item.Scope), item.OwnerID, item.SessionID, item.Content, item.ContentHash,
		item.Summary, string(item.Kind), item.Category, jsonValue(item.Tags), item.Confidence, item.Importance,
		string(item.State), item.Version, item.SupersedesID, item.SourceKind, item.SourceSessionID,
		item.SourceTraceID, item.EvidenceCount, item.Weight, item.ValidFrom, item.ValidTo,
		item.CreatedAt, item.UpdatedAt); err != nil {
		return err
	}
	if len(vector) > 0 {
		if err := r.upsertVector(ctx, tx, item.ID, vector); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *MemoryRepo) upsertVector(ctx context.Context, tx pgx.Tx, id string, vector []float32) error {
	sql := `INSERT INTO ` + r.embeddingsTable() + `(space, owner_kind, owner_id, model_key, embedding)
		VALUES ($1,'memory',$2,$3,CAST($4 AS vector))
		ON CONFLICT (space, owner_kind, owner_id)
		DO UPDATE SET embedding=EXCLUDED.embedding, model_key=EXCLUDED.model_key`
	_, err := tx.Exec(ctx, sql, r.Space, id, r.ModelKey, VectorLiteral(vector))
	return err
}

func (r *MemoryRepo) Reinforce(ctx context.Context, id string, strength float64, now time.Time) error {
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE memory SET evidence_count=evidence_count+1,
		                  weight=5*(1-exp(-((weight+$2+1)/5))),
		                  updated_at=$3
		WHERE memory_id=$1`, id, strength, now)
	return err
}

func (r *MemoryRepo) MarkAccessed(ctx context.Context, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE memory SET access_count=access_count+1, used_count=used_count+1, last_accessed_at=$2
		WHERE memory_id = ANY($1)`, ids, now)
	return err
}

func (r *MemoryRepo) Supersede(ctx context.Context, oldID, newID string, now time.Time) error {
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE memory SET state='superseded', superseded_by=$2, valid_to=$3, updated_at=$3
		WHERE memory_id=$1`, oldID, newID, now)
	return err
}

func (r *MemoryRepo) Merge(ctx context.Context, ids []string, into string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.DB.Pool.Exec(ctx, `
		UPDATE memory SET state='merged', merged_into=$2, updated_at=$3
		WHERE memory_id = ANY($1) AND memory_id <> $2`, ids, into, now)
	return err
}

func (r *MemoryRepo) Tombstone(ctx context.Context, id string, now time.Time) error {
	_, err := r.DB.Pool.Exec(ctx, `UPDATE memory SET state='tombstone', updated_at=$2 WHERE memory_id=$1`, id, now)
	return err
}

func (r *MemoryRepo) ListActive(ctx context.Context, tenant string, before time.Time, limit int) ([]memory.Item, error) {
	if limit <= 0 {
		limit = 500
	}
	query := `SELECT ` + itemColumns + ` FROM memory WHERE tenant_id=$1 AND state='active'`
	args := []any{tenant}
	if !before.IsZero() {
		query += ` AND updated_at <= $2`
		args = append(args, before)
	}
	query += fmt.Sprintf(` ORDER BY updated_at ASC LIMIT %d`, limit)
	rows, err := r.DB.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Item
	for rows.Next() {
		item, err := scanMemoryItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

func (r *MemoryRepo) AppendOp(ctx context.Context, op memory.OpLog) error {
	payload, _ := json.Marshal(op.Payload)
	_, err := r.DB.Pool.Exec(ctx, `
		INSERT INTO memory_op_log(op_id, memory_id, op, before_hash, after_hash, reason, actor,
		                          source_trace_id, scope, tenant_id, payload, occurred_at)
		VALUES ($1,NULLIF($2,''),$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),
		        NULLIF($8,''),NULLIF($9,''),$10,$11::jsonb,$12)
		ON CONFLICT (op_id) DO NOTHING`,
		op.OpID, op.MemoryID, op.Op, op.BeforeHash, op.AfterHash, op.Reason, op.Actor,
		op.SourceTraceID, string(op.Scope), op.TenantID, string(payload), op.OccurredAt)
	return err
}

func (r *MemoryRepo) Search(ctx context.Context, q memory.SearchQuery) ([]memory.Scored, error) {
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}
	filter := `m.tenant_id=$1 AND m.state='active' AND (m.valid_to IS NULL OR m.valid_to > $2)`
	args := []any{q.TenantID, now}
	next := 3
	if len(q.Scopes) > 0 {
		scopes := make([]string, len(q.Scopes))
		for i, s := range q.Scopes {
			scopes[i] = string(s)
		}
		filter += fmt.Sprintf(" AND m.scope = ANY($%d)", next)
		args = append(args, scopes)
		next++
	}
	if len(q.Kinds) > 0 {
		kinds := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			kinds[i] = string(k)
		}
		filter += fmt.Sprintf(" AND m.kind = ANY($%d)", next)
		args = append(args, kinds)
		next++
	}
	if q.OwnerID != "" {
		filter += fmt.Sprintf(" AND (m.owner_id IS NULL OR m.owner_id='' OR m.owner_id=$%d)", next)
		args = append(args, q.OwnerID)
		next++
	}
	if q.SessionID != "" {
		filter += fmt.Sprintf(" AND (m.scope <> 'session' OR m.session_id=$%d)", next)
		args = append(args, q.SessionID)
		next++
	}

	vectorScore := map[string]float64{}
	if len(q.Vector) > 0 {
		limit := q.TopK
		if limit <= 0 {
			limit = 20
		}
		limit *= 4
		rows, err := r.DB.Pool.Query(ctx, fmt.Sprintf(`
			SELECT owner_id, embedding <=> CAST($1 AS vector) AS dist
			FROM %s WHERE space=$2 AND owner_kind='memory'
			ORDER BY embedding <=> CAST($1 AS vector) LIMIT %d`, r.embeddingsTable(), limit),
			VectorLiteral(q.Vector), r.Space)
		if err == nil {
			for rows.Next() {
				var owner string
				var dist float64
				if rows.Scan(&owner, &dist) == nil {
					score := 1 - dist
					if score < 0 {
						score = 0
					}
					vectorScore[owner] = score
				}
			}
			rows.Close()
		}
	}

	limit := 500
	query := `SELECT ` + prefixedItemColumns + ` FROM memory m WHERE ` + filter + ` ORDER BY m.updated_at DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := r.DB.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	weight := q.LexicalWeight
	if weight < 0 || weight > 1 {
		weight = 0.3
	}
	var out []memory.Scored
	for rows.Next() {
		item, err := scanMemoryItem(rows)
		if err != nil {
			return nil, err
		}
		lexical := memory.LexicalOverlap(q.Query, item.Content)
		vec := vectorScore[item.ID]
		score := (1-weight)*vec + weight*lexical
		if score <= 0 {
			continue
		}
		out = append(out, memory.Scored{Item: *item, Score: score, MatchReason: "vector+lexical"})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if q.TopK > 0 && len(out) > q.TopK {
		out = out[:q.TopK]
	}
	return out, nil
}

const itemColumns = `memory_id, tenant_id, scope, COALESCE(owner_id,''), COALESCE(session_id,''), content,
	content_hash, COALESCE(summary,''), kind, COALESCE(category,''), tags, confidence, importance,
	state, version, COALESCE(supersedes_id,''), COALESCE(source_kind,''), COALESCE(source_session_id,''),
	COALESCE(source_trace_id,''), evidence_count, access_count, used_count, weight, valid_from, valid_to,
	last_accessed_at, last_decayed_at, created_at, updated_at`

const prefixedItemColumns = `m.memory_id, m.tenant_id, m.scope, COALESCE(m.owner_id,''), COALESCE(m.session_id,''), m.content,
	m.content_hash, COALESCE(m.summary,''), m.kind, COALESCE(m.category,''), m.tags, m.confidence, m.importance,
	m.state, m.version, COALESCE(m.supersedes_id,''), COALESCE(m.source_kind,''), COALESCE(m.source_session_id,''),
	COALESCE(m.source_trace_id,''), m.evidence_count, m.access_count, m.used_count, m.weight, m.valid_from, m.valid_to,
	m.last_accessed_at, m.last_decayed_at, m.created_at, m.updated_at`

func scanMemoryItem(row rowScanner) (*memory.Item, error) {
	var item memory.Item
	var scope, kind, state string
	var tags []byte
	var validTo, lastAccess, lastDecay *time.Time
	err := row.Scan(&item.ID, &item.TenantID, &scope, &item.OwnerID, &item.SessionID, &item.Content,
		&item.ContentHash, &item.Summary, &kind, &item.Category, &tags, &item.Confidence, &item.Importance,
		&state, &item.Version, &item.SupersedesID, &item.SourceKind, &item.SourceSessionID,
		&item.SourceTraceID, &item.EvidenceCount, &item.AccessCount, &item.UsedCount, &item.Weight,
		&item.ValidFrom, &validTo, &lastAccess, &lastDecay, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	item.Scope = memory.Scope(scope)
	item.Kind = memory.Kind(kind)
	item.State = memory.State(state)
	item.ValidTo = validTo
	item.LastAccessedAt = lastAccess
	item.LastDecayedAt = lastDecay
	_ = json.Unmarshal(tags, &item.Tags)
	return &item, nil
}

// VectorLiteral 生成 pgvector 的文本字面量。调用方必须用 CAST($n AS vector)。
func VectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fmt.Sprintf("%.6f", v))
	}
	b.WriteByte(']')
	return b.String()
}
