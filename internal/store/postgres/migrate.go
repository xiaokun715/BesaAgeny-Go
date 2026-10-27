package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/besa-ai/besa-agent/migrations"
)

// Migrate 顺序执行内嵌 SQL，并在 schema_migrations 里记录。
// 用 advisory lock 防止多个副本同时迁移。
func (db *DB) Migrate(ctx context.Context) error {
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(743921)`); err != nil {
		return fmt.Errorf("获取迁移锁: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(743921)`) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		raw, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		start := time.Now()
		// PgConn().Exec 使用简单协议，支持一个文件里的多条语句。
		if _, err := conn.Conn().PgConn().Exec(ctx, string(raw)).ReadAll(); err != nil {
			return fmt.Errorf("执行迁移 %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1) ON CONFLICT DO NOTHING`, name); err != nil {
			return err
		}
		_ = start
	}
	return nil
}
