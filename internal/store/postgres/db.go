// Package postgres 是持久化的权威实现。
//
// Postgres 是权威，Redis 只是快路径；事件表兼作 transactional outbox。
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
}

// NormalizeDSN 把 SQLAlchemy 风格的 DSN 转成 pgx 可用的。
func NormalizeDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	dsn = strings.Replace(dsn, "postgresql+asyncpg://", "postgresql://", 1)
	dsn = strings.Replace(dsn, "postgresql+psycopg://", "postgresql://", 1)
	dsn = strings.Replace(dsn, "postgres+asyncpg://", "postgresql://", 1)
	return dsn
}

type Options struct {
	MaxConns int32
	MinConns int32
	// RejectDatabase 用于防止连错库（默认拒绝库名 "besa"）。
	RejectDatabase string
}

func Open(ctx context.Context, dsn string, opts Options) (*DB, error) {
	dsn = NormalizeDSN(dsn)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析 Postgres DSN: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("连接 Postgres: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("Postgres 健康检查失败: %w", err)
	}
	reject := opts.RejectDatabase
	if reject == "" {
		reject = "besa"
	}
	if cfg.ConnConfig.Database == reject {
		pool.Close()
		return nil, fmt.Errorf("拒绝连接库名 %q：它属于另一个项目，共用会让 alembic/schema 版本互相覆盖", reject)
	}
	return &DB{Pool: pool}, nil
}

func (db *DB) Close() { db.Pool.Close() }

func (db *DB) Ping(ctx context.Context) error { return db.Pool.Ping(ctx) }

// DatabaseName 供 doctor 输出。
func (db *DB) DatabaseName() string {
	cfg := db.Pool.Config()
	return cfg.ConnConfig.Database
}

func (db *DB) Host() string {
	cfg := db.Pool.Config()
	return cfg.ConnConfig.Host
}

func (db *DB) DSN() string {
	cfg := db.Pool.Config()
	u := &url.URL{Scheme: "postgresql", Host: cfg.ConnConfig.Host, Path: cfg.ConnConfig.Database}
	return u.String()
}

// jsonValue 把任意值编成 JSON 字符串；nil 保持 nil（SQL NULL），
// 其余一律用 `$n::jsonb` 显式转换。**不要把 []byte 直接绑到 jsonb** ——
// pgx 会按 bytea 发送，Postgres 会报类型不匹配。
func jsonValue(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	if r, ok := v.(json.RawMessage); ok {
		return string(r)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(raw)
}
