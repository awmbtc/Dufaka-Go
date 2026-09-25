package httpx

import (
	"bytes"
	"dufaka/internal/store"
	"encoding/base64"
	"github.com/skip2/go-qrcode"
	"html/template"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserOrderCookie(t *testing.T) {
	t.Setenv("DUFAKA_SESSION_KEY", "audit-test-key")
	r := httptest.NewRequest("GET", "https://shop.example/", nil)
	w := httptest.NewRecorder()
	remember(w, r, "ORDER1")
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing cookie")
	}
	r.AddCookie(cookies[0])
	if !ownsOrder(r, "ORDER1") {
		t.Fatal("browser failed to retain order")
	}
	if got := browserOrders(orderCookie(base64.RawURLEncoding.EncodeToString([]byte(`["ORDER1"]`))) + "x"); len(got) != 0 {
		t.Fatal("accepted tampering")
	}
	if len(browserOrders(`["ORDER1"]`)) != 0 {
		t.Fatal("accepted unsigned order ownership")
	}
}

func TestRichTextAndAppLink(t *testing.T) {
	clean := string(rich(`<p>说明<strong>重点</strong><script>alert(1)</script><img src=x onerror=alert(1)><a href="javascript:alert(1)">link</a></p>`))
	if !strings.Contains(clean, "<strong>重点</strong>") || strings.Contains(clean, "<script") || strings.Contains(clean, "onerror") || strings.Contains(clean, "javascript:") {
		t.Fatalf("bad sanitized HTML %s", clean)
	}
	app, _ := New(nil, "http://localhost", "")
	for _, theme := range []string{"unicorn", "luna", "hyper"} {
		var buf bytes.Buffer
		data := map[string]any{"Site": store.Site{Title: "Test", Template: theme}, "Order": store.Order{SN: "ORDER", Status: 1}, "Direct": true, "Cldx": "1", "PayDeadline": time.Now().Add(time.Hour).Unix(), "AppLink": template.URL("clodex://pay?to=shop&amount=10000&order=ORDER&exp=9999999999")}
		if err := app.render(&buf, "cldxpay.html", data); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "ZgotmplZ") || !strings.Contains(buf.String(), "clodex://pay?") {
			t.Fatalf("%s lost app link", theme)
		}
		if theme == "luna" && strings.Contains(buf.String(), "navbar-expand-lg") {
			t.Fatal("luna payment uses bootstrap shell")
		}
	}
}

func TestExportAuditPages(t *testing.T) {
	dir := os.Getenv("DUFAKA_AUDIT_OUTPUT")
	if dir == "" {
		t.Skip("optional browser fixtures")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	app, err := New(nil, "http://localhost", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, theme := range []string{"unicorn", "luna", "hyper"} {
		site := store.Site{Title: "Clodex小店", TextLogo: "Clodex小店", Template: theme, SearchPwd: true, ExpireMin: 30, Notice: "<p>欢迎来到 <strong>Clodex 小店</strong>，请按商品说明购买。</p>", Footer: "<p>客服说明 · 本地审核环境</p>"}
		g := store.Good{ID: 1, Name: "会员月卡：长标题显示与自动发货测试", Type: 2, InStock: 12, BuyLimit: 5, Actual: 1990, Retail: 2990, Wholesale: "3=15.00\n10=12.00", Other: "account=账号=1", Prompt: "<p>请确认商品适用于你的账号。</p>", Description: "<h3>使用说明</h3><p>购买后请在订单详情查看卡密。</p>"}
		o := store.Order{SN: "AUDIT-ORDER-2026", Title: g.Name, Amount: 1, Type: 2, Actual: 1990, GoodsPrice: 1990, Status: 1, Email: "audit@example.com", Created: time.Now(), Info: "AUDIT-CARD-001"}
		qr, _ := qrcode.Encode("https://example.com/audit", qrcode.Medium, 256)
		data := map[string]any{"QR": base64.StdEncoding.EncodeToString(qr), "Title": "页面审核", "Site": site, "Good": g, "Groups": []store.Group{{ID: 1, Name: "会员服务", Goods: []store.Good{g}}, {ID: 2, Name: "第二分类", Goods: []store.Good{{ID: 2, Name: "缺货商品", Type: 1, InStock: 0, Actual: 100}}}}, "Order": o, "Orders": []store.Order{o}, "Pay": store.Pay{Name: "微信扫码", Check: "wescan"}, "Pays": []store.Pay{{ID: 1, Name: "微信扫码", Check: "wescan"}, {ID: 2, Name: "cldx", Check: "cldx"}}, "PayDeadline": time.Now().Add(30 * time.Minute).Unix(), "Cldx": "2.85", "Direct": true, "AppLink": template.URL("clodex://pay?to=shop&amount=28500&order=AUDIT&exp=9999999999"), "Message": "订单已过期，请重新选购"}
		for _, page := range []string{"home", "buy", "bill", "search", "orders", "qrpay", "cldxpay", "error"} {
			var buf bytes.Buffer
			if err := app.render(&buf, page+".html", data); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, theme+"-"+page+".html"), buf.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestAdminOriginGuard(t *testing.T) {
	app, _ := New(nil, "http://shop.test", "")
	r := httptest.NewRequest("POST", "http://shop.test/admin/goods", strings.NewReader("x=1"))
	r.Header.Set("Origin", "https://foreign.example")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-origin admin write: %d", w.Code)
	}
}
