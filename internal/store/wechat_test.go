package store

import (
	"context"
	"testing"

	"dufaka/internal/testdb"
)

// PayByCheck is the one lookup by pay_check: it returns the channel open or closed, with
// its keys, skips a soft-deleted row and fails for an unknown slug. pays.pay_check is
// UNIQUE in the schema, so there is never more than one row to choose from.
func TestPayByCheck(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
 INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_id,merchant_key,merchant_pem,pay_handleroute,is_open) VALUES
  (1,'微信扫码','wescan',1,3,'mch-1','key-1','pem-1','/pay/wepay',0),
  (2,'cldx','cldx',2,3,'','','','/pay/cldx',1);`)
	if err != nil {
		t.Fatal(err)
	}
	// The schema really does forbid a second wescan row.
	if _, err := pool.Exec(ctx, `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute) VALUES(3,'第二个','wescan',1,3,'','/pay/wepay')`); err == nil {
		t.Fatal("pays.pay_check is not UNIQUE")
	}
	p, err := db.PayByCheck(ctx, "wescan")
	if err != nil || p.ID != 1 || p.Open != 0 || p.MerchantID != "mch-1" || p.MerchantKey != "key-1" || p.MerchantPem != "pem-1" {
		t.Fatalf("closed wescan row: %v %+v", err, p)
	}
	if c, err := db.PayByCheck(ctx, "cldx"); err != nil || c.ID != 2 || c.Open != 1 {
		t.Fatalf("cldx row: %v %+v", err, c)
	}
	if _, err := db.PayByCheck(ctx, "alipay"); err == nil {
		t.Fatal("unknown slug found")
	}
	if _, err := pool.Exec(ctx, `UPDATE pays SET deleted_at=now() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PayByCheck(ctx, "wescan"); err == nil {
		t.Fatal("soft-deleted row returned")
	}
}

// OrderBySNChecked answers 订单不存在 only for an order that really is not there: a broken
// table, a cancelled context or a closed pool come back as ordinary errors, so the cashier
// poll keeps polling instead of telling a paying customer the order expired.
func TestOrderBySNCheckedTellsMissingFromFailure(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'test');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type,in_stock) VALUES(1,1,'manual','','',10,2,5);
 INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute) VALUES(1,'cldx','cldx',2,3,'','/pay/cldx');`); err != nil {
		t.Fatal(err)
	}
	o, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "c@example.com"}, Site{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db.OrderBySNChecked(ctx, o.SN); err != nil || got.SN != o.SN {
		t.Fatalf("existing order: %v %+v", err, got)
	}
	if _, err := db.OrderBySNChecked(ctx, "FFFFFFFFFFFFFFFF"); !IsNotFound(err) {
		t.Fatalf("unknown order: %v", err)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.OrderBySNChecked(gone, o.SN); err == nil || IsNotFound(err) {
		t.Fatalf("cancelled context reported as not found: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE orders RENAME TO orders_gone`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OrderBySNChecked(ctx, o.SN); err == nil || IsNotFound(err) {
		t.Fatalf("broken table reported as not found: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE orders_gone RENAME TO orders`); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	if _, err := db.OrderBySNChecked(ctx, o.SN); err == nil || IsNotFound(err) {
		t.Fatalf("closed pool reported as not found: %v", err)
	}
}

// Repair pass 2: an order number that cannot be stored (NUL, other control characters,
// invalid UTF-8, longer than the column) is 订单不存在 without a query. The pool is closed
// first, so any query would fail and turn the answer into a database error.
func TestOrderBySNCheckedRejectsImpossibleNumbersWithoutQuery(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx := context.Background()
	// Before closing: a NUL reaching PostgreSQL would be SQLSTATE 22021, not "not found".
	if _, err := db.OrderBySNChecked(ctx, "a\x00\nFORGED"); !IsNotFound(err) {
		t.Fatalf("NUL order number: %v", err)
	}
	pool.Close()
	long := make([]byte, maxOrderSNRunes+1)
	for i := range long {
		long[i] = 'A'
	}
	for _, sn := range []string{"", "a\x00b", "a\nb", "a\rb", "\x7f", "a\u0085b", "\xff\xfe", string(long)} {
		if _, err := db.OrderBySNChecked(ctx, sn); !IsNotFound(err) {
			t.Fatalf("%q: want 订单不存在 without a query, got %v", sn, err)
		}
	}
	// A plausible number (imported dujiaoka shape, 150 runes) still goes to the database,
	// which is closed here, so it must come back as a failure rather than not-found.
	for _, sn := range []string{"DJK20260925abcXYZ", string(long[:maxOrderSNRunes]), "订单甲乙丙"} {
		if _, err := db.OrderBySNChecked(ctx, sn); err == nil || IsNotFound(err) {
			t.Fatalf("%q skipped the database: %v", sn, err)
		}
	}
}
