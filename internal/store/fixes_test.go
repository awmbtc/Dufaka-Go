package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"dufaka/internal/testdb"
)

func seedShop(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
 INSERT INTO goods_group(id,gp_name) VALUES(1,'test');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(1,1,'auto','','',10,0,1),(2,1,'manual','','',10,1,2);
 INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute) VALUES(1,'cldx','cldx',2,3,'','/pay/cldx'),(2,'wx','wescan',1,3,'','/pay/wescan');`)
	if err != nil {
		t.Fatal(err)
	}
}

// backendPID returns the server process id behind tx so a test can wait for
// another session to block on exactly that transaction's locks.
func backendPID(t *testing.T, tx pgx.Tx) int {
	t.Helper()
	var pid int
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitBlockedBy polls pg_stat_activity until some backend in this database is
// waiting on a lock held by pid, and fails the test after 10 seconds.
func waitBlockedBy(t *testing.T, pool *pgxpool.Pool, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND datname = current_database()
			  AND $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend blocked on pid %d within 10s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// #3: only unpaid orders and recently expired ones are polled, oldest first, bounded.
func TestWaitingSNsWindowIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	now := time.Now().Unix()
	insert := func(sn string, status int, minor any, exp any, payID int, age string) {
		mustExec(t, pool, `INSERT INTO orders (order_sn, goods_id, title, type, actual_price, email, buy_ip, pay_id, status, cldx_minor, cldx_expires_at, created_at, updated_at)
 VALUES ($1,1,'t',1,10,'a@b.c','1.1.1.1',$5,$2,$3,$4,now(),now()-$6::interval)`, sn, status, minor, exp, payID, age)
	}
	insert("UNPAID", 1, 10000, now+300, 1, "1 minute")
	insert("RECENT", -1, 10000, now-3600, 1, "3 minutes")
	insert("STALE", -1, 10000, now-int64(CldxLateWindow.Seconds())-3600, 1, "5 minutes")
	insert("NOQUOTE", 1, nil, nil, 1, "1 minute")
	insert("OTHERPAY", 1, 10000, now+300, 2, "1 minute")
	insert("DONE", 4, 10000, now+300, 1, "1 minute")
	got, err := db.WaitingSNs(ctx, "cldx")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "UNPAID,RECENT" {
		t.Fatalf("want UNPAID,RECENT (live orders first, then least recently polled), got %v", got)
	}
	// Once UNPAID has been polled it moves behind never-polled orders of its status.
	if err := db.MarkCldxPolled(ctx, "UNPAID", now); err != nil {
		t.Fatal(err)
	}
	insert("UNPAID2", 1, 10000, now+300, 1, "1 minute")
	got, err = db.WaitingSNs(ctx, "cldx")
	if err != nil {
		t.Fatal(err)
	}
	// Buckets are interleaved (B3-6 repair): first live, first late, then
	// the rest of the live bucket, never-polled first.
	if strings.Join(got, ",") != "UNPAID2,RECENT,UNPAID" {
		t.Fatalf("want UNPAID2,RECENT,UNPAID (never-polled live first, late interleaved), got %v", got)
	}
}

func TestWaitingSNsLimitIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO orders (order_sn, goods_id, title, type, actual_price, email, buy_ip, pay_id, status, cldx_minor, cldx_expires_at, created_at, updated_at)
 SELECT 'SN'||i, 1, 't', 1, 10, 'a@b.c', '1.1.1.1', 1, 1, 10000, $1, now(), now() FROM generate_series(1, $2) i`, time.Now().Unix()+300, cldxPollLimit+50)
	got, err := db.WaitingSNs(context.Background(), "cldx")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != cldxPollLimit {
		t.Fatalf("one tick returned %d orders, limit is %d", len(got), cldxPollLimit)
	}
}

// #9: a late payment on an expired order takes the coupon use back.
func TestLatePaymentTakesCouponBackIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'C1'),(1,'C2');
 INSERT INTO coupons(id,discount,coupon,ret) VALUES(1,2,'SAVE',3);
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(1,1);`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "late@example.com", Coupon: "SAVE"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	couponRet := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT ret FROM coupons WHERE id=1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if couponRet() != 2 {
		t.Fatalf("order did not take a coupon use: %d", couponRet())
	}
	mustExec(t, pool, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	if err := db.ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if couponRet() != 3 {
		t.Fatalf("expiry did not hand the coupon back: %d", couponRet())
	}
	if _, err := db.Complete(ctx, o.SN, o.Actual, "late"); err != nil {
		t.Fatal(err)
	}
	if couponRet() != 2 {
		t.Fatalf("late payment used the coupon twice, ret=%d want 2", couponRet())
	}
	var status, retBack int
	if err := pool.QueryRow(ctx, `SELECT status, coupon_ret_back FROM orders WHERE id=$1`, o.ID).Scan(&status, &retBack); err != nil {
		t.Fatal(err)
	}
	if status != 4 || retBack != 0 {
		t.Fatalf("status=%d coupon_ret_back=%d, want 4/0", status, retBack)
	}
	if already, err := db.Complete(ctx, o.SN, o.Actual, "late"); err != nil || !already {
		t.Fatalf("second notice not idempotent: %v %v", already, err)
	}
	if couponRet() != 2 {
		t.Fatalf("repeat notice touched the coupon: %d", couponRet())
	}
}

// #9: when the coupon has no uses left the customer still gets the goods,
// and coupon_ret_back=2 marks the use as handed back and no longer deductible
// so the owner can reconcile and no later call retries the take.
func TestLatePaymentDeliversWhenCouponExhaustedIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'C1');
 INSERT INTO coupons(id,discount,coupon,ret) VALUES(1,2,'SAVE',1);
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(1,1);`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "late@example.com", Coupon: "SAVE"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	if err := db.ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE coupons SET ret=0 WHERE id=1`)
	if _, err := db.Complete(ctx, o.SN, o.Actual, "late"); err != nil {
		t.Fatalf("paid customer was refused delivery: %v", err)
	}
	var status, retBack, ret int
	if err := pool.QueryRow(ctx, `SELECT o.status, o.coupon_ret_back, c.ret FROM orders o, coupons c WHERE o.id=$1 AND c.id=1`, o.ID).Scan(&status, &retBack, &ret); err != nil {
		t.Fatal(err)
	}
	if status != 4 || retBack != couponRetBackUnrecoverable || ret != 0 {
		t.Fatalf("status=%d coupon_ret_back=%d ret=%d, want 4/2/0", status, retBack, ret)
	}
	// Restocking the coupon later must not make a repeated notice take it.
	mustExec(t, pool, `UPDATE coupons SET ret=5 WHERE id=1`)
	if already, err := db.Complete(ctx, o.SN, o.Actual, "late"); err != nil || !already {
		t.Fatalf("repeat notice: %v %v", already, err)
	}
	if err := pool.QueryRow(ctx, `SELECT ret FROM coupons WHERE id=1`).Scan(&ret); err != nil || ret != 5 {
		t.Fatalf("marker 2 did not stop a retry: ret=%d err=%v", ret, err)
	}
}

// #9: a coupon deleted after expiry cannot be taken back either; the order is
// delivered and marked 2, and a later Redeliver path never retries the take.
func TestLatePaymentDeletedCouponMarksUnrecoverableIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi) VALUES(1,1,'C0');
 INSERT INTO coupons(id,discount,coupon,ret) VALUES(1,2,'SAVE',5);
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(1,1);`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "late@example.com", Coupon: "SAVE"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	if err := db.ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE coupons SET deleted_at=now() WHERE id=1; UPDATE carmis SET deleted_at=now() WHERE id=1`)
	// No card left: the late payment lands as status 6 with the marker.
	var rule RuleError
	if _, err := db.Complete(ctx, o.SN, o.Actual, "late"); !errors.As(err, &rule) || rule.Msg != "库存不足" {
		t.Fatalf("want 库存不足, got %v", err)
	}
	var status, retBack, ret int
	if err := pool.QueryRow(ctx, `SELECT o.status, o.coupon_ret_back, c.ret FROM orders o, coupons c WHERE o.id=$1 AND c.id=1`, o.ID).Scan(&status, &retBack, &ret); err != nil {
		t.Fatal(err)
	}
	if status != 6 || retBack != couponRetBackUnrecoverable || ret != 5 {
		t.Fatalf("status=%d coupon_ret_back=%d ret=%d, want 6/2/5", status, retBack, ret)
	}
	mustExec(t, pool, `UPDATE coupons SET deleted_at=NULL WHERE id=1; INSERT INTO carmis(id,goods_id,carmi) VALUES(2,1,'C1')`)
	if delivered, err := db.Redeliver(ctx, o.SN); err != nil || !delivered {
		t.Fatalf("redeliver: %v %v", delivered, err)
	}
	if err := pool.QueryRow(ctx, `SELECT o.status, o.coupon_ret_back, c.ret FROM orders o, coupons c WHERE o.id=$1 AND c.id=1`, o.ID).Scan(&status, &retBack, &ret); err != nil {
		t.Fatal(err)
	}
	if status != 4 || retBack != couponRetBackUnrecoverable || ret != 5 {
		t.Fatalf("Redeliver retried the coupon: status=%d coupon_ret_back=%d ret=%d, want 4/2/5", status, retBack, ret)
	}
}

// #10: a reserved card skipped by SKIP LOCKED must not stay pinned to the finished order.
func TestCompleteReleasesSkippedReservationIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi) VALUES(1,1,'A'),(2,1,'B'),(3,1,'C')`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 2, Email: "two@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	var reserved int
	pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE reserved_order_id=$1`, o.ID).Scan(&reserved)
	if reserved != 2 {
		t.Fatalf("reserved %d cards, want 2", reserved)
	}
	// Another transaction holds card B, so Complete's SKIP LOCKED must ship A and C.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx2.Exec(ctx, `SELECT id FROM carmis WHERE id=2 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := db.Complete(ctx, o.SN, o.Actual, "receipt")
		done <- err
	}()
	// Wait until Complete is blocked on B's lock (its release UPDATE), then let go.
	waitBlockedBy(t, pool, backendPID(t, tx2))
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	final, _ := db.OrderBySN(ctx, o.SN)
	if final.Status != 4 || final.Info != "A\nC" {
		t.Fatalf("expected A and C shipped around the locked card: %+v", final)
	}
	var leftover int
	pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE reserved_order_id=$1`, o.ID).Scan(&leftover)
	if leftover != 0 {
		t.Fatalf("%d card(s) still reserved for the finished order", leftover)
	}
	var bStatus int
	var bReserved *int64
	pool.QueryRow(ctx, `SELECT status, reserved_order_id FROM carmis WHERE id=2`).Scan(&bStatus, &bReserved)
	if bStatus != 1 || bReserved != nil {
		t.Fatalf("card B should be back on sale: status=%d reserved=%v", bStatus, bReserved)
	}
	var free int
	pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE goods_id=1 AND status=1 AND reserved_order_id IS NULL`).Scan(&free)
	if free != 1 {
		t.Fatalf("storefront stock %d, want 1", free)
	}
}

// #11: an order paid while stock was short (status 6) can be delivered later, once.
func TestRedeliverIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi) VALUES(1,1,'FIRST')`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "six@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing to do on an unpaid order.
	if delivered, err := db.Redeliver(ctx, o.SN); err != nil || delivered {
		t.Fatalf("Redeliver touched an unpaid order: %v %v", delivered, err)
	}
	if cur, _ := db.OrderBySN(ctx, o.SN); cur.Status != 1 {
		t.Fatalf("unpaid order changed to %d", cur.Status)
	}
	// The reserved card disappears before the payment lands.
	mustExec(t, pool, `UPDATE carmis SET deleted_at=now() WHERE id=1`)
	var rule RuleError
	if _, err := db.Complete(ctx, o.SN, o.Actual, "receipt"); !errors.As(err, &rule) || rule.Msg != "库存不足" {
		t.Fatalf("want 库存不足, got %v", err)
	}
	if cur, _ := db.OrderBySN(ctx, o.SN); cur.Status != 6 || cur.TradeNo != "receipt" {
		t.Fatalf("paid-but-short order not at 6: %+v", cur)
	}
	if delivered, err := db.Redeliver(ctx, o.SN); delivered || !errors.As(err, &rule) || rule.Msg != "库存不足" {
		t.Fatalf("still short: want (false, 库存不足), got %v %v", delivered, err)
	}
	if cur, _ := db.OrderBySN(ctx, o.SN); cur.Status != 6 || cur.TradeNo != "receipt" || cur.Info != "库存不足" {
		t.Fatalf("failed Redeliver changed the order: %+v", cur)
	}
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi) VALUES(2,1,'SECOND')`)
	delivered, err := db.Redeliver(ctx, o.SN)
	if err != nil || !delivered {
		t.Fatalf("redeliver after restock: %v %v", delivered, err)
	}
	cur, _ := db.OrderBySN(ctx, o.SN)
	if cur.Status != 4 || cur.Info != "SECOND" || cur.TradeNo != "receipt" {
		t.Fatalf("not delivered: %+v", cur)
	}
	var sold int
	pool.QueryRow(ctx, `SELECT status FROM carmis WHERE id=2`).Scan(&sold)
	if sold != 2 {
		t.Fatalf("card not marked sold: %d", sold)
	}
	if delivered, err := db.Redeliver(ctx, o.SN); err != nil || delivered {
		t.Fatalf("double delivery: %v %v", delivered, err)
	}
	if already, err := db.Complete(ctx, o.SN, o.Actual, "receipt"); err != nil || !already {
		t.Fatalf("Complete after Redeliver not idempotent: %v %v", already, err)
	}
	var sales, cards int
	pool.QueryRow(ctx, `SELECT sales_volume FROM goods WHERE id=1`).Scan(&sales)
	pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE status=2`).Scan(&cards)
	if sales != 1 || cards != 1 {
		t.Fatalf("sales=%d sold cards=%d, want 1/1", sales, cards)
	}
	if _, err := db.Redeliver(ctx, "NOPE"); !errors.As(err, &rule) {
		t.Fatalf("unknown order: %v", err)
	}
}

func TestRedeliverManualTradeNoIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi) VALUES(1,1,'ONLY')`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "six@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	// Shop owner marked it paid by hand without a trade number.
	mustExec(t, pool, `UPDATE orders SET status=6, trade_no='' WHERE id=$1`, o.ID)
	if delivered, err := db.Redeliver(ctx, o.SN); err != nil || !delivered {
		t.Fatalf("%v %v", delivered, err)
	}
	cur, _ := db.OrderBySN(ctx, o.SN)
	if cur.Status != 4 || cur.TradeNo != "manual" || cur.Info != "ONLY" {
		t.Fatalf("%+v", cur)
	}
}

// #17a: Installed stops querying once it has seen an admin user.
func TestInstalledCachesPositiveIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	if db.Installed(ctx) {
		t.Fatal("empty site reported installed")
	}
	mustExec(t, pool, `INSERT INTO admin_users(username,password,name) VALUES('root','x','root')`)
	if !db.Installed(ctx) {
		t.Fatal("admin user not detected")
	}
	// With the table gone a real query would fail; the cached answer must hold.
	mustExec(t, pool, `DROP TABLE admin_users CASCADE`)
	if !db.Installed(ctx) {
		t.Fatal("positive Installed result was not cached")
	}
	if (&DB{Pool: pool}).Installed(ctx) {
		t.Fatal("cache leaked across DB values")
	}
}

// #17b: Site is served from cache for a while and refreshed on demand.
func TestSiteCacheIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	if err := db.PutSetting(ctx, "title", "First"); err != nil {
		t.Fatal(err)
	}
	if got := db.Site(ctx).Title; got != "First" {
		t.Fatalf("title %q", got)
	}
	mustExec(t, pool, `UPDATE settings SET value='Second' WHERE key='title'`)
	if got := db.Site(ctx).Title; got != "First" {
		t.Fatalf("Site re-read the database inside the cache window: %q", got)
	}
	db.InvalidateSite()
	if got := db.Site(ctx).Title; got != "Second" {
		t.Fatalf("InvalidateSite did not refresh: %q", got)
	}
	if err := db.PutSetting(ctx, "order_expire_time", "9"); err != nil {
		t.Fatal(err)
	}
	if got := db.Site(ctx).ExpireMin; got != 9 {
		t.Fatalf("PutSetting did not invalidate the cache: %d", got)
	}
	db.siteMu.Lock()
	db.siteExpires = time.Now().Add(-time.Second)
	db.siteMu.Unlock()
	mustExec(t, pool, `UPDATE settings SET value='Third' WHERE key='title'`)
	if got := db.Site(ctx).Title; got != "Third" {
		t.Fatalf("expired cache entry was reused: %q", got)
	}
}

// A failed settings read must not be cached, or the defaults would stick after install.
func TestSiteDoesNotCacheFailureIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	mustExec(t, pool, `ALTER TABLE settings RENAME TO settings_hidden`)
	if got := db.Site(ctx).Title; got != "Dufaka-Go" {
		t.Fatalf("default title %q", got)
	}
	mustExec(t, pool, `ALTER TABLE settings_hidden RENAME TO settings; INSERT INTO settings(key,value) VALUES('title','Live')`)
	if got := db.Site(ctx).Title; got != "Live" {
		t.Fatalf("failed read was cached: %q", got)
	}
}

// Round 2 B-1: settle locks the goods row before the coupon row, the same order
// CreateOrder uses, so a late coupon payment cannot deadlock a concurrent
// checkout. tx1 plays the checkout: it holds goods, then takes the coupon while
// Complete is already waiting on goods; before the fix Complete held the coupon
// and waited on goods, and one side died with SQLSTATE 40P01.
func TestLatePaymentNoDeadlockWithCheckoutIntegration(t *testing.T) {
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
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(ctx)
	if _, err = tx1.Exec(ctx, `SELECT id FROM goods WHERE id=1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := db.Complete(ctx, o.SN, o.Actual, "late")
		done <- err
	}()
	waitBlockedBy(t, pool, backendPID(t, tx1))
	// Checkout order: goods (held) → carmis → coupons. The coupon must still be free.
	if _, err = tx1.Exec(ctx, `UPDATE coupons SET ret=ret-1, updated_at=now() WHERE id=1 AND ret>0`); err != nil {
		t.Fatalf("checkout side hit the lock inversion: %v", err)
	}
	if err = tx1.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "40P01" {
		t.Fatalf("late payment deadlocked: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	var status, ret int
	if err := pool.QueryRow(ctx, `SELECT o.status, c.ret FROM orders o, coupons c WHERE o.id=$1 AND c.id=1`, o.ID).Scan(&status, &ret); err != nil {
		t.Fatal(err)
	}
	if status != 4 || ret != 2 {
		t.Fatalf("status=%d ret=%d, want 4/2", status, ret)
	}
}

// Round 2 B-1: ExpireDue takes the goods row before the coupon row too. A
// manual (type 2) order is used because its expiry also writes the goods row
// (stock hand-back), which is where the old coupons-first order deadlocked.
func TestExpireNoDeadlockWithCheckoutIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO coupons(id,discount,coupon,ret) VALUES(1,2,'SAVE',3);
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(2,1);`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 2, PayID: 1, Amount: 1, Email: "exp@example.com", Coupon: "SAVE"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(ctx)
	if _, err = tx1.Exec(ctx, `SELECT id FROM goods WHERE id=2 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- db.ExpireDue(ctx, 5) }()
	waitBlockedBy(t, pool, backendPID(t, tx1))
	if _, err = tx1.Exec(ctx, `UPDATE coupons SET ret=ret-1, updated_at=now() WHERE id=1 AND ret>0`); err != nil {
		t.Fatalf("checkout side hit the lock inversion: %v", err)
	}
	if err = tx1.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("expiry failed: %v", err)
	}
	var status, retBack, ret, stock int
	if err := pool.QueryRow(ctx, `SELECT o.status, o.coupon_ret_back, c.ret, g.in_stock FROM orders o, coupons c, goods g WHERE o.id=$1 AND c.id=1 AND g.id=2`, o.ID).Scan(&status, &retBack, &ret, &stock); err != nil {
		t.Fatal(err)
	}
	if status != -1 || retBack != 1 || ret != 3 || stock != 1 {
		t.Fatalf("status=%d coupon_ret_back=%d ret=%d stock=%d, want -1/1/3/1", status, retBack, ret, stock)
	}
}

// Round 2 B-2: a manual (type 2) order already at status 6 keeps its stock and
// sales untouched on a repeated notice; the payment is simply acknowledged.
func TestManualStatusSixDoesNotRedeductIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `UPDATE goods SET in_stock=3 WHERE id=2`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 2, PayID: 1, Amount: 1, Email: "m@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	// Owner marked it 异常 by hand while the checkout stock deduction still stands.
	mustExec(t, pool, `UPDATE orders SET status=6, trade_no='gw' WHERE id=$1`, o.ID)
	already, err := db.Complete(ctx, o.SN, o.Actual, "gw")
	if err != nil || !already {
		t.Fatalf("repeat notice on a paid-but-short manual order: already=%v err=%v", already, err)
	}
	var stock, sales, status int
	if err := pool.QueryRow(ctx, `SELECT g.in_stock, COALESCE(g.sales_volume,0), o.status FROM goods g, orders o WHERE g.id=2 AND o.id=$1`, o.ID).Scan(&stock, &sales, &status); err != nil {
		t.Fatal(err)
	}
	if stock != 2 || sales != 0 || status != 6 {
		t.Fatalf("stock=%d sales=%d status=%d, want 2/0/6 (no second deduction)", stock, sales, status)
	}
}

// Round 2 B-2: Redeliver is for auto-delivery goods only.
func TestRedeliverRefusesManualGoodsIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 2, PayID: 1, Amount: 1, Email: "m@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE orders SET status=6, trade_no='gw' WHERE id=$1`, o.ID)
	delivered, err := db.Redeliver(ctx, o.SN)
	var rule RuleError
	if delivered || !errors.As(err, &rule) || rule.Msg != "仅自动发卡商品支持重新发货" {
		t.Fatalf("want (false, 仅自动发卡商品支持重新发货), got %v %v", delivered, err)
	}
	var status, stock int
	if err := pool.QueryRow(ctx, `SELECT o.status, g.in_stock FROM orders o, goods g WHERE o.id=$1 AND g.id=2`, o.ID).Scan(&status, &stock); err != nil {
		t.Fatal(err)
	}
	if status != 6 || stock != 0 {
		t.Fatalf("status=%d stock=%d, want 6/0 untouched", status, stock)
	}
}

// Round 2 B-2: a manual order paid late when the stock is gone becomes status 6
// with the trade number on file, like auto-delivery goods, instead of staying
// -1 and being polled for another day. Round 3 B3-3: the buyer's inputs in
// info survive, with "库存不足" put in front of them.
func TestManualLatePaymentShortStockBecomesSixIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `UPDATE goods SET other_ipu_cnf='account=充值账号=true' WHERE id=2`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 2, PayID: 1, Amount: 1, Email: "m@example.com", Extra: map[string]string{"account": "u-123"}}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Info != "充值账号:u-123\n" {
		t.Fatalf("setup: buyer input not recorded: %q", o.Info)
	}
	mustExec(t, pool, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	if err := db.ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	// Expiry handed the single unit back; someone else buys it.
	if _, err := db.CreateOrder(ctx, CreateInput{GID: 2, PayID: 1, Amount: 1, Email: "other@example.com", Extra: map[string]string{"account": "u-456"}}, Site{}); err != nil {
		t.Fatal(err)
	}
	var rule RuleError
	if _, err := db.Complete(ctx, o.SN, o.Actual, "late-gw"); !errors.As(err, &rule) || rule.Msg != "库存不足" {
		t.Fatalf("want 库存不足, got %v", err)
	}
	cur, err := db.OrderBySN(ctx, o.SN)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != 6 || cur.Info != "库存不足\n充值账号:u-123\n" || cur.TradeNo != "late-gw" {
		t.Fatalf("paid-but-short manual order not recorded: %+v", cur)
	}
	var stock, sales int
	if err := pool.QueryRow(ctx, `SELECT in_stock, COALESCE(sales_volume,0) FROM goods WHERE id=2`).Scan(&stock, &sales); err != nil {
		t.Fatal(err)
	}
	if stock != 0 || sales != 0 {
		t.Fatalf("stock=%d sales=%d, want 0/0", stock, sales)
	}
	var owed bool
	if err := pool.QueryRow(ctx, `SELECT stock_owed FROM orders WHERE order_sn=$1`, cur.SN).Scan(&owed); err != nil || !owed {
		t.Fatalf("paid-but-short manual order must be flagged stock_owed: %v %v", owed, err)
	}
	// Not in the poll set any more, and a repeat notice is acknowledged.
	if sns, err := db.WaitingSNs(ctx, "cldx"); err != nil || strings.Contains(strings.Join(sns, ","), o.SN) {
		t.Fatalf("status-6 order still polled: %v %v", sns, err)
	}
	if already, err := db.Complete(ctx, o.SN, o.Actual, "late-gw"); err != nil || !already {
		t.Fatalf("repeat notice: %v %v", already, err)
	}
	if again, _ := db.OrderBySN(ctx, o.SN); again.Info != cur.Info {
		t.Fatalf("repeat notice rewrote info: %q", again.Info)
	}
}

// Round 2 B-3: a channel without a cashier is neither listed nor accepted,
// even while its row says is_open=1.
func TestStorefrontRejectsChannelWithoutCashierIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'C1');
 INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute,is_open) VALUES(3,'支付宝','alipay',1,3,'','/pay/yipay',1)`)
	list, err := db.Pays(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var checks []string
	for _, p := range list {
		checks = append(checks, p.Check)
	}
	if strings.Join(checks, ",") != "cldx,wescan" {
		t.Fatalf("storefront channels %v, want only cldx,wescan", checks)
	}
	var rule RuleError
	if _, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 3, Amount: 1, Email: "a@example.com"}, Site{}); !errors.As(err, &rule) || rule.Msg != "支付方式不可用" {
		t.Fatalf("alipay checkout: want 支付方式不可用, got %v", err)
	}
	var reserved int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE reserved_order_id IS NOT NULL`).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("refused checkout reserved a card: %d %v", reserved, err)
	}
	if _, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "a@example.com"}, Site{}); err != nil {
		t.Fatalf("cldx checkout refused: %v", err)
	}
}

// Round 2 B-3: the startup sweep disables enabled channels without a cashier
// and leaves wired, disabled and deleted rows alone.
func TestDisableUnwiredPaysIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute,is_open,deleted_at) VALUES
 (3,'支付宝','alipay',1,3,'','/pay/yipay',1,NULL),
 (4,'PayPal','paypal',1,3,'','/pay/paypal',1,NULL),
 (5,'Stripe','stripe',1,3,'','/pay/stripe',0,NULL),
 (6,'Coinbase','coinbase',1,3,'','/pay/coinbase',1,now())`)
	n, err := db.DisableUnwiredPays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("disabled %d rows, want 2 (alipay, paypal)", n)
	}
	var open []string
	rows, err := pool.Query(ctx, `SELECT pay_check FROM pays WHERE is_open=1 AND deleted_at IS NULL ORDER BY pay_check`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
	}
	rows.Close()
	if strings.Join(open, ",") != "cldx,wescan" {
		t.Fatalf("still enabled: %v", open)
	}
	if n, err = db.DisableUnwiredPays(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
}

// Round 2 B-4: an invalidation that lands while Site is reading the settings
// must not be lost behind the value that read returns.
func TestSiteInvalidateDuringLoadIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO settings(key,value) VALUES('title','First')`)
	var fired int
	db.siteLoadHook = func() {
		// The read is done and returned "First"; now the admin saves "Second".
		fired++
		mustExec(t, pool, `UPDATE settings SET value='Second' WHERE key='title'`)
		db.InvalidateSite()
	}
	if got := db.Site(ctx).Title; got != "First" {
		t.Fatalf("racing read returned %q", got)
	}
	db.siteLoadHook = nil
	if fired != 1 {
		t.Fatalf("hook fired %d times", fired)
	}
	if got := db.Site(ctx).Title; got != "Second" {
		t.Fatalf("invalidation during load was lost: cached %q", got)
	}
	// Same interleaving through PutSetting, which invalidates after its write.
	db.siteLoadHook = func() {
		if err := db.PutSetting(ctx, "title", "Third"); err != nil {
			t.Fatal(err)
		}
	}
	db.InvalidateSite()
	if got := db.Site(ctx).Title; got != "Second" {
		t.Fatalf("racing read returned %q", got)
	}
	db.siteLoadHook = nil
	if got := db.Site(ctx).Title; got != "Third" {
		t.Fatalf("PutSetting during load was lost: cached %q", got)
	}
}

// Round 2 B-5b: only a missing row is "订单不存在"; a real database error is
// returned as-is so the caller does not answer the gateway with a rule message.
func TestSettleSplitsLookupErrorsIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	var rule RuleError
	if _, err := db.Complete(ctx, "NOPE", 1, "x"); !errors.As(err, &rule) || rule.Msg != "订单不存在" {
		t.Fatalf("missing order: %v", err)
	}
	mustExec(t, pool, `ALTER TABLE orders RENAME TO orders_gone`)
	_, err := db.Complete(ctx, "NOPE", 1, "x")
	if err == nil || errors.As(err, &rule) {
		t.Fatalf("database error was reported as a rule: %v", err)
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("want the driver error, got %T %v", err, err)
	}
}

// Round 2 B-5a: the reservation release is scoped to the order's goods.
func TestCompleteReleaseScopedToGoodsIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	seedShop(t, pool)
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi) VALUES(1,1,'A')`)
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "a@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	// A stray card of another goods carrying this order id must be left alone.
	mustExec(t, pool, `INSERT INTO carmis(id,goods_id,carmi,reserved_order_id) VALUES(2,2,'OTHER',$1)`, o.ID)
	if _, err := db.Complete(ctx, o.SN, o.Actual, "gw"); err != nil {
		t.Fatal(err)
	}
	var mine, other *int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT reserved_order_id FROM carmis WHERE id=1), (SELECT reserved_order_id FROM carmis WHERE id=2)`).Scan(&mine, &other); err != nil {
		t.Fatal(err)
	}
	if mine != nil || other == nil || int(*other) != o.ID {
		t.Fatalf("release touched the wrong goods: mine=%v other=%v", mine, other)
	}
}
