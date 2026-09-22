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
			if theme == "luna" && name == "home.html" && !bytes.Contains(buf.Bytes(), []byte("goods-list")) {
				t.Fatalf("luna home missing goods list")
			}
			if theme == "hyper" && name == "home.html" && !bytes.Contains(buf.Bytes(), []byte("home-card")) {
				t.Fatalf("hyper home missing card")
			}
			if bytes.Contains(buf.Bytes(), []byte("独角")) {
				t.Fatalf("%s %s still contains old brand", theme, name)
			}
		}
	}
}
