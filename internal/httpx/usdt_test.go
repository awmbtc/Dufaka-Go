package httpx

import (
	"context"
	"dufaka/internal/pay"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUSDTQuoteLockAndLateFulfillment(t *testing.T) {
	app, o := auditOrder(t)
	ctx := context.Background()
	if _, err := app.live().Pool.Exec(ctx, `UPDATE pays SET pay_check='usdt',pay_name='USDT',pay_handleroute='/pay/usdt' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	var paid atomic.Bool
	var mu sync.Mutex
	p := pay.USDTInvoice{ID: "usdt_0123456789abcdef01234567", Status: "pending", Network: "TRON", Token: "USDT", Chain: "TRC20", Address: "TJRabPrwbZy45sbavfcjinPJC18kjpRTv8", CNY: "10.00", Amount: "1.370013", Expires: time.Now().UTC().Add(30 * time.Minute)}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer server-only-secret" {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/payments" {
			count.Add(1)
			var b struct {
				Ref string `json:"order_no"`
			}
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Error(err)
			}
			p.OrderNo = b.Ref
		}
		res := p
		if paid.Load() {
			now := time.Now().UTC()
			res.Status = "paid"
			res.PaidAt = &now
			res.TxID = strings.Repeat("a", 64)
		}
		json.NewEncoder(w).Encode(res)
	}))
	defer provider.Close()
	app.usdt = pay.USDT{Base: provider.URL, Secret: "server-only-secret", Address: p.Address}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			app.usdtPay(w, httptest.NewRequest("GET", "/pay/usdt/"+o.SN, nil), o)
			body := w.Body.String()
			if !strings.Contains(body, "1.370013") || !strings.Contains(body, "TRC20") || strings.Contains(body, app.usdt.Secret) {
				t.Errorf("cashier missing quote or exposed secret")
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("concurrent refresh created %d invoices", count.Load())
	}
	// An expired reservation can still receive a confirmed chain payment.
	if _, err := app.live().Pool.Exec(ctx, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.live().ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	paid.Store(true)
	app.SyncUSDT(ctx)
	app.SyncUSDT(ctx)
	done, err := app.live().OrderBySN(ctx, o.SN)
	if err != nil || done.Status != 4 || done.Info != "AUDIT-SECRET" || done.TradeNo != "usdt:"+strings.Repeat("a", 64) {
		t.Fatalf("late payment not fulfilled once: %+v %v", done, err)
	}
	var delivered int
	if err := app.live().Pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE goods_id=1 AND status=2`).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatal("duplicate delivery")
	}
}
