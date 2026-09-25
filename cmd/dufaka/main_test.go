package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dufaka/internal/httpx"
	"dufaka/internal/store"
	"dufaka/internal/testdb"
)

// The running guard must skip ticks while a sweep is still in flight, and
// resume once it has finished.
func TestTickLoopNeverStacksSweeps(t *testing.T) {
	ticks := make(chan time.Time)
	t.Cleanup(func() { close(ticks) })
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	var runs atomic.Int32
	db := &store.DB{}
	go tickLoop(ticks, func() *store.DB { return db }, func(*store.DB) {
		runs.Add(1)
		started <- struct{}{}
		<-release
	})
	ticks <- time.Now()
	<-started
	// These two ticks arrive while the first sweep is blocked; the unbuffered
	// channel guarantees the loop has consumed each one before we continue.
	ticks <- time.Now()
	ticks <- time.Now()
	if got := runs.Load(); got != 1 {
		t.Fatalf("stacked sweeps: %d runs while first one still running", got)
	}
	close(release)
	deadline := time.After(5 * time.Second)
	for runs.Load() < 2 {
		select {
		case ticks <- time.Now():
		case <-deadline:
			t.Fatal("sweep never resumed after the previous one finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTickLoopSkipsWithoutDB(t *testing.T) {
	ticks := make(chan time.Time)
	t.Cleanup(func() { close(ticks) })
	ran := make(chan struct{}, 10)
	go tickLoop(ticks, func() *store.DB { return nil }, func(*store.DB) { ran <- struct{}{} })
	ticks <- time.Now()
	ticks <- time.Now()
	select {
	case <-ran:
		t.Fatal("ran a sweep with no live database")
	case <-time.After(200 * time.Millisecond):
	}
}

// Round 2 B-6: a DSN that parses but points nowhere is named in the log; a
// healthy one is not.
func TestOpenStoreReportsUnreachableDatabase(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	db, reachable := openStore(context.Background(), "postgresql://shop_test@127.0.0.1:1/postgres?connect_timeout=2")
	if db == nil {
		t.Fatal("a parsable DSN must still yield a pool so the site recovers when the database returns")
	}
	if reachable {
		t.Fatal("unreachable database reported reachable")
	}
	db.Pool.Close()
	if !strings.Contains(buf.String(), "请检查 DUFAKA_DATABASE_URL") {
		t.Fatalf("unreachable database was not reported: %q", buf.String())
	}
	buf.Reset()
	if db, reachable := openStore(context.Background(), "this is not a dsn"); db != nil || reachable || !strings.Contains(buf.String(), "进入安装页") {
		t.Fatalf("unparsable DSN: %q", buf.String())
	}
	dsn := os.Getenv("DUFAKA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipped: DUFAKA_TEST_DATABASE_URL is not set")
	}
	buf.Reset()
	db, reachable = openStore(context.Background(), dsn)
	if db == nil || !reachable {
		t.Fatal("healthy database rejected")
	}
	db.Pool.Close()
	if buf.Len() != 0 {
		t.Fatalf("healthy database logged a warning: %q", buf.String())
	}
}

// Round 2 B-3: startup disables enabled channels without a cashier and says how many.
func TestDisableUnwiredPaysAtStartup(t *testing.T) {
	pool := testdb.Open(t)
	db := &store.DB{Pool: pool}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO pays(pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute,is_open) VALUES
 ('支付宝','alipay',1,3,'','/pay/yipay',1),('微信扫码','wescan',2,3,'','/pay/wepay',1),('PayPal','paypal',1,3,'','/pay/paypal',1)`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	disableUnwiredPays(ctx, db)
	if !strings.Contains(buf.String(), "已停用 2 个没有收银台的支付渠道") {
		t.Fatalf("startup sweep did not report its count: %q", buf.String())
	}
	var open int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pays WHERE is_open=1`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("enabled channels after sweep: %d %v", open, err)
	}
	buf.Reset()
	disableUnwiredPays(ctx, db)
	if buf.Len() != 0 {
		t.Fatalf("a clean sweep should stay quiet: %q", buf.String())
	}
}

// runTick must finish under its own deadline and log, not swallow, failures.
func TestRunTickLogsExpireErrors(t *testing.T) {
	pool := testdb.Open(t)
	db := &store.DB{Pool: pool}
	app, err := httpx.New(db, "http://127.0.0.1:8080", filepath.Join(t.TempDir(), "dufaka.json"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	runTick(db, app)
	if strings.Contains(buf.String(), "过期订单清理失败") {
		t.Fatalf("healthy tick logged an error: %s", buf.String())
	}
	if _, err := pool.Exec(context.Background(), `DROP TABLE orders CASCADE`); err != nil {
		t.Fatal(err)
	}
	runTick(db, app)
	if !strings.Contains(buf.String(), "过期订单清理失败") {
		t.Fatalf("ExpireDue error was swallowed: %q", buf.String())
	}
}

// B3-9: with settings unreadable the tick must not expire orders with the
// built-in 5-minute default.
func TestRunTickSkipsExpireWithoutSettings(t *testing.T) {
	pool := testdb.Open(t)
	db := &store.DB{Pool: pool}
	ctx := context.Background()
	app, err := httpx.New(db, "http://127.0.0.1:8080", filepath.Join(t.TempDir(), "dufaka.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES('order_expire_time','60');
 INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(1,1,'m','','',10,5,2);
 INSERT INTO orders (order_sn, goods_id, title, type, actual_price, email, buy_ip, status, created_at, updated_at)
 VALUES ('TEN', 1, 't', 2, 10, 'a@b.c', '1.1.1.1', 1, now()-interval '10 minutes', now())`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if _, err := pool.Exec(ctx, `ALTER TABLE settings RENAME TO settings_gone`); err != nil {
		t.Fatal(err)
	}
	runTick(db, app)
	if !strings.Contains(buf.String(), "本轮跳过过期订单清理") {
		t.Fatalf("skip not logged: %q", buf.String())
	}
	status := func() int {
		var s int
		if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE order_sn='TEN'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := status(); s != 1 {
		t.Fatalf("order expired with the default while settings were unreadable: status %d", s)
	}
	// Settings back: 60 minutes, a 10-minute-old order stays open.
	if _, err := pool.Exec(ctx, `ALTER TABLE settings_gone RENAME TO settings`); err != nil {
		t.Fatal(err)
	}
	runTick(db, app)
	if s := status(); s != 1 {
		t.Fatalf("order expired before order_expire_time: status %d", s)
	}
}

// B3-9: an unreachable database skips every boot step and says so.
func TestBootChecksSkipWhenUnreachable(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if bootChecks(nil, false, true) {
		t.Fatal("no database reported installed")
	}
	db, reachable := openStore(context.Background(), "postgresql://shop_test@127.0.0.1:1/postgres?connect_timeout=2")
	t.Cleanup(db.Pool.Close)
	buf.Reset()
	start := time.Now()
	if bootChecks(db, reachable, true) {
		t.Fatal("unreachable database reported installed")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("skipped boot still took %v", time.Since(start))
	}
	if !strings.Contains(buf.String(), "数据库不可达，跳过启动检查") {
		t.Fatalf("skip not logged: %q", buf.String())
	}
}

// B3-5/B3-7/B3-9: an installed site gets its cldx schema whether or not the
// wallet is configured, the cldx pays row only with the secret, and the
// captcha switches cleared.
func TestBootChecksInstalledSite(t *testing.T) {
	pool := testdb.Open(t)
	db := &store.DB{Pool: pool}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO admin_users(username,password,name) VALUES('root','x','root');
 INSERT INTO settings(key,value) VALUES('is_open_geetest','1'),('is_open_img_code','1');
 INSERT INTO pays(pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute,is_open) VALUES('支付宝','alipay',1,3,'','/pay/yipay',1);
 DROP INDEX idx_orders_cldx_live; ALTER TABLE orders DROP COLUMN cldx_polled_at`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if !bootChecks(db, true, false) {
		t.Fatalf("installed site not detected: %s", buf.String())
	}
	var cols, cldxPays, openPays int
	var live bool
	var captcha string
	if err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='orders' AND column_name='cldx_polled_at'),
 to_regclass('idx_orders_cldx_live') IS NOT NULL,
 (SELECT count(*) FROM pays WHERE pay_check='cldx'),
 (SELECT count(*) FROM pays WHERE is_open=1),
 (SELECT string_agg(value, ',' ORDER BY key) FROM settings WHERE key IN ('is_open_geetest','is_open_img_code'))`).Scan(&cols, &live, &cldxPays, &openPays, &captcha); err != nil {
		t.Fatal(err)
	}
	if cols != 1 || !live || cldxPays != 0 || openPays != 0 || captcha != "0,0" {
		t.Fatalf("boot without wallet secret: cols=%d live=%v cldxPays=%d openPays=%d captcha=%s log=%s", cols, live, cldxPays, openPays, captcha, buf.String())
	}
	if !strings.Contains(buf.String(), "已关闭 2 个未接通的验证码开关") {
		t.Fatalf("captcha clear not logged: %q", buf.String())
	}
	if !bootChecks(db, true, true) {
		t.Fatal("second boot")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pays WHERE pay_check='cldx'`).Scan(&cldxPays); err != nil || cldxPays != 1 {
		t.Fatalf("cldx pays row with secret: %d %v", cldxPays, err)
	}
}

// B3-9: every boot step runs under its own deadline, so a locked table cannot
// hang the start.
func TestBootChecksStepTimeout(t *testing.T) {
	pool := testdb.Open(t)
	db := &store.DB{Pool: pool}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO admin_users(username,password,name) VALUES('root','x','root')`); err != nil {
		t.Fatal(err)
	}
	old := bootStepTimeout
	bootStepTimeout = 300 * time.Millisecond
	t.Cleanup(func() { bootStepTimeout = old })
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE settings IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	start := time.Now()
	if !bootChecks(db, true, false) {
		t.Fatal("installed site not detected")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("boot waited %v on a locked table", took)
	}
	if !strings.Contains(buf.String(), "关闭图形验证码与极验开关失败") {
		t.Fatalf("timed-out step not logged: %q", buf.String())
	}
}
