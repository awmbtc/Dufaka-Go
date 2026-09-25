package httpx

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skip2/go-qrcode"

	"dufaka/internal/store"
)

// #2: no directory listing, no dotfiles, real assets still served.
func TestStaticFilesNoListingNoDotfiles(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"brand/logo.svg", "brand/._logo.svg", "._NOTICE", ".hidden/x.css", "style/app.css", "index.html", "brand/index.html"} {
		if err := os.MkdirAll(filepath.Join(root, "web/assets", filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "web/assets", f), []byte("data:"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	app, err := New(nil, "http://shop.test", "")
	if err != nil {
		t.Fatal(err)
	}
	h := app.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test"+path, nil))
		return w
	}
	for _, path := range []string{
		"/assets/", "/assets/brand/", "/assets/brand", "/assets/._NOTICE", "/assets/brand/._logo.svg", "/assets/.hidden/x.css", "/assets/nope.css",
		"/assets/%2E_NOTICE", "/assets/brand%2F", "/assets/brand/%2E%2E/._NOTICE", "/assets/index.html", "/assets/brand/index.html",
	} {
		if w := get(path); w.Code != 404 {
			t.Fatalf("%s answered %d: %q", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/assets/brand/logo.svg", "/assets/style/app.css"} {
		w := get(path)
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "data:") {
			t.Fatalf("%s answered %d: %q", path, w.Code, w.Body.String())
		}
	}
	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest("HEAD", "http://shop.test/assets/", nil))
	if head.Code != 404 {
		t.Fatalf("HEAD /assets/ answered %d", head.Code)
	}
	// Plain http.FileServer semantics are kept: HEAD, Range and conditional requests work.
	head = httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest("HEAD", "http://shop.test/assets/brand/logo.svg", nil))
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(len("data:brand/logo.svg")) {
		t.Fatalf("HEAD asset: %d %q %q", head.Code, head.Body.String(), head.Header().Get("Content-Length"))
	}
	ranged := httptest.NewRequest("GET", "http://shop.test/assets/brand/logo.svg", nil)
	ranged.Header.Set("Range", "bytes=0-4")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, ranged)
	if w.Code != 206 || w.Body.String() != "data:" {
		t.Fatalf("Range asset: %d %q", w.Code, w.Body.String())
	}
}

// #4: the visitor address comes from the proxy headers only when the peer is the proxy
// (loopback by default). clientIP is netx.ClientIP: X-Real-IP first, then the last
// parsable X-Forwarded-For entry (the one the proxy appended), and only values that parse
// as an IP.
func TestClientIP(t *testing.T) {
	cases := []struct {
		remote, real, fwd, want string
	}{
		{"127.0.0.1:41234", "203.0.113.9", "", "203.0.113.9"},
		// Default mode reads only X-Real-IP (nginx overwrites it); X-Forwarded-For is
		// ignored so a client cannot supply it when the proxy does not set it.
		{"127.0.0.1:41234", "", "203.0.113.9, 10.0.0.2", "127.0.0.1"},
		{"127.0.0.1:41234", "203.0.113.9:8080", "198.51.100.1", "203.0.113.9"},
		{"[::1]:41234", "", "[2001:db8::7]:443", "::1"},
		{"[::1]:5", "", " 198.51.100.7 ,203.0.113.1", "::1"},
		// A private-network peer is not a trusted proxy unless DUFAKA_TRUSTED_PROXIES lists it.
		{"10.1.2.3:5", "", " 198.51.100.7 ,203.0.113.1", "10.1.2.3"},
		{"127.0.0.1:41234", "not an ip", "203.0.113.9", "127.0.0.1"},
		{"127.0.0.1:41234", strings.Repeat("9", 60), "", "127.0.0.1"},
		{"192.168.1.5:5", "", "", "192.168.1.5"},
		{"127.0.0.1:41234", "", "", "127.0.0.1"},
		{"203.0.113.50:9", "1.1.1.1", "2.2.2.2", "203.0.113.50"},
		{"[2001:db8::1]:9", "1.1.1.1", "", "2001:db8::1"},
		{"203.0.113.50", "1.1.1.1", "", "203.0.113.50"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/create-order", nil)
		r.RemoteAddr = c.remote
		if c.real != "" {
			r.Header.Set("X-Real-IP", c.real)
		}
		if c.fwd != "" {
			r.Header.Set("X-Forwarded-For", c.fwd)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("remote=%q real=%q fwd=%q: got %q want %q", c.remote, c.real, c.fwd, got, c.want)
		}
	}
}

// #15: hardening headers on every response, HSTS only over https.
func TestSecurityHeaders(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	app, err := New(nil, "http://shop.test", "")
	if err != nil {
		t.Fatal(err)
	}
	h := app.Handler()
	check := func(r *http.Request, hsts bool) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "strict-origin-when-cross-origin",
			"Content-Security-Policy": "frame-ancestors 'none'",
		}
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Fatalf("%s %s: %s=%q want %q", r.Method, r.URL, k, got, v)
			}
		}
		got := w.Header().Get("Strict-Transport-Security")
		if hsts && got != "max-age=31536000" {
			t.Fatalf("%s: HSTS missing over https: %q", r.URL, got)
		}
		if !hsts && got != "" {
			t.Fatalf("%s: HSTS sent over plain http: %q", r.URL, got)
		}
	}
	check(httptest.NewRequest("GET", "http://shop.test/", nil), false)
	check(httptest.NewRequest("GET", "https://shop.test/", nil), true)
	proxied := httptest.NewRequest("GET", "http://shop.test/order-search", nil)
	proxied.RemoteAddr = "127.0.0.1:40000"
	proxied.Header.Set("X-Forwarded-Proto", "https")
	check(proxied, true)
	// A3-8: X-Forwarded-Proto from a peer that is not a trusted proxy is ignored.
	spoofed := httptest.NewRequest("GET", "http://shop.test/order-search", nil)
	spoofed.RemoteAddr = "203.0.113.9:40000"
	spoofed.Header.Set("X-Forwarded-Proto", "https")
	check(spoofed, false)
	lan := httptest.NewRequest("GET", "http://shop.test/order-search", nil)
	lan.RemoteAddr = "10.0.0.5:40000"
	lan.Header.Set("X-Forwarded-Proto", "https")
	check(lan, false)
	check(httptest.NewRequest("GET", "http://shop.test/assets/missing.css", nil), false)
	// A served static asset carries the same headers (framing refused for files too).
	if err := os.MkdirAll(filepath.Join(root, "web/assets/style"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web/assets/style/app.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	served := httptest.NewRecorder()
	h.ServeHTTP(served, httptest.NewRequest("GET", "http://shop.test/assets/style/app.css", nil))
	if served.Code != 200 || served.Header().Get("X-Frame-Options") != "DENY" || served.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("static response lacks security headers: %d %v", served.Code, served.Header())
	}
	admin := httptest.NewRequest("POST", "http://shop.test/admin/goods", strings.NewReader("x=1"))
	admin.Header.Set("Origin", "https://foreign.example")
	check(admin, false)
}

// #6: /cldx/pay only trusts the order number; payee, amount and expiry are rebuilt server side.
func TestCldxLaunchIgnoresCraftedParameters(t *testing.T) {
	app, o := auditOrder(t)
	app.wallet.Secret = "test-only"
	app.wallet.MerchantID = "shop-test"
	h := installed(t, app)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test"+path, nil))
		return w
	}
	// No locked quote yet: nothing to redirect to.
	if w := get("/cldx/pay?order=" + o.SN); w.Code != 404 {
		t.Fatalf("unquoted order answered %d", w.Code)
	}
	exp := time.Now().Add(10 * time.Minute).Unix()
	if _, _, err := app.live().LockCldxQuote(context.Background(), o.SN, 15000, exp); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("clodex://pay?amount=15000&exp=%d&order=%s&to=shop-test", exp, o.SN)
	w := get("/cldx/pay?order=" + o.SN + "&to=attacker&amount=1&exp=9999999999&clodex_app=1")
	if w.Code != 302 || w.Header().Get("Location") != want {
		t.Fatalf("crafted parameters leaked: %d %q", w.Code, w.Header().Get("Location"))
	}
	for _, path := range []string{"/cldx/pay", "/cldx/pay?to=attacker&amount=1", "/cldx/pay?order=NOPE", "/cldx/" + o.SN, "/cldx/pay?order=" + o.SN + "%20"} {
		if w := get(path); w.Code != 404 {
			t.Fatalf("%s answered %d %q", path, w.Code, w.Header().Get("Location"))
		}
	}
	// The QR on the cashier page carries only the order number; the on-page app link carries the server-built query.
	r := httptest.NewRequest("GET", "http://shop.test/pay/cldx/x/"+o.SN, nil)
	page := httptest.NewRecorder()
	app.cldxPay(page, r, o)
	qr, _ := qrcode.Encode("http://shop.test/cldx/pay?order="+o.SN, qrcode.Medium, 256)
	// html/template writes "+" in the data URI as "&#43;", so unescape what the page shows.
	m := regexp.MustCompile(`base64,([^"]+)"`).FindStringSubmatch(page.Body.String())
	if page.Code != 200 || m == nil || html.UnescapeString(m[1]) != base64.StdEncoding.EncodeToString(qr) {
		t.Fatalf("QR is not the order-only link: %d", page.Code)
	}
	direct := httptest.NewRecorder()
	app.cldxPay(direct, httptest.NewRequest("GET", "http://shop.test/pay/cldx/x/"+o.SN+"?clodex_app=1", nil), o)
	if !strings.Contains(direct.Body.String(), "clodex://pay?amount=15000&amp;exp=") || !strings.Contains(direct.Body.String(), "to=shop-test") {
		t.Fatalf("app link lost the server-built query: %s", direct.Body.String())
	}
	// Expired lock (status -1 past its deadline) is refused too.
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE orders SET status=-1, cldx_expires_at=$2 WHERE order_sn=$1`, o.SN, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if w := get("/cldx/pay?order=" + o.SN); w.Code != 404 {
		t.Fatalf("expired order redirected: %d", w.Code)
	}
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE orders SET status=-1, cldx_expires_at=$2 WHERE order_sn=$1`, o.SN, exp); err != nil {
		t.Fatal(err)
	}
	if w := get("/cldx/pay?order=" + o.SN); w.Code != 302 || w.Header().Get("Location") != want {
		t.Fatalf("expired-but-within-deadline order refused: %d", w.Code)
	}
}

// installed marks the throwaway schema as installed so the guard routes storefront requests.
func installed(t *testing.T, app *App) http.Handler {
	t.Helper()
	if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO admin_users(username,password,name) VALUES('audit','x','audit')`); err != nil {
		t.Fatal(err)
	}
	return app.Handler()
}

func deliver(t *testing.T, app *App, sn string) {
	t.Helper()
	o, err := app.live().OrderBySN(context.Background(), sn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.live().Complete(context.Background(), sn, o.Actual, "test:"+sn); err != nil {
		t.Fatal(err)
	}
	if o, _ = app.live().OrderBySN(context.Background(), sn); o.Info != "AUDIT-SECRET" {
		t.Fatalf("delivery failed: %+v", o)
	}
}

// #13: the query password travels in the POST body and never in a URL.
func TestSearchBySNPostsPassword(t *testing.T) {
	app, o := auditOrder(t)
	deliver(t, app, o.SN)
	if err := app.live().PutSetting(context.Background(), "is_open_search_pwd", "1"); err != nil {
		t.Fatal(err)
	}
	h := installed(t, app)
	post := func(form string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://shop.test/search-order-by-sn", strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	right := post("order_sn=" + o.SN + "&search_pwd=query-secret")
	if right.Code != 200 || !strings.Contains(right.Body.String(), "AUDIT-SECRET") {
		t.Fatalf("right password hid the card: %d %s", right.Code, right.Body.String())
	}
	if strings.Contains(right.Body.String(), "pwd=") || right.Header().Get("Location") != "" {
		t.Fatal("password leaked into a URL")
	}
	wrong := post("order_sn=" + o.SN + "&search_pwd=nope")
	if wrong.Code != 200 || strings.Contains(wrong.Body.String(), "AUDIT-SECRET") || !strings.Contains(wrong.Body.String(), o.SN) {
		t.Fatalf("wrong password: %d %s", wrong.Code, wrong.Body.String())
	}
	if w := post("order_sn=&search_pwd=x"); w.Code != 200 || !strings.Contains(w.Body.String(), "请输入订单号") {
		t.Fatalf("empty order number: %d", w.Code)
	}
	// GET keeps working but ignores ?pwd= entirely.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/detail-order-sn/"+o.SN+"?pwd=query-secret", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "AUDIT-SECRET") || !strings.Contains(w.Body.String(), o.SN) {
		t.Fatalf("GET honoured the query password: %d", w.Code)
	}
	// The browser that placed the order still sees it without a password.
	owner := httptest.NewRequest("GET", "http://shop.test/detail-order-sn/"+o.SN, nil)
	t.Setenv("DUFAKA_SESSION_KEY", "search-test-key")
	cookieRec := httptest.NewRecorder()
	remember(cookieRec, owner, o.SN)
	owner.AddCookie(cookieRec.Result().Cookies()[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, owner)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AUDIT-SECRET") {
		t.Fatalf("owner lost the card: %d", w.Code)
	}
}

// #12: with the query password off, an email alone lists orders but hands out neither the
// card nor the order-number capability: numbers are masked and nothing links to the
// order. The buyer's browser (cookie) still sees its cards; with the password on, the
// password decides and full numbers come back.
func TestSearchByEmailHidesCards(t *testing.T) {
	t.Setenv("DUFAKA_SESSION_KEY", "email-test-key")
	app, o := auditOrder(t)
	deliver(t, app, o.SN)
	// A second, still unpaid order for the same email would normally show a 继续支付 link.
	if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO carmis(goods_id,carmi) VALUES(1,'AUDIT-SECRET-2')`); err != nil {
		t.Fatal(err)
	}
	unpaid, err := app.live().CreateOrder(context.Background(), store.CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "audit@example.com", SearchPwd: "query-secret"}, store.Site{})
	if err != nil {
		t.Fatal(err)
	}
	h := installed(t, app)
	search := func(form string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://shop.test/search-order-by-email", strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	masked := "…" + o.SN[len(o.SN)-4:] // A3-9: only the last four characters
	if maskSN(o.SN) != masked || strings.Contains(maskSN(o.SN), o.SN[:4]) || maskSN("ABCD") != "****" {
		t.Fatalf("maskSN: %q", maskSN(o.SN))
	}
	for _, theme := range []string{"unicorn", "luna", "hyper"} {
		if err := app.live().PutSetting(context.Background(), "template", theme); err != nil {
			t.Fatal(err)
		}
		app.live().InvalidateSite()
		if got := app.live().Site(context.Background()).Template; got != theme {
			t.Fatalf("theme %q not applied: %q", theme, got)
		}
		w := search("email=audit%40example.com", nil)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, masked) || strings.Contains(body, "AUDIT-SECRET") {
			t.Fatalf("%s: email search: %d %s", theme, w.Code, body)
		}
		if strings.Contains(body, o.SN) || strings.Contains(body, unpaid.SN) {
			t.Fatalf("%s: email search disclosed a full order number: %s", theme, body)
		}
		if strings.Contains(body, "/bill/") || strings.Contains(body, "/detail-order-sn/") || strings.Contains(body, "继续支付") {
			t.Fatalf("%s: email search linked to an order: %s", theme, body)
		}
	}
	if err := app.live().PutSetting(context.Background(), "template", "unicorn"); err != nil {
		t.Fatal(err)
	}
	app.live().InvalidateSite()
	// The buyer's browser keeps its cards in the listing (numbers stay masked there; the
	// browser search itself shows them in full).
	rec := httptest.NewRecorder()
	remember(rec, httptest.NewRequest("GET", "http://shop.test/", nil), o.SN)
	cookie := rec.Result().Cookies()[0]
	if w := search("email=audit%40example.com", cookie); w.Code != 200 || !strings.Contains(w.Body.String(), "AUDIT-SECRET") || strings.Contains(w.Body.String(), o.SN) {
		t.Fatalf("buyer's browser lost the card or leaked the number: %d %s", w.Code, w.Body.String())
	}
	browse := httptest.NewRequest("POST", "http://shop.test/search-order-by-browser", nil)
	browse.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, browse)
	if w.Code != 200 || !strings.Contains(w.Body.String(), o.SN) || !strings.Contains(w.Body.String(), "AUDIT-SECRET") {
		t.Fatalf("browser search lost the full number or the card: %d", w.Code)
	}
	// Whoever holds the full order number still opens the detail page: the random number
	// is the capability.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/detail-order-sn/"+o.SN, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AUDIT-SECRET") || !strings.Contains(w.Body.String(), o.SN) {
		t.Fatalf("detail by order number broke: %d", w.Code)
	}
	// With the query password on, the password decides and the listing is unmasked.
	if err := app.live().PutSetting(context.Background(), "is_open_search_pwd", "1"); err != nil {
		t.Fatal(err)
	}
	w = search("email=audit%40example.com&search_pwd=query-secret", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AUDIT-SECRET") || !strings.Contains(w.Body.String(), o.SN) || !strings.Contains(w.Body.String(), "/bill/"+unpaid.SN) {
		t.Fatalf("password search hid the card, the number or the pay link: %d %s", w.Code, w.Body.String())
	}
	if w := search("email=audit%40example.com&search_pwd=nope", nil); strings.Contains(w.Body.String(), "AUDIT-SECRET") {
		t.Fatal("wrong password disclosed the card")
	}
}

// #18 + #23 + #16: the cashier poll asks the wallet at most once per window, the wallet's
// payer address joins the trade number, and failures are logged with the order number.
func TestCldxPollThrottleAndTradeID(t *testing.T) {
	app, o := auditOrder(t)
	var hits atomic.Int32
	var paid atomic.Bool
	var fail atomic.Bool
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payments" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "down", 500)
			return
		}
		if r.URL.Query().Get("order") != o.SN || r.URL.Query().Get("to") != "shop-test" {
			t.Errorf("wallet asked for %s", r.URL.RawQuery)
		}
		fmt.Fprintf(w, `{"to":"shop-test","order":%q,"from":"cldx1payer","amount":15000,"refunded":0,"paid":%t,"at":"2026-09-25T08:00:00Z"}`, o.SN, paid.Load())
	}))
	defer wallet.Close()
	app.wallet.Base, app.wallet.Secret, app.wallet.MerchantID = wallet.URL, "test-only", "shop-test"
	clock := time.Now()
	app.receipts.now = func() time.Time { return clock }
	if _, _, err := app.live().LockCldxQuote(context.Background(), o.SN, 15000, time.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	poll := func() string {
		req := httptest.NewRequest("GET", "/check-order-status/"+o.SN, nil)
		req.SetPathValue("sn", o.SN)
		w := httptest.NewRecorder()
		app.poll(w, req)
		return w.Body.String()
	}
	for i := 0; i < 3; i++ {
		if body := poll(); !strings.Contains(body, `"code":400000`) {
			t.Fatalf("poll %d: %s", i, body)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("wallet asked %d times inside the window", hits.Load())
	}
	clock = clock.Add(receiptWindow)
	poll()
	if hits.Load() != 2 {
		t.Fatalf("wallet asked %d times after the window", hits.Load())
	}
	// Wallet failure inside a fresh window is logged with the order number, without secrets.
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	fail.Store(true)
	clock = clock.Add(receiptWindow)
	poll()
	if !strings.Contains(logs.String(), o.SN) || !strings.Contains(logs.String(), "查询钱包到账失败") || strings.Contains(logs.String(), "test-only") {
		t.Fatalf("wallet failure not logged properly: %q", logs.String())
	}
	// The failed answer is cached too, then the next window sees the payment with its wallet id.
	fail.Store(false)
	paid.Store(true)
	before := hits.Load()
	poll()
	if hits.Load() != before {
		t.Fatal("failure was not throttled")
	}
	clock = clock.Add(receiptWindow)
	if body := poll(); !strings.Contains(body, `"code":200`) {
		t.Fatalf("payment not picked up: %s", body)
	}
	done, _ := app.live().OrderBySN(context.Background(), o.SN)
	if done.Status != 4 || done.TradeNo != "cldx:"+o.SN+":cldx1payer" || done.Info != "AUDIT-SECRET" {
		t.Fatalf("delivered order: %+v", done)
	}
	// A cancelled tick is silent; a real wallet failure during SyncCldx names the order.
	logs.Reset()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	app.SyncCldx(cancelled)
	if logs.Len() != 0 {
		t.Fatalf("cancelled sync logged: %q", logs.String())
	}
}

// #16: the background pass logs wallet failures per order and is bounded by a deadline.
func TestSyncCldxLogsAndTimeout(t *testing.T) {
	app, o := auditOrder(t)
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", 502)
	}))
	defer wallet.Close()
	app.wallet.Base, app.wallet.Secret, app.wallet.MerchantID = wallet.URL, "test-only", "shop-test"
	if _, _, err := app.live().LockCldxQuote(context.Background(), o.SN, 15000, time.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	app.SyncCldx(context.Background())
	if !strings.Contains(logs.String(), "cldx 订单 "+o.SN) || !strings.Contains(logs.String(), "钱包返回 502") {
		t.Fatalf("sync failure not logged: %q", logs.String())
	}
	if o2, _ := app.live().OrderBySN(context.Background(), o.SN); o2.Status != 1 {
		t.Fatalf("order changed on wallet failure: %+v", o2)
	}
	// The tick derives a bounded context: a parent that is already past its deadline ends the pass quietly.
	logs.Reset()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	app.SyncCldx(expired)
	if logs.Len() != 0 {
		t.Fatalf("expired tick logged noise: %q", logs.String())
	}
}
