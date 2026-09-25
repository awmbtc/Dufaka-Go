package admin

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"dufaka/internal/testdb"
)

// Closeout: a 人工处理 order already in 异常 before the audit (no short-stock marker,
// stock taken long ago) must not be charged stock again when the owner moves it on,
// while an order the payment path parked in 异常 takes its stock and loses the marker.
func TestOrderSaveLegacyManualShortTakesNoStockIntegration(t *testing.T) {
	pool := testdb.Open(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type,in_stock) VALUES(1,1,'manual','','',10,2,5);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,buy_amount,trade_no,info) VALUES
  (1,'LEGACY1',1,'manual','a@example.com','local',6,2,2,'wx-1',E'库存不足，等补货'),
  (2,'PARKED2',1,'manual','a@example.com','local',6,2,1,'wx-2',E'库存不足\n充值账号:QQ2'),
  (3,'PARKED3',1,'manual','a@example.com','local',6,2,1,'wx-3',E'库存不足\n充值账号:QQ3');
 UPDATE orders SET stock_owed=true WHERE id IN (2,3);`)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	save := func(id int, status, info string) *httptest.ResponseRecorder {
		req := formReq(t, "POST", "/admin/orders/"+strconv.Itoa(id), url.Values{"title": {"manual"}, "status": {status}, "info": {info}})
		req.SetPathValue("id", strconv.Itoa(id))
		w := httptest.NewRecorder()
		s.orderSave(w, req, session{UID: 1, Name: "admin"})
		return w
	}
	stock := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT in_stock FROM goods WHERE id=1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A legacy note that happens to start with 库存不足 does not mean stock is owed.
	if w := save(1, "3", "库存不足，等补货"); w.Code != 302 {
		t.Fatalf("legacy 6->3: %d %s", w.Code, w.Body.String())
	}
	if got := stock(); got != 5 {
		t.Fatalf("legacy order must not take stock again, in_stock=%d", got)
	}
	if w := save(2, "3", "库存不足\n充值账号:QQ2"); w.Code != 302 {
		t.Fatalf("parked 6->3: %d %s", w.Code, w.Body.String())
	}
	if got := stock(); got != 4 {
		t.Fatalf("parked order must take its stock, in_stock=%d", got)
	}
	var info string
	if err := pool.QueryRow(ctx, `SELECT info FROM orders WHERE id=2`).Scan(&info); err != nil {
		t.Fatal(err)
	}
	if info != "充值账号:QQ2" {
		t.Fatalf("short-stock marker must be dropped, buyer input kept: %q", info)
	}
	var owed bool
	if err := pool.QueryRow(ctx, `SELECT stock_owed FROM orders WHERE id=2`).Scan(&owed); err != nil || owed {
		t.Fatalf("stock_owed must be cleared once the stock is taken: %v %v", owed, err)
	}
	// Editing the info text of a parked order (dropping the note) does not lose the debt:
	// 6->5 is still refused and 6->3 still takes the stock.
	if w := save(3, "6", "充值账号:QQ3"); w.Code != 302 {
		t.Fatalf("info edit on parked order: %d", w.Code)
	}
	if w := save(3, "5", "充值账号:QQ3"); w.Code != 400 {
		t.Fatalf("parked 6->5 must stay refused after an info edit: %d", w.Code)
	}
	if w := save(3, "4", "充值账号:QQ3"); w.Code != 302 {
		t.Fatalf("parked 6->4: %d", w.Code)
	}
	if got := stock(); got != 3 {
		t.Fatalf("parked order must still take its stock after an info edit, in_stock=%d", got)
	}
	// A legacy order may still go to 处理失败; a parked one may not.
	if w := save(1, "5", "库存不足，等补货"); w.Code != 302 {
		t.Fatalf("legacy 3->5: %d", w.Code)
	}
}

// Closeout: behind a trusted proxy that forwards no client address every login would
// share the proxy's key; the hard per-address limit must be skipped so strangers cannot
// lock the owner out.
func TestLoginIPKeyWithoutForwardedAddress(t *testing.T) {
	r := httptest.NewRequest("POST", "/admin/login", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	if key, ok := loginIPKey(r); ok || key != loginUnknownKey {
		t.Fatalf("loopback proxy without headers must use the shared key, got %q %v", key, ok)
	}
	// The shared key has a real, wider cap.
	var l loginLimiter
	for i := 0; i < loginUnknownMaxFails; i++ {
		if !l.reserveMax(loginUnknownKey, loginUnknownMaxFails) {
			t.Fatalf("attempt %d refused inside the shared budget", i+1)
		}
	}
	if l.reserveMax(loginUnknownKey, loginUnknownMaxFails) {
		t.Fatal("shared budget must be enforced")
	}
	r.Header.Set("X-Real-IP", "198.51.100.7")
	if key, ok := loginIPKey(r); !ok || key != "ip:198.51.100.7" {
		t.Fatalf("forwarded address must be keyed: %q %v", key, ok)
	}
	d := httptest.NewRequest("POST", "/admin/login", nil)
	d.RemoteAddr = "203.0.113.9:4000"
	if key, ok := loginIPKey(d); !ok || key != "ip:203.0.113.9" {
		t.Fatalf("direct client must be keyed: %q %v", key, ok)
	}
}
