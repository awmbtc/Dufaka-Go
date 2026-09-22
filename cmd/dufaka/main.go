package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"dufaka/internal/httpx"
	"dufaka/internal/store"
)

func main() {
	if os.Getenv("DUFAKA_SESSION_KEY") == "" {
		log.Fatal("缺少 DUFAKA_SESSION_KEY")
	}
	url := os.Getenv("DUFAKA_DATABASE_URL")
	if url == "" {
		log.Fatal("缺少 DUFAKA_DATABASE_URL")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	base := os.Getenv("DUFAKA_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	app, err := httpx.New(db, base)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		for range t.C {
			_ = db.ExpireDue(context.Background(), db.Site(context.Background()).ExpireMin)
		}
	}()
	addr := os.Getenv("DUFAKA_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	log.Printf("Dufaka-Go listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, app.Handler()))
}
