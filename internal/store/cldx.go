package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// CldxLateWindow is how long after its cldx deadline an expired (status -1)
// order keeps being polled for a late wallet payment. Older expired orders are
// left to manual reconciliation so the poll set cannot grow without bound.
const CldxLateWindow = 24 * time.Hour

// cldxPollLimit caps how many orders one SyncCldx tick may touch. Live
// (status 1) orders are guaranteed cldxLiveShare of it and late-window (-1)
// orders cldxLateShare, so a backlog in one bucket can never starve the
// other; a bucket with fewer rows leaves its unused share to the other one.
const (
	cldxPollLimit = 200
	cldxLiveShare = 160
	cldxLateShare = cldxPollLimit - cldxLiveShare
)

// cldxIndexes are the two partial indexes behind the two WaitingSNs buckets.
// Their definitions are repeated in install/schema.sql, sql/001_schema.sql and
// sql/003_cldx_poll.sql.
var cldxIndexes = []struct{ name, ddl string }{
	{"idx_orders_cldx_live", `CREATE INDEX IF NOT EXISTS idx_orders_cldx_live ON orders (cldx_polled_at NULLS FIRST, id) WHERE status = 1 AND cldx_minor IS NOT NULL AND deleted_at IS NULL`},
	{"idx_orders_cldx_late", `CREATE INDEX IF NOT EXISTS idx_orders_cldx_late ON orders (cldx_expires_at) WHERE status = -1 AND cldx_minor IS NOT NULL AND deleted_at IS NULL`},
}

// EnsureCldxSchema brings an installed site's orders table up to what the cldx
// code reads: the three cldx columns and the two poll indexes, replacing the
// older single idx_orders_cldx_wait. It runs on every start of an installed
// site whether or not the wallet is configured (orders from an earlier
// configuration still carry cldx columns the storefront reads), and it does
// not depend on any constraint of the pays table. Each step is skipped when it
// is already in place, so a normal start takes no table lock. On a large
// orders table run sql/003_cldx_poll.sql (CREATE INDEX CONCURRENTLY) before
// upgrading so this step finds the indexes already built.
func (db *DB) EnsureCldxSchema(ctx context.Context) error {
	var errs []error
	cols, err := db.orderColumns(ctx)
	if err != nil {
		return err
	}
	var add []string
	for _, c := range schemaColumns {
		if !cols[c.name] {
			add = append(add, "ADD COLUMN IF NOT EXISTS "+c.name+" "+c.ddl)
		}
	}
	if len(add) > 0 {
		// One statement for every missing column (all metadata-only), so a partial
		// upgrade never leaves the storefront and the back office disagreeing.
		errs = append(errs, db.ddl(ctx, "ALTER TABLE orders "+strings.Join(add, ", ")))
	}
	for _, ix := range cldxIndexes {
		var exists bool
		if err := db.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, ix.name).Scan(&exists); err != nil {
			errs = append(errs, err)
			continue
		}
		if !exists {
			errs = append(errs, db.ddl(ctx, ix.ddl))
		}
	}
	var old bool
	if err := db.Pool.QueryRow(ctx, `SELECT to_regclass('idx_orders_cldx_wait') IS NOT NULL`).Scan(&old); err != nil {
		errs = append(errs, err)
	} else if old {
		errs = append(errs, db.ddl(ctx, `DROP INDEX IF EXISTS idx_orders_cldx_wait`))
	}
	return errors.Join(errs...)
}

// schemaColumns are the orders columns added after the original dujiaoka schema; their
// definitions are repeated in install/schema.sql, sql/001_schema.sql and sql/002-004.
var schemaColumns = []struct{ name, ddl string }{
	{"cldx_minor", "bigint"},
	{"cldx_expires_at", "bigint"},
	{"cldx_polled_at", "bigint"},
	{"stock_owed", "boolean NOT NULL DEFAULT false"},
}

// schemaLockTimeout bounds how long one upkeep statement waits for its table lock, so a
// long transaction on orders (a backup, an old process) makes the step fail and retry on
// the next tick instead of queueing every order read behind it.
const schemaLockTimeout = "3s"

func (db *DB) ddl(ctx context.Context, stmt string) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+schemaLockTimeout+"'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (db *DB) orderColumns(ctx context.Context) (map[string]bool, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'orders'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out[c] = true
	}
	return out, rows.Err()
}

// EnsureSchemaOnce runs EnsureCldxSchema until it succeeds once per process; after that
// it is free. The background tick calls it so an upgrade whose first start could not
// finish the schema (database not up yet, a table lock held) repairs itself.
func (db *DB) EnsureSchemaOnce(ctx context.Context) error {
	if db.schemaOK.Load() {
		return nil
	}
	if err := db.EnsureCldxSchema(ctx); err != nil {
		return err
	}
	db.schemaOK.Store(true)
	return nil
}

// EnsureCldxPay seeds the cldx pays row when there is none. It is a plain
// INSERT … WHERE NOT EXISTS with no ON CONFLICT arbiter, so it also works on an
// older pays table without a unique pay_check; a unique violation from a
// concurrent seed means the row is there and counts as success.
func (db *DB) EnsureCldxPay(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO pays (pay_name, pay_check, pay_method, pay_client, merchant_id, merchant_key, merchant_pem, pay_handleroute, is_open, created_at, updated_at)
		SELECT 'cldx', 'cldx', 2, 3, '', '', '', '/pay/cldx', 1, now(), now()
		WHERE NOT EXISTS (SELECT 1 FROM pays WHERE pay_check='cldx')`)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return nil
	}
	return err
}

// MarkCldxPolled records when the wallet was last asked about an order so the
// poll set rotates instead of re-polling the same head every tick.
func (db *DB) MarkCldxPolled(ctx context.Context, sn string, at int64) error {
	_, err := db.Pool.Exec(ctx, `UPDATE orders SET cldx_polled_at=$2 WHERE order_sn=$1`, sn, at)
	return err
}

// LockCldxQuote makes the first successful quote authoritative, including concurrent requests.
func (db *DB) LockCldxQuote(ctx context.Context, sn string, minor, expires int64) (int64, int64, error) {
	var amount, exp int64
	err := db.Pool.QueryRow(ctx, `UPDATE orders SET cldx_minor=COALESCE(cldx_minor,$2),
 cldx_expires_at=COALESCE(cldx_expires_at,$3), updated_at=now()
 WHERE order_sn=$1 AND status=1 RETURNING cldx_minor,cldx_expires_at`, sn, minor, expires).Scan(&amount, &exp)
	return amount, exp, err
}

func (db *DB) CldxMinor(ctx context.Context, sn string) (int64, error) {
	var minor *int64
	err := db.Pool.QueryRow(ctx, `SELECT cldx_minor FROM orders WHERE order_sn=$1`, sn).Scan(&minor)
	if err != nil || minor == nil {
		return 0, err
	}
	return *minor, nil
}

// WaitingSNs lists cldx orders still worth polling: unpaid orders (status 1)
// with a locked quote, plus expired orders (status -1) whose cldx deadline is
// within CldxLateWindow. The two buckets are read separately (UNION ALL), each
// least recently polled first (never polled first, then by id): live orders
// get up to cldxLiveShare rows and late ones up to cldxLateShare, and a bucket
// that has fewer rows than its share leaves the rest to the other, never more
// than cldxPollLimit in total.
//
// The result interleaves the buckets in proportion to their shares (one
// late SN after every four live ones: L l L L L L l …), so the late share is
// spread over the whole list instead of sitting at its end. SyncCldx walks
// the list in order and stops at its deadline; with this order any prefix it
// manages to poll holds about a fifth late orders (while there are any), and
// the ones it polls are stamped and rotate to the back of their bucket. A
// slow wallet or a backlog of unpaid orders can therefore slow late payments
// down but never starve them.
func (db *DB) WaitingSNs(ctx context.Context, check string) ([]string, error) {
	cutoff := time.Now().Add(-CldxLateWindow).Unix()
	rows, err := db.Pool.Query(ctx, `
		WITH live AS (
			SELECT o.order_sn, row_number() OVER (ORDER BY o.cldx_polled_at ASC NULLS FIRST, o.id ASC) AS rn
			FROM orders o JOIN pays p ON p.id = o.pay_id
			WHERE p.pay_check=$1 AND o.status = 1 AND o.cldx_minor IS NOT NULL AND o.deleted_at IS NULL
			ORDER BY o.cldx_polled_at ASC NULLS FIRST, o.id ASC
			LIMIT $3
		), late AS (
			SELECT o.order_sn, row_number() OVER (ORDER BY o.cldx_polled_at ASC NULLS FIRST, o.id ASC) AS rn
			FROM orders o JOIN pays p ON p.id = o.pay_id
			WHERE p.pay_check=$1 AND o.status = -1 AND o.cldx_minor IS NOT NULL AND o.deleted_at IS NULL
			  AND o.cldx_expires_at > $2
			ORDER BY o.cldx_polled_at ASC NULLS FIRST, o.id ASC
			LIMIT $3
		), n AS (
			SELECT (SELECT count(*) FROM live) AS live_n, (SELECT count(*) FROM late) AS late_n
		)
		SELECT sn FROM (
			SELECT live.order_sn AS sn, 0 AS bucket, live.rn FROM live, n
			WHERE live.rn <= GREATEST($4, $3 - LEAST(n.late_n, $5))
			UNION ALL
			SELECT late.order_sn, 1, late.rn FROM late, n
			WHERE late.rn <= GREATEST($5, $3 - LEAST(n.live_n, $4))
		) picked
		ORDER BY (rn - 1)::float8 / CASE bucket WHEN 0 THEN $4 ELSE $5 END, bucket, rn`, check, cutoff, cldxPollLimit, cldxLiveShare, cldxLateShare)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sn string
		if err := rows.Scan(&sn); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}
