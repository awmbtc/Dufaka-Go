package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"dufaka/internal/testdb"
)

// SHOP-01: anyone can place an order, and an unpaid order holds its stock until
// it expires, so one address (or one email) gets only a few unpaid orders at a
// time. The audit probe held all 40 cards with 40 unpaid orders from one buyer.
func TestUnpaidQuotaIntegration(t *testing.T) {
	pool := testdb.Open(t)
	db := &DB{Pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	seed := `INSERT INTO goods_group(id,gp_name) VALUES(1,'test');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(1,1,'auto','','',10,0,1);
 INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute) VALUES(1,'cldx','cldx',2,3,'','/pay/cldx');`
	for i := 0; i < 40; i++ {
		seed += fmt.Sprintf("\n INSERT INTO carmis(goods_id,carmi) VALUES(1,'CARD-%d');", i)
	}
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatal(err)
	}
	site := Site{ExpireMin: 30}
	order := func(email, source string) error {
		_, err := db.CreateOrder(ctx, CreateInput{GID: 1, PayID: 1, Amount: 1, Email: email, IP: "203.0.113.9", Source: source}, site)
		return err
	}
	refused := func(t *testing.T, err error, why string) {
		t.Helper()
		if err == nil || err.Error() != msgTooManyUnpaid {
			t.Fatalf("%s: want %q, got %v", why, msgTooManyUnpaid, err)
		}
	}
	reserved := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE reserved_order_id IS NOT NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// One address, a fresh email each time: five unpaid orders, then no more.
	for i := 0; i < maxUnpaidPerAddress; i++ {
		if err := order(fmt.Sprintf("a%d@example.com", i), "203.0.113.9"); err != nil {
			t.Fatalf("order %d: %v", i, err)
		}
	}
	refused(t, order("a-next@example.com", "203.0.113.9"), "sixth unpaid order from one address")
	if got := reserved(); got != maxUnpaidPerAddress {
		t.Fatalf("a refused order must not reserve stock: reserved %d", got)
	}

	// One email from changing addresses: three, then no more (case does not help).
	for i := 0; i < maxUnpaidPerEmail; i++ {
		if err := order("same@example.com", fmt.Sprintf("198.51.100.%d", i+1)); err != nil {
			t.Fatalf("email order %d: %v", i, err)
		}
	}
	refused(t, order("Same@example.com", "198.51.100.200"), "fourth unpaid order for one email")

	// Another buyer is not affected.
	if err := order("other@example.com", "192.0.2.50"); err != nil {
		t.Fatalf("another buyer: %v", err)
	}

	// Concurrent checkouts from one new address cannot all pass the count.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if order(fmt.Sprintf("burst%d@example.com", i), "2001:db8:1:2::/64") == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != maxUnpaidPerAddress {
		t.Fatalf("concurrent burst from one address: %d orders, want %d", ok, maxUnpaidPerAddress)
	}

	// Paid and expired orders no longer count.
	if _, err := pool.Exec(ctx, `UPDATE orders SET status=4 WHERE id = (SELECT min(id) FROM orders WHERE buy_source='203.0.113.9')`); err != nil {
		t.Fatal(err)
	}
	if err := order("a-paid@example.com", "203.0.113.9"); err != nil {
		t.Fatalf("after one order was paid: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orders SET created_at = now() - interval '1 hour' WHERE lower(email)='same@example.com'`); err != nil {
		t.Fatal(err)
	}
	if err := order("same@example.com", "198.51.100.201"); err != nil {
		t.Fatalf("after the email's orders passed their expiry: %v", err)
	}

	// Without a known address (a proxy that forwards none) only the email counts,
	// so the proxy's own address is never a quota shared by every buyer.
	for i := 0; i < maxUnpaidPerAddress+2; i++ {
		if err := order(fmt.Sprintf("noaddr%d@example.com", i), ""); err != nil {
			t.Fatalf("unknown address, order %d: %v", i, err)
		}
	}
}
