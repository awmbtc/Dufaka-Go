package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// SHOP-01: one address cannot hold the stock with unpaid orders. The store's
// unpaid quota counts an IPv6 /64 as one address, so rotating the host part does
// not reset it, and checkout has its own request budget per address.
func TestCheckoutLimitsPerAddress(t *testing.T) {
	app, _ := auditOrder(t)
	app.wallet.Secret, app.wallet.MerchantID = "test-only", "shop-test" // cldx checkout needs the wallet (A3-10)
	for i := 0; i < 12; i++ {
		if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO carmis(goods_id,carmi) VALUES(1,$1)`, fmt.Sprintf("QUOTA-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	h := installed(t, app)
	post := func(remote, email string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://shop.test/create-order",
			strings.NewReader("gid=1&payway=1&by_amount=1&email="+url.QueryEscape(email)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// One /64, a new host address and email each time: five unpaid orders.
	for i := 0; i < 5; i++ {
		if w := post(fmt.Sprintf("[2001:db8:5:6::%x]:1", i+1), fmt.Sprintf("v6-%d@example.com", i)); w.Code != http.StatusFound {
			t.Fatalf("order %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	w := post("[2001:db8:5:6::99]:1", "v6-next@example.com")
	if w.Code == http.StatusFound || !strings.Contains(w.Body.String(), "未付款订单过多") {
		t.Fatalf("sixth unpaid order from one /64: %d %s", w.Code, w.Body.String())
	}
	// Six checkouts so far; four more fit the request budget, the eleventh does not.
	for i := 0; i < orderLimit-6; i++ {
		if w := post("[2001:db8:5:6::aa]:1", "v6-more@example.com"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("checkout %d throttled early", i+7)
		}
	}
	w = post("[2001:db8:5:6::ab]:1", "v6-last@example.com")
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "下单过于频繁") {
		t.Fatalf("checkout over budget: %d %s", w.Code, w.Body.String())
	}
	// Another address still buys.
	if w := post("192.0.2.80:1", "fresh@example.com"); w.Code != http.StatusFound {
		t.Fatalf("another address: %d %s", w.Code, w.Body.String())
	}
}
