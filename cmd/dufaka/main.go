package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"dufaka/internal/httpx"
	"dufaka/internal/install"
	"dufaka/internal/store"
)

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
	if dsn := os.Getenv("DUFAKA_DATABASE_URL"); dsn != "" {
		opened, err := store.Open(context.Background(), dsn)
		if err != nil {
			log.Printf("数据库暂时连不上，进入安装页: %v", err)
		} else {
			db = opened
			if db.Installed(context.Background()) && os.Getenv("WALLET_MERCHANT_SECRET") != "" {
				if err := db.EnsureCldxPay(context.Background()); err != nil {
					log.Printf("cldx 支付渠道没有写上: %v", err)
				}
			}
			if db.Installed(context.Background()) && os.Getenv("DUFAKA_SESSION_KEY") == "" {
				log.Fatal("站点已安装，但缺少 DUFAKA_SESSION_KEY")
			}
		}
	}
	app, err := httpx.New(db, base, configPath)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		for range t.C {
			current := app.Live()
			if current == nil {
				continue
			}
			_ = current.ExpireDue(context.Background(), current.Site(context.Background()).ExpireMin)
			app.SyncCldx(context.Background())
		}
	}()
	addr := os.Getenv("DUFAKA_ADDR")
	if file.Addr != "" && os.Getenv("DUFAKA_ADDR") == "" {
		addr = file.Addr
	}
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	log.Printf("Dufaka-Go listening on %s", addr)
	if db == nil || !db.Installed(context.Background()) {
		log.Printf("尚未安装，打开 %s/install", base)
	}
	log.Fatal(http.ListenAndServe(addr, app.Handler()))
}
