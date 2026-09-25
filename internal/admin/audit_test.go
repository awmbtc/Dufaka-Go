package admin

import (
	"context"
	"dufaka/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrdersTitleAndPaymentStateIntegration(t *testing.T) {
	pool := testdb.Open(t)
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'test');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type) VALUES(1,1,'card','','',10,1);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status) VALUES(1,'AUDIT',1,'商品','a@example.com','local',1);
 INSERT INTO carmis(id,goods_id,carmi,reserved_order_id) VALUES(1,1,'reserved',1);`)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/admin/orders?title=商品", nil)
	w := httptest.NewRecorder()
	s.ordersList(w, r, session{Name: "audit"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<title>订单列表 - Dufaka-Go</title>") {
		t.Fatal("order filter overwrote page title")
	}
	r = httptest.NewRequest("POST", "/admin/orders/1", strings.NewReader("title=card&status=3"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", "1")
	w = httptest.NewRecorder()
	s.orderSave(w, r, session{Name: "audit"})
	if w.Code != 400 {
		t.Fatalf("unpaid order promoted to processing: %d", w.Code)
	}
	r = httptest.NewRequest("POST", "/admin/carmis/1/delete", nil)
	r.SetPathValue("id", "1")
	w = httptest.NewRecorder()
	s.carmiDelete(w, r, session{Name: "audit"})
	var live bool
	pool.QueryRow(ctx, `SELECT deleted_at IS NULL FROM carmis WHERE id=1`).Scan(&live)
	if !live {
		t.Fatal("deleted reserved card")
	}
}
