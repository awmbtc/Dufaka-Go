package httpx

import (
	"bytes"
	"testing"

	"dufaka/internal/order"
	"dufaka/internal/store"
)

func TestTemplatesParse(t *testing.T) {
	app, err := New(nil, "http://127.0.0.1", "dufaka.json")
	if err != nil {
		t.Fatal(err)
	}
	site := store.Site{Title: "Dufaka-Go", TextLogo: "Dufaka-Go", Notice: "公告", ExpireMin: 5}
	groups := []store.Group{{ID: 1, Name: "测试", Goods: []store.Good{{ID: 1, Name: "支付测试", Actual: 100, InStock: 1, Type: 1}}}}
	pages := []string{"home.html", "buy.html", "bill.html", "orders.html", "search.html", "qrpay.html", "error.html"}
	data := map[string]any{
		"Title": "首页", "Site": site, "Groups": groups,
		"Good": groups[0].Goods[0], "Pays": []store.Pay{{ID: 1, Name: "微信扫码", Check: "wescan"}},
		"Order":       store.Order{SN: "ABC", Title: "支付测试", Amount: 1, Actual: order.Cents(100), Status: 1},
		"PayDeadline": int64(1893456000),
		"Pay":         store.Pay{Name: "微信扫码", Check: "wescan"},
		"Orders":      []store.Order{{SN: "ABC", Title: "支付测试", Actual: 100, Status: 4, Info: "ok"}},
		"Message":     "错误", "QR": "aaaa",
	}
	for _, name := range pages {
		var buf bytes.Buffer
		if err := app.render(&buf, name, data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(buf.Bytes(), []byte("<<")) {
			t.Fatalf("%s leaked a template marker", name)
		}
		if name == "qrpay.html" && !bytes.Contains(buf.Bytes(), []byte("check-order-status")) {
			t.Fatalf("%s qr page does not poll payment status", name)
		}
		if bytes.Contains(buf.Bytes(), []byte("Powered by")) && !bytes.Contains(buf.Bytes(), []byte("https://github.com/awmbtc/Dufaka-Go")) {
			t.Fatalf("%s footer does not link to GitHub", name)
		}
		if bytes.Contains(buf.Bytes(), []byte("独角")) || bytes.Contains(buf.Bytes(), []byte("default.jpg")) {
			t.Fatalf("%s still contains the old brand image", name)
		}
		if (name == "home.html" || name == "buy.html") && !bytes.Contains(buf.Bytes(), []byte("/assets/brand/logo.svg")) {
			t.Fatalf("%s missing the Dufaka logo", name)
		}
		if name == "home.html" && !bytes.Contains(buf.Bytes(), []byte(`class="site-mark"`)) {
			t.Fatalf("home title has no site logo")
		}
		if name == "bill.html" && !bytes.Contains(buf.Bytes(), []byte(`id="pay-left"`)) {
			t.Fatalf("bill has no payment countdown")
		}
	}
}
