// Package testdb provides disposable schemas for integration tests on a dedicated local database.
package testdb

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func Open(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DUFAKA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DUFAKA_TEST_DATABASE_URL to a dedicated test database")
	}
	ctx := context.Background()
	root, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("audit_%d", time.Now().UnixNano())
	if _, err = root.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		root.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); root.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); root.Close() })
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../install/schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	return pool
}
