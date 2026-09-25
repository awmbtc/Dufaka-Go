package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"dufaka/internal/httpx"
	"dufaka/internal/install"
	"dufaka/internal/store"
)

// tickEvery is the background sweep interval; tickTimeout bounds one sweep so
// it always finishes before the next tick fires.
const (
	tickEvery   = 30 * time.Second
	tickTimeout = 25 * time.Second
)

// runTick expires overdue orders and polls the cldx wallet once, under one
// shared deadline. Errors are logged instead of dropped. When the settings
// cannot be read the expiry sweep is skipped: the built-in 5-minute default
// could expire orders the shop gives much longer to pay.
func runTick(current *store.DB, app *httpx.App) {
	ctx, cancel := context.WithTimeout(context.Background(), tickTimeout)
	defer cancel()
	// Finish the schema upkeep if the boot could not (database not up yet, a table lock).
	if current.Installed(ctx) {
		if err := current.EnsureSchemaOnce(ctx); err != nil {
			log.Printf("表结构还没有补齐，下一轮重试: %v", err)
		}
	}
	if site, ok := current.SiteOK(ctx); !ok {
		log.Printf("读取系统设置失败，本轮跳过过期订单清理")
	} else if err := current.ExpireDue(ctx, site.ExpireMin); err != nil {
		log.Printf("过期订单清理失败: %v", err)
	}
	app.SyncCldx(ctx)
}

// pingTimeout bounds the startup connectivity check so a wrong DSN is reported
// quickly instead of hanging the boot.
const pingTimeout = 5 * time.Second

// openStore opens the pool and checks that the database actually answers.
// reachable reports whether that check passed. A pool that cannot reach the
// server is still returned: the request guards then treat the site as not
// installed (the install page) and recover on their own once the database is
// back; the wrong DSN is named in the log instead of being silent.
func openStore(ctx context.Context, dsn string) (db *store.DB, reachable bool) {
	db, err := store.Open(ctx, dsn)
	if err != nil {
		log.Printf("数据库暂时连不上，进入安装页: %v", err)
		return nil, false
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := db.Pool.Ping(pingCtx); err != nil {
		log.Printf("数据库连接失败，请检查 DUFAKA_DATABASE_URL（主机、端口、库名、账号密码）: %v；在数据库可用前进入安装页", err)
		return db, false
	}
	return db, true
}

// bootStepTimeout bounds each startup database step, so a locked table or a
// database that stops answering after the ping cannot hang the boot.
var bootStepTimeout = 10 * time.Second

// bootStep runs one startup step under its own bootStepTimeout.
func bootStep(fn func(ctx context.Context)) {
	ctx, cancel := context.WithTimeout(context.Background(), bootStepTimeout)
	defer cancel()
	fn(ctx)
}

// bootChecks runs the startup upkeep of an installed site and reports whether
// the site is installed. With no reachable database every step is skipped
// (and said so in the log); the request guards take over once it is back.
// Steps: cldx columns and poll indexes (always), the cldx pays row (only with
// a wallet secret), switching off channels without a cashier, and clearing the
// captcha switches this build does not wire.
func bootChecks(db *store.DB, reachable, walletSecret bool) (installed bool) {
	if db == nil {
		return false
	}
	if !reachable {
		log.Printf("数据库不可达，跳过启动检查（安装状态、cldx 表结构与渠道、支付渠道停用、验证码开关）")
		return false
	}
	bootStep(func(ctx context.Context) { installed = db.Installed(ctx) })
	if !installed {
		return false
	}
	bootStep(func(ctx context.Context) {
		if err := db.EnsureSchemaOnce(ctx); err != nil {
			log.Printf("表结构没有补齐（后台任务每 30 秒重试）: %v", err)
		}
	})
	if walletSecret {
		bootStep(func(ctx context.Context) {
			if err := db.EnsureCldxPay(ctx); err != nil {
				log.Printf("cldx 支付渠道没有写上: %v", err)
			}
		})
	}
	bootStep(func(ctx context.Context) { disableUnwiredPays(ctx, db) })
	bootStep(func(ctx context.Context) { disableCaptchaSwitches(ctx, db) })
	return true
}

// disableCaptchaSwitches clears the image-captcha and GeeTest switches, which
// this build does not wire, and logs how many settings rows changed.
func disableCaptchaSwitches(ctx context.Context, db *store.DB) {
	n, err := db.DisableCaptchaSwitches(ctx)
	if err != nil {
		log.Printf("关闭图形验证码与极验开关失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("已关闭 %d 个未接通的验证码开关（图形验证码、极验）", n)
	}
}

// disableUnwiredPays switches off every enabled payment channel that has no
// cashier in this build (store.CashierChecks) and logs how many rows changed.
func disableUnwiredPays(ctx context.Context, db *store.DB) {
	n, err := db.DisableUnwiredPays(ctx)
	if err != nil {
		log.Printf("停用无收银台的支付渠道失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("已停用 %d 个没有收银台的支付渠道（仅 %s 可用）", n, strings.Join(store.CashierChecks, "、"))
	}
}

// tickLoop runs one sweep per tick. A slow sweep (wallet timeouts, busy
// database) must never stack on the next one, so a tick that arrives while the
// previous sweep is still running is skipped and logged.
func tickLoop(ticks <-chan time.Time, live func() *store.DB, run func(*store.DB)) {
	var running atomic.Bool
	for range ticks {
		current := live()
		if current == nil {
			continue
		}
		if !running.CompareAndSwap(false, true) {
			log.Printf("后台任务上一轮还没结束，跳过本轮")
			continue
		}
		go func() {
			defer running.Store(false)
			run(current)
		}()
	}
}

func main() {
	configPath := os.Getenv("DUFAKA_CONFIG")
	if configPath == "" {
		configPath = "dufaka.json"
	}
	file, err := install.LoadConfig(configPath)
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("读取配置失败: %v", err)
	}
	if os.Getenv("DUFAKA_DATABASE_URL") == "" && file.DatabaseURL != "" {
		_ = os.Setenv("DUFAKA_DATABASE_URL", file.DatabaseURL)
	}
	if os.Getenv("DUFAKA_SESSION_KEY") == "" && file.SessionKey != "" {
		_ = os.Setenv("DUFAKA_SESSION_KEY", file.SessionKey)
	}
	if os.Getenv("DUFAKA_BASE_URL") == "" && file.BaseURL != "" {
		_ = os.Setenv("DUFAKA_BASE_URL", file.BaseURL)
	}
	base := os.Getenv("DUFAKA_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	var db *store.DB
	installed := false
	if dsn := os.Getenv("DUFAKA_DATABASE_URL"); dsn != "" {
		var reachable bool
		db, reachable = openStore(context.Background(), dsn)
		installed = bootChecks(db, reachable, os.Getenv("WALLET_MERCHANT_SECRET") != "")
		if installed && os.Getenv("DUFAKA_SESSION_KEY") == "" {
			log.Fatal("站点已安装，但缺少 DUFAKA_SESSION_KEY")
		}
	}
	app, err := httpx.New(db, base, configPath)
	if err != nil {
		log.Fatal(err)
	}
	go tickLoop(time.NewTicker(tickEvery).C, app.Live, func(current *store.DB) { runTick(current, app) })
	addr := os.Getenv("DUFAKA_ADDR")
	if file.Addr != "" && os.Getenv("DUFAKA_ADDR") == "" {
		addr = file.Addr
	}
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	log.Printf("Dufaka-Go listening on %s", addr)
	if !installed {
		log.Printf("尚未安装（或数据库暂不可达），打开 %s/install", base)
	}
	log.Fatal(http.ListenAndServe(addr, app.Handler()))
}
