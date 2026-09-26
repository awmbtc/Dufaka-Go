package pay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The wallet answers GET /v1/payments only to the payee itself, so the receipt lookup
// must carry the shop's merchant credentials; an anonymous call is refused with 401.
func TestReceiptSendsMerchantCredentials(t *testing.T) {
	var got http.Header
	var query string
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payments" {
			http.NotFound(w, r)
			return
		}
		got = r.Header.Clone()
		query = r.URL.RawQuery
		if r.Header.Get("X-Merchant-Id") != "shop" || r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		fmt.Fprint(w, `{"to":"shop","order":"SN1","from":"cldx1payer","amount":15000,"refunded":0,"paid":true,"at":"2026-09-26T08:00:00Z"}`)
	}))
	defer wallet.Close()

	w := Wallet{Base: wallet.URL + "/", MerchantID: "shop", Secret: "s3cret"}
	r, err := w.Receipt(context.Background(), "shop", "SN1")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Paid || r.Amount != 15000 || r.From != "cldx1payer" {
		t.Fatalf("receipt %+v", r)
	}
	if query != "to=shop&order=SN1" {
		t.Fatalf("query %q", query)
	}
	if got.Get("X-Merchant-Id") != "shop" || got.Get("Authorization") != "Bearer s3cret" {
		t.Fatalf("headers %v", got)
	}

	// Without a secret the lookup is anonymous and the wallet's 401 surfaces as an error
	// instead of a silently unpaid receipt.
	if _, err := (Wallet{Base: wallet.URL, MerchantID: "shop"}).Receipt(context.Background(), "shop", "SN1"); err == nil {
		t.Fatal("anonymous receipt lookup did not fail")
	}
}
