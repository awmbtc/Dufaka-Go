package store

import (
	"context"
	"testing"
	"time"

	"dufaka/internal/testdb"
)

// An upgrade whose schema step cannot get its table lock (a backup or a long
// transaction on orders) must fail fast instead of queueing every order read, and the
// next attempt must finish the job. EnsureSchemaOnce is then free.
func TestEnsureSchemaOnceHealsAfterLockTimeoutIntegration(t *testing.T) {
	pool := testdb.Open(t)
	ctx := context.Background()
	db := &DB{Pool: pool}
	if _, err := pool.Exec(ctx, `ALTER TABLE orders DROP COLUMN stock_owed; DROP INDEX IF EXISTS idx_orders_cldx_late`); err != nil {
		t.Fatal(err)
	}
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `LOCK TABLE orders IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := db.EnsureSchemaOnce(ctx); err == nil {
		t.Fatal("schema step must fail while another transaction holds orders")
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("lock wait not bounded: %v", waited)
	}
	if db.schemaOK.Load() {
		t.Fatal("a failed attempt must not be remembered as done")
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureSchemaOnce(ctx); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	var owed, late int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='orders' AND column_name='stock_owed'),
		(SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='idx_orders_cldx_late')`).Scan(&owed, &late); err != nil {
		t.Fatal(err)
	}
	if owed != 1 || late != 1 {
		t.Fatalf("schema not healed: stock_owed=%d idx_late=%d", owed, late)
	}
	if !db.schemaOK.Load() {
		t.Fatal("success must be remembered")
	}
}
