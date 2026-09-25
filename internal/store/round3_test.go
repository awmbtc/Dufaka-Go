package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"dufaka/internal/testdb"
)

// smallPool opens a second pool on the same test schema capped at n connections.
func smallPool(t *testing.T, pool *pgxpool.Pool, n int32) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.MaxConns = n
	cfg.MinConns = 0
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	return small
}

func noDeadlock(t *testing.T, what string, err error) {
	t.Helper()
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "40P01" {
		t.Fatalf("%s deadlocked: %v", what, err)
	}
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// adminCouponSave starts the admin coupon save as the admin code does it:
// UPDATE coupons first, the coupons_goods INSERT comes later (see adminLink).
func adminCouponSave(t *testing.T, pool *pgxpool.Pool, couponID, ret int) (commit func(), link func(goodsID int) error, pid int) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err = tx.Exec(ctx, `UPDATE coupons SET ret=$2, updated_at=now() WHERE id=$1`, couponID, ret); err != nil {
		t.Fatal(err)
	}
	link = func(goodsID int) error {
		// Bounded so a regression shows up as a failure, not a hung test.
		lctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		_, err := tx.Exec(lctx, `INSERT INTO coupons_goods (goods_id, coupons_id) VALUES ($1,$2)`, goodsID, couponID)
		return err
	}
	commit = func() {
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("admin commit: %v", err)
		}
	}
	return commit, link, backendPID(t, tx)
}

// B3-1: a late payment holds the goods row (FOR NO KEY UPDATE) and waits for
// the coupon row the admin is saving; the admin's coupons_goods INSERT takes
// FOR KEY SHARE on that goods row. With FOR UPDATE the two deadlocked (40P01);
// now the INSERT goes through and both finish.
func TestAdminCouponSaveVsLatePaymentIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'C1');
 INSERT INTO coupons(id,discount,coupon,ret) VALUES(1,2,'SAVE',3);
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(1,1);`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "late@example.com", Coupon: "SAVE"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	if err := db.ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	commit, link, pid := adminCouponSave(t, pool, 1, 10)
	done := make(chan error, 1)
	go func() {
		_, err := db.Complete(ctx, o.SN, o.Actual, "late")
		done <- err
	}()
	// Complete now holds the orders and goods rows and waits on the coupon.
	waitBlockedBy(t, pool, pid)
	noDeadlock(t, "admin coupons_goods insert", link(1))
	commit()
	noDeadlock(t, "late payment", <-done)
	var status, ret, links int
	if err := pool.QueryRow(ctx, `SELECT o.status, c.ret, (SELECT count(*) FROM coupons_goods WHERE coupons_id=1) FROM orders o, coupons c WHERE o.id=$1 AND c.id=1`, o.ID).Scan(&status, &ret, &links); err != nil {
		t.Fatal(err)
	}
	if status != 4 || ret != 9 || links != 2 {
		t.Fatalf("status=%d ret=%d links=%d, want 4/9/2 (admin set 10, late payment took one)", status, ret, links)
	}
}

// B3-1: the same interleaving with the expiry sweep.
func TestAdminCouponSaveVsExpireIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'C1');
 INSERT INTO coupons(id,discount,coupon,ret) VALUES(1,2,'SAVE',3);
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(1,1);`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "exp@example.com", Coupon: "SAVE"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	commit, link, pid := adminCouponSave(t, pool, 1, 10)
	done := make(chan error, 1)
	go func() { done <- db.ExpireDue(ctx, 5) }()
	waitBlockedBy(t, pool, pid)
	noDeadlock(t, "admin coupons_goods insert", link(1))
	commit()
	noDeadlock(t, "expiry", <-done)
	var status, retBack, ret int
	var reserved *int64
	if err := pool.QueryRow(ctx, `SELECT o.status, o.coupon_ret_back, c.ret, (SELECT reserved_order_id FROM carmis WHERE goods_id=1) FROM orders o, coupons c WHERE o.id=$1 AND c.id=1`, o.ID).Scan(&status, &retBack, &ret, &reserved); err != nil {
		t.Fatal(err)
	}
	if status != -1 || retBack != 1 || ret != 11 || reserved != nil {
		t.Fatalf("status=%d coupon_ret_back=%d ret=%d reserved=%v, want -1/1/11/nil", status, retBack, ret, reserved)
	}
}

// B3-1: checkout also leaves foreign-key inserts on its goods row alone.
func TestCheckoutAllowsCouponLinkInsertIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'C1');
 INSERT INTO coupons(id,discount,coupon,ret) VALUES(1,2,'SAVE',3);
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(1,1);`)
	commit, link, pid := adminCouponSave(t, pool, 1, 10)
	done := make(chan error, 1)
	go func() {
		_, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "c@example.com", Coupon: "SAVE"}, Site{})
		done <- err
	}()
	waitBlockedBy(t, pool, pid)
	noDeadlock(t, "admin coupons_goods insert", link(1))
	commit()
	noDeadlock(t, "checkout", <-done)
	var ret int
	if err := pool.QueryRow(ctx, `SELECT ret FROM coupons WHERE id=1`).Scan(&ret); err != nil || ret != 9 {
		t.Fatalf("ret=%d err=%v, want 9", ret, err)
	}
}

// B3-2: CreateOrder does every read on its own transaction, so N concurrent
// checkouts on a 2-connection pool finish instead of wedging the pool.
func TestCreateOrderSmallPoolIntegration(t *testing.T) {
	pool := testdb.Open(t)
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) SELECT 1, 'C'||i FROM generate_series(1,6) i`)
	db := &DB{Pool: smallPool(t, pool, 2)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: fmt.Sprintf("p%d@example.com", i)}, Site{})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("checkout on a 2-connection pool: %v", err)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM carmis WHERE reserved_order_id IS NOT NULL`).Scan(&n); err != nil || n != 6 {
		t.Fatalf("reserved %d cards, want 6 (%v)", n, err)
	}
}

// B3-2: the whole lifecycle also runs on a 2-connection pool.
func TestOrderLifecycleSmallPoolIntegration(t *testing.T) {
	pool := testdb.Open(t)
	orderLifecycle(t, pool, smallPool(t, pool, 2))
}

// B3-3: a failed Redeliver writes nothing — no "manual" placeholder, no info rewrite.
func TestRedeliverStillShortLeavesOrderIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi) VALUES(1,1,'ONLY')`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "six@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	// Marked 6 by hand with a note and no trade number; the card is gone.
	mustExec(t, pool, `UPDATE orders SET status=6, trade_no='', info='店主备注' WHERE id=$1`, o.ID)
	mustExec(t, pool, `UPDATE carmis SET deleted_at=now() WHERE id=1`)
	delivered, err := db.Redeliver(ctx, o.SN)
	if delivered || !IsPaidShort(err) {
		t.Fatalf("want (false, 库存不足), got %v %v", delivered, err)
	}
	cur, err := db.OrderBySN(ctx, o.SN)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != 6 || cur.TradeNo != "" || cur.Info != "店主备注" {
		t.Fatalf("failed Redeliver changed the order: %+v", cur)
	}
	// The same holds for an order the gateway paid: trade_no stays the gateway's.
	mustExec(t, pool, `UPDATE orders SET trade_no='gw-1', info='库存不足' WHERE id=$1`, o.ID)
	if _, err := db.Redeliver(ctx, o.SN); !IsPaidShort(err) {
		t.Fatalf("want 库存不足, got %v", err)
	}
	if cur, _ = db.OrderBySN(ctx, o.SN); cur.TradeNo != "gw-1" || cur.Info != "库存不足" || cur.Status != 6 {
		t.Fatalf("failed Redeliver changed the order: %+v", cur)
	}
}

// B3-4: only a missing row is "订单不存在"; other failures are real errors.
func TestOrderBySNSplitsErrorsIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	if _, err := db.OrderBySN(ctx, "NOPE"); !IsNotFound(err) {
		t.Fatalf("missing order: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := db.OrderBySN(cancelled, "NOPE")
	if err == nil || IsNotFound(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup must be a wrapped context error, got %v", err)
	}
	if err.Error() != msgBusy {
		t.Fatalf("cancelled lookup must show the generic message, got %q", err.Error())
	}
	// Bytes Postgres rejects (invalid UTF-8) are "no such order", never an
	// echoed SQL error (/bill/AB%FFCD, order search form).
	for _, bad := range []string{"AB\xffCD", "AB\x00CD", strings.Repeat("A", 65)} {
		_, err := db.OrderBySN(ctx, bad)
		if !IsNotFound(err) || strings.Contains(err.Error(), "SQLSTATE") {
			t.Fatalf("OrderBySN(%q) = %v, want 订单不存在", bad, err)
		}
	}
	if list, err := db.OrdersByEmail(ctx, "a\xff@example.com", "", false); err != nil || len(list) != 0 {
		t.Fatalf("OrdersByEmail with invalid UTF-8: %v %v", list, err)
	}

	// A closed pool (database unreachable) must not print pgx's connection
	// details (user=…, database=…, host:port) through err.Error().
	closed := smallPool(t, pool, 1)
	closed.Close()
	_, err = (&DB{Pool: closed}).OrderBySN(ctx, "NOPE")
	if err == nil || IsNotFound(err) || err.Error() != msgBusy || strings.Contains(err.Error(), "user=") {
		t.Fatalf("closed-pool lookup: %v", err)
	}
	var ie InternalError
	if !errors.As(err, &ie) || ie.Detail() == "" {
		t.Fatalf("closed-pool lookup must keep its cause: %T %v", err, err)
	}

	mustExec(t, pool, `ALTER TABLE orders RENAME TO orders_gone`)
	_, err = db.OrderBySN(ctx, "NOPE")
	var pg *pgconn.PgError
	if err == nil || IsNotFound(err) || !errors.As(err, &pg) {
		t.Fatalf("database error reported as not found: %T %v", err, err)
	}
	if err.Error() != msgBusy || strings.Contains(err.Error(), "SQLSTATE") {
		t.Fatalf("database error text leaks: %q", err.Error())
	}
	// OrdersByEmail and CreateOrder surface the same error instead of a rule,
	// and neither prints the Postgres text.
	if _, err := db.OrdersByEmail(ctx, "a@example.com", "", false); err == nil || IsNotFound(err) || err.Error() != msgBusy {
		t.Fatalf("OrdersByEmail: %v", err)
	}
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'C1')`)
	_, err = db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "a@example.com"}, Site{})
	var rule RuleError
	if err == nil || errors.As(err, &rule) || err.Error() != msgBusy || !errors.As(err, &pg) {
		t.Fatalf("CreateOrder on a broken schema: %T %v", err, err)
	}
	// Business rules still reach the customer verbatim.
	if _, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 0, Email: "a@example.com"}, Site{}); err == nil || err.Error() != "购买数量不正确" {
		t.Fatalf("rule error changed: %v", err)
	}
	mustExec(t, pool, `ALTER TABLE goods_group RENAME TO goods_group_gone`)
	if _, err := db.Home(ctx); err == nil || err.Error() != msgBusy {
		t.Fatalf("Home on a broken schema: %v", err)
	}
}

func indexDefs(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname=current_schema() AND tablename='orders'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, d string
		if err := rows.Scan(&n, &d); err != nil {
			t.Fatal(err)
		}
		out[n] = d
	}
	return out
}

// B3-5: schema upkeep does not depend on the pays table: it works on an old
// installation whose pays.pay_check has no unique constraint, swaps the old
// single index for the two partial ones, and seeds no pays row.
func TestEnsureCldxSchemaIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	mustExec(t, pool, `ALTER TABLE pays DROP CONSTRAINT pays_pay_check_key;
 DROP INDEX idx_orders_cldx_live; DROP INDEX idx_orders_cldx_late;
 ALTER TABLE orders DROP COLUMN cldx_minor, DROP COLUMN cldx_expires_at, DROP COLUMN cldx_polled_at;
 ALTER TABLE orders ADD COLUMN cldx_minor bigint;
 CREATE INDEX idx_orders_cldx_wait ON orders (status, id) WHERE cldx_minor IS NOT NULL`)
	for i := 0; i < 2; i++ {
		if err := db.EnsureCldxSchema(ctx); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='orders' AND column_name LIKE 'cldx_%'`).Scan(&cols); err != nil || cols != 3 {
		t.Fatalf("cldx columns: %d %v", cols, err)
	}
	defs := indexDefs(t, pool)
	if _, ok := defs["idx_orders_cldx_wait"]; ok {
		t.Fatal("old idx_orders_cldx_wait kept")
	}
	if d := defs["idx_orders_cldx_live"]; !strings.Contains(d, "cldx_polled_at NULLS FIRST, id") || !strings.Contains(d, "status = 1") {
		t.Fatalf("live index: %q", d)
	}
	if d := defs["idx_orders_cldx_late"]; !strings.Contains(d, "(cldx_expires_at)") || !strings.Contains(d, "status = '-1'") && !strings.Contains(d, "status = -1") {
		t.Fatalf("late index: %q", d)
	}
	var pays int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pays WHERE pay_check='cldx'`).Scan(&pays); err != nil || pays != 0 {
		t.Fatalf("schema upkeep seeded a pays row: %d %v", pays, err)
	}
	// Seeding works without the unique constraint and never duplicates.
	for i := 0; i < 2; i++ {
		if err := db.EnsureCldxPay(ctx); err != nil {
			t.Fatalf("seed %d: %v", i+1, err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pays WHERE pay_check='cldx'`).Scan(&pays); err != nil || pays != 1 {
		t.Fatalf("cldx pays rows: %d %v", pays, err)
	}
}

// B3-5: the shipped schema already has the two partial indexes.
func TestSchemaHasCldxPollIndexesIntegration(t *testing.T) {
	pool := testdb.Open(t)
	defs := indexDefs(t, pool)
	if _, ok := defs["idx_orders_cldx_wait"]; ok {
		t.Fatal("schema still creates idx_orders_cldx_wait")
	}
	if defs["idx_orders_cldx_live"] == "" || defs["idx_orders_cldx_late"] == "" {
		t.Fatalf("poll indexes missing: %v", defs)
	}
}

// B3-6: the late bucket keeps its share next to a backlog of live orders.
func TestWaitingSNsBucketsIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	now := time.Now().Unix()
	add := func(prefix string, status, n int, exp int64) {
		mustExec(t, pool, `INSERT INTO orders (order_sn, goods_id, title, type, actual_price, email, buy_ip, pay_id, status, cldx_minor, cldx_expires_at, created_at, updated_at)
 SELECT $1||i, 1, 't', 1, 10, 'a@b.c', '1.1.1.1', 1, $2, 10000, $3, now(), now() FROM generate_series(1, $4) i`, prefix, status, exp, n)
	}
	count := func() (live, late int, total int) {
		got, err := db.WaitingSNs(ctx, "cldx")
		if err != nil {
			t.Fatal(err)
		}
		for _, sn := range got {
			switch {
			case strings.HasPrefix(sn, "LIVE"):
				live++
			case strings.HasPrefix(sn, "LATE"):
				late++
			}
		}
		return live, late, len(got)
	}
	add("LIVE", 1, 250, now+300)
	if live, late, total := count(); live != 200 || late != 0 || total != 200 {
		t.Fatalf("no late orders: live=%d late=%d total=%d, want 200/0/200", live, late, total)
	}
	add("LATE", -1, 5, now-3600)
	if live, late, total := count(); live != 195 || late != 5 || total != 200 {
		t.Fatalf("5 late: live=%d late=%d total=%d, want 195/5/200", live, late, total)
	}
	add("LATEB", -1, 295, now-3600)
	if live, late, total := count(); live != 160 || late != 40 || total != 200 {
		t.Fatalf("both full: live=%d late=%d total=%d, want 160/40/200", live, late, total)
	}
	mustExec(t, pool, `UPDATE orders SET status=4 WHERE order_sn LIKE 'LIVE%' AND id > (SELECT min(id)+9 FROM orders WHERE order_sn LIKE 'LIVE%')`)
	if live, late, total := count(); live != 10 || late != 190 || total != 200 {
		t.Fatalf("few live: live=%d late=%d total=%d, want 10/190/200", live, late, total)
	}
}

// B3-7: the store no longer refuses checkouts because of the GeeTest switch.
func TestCheckoutIgnoresGeeTestIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'C1')`)
	if _, err := db.CreateOrder(context.Background(), CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "g@example.com"}, Site{GeeTest: true}); err != nil {
		t.Fatalf("checkout refused with geetest on: %v", err)
	}
}

// B3-7: the startup sweep clears both captcha switches and the Site cache.
func TestDisableCaptchaSwitchesIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO settings(key,value) VALUES('is_open_geetest','1'),('is_open_img_code','1'),('title','T'),('is_open_search_pwd','1')`)
	if !db.Site(ctx).GeeTest {
		t.Fatal("setup: geetest not read")
	}
	n, err := db.DisableCaptchaSwitches(ctx)
	if err != nil || n != 2 {
		t.Fatalf("changed %d rows (%v), want 2", n, err)
	}
	if s := db.Site(ctx); s.GeeTest || !s.SearchPwd || s.Title != "T" {
		t.Fatalf("cache not dropped or wrong rows touched: %+v", s)
	}
	var vals string
	if err := pool.QueryRow(ctx, `SELECT string_agg(key||'='||value, ',' ORDER BY key) FROM settings`).Scan(&vals); err != nil {
		t.Fatal(err)
	}
	if vals != "is_open_geetest=0,is_open_img_code=0,is_open_search_pwd=1,title=T" {
		t.Fatalf("settings: %s", vals)
	}
	if n, err = db.DisableCaptchaSwitches(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
}

// B3-8: expiry releases only the order's own goods' reservations.
func TestExpireReleaseScopedToGoodsIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi) VALUES(1,1,'A')`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "a@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi,reserved_order_id) VALUES(2,2,'OTHER',$1)`, o.ID)
	mustExec(t, pool, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	if err := db.ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	var mine, other *int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT reserved_order_id FROM carmis WHERE id=1), (SELECT reserved_order_id FROM carmis WHERE id=2)`).Scan(&mine, &other); err != nil {
		t.Fatal(err)
	}
	if mine != nil || other == nil || int(*other) != o.ID {
		t.Fatalf("expiry release touched the wrong goods: mine=%v other=%v", mine, other)
	}
}

// B3-9: SiteOK says whether the settings were really read.
func TestSiteOKIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO settings(key,value) VALUES('order_expire_time','30')`)
	if s, ok := db.SiteOK(ctx); !ok || s.ExpireMin != 30 {
		t.Fatalf("fresh read: %+v %v", s, ok)
	}
	if s, ok := db.SiteOK(ctx); !ok || s.ExpireMin != 30 {
		t.Fatalf("cached read: %+v %v", s, ok)
	}
	db.InvalidateSite()
	mustExec(t, pool, `ALTER TABLE settings RENAME TO settings_gone`)
	if s, ok := db.SiteOK(ctx); ok || s.ExpireMin != 5 {
		t.Fatalf("failed read: %+v %v, want defaults and ok=false", s, ok)
	}
	if s := db.Site(ctx); s.ExpireMin != 5 {
		t.Fatalf("Site on failure: %+v", s)
	}
}

// B3-6 (repair): the late share is interleaved through the list, not
// appended, so a poller that only gets through a prefix of it (slow wallet,
// deadline) still reaches late payments, and repeated short ticks cover them
// all instead of re-queuing them at the end forever.
func TestWaitingSNsInterleavesLateIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	now := time.Now().Unix()
	add := func(prefix string, status, n int, exp int64) {
		mustExec(t, pool, `INSERT INTO orders (order_sn, goods_id, title, type, actual_price, email, buy_ip, pay_id, status, cldx_minor, cldx_expires_at, created_at, updated_at)
 SELECT $1||i, 1, 't', 1, 10, 'a@b.c', '1.1.1.1', 1, $2, 10000, $3, now(), now() FROM generate_series(1, $4) i`, prefix, status, exp, n)
	}
	add("LIVE", 1, 400, now+300)
	add("LATE", -1, 60, now-3600)
	got, err := db.WaitingSNs(ctx, "cldx")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 200 {
		t.Fatalf("got %d SNs", len(got))
	}
	late := 0
	for i, sn := range got {
		if strings.HasPrefix(sn, "LATE") {
			late++
		}
		// Every prefix of k SNs holds at least floor(k/5) late ones.
		if k := i + 1; late < k/5 {
			t.Fatalf("prefix of %d SNs has only %d late orders: %v", k, late, got[:k])
		}
	}
	if !strings.HasPrefix(got[1], "LATE") {
		t.Fatalf("a late order must come within the first two SNs: %v", got[:6])
	}

	// A poller that only manages 10 orders per tick (and stamps what it
	// polls, as SyncCldx does) reaches every late order within a bounded
	// number of ticks.
	polled := map[string]bool{}
	for tick := 0; tick < 40; tick++ {
		sns, err := db.WaitingSNs(ctx, "cldx")
		if err != nil {
			t.Fatal(err)
		}
		for _, sn := range sns[:10] {
			if strings.HasPrefix(sn, "LATE") {
				polled[sn] = true
			}
			if err := db.MarkCldxPolled(ctx, sn, now+int64(tick)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(polled) != 60 {
		t.Fatalf("after 40 short ticks only %d of 60 late orders were polled", len(polled))
	}
}
