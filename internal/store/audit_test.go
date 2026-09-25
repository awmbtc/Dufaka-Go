package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dufaka/internal/testdb"
)

func TestOrderLifecycleIntegration(t *testing.T) {
	pool := testdb.Open(t)
	orderLifecycle(t, pool, pool)
}

// orderLifecycle seeds through pool and drives the store through dbPool, so
// the same run can use a deliberately small pool (see TestOrderLifecycleSmallPoolIntegration).
func orderLifecycle(t *testing.T, pool, dbPool *pgxpool.Pool) {
	t.Helper()
	db := &DB{Pool: dbPool}
	// Bounded: a checkout that wedges a small pool fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'test');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(1,1,'auto','','',10,0,1),(2,1,'manual','','',10,1,2);
 INSERT INTO carmis(goods_id,carmi) VALUES(1,'ONLY-CARD');
 INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute) VALUES(1,'cldx','cldx',2,3,'','/pay/cldx');
 INSERT INTO coupons(id,discount,coupon,ret) VALUES(1,2,'SAVE',1);
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(1,1);`)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var orders []Order
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, e := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "audit@example.com", Coupon: "SAVE"}, Site{})
			if e == nil {
				mu.Lock()
				orders = append(orders, o)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(orders) != 1 {
		t.Fatalf("last-card sale succeeded %d times", len(orders))
	}
	o := orders[0]
	amount, exp, err := db.LockCldxQuote(ctx, o.SN, 10000, 9999999999)
	if err != nil {
		t.Fatal(err)
	}
	a2, e2, err := db.LockCldxQuote(ctx, o.SN, 25000, 19999999999)
	if err != nil || a2 != amount || e2 != exp {
		t.Fatalf("quote changed: %d %d %v", a2, e2, err)
	}
	pool.Exec(ctx, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	if err = db.ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	expired, _ := db.OrderBySN(ctx, o.SN)
	if expired.Actual != o.Actual {
		t.Fatal("expiry changed the payable amount")
	}
	if _, err = db.Complete(ctx, o.SN, o.Actual+1, "wrong"); err == nil {
		t.Fatal("accepted wrong amount")
	}
	if _, err = db.Complete(ctx, o.SN, o.Actual, "receipt"); err != nil {
		t.Fatal(err)
	}
	already, err := db.Complete(ctx, o.SN, o.Actual, "receipt")
	if err != nil || !already {
		t.Fatalf("not idempotent %v", err)
	}
	done, _ := db.OrderBySN(ctx, o.SN)
	if done.Info != "ONLY-CARD" || done.Status != 4 {
		t.Fatalf("not delivered: %+v", done)
	}
	var sales int
	pool.QueryRow(ctx, `SELECT sales_volume FROM goods WHERE id=1`).Scan(&sales)
	if sales != 1 {
		t.Fatalf("duplicate sales %d", sales)
	}
	manual, err := db.CreateOrder(ctx, CreateInput{GID: 2, PayID: 1, Amount: 1, Email: "audit@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, manual.ID)
	if err = db.ExpireDue(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Complete(ctx, manual.SN, manual.Actual, "late"); err != nil {
		t.Fatal(err)
	}
	var stock int
	pool.QueryRow(ctx, `SELECT in_stock FROM goods WHERE id=2`).Scan(&stock)
	if stock != 0 {
		t.Fatalf("late manual payment failed to reserve stock: %d", stock)
	}
}
