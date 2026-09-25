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
			"Order":       store.Order{SN: "ABC", Title: "支付测试", Amount: 1, Actual: order.Cents(100), Status: 1},
			"PayDeadline": int64(1893456000),
			"Pay":         store.Pay{Name: "微信扫码", Check: "wescan"},
			"Orders":      []store.Order{{SN: "ABC", Title: "支付测试", Actual: 100, Status: 4, Info: "ok"}},
			"Message":     "错误", "QR": "aaaa",
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
			if bytes.Contains(buf.Bytes(), []byte("Powered by")) && !bytes.Contains(buf.Bytes(), []byte("https://github.com/awmbtc/Dufaka-Go")) {
				t.Fatalf("%s %s footer does not link to GitHub", theme, name)
			}
			if bytes.Contains(buf.Bytes(), []byte("独角")) || bytes.Contains(buf.Bytes(), []byte("default.jpg")) {
				t.Fatalf("%s %s still contains the old brand image", theme, name)
			}
			if (name == "home.html" || name == "buy.html") && !bytes.Contains(buf.Bytes(), []byte("/assets/brand/logo.svg")) {
				t.Fatalf("%s %s missing the Dufaka logo", theme, name)
			}
			if name == "home.html" && !bytes.Contains(buf.Bytes(), []byte(`class="site-mark"`)) {
				t.Fatalf("%s home title has no site logo", theme)
			}
			if theme == "luna" && name == "error.html" && !bytes.Contains(buf.Bytes(), []byte("err_title")) {
				t.Fatalf("luna error page is not using the centered notice layout")
			}
			if theme == "luna" && name == "search.html" && !bytes.Contains(buf.Bytes(), []byte("layui-tab-item")) {
				t.Fatalf("luna search is not using the tab layout")
			}
			if name == "bill.html" && !bytes.Contains(buf.Bytes(), []byte(`id="pay-left"`)) {
				t.Fatalf("%s bill has no payment countdown", theme)
			}
			if theme == "luna" && name == "orders.html" && !bytes.Contains(buf.Bytes(), []byte("order-list")) {
				t.Fatalf("luna order result is not using the order list layout")
			}
		}
	}
}
