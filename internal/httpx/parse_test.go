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
	site := store.Site{Title: "Dufaka-Go", TextLogo: "Dufaka-Go", Template: "luna", Notice: "公告", ExpireMin: 5}
	groups := []store.Group{{ID: 1, Name: "测试", Goods: []store.Good{{ID: 1, Name: "支付测试", Actual: 100, InStock: 1, Type: 1}}}}
	pages := []string{"home.html", "buy.html", "bill.html", "orders.html", "search.html", "qrpay.html", "error.html"}
	for _, theme := range []string{"unicorn", "luna", "hyper"} {
		site.Template = theme
		data := map[string]any{
			"Title": "首页", "Site": site, "Groups": groups,
			"Good": groups[0].Goods[0], "Pays": []store.Pay{{ID: 1, Name: "微信扫码", Check: "wescan"}},
			"Order":   store.Order{SN: "ABC", Title: "支付测试", Amount: 1, Actual: order.Cents(100), Status: 1},
			"Pay":     store.Pay{Name: "微信扫码", Check: "wescan"},
			"Orders":  []store.Order{{SN: "ABC", Title: "支付测试", Actual: 100, Status: 4, Info: "ok"}},
			"Message": "错误", "QR": "aaaa",
		}
		for _, name := range pages {
			var buf bytes.Buffer
			if err := app.render(&buf, name, data); err != nil {
				t.Fatalf("%s %s: %v", theme, name, err)
			}
			if bytes.Contains(buf.Bytes(), []byte("<<")) {
				t.Fatalf("%s %s leaked a template marker", theme, name)
			}
			if theme == "luna" && name == "home.html" {
				if !bytes.Contains(buf.Bytes(), []byte("支付测试")) || !bytes.Contains(buf.Bytes(), []byte("cate-box")) {
					t.Fatalf("luna home did not render the category and product")
				}
			}
			if theme == "luna" && name == "buy.html" && !bytes.Contains(buf.Bytes(), []byte("微信扫码")) {
				t.Fatalf("luna buy hid the pay method name")
			}
			if theme == "hyper" && name == "home.html" {
				if !bytes.Contains(buf.Bytes(), []byte("home-card")) || !bytes.Contains(buf.Bytes(), []byte(`id="group-1"`)) {
					t.Fatalf("hyper home missing a category pane")
				}
			}
			if name == "qrpay.html" && !bytes.Contains(buf.Bytes(), []byte("check-order-status")) {
				t.Fatalf("%s qr page does not poll payment status", theme)
			}
			if bytes.Contains(buf.Bytes(), []byte("独角")) {
				t.Fatalf("%s %s still contains old brand", theme, name)
			}
		}
	}
}
