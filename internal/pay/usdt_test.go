package pay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testTronAddress = "TJRabPrwbZy45sbavfcjinPJC18kjpRTv8"

func testInvoice() USDTInvoice {
	return USDTInvoice{ID: "usdt_0123456789abcdef01234567", OrderNo: "shop-TEST-a1b2", Status: "pending", Network: "TRON", Token: "USDT", Chain: "TRC20", Address: testTronAddress, CNY: "10.00", Amount: "1.370013", Expires: time.Now().UTC().Add(30 * time.Minute)}
}
func TestUSDTCreateAndReceipt(t *testing.T) {
	p := testInvoice()
	paidAt := time.Now().UTC()
	reply := p
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer only-on-server" {
			t.Error("missing server authentication")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/payments" {
			var body struct {
				Ref    string      `json:"order_no"`
				Amount json.Number `json:"cny_amount"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !strings.HasPrefix(body.Ref, "shop-TEST-") || body.Amount != "10.00" {
				t.Error("wrong order request")
			}
			reply = p
			reply.OrderNo = body.Ref
			p.OrderNo = body.Ref
		}
		json.NewEncoder(w).Encode(reply)
	}))
	defer provider.Close()
	u := USDT{Base: provider.URL, Secret: "only-on-server", Address: testTronAddress}
	want, err := u.Create(context.Background(), "TEST", 1000)
	if err != nil {
		t.Fatal(err)
	}
	reply = want
	reply.Status = "paid"
	reply.TxID = strings.Repeat("a", 64)
	reply.PaidAt = &paidAt
	got, err := u.Check(context.Background(), want, 1000, time.Now().Add(-time.Minute))
	if err != nil || !got.Paid || got.Amount != 1000 {
		t.Fatalf("valid receipt rejected: %+v %v", got, err)
	}
	good := reply
	cases := map[string]func(*USDTInvoice){
		"amount": func(p *USDTInvoice) { p.Amount = "1.370014" }, "fiat": func(p *USDTInvoice) { p.CNY = "10.001" },
		"address": func(p *USDTInvoice) { p.Address = "T" + strings.Repeat("a", 33) }, "order": func(p *USDTInvoice) { p.OrderNo = "another" },
		"id": func(p *USDTInvoice) { p.ID = "usdt_1123456789abcdef01234567" }, "chain": func(p *USDTInvoice) { p.Chain = "ERC20" },
		"token": func(p *USDTInvoice) { p.Token = "USDC" }, "status": func(p *USDTInvoice) { p.Status = "success" },
		"tx": func(p *USDTInvoice) { p.TxID = "" }, "time_missing": func(p *USDTInvoice) { p.PaidAt = nil },
		"too_early": func(p *USDTInvoice) { x := time.Now().Add(-time.Hour); p.PaidAt = &x },
		"too_late":  func(p *USDTInvoice) { x := p.Expires.Add(time.Hour); p.PaidAt = &x },
		"deadline":  func(p *USDTInvoice) { p.Expires = p.Expires.Add(time.Second) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			reply = good
			change(&reply)
			got, err := u.Check(context.Background(), want, 1000, time.Now().Add(-time.Minute))
			if err == nil || got.Paid {
				t.Fatal("accepted mismatched receipt")
			}
		})
	}
	for _, status := range []string{"pending", "expired"} {
		reply = want
		reply.Status = status
		got, err := u.Check(context.Background(), want, 1000, time.Now())
		if err != nil || got.Paid {
			t.Fatal("unpaid receipt treated as payment")
		}
	}
}
func TestUSDTRefusesUnsafeEndpointAndRedirect(t *testing.T) {
	for _, base := range []string{"http://payments.example", "https://name:pass@payments.example", "https://payments.example?key=x", "https://payments.example/#x"} {
		if (USDT{Base: base, Secret: "secret", Address: testTronAddress}).Enabled() {
			t.Fatal("unsafe endpoint accepted")
		}
	}
	redirected := false
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer dest.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 307) }))
	defer source.Close()
	u := USDT{Base: source.URL, Secret: "secret", Address: testTronAddress}
	_, err := u.Create(context.Background(), "TEST", 1000)
	if err == nil || redirected {
		t.Fatal("followed authenticated redirect")
	}
}
