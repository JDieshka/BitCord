package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a connection pool from a postgres URL.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

var (
	upHeader   = regexp.MustCompile(`(?m)^#\s*\+goose\s+Up\s*$`)
	downHeader = regexp.MustCompile(`(?m)^#\s*\+goose\s+Down\s*$`)
)

// Migrate applies all *.sql migrations in dir (goose-style "# +goose Up/Down")
// tracked in schema_migrations. Simple built-in runner so no external binary is needed.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version BIGINT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	sort.Strings(files)

	for _, f := range files {
		base := filepath.Base(f)
		var version int64
		if _, err := fmt.Sscanf(base, "%d_", &version); err != nil {
			continue
		}
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		upSQL := extractSection(string(data), upHeader, downHeader)
		if strings.TrimSpace(upSQL) == "" {
			continue
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, upSQL); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", base, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// extractSection returns text between the `start` header and the `until` header (or EOF).
func extractSection(s string, start, until *regexp.Regexp) string {
	loc := start.FindStringIndex(s)
	if loc == nil {
		return ""
	}
	rest := s[loc[1]:]
	if l2 := until.FindStringIndex(rest); l2 != nil {
		return rest[:l2[0]]
	}
	return rest
}
