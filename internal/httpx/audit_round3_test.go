package httpx

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dufaka/internal/store"
)

var wechatEnv = []string{"WECHAT_PAY_APP_ID", "WECHAT_PAY_MCH_ID", "WECHAT_PAY_CERT_SERIAL_NO", "WECHAT_PAY_API_V3_KEY", "WECHAT_PAY_PRIVATE_KEY", "WECHAT_PAY_PLATFORM_KEY", "WECHAT_PAY_PLATFORM_SERIAL", "WECHAT_PAY_PUBLIC_KEY_ID"}

func clearWechatEnv(t *testing.T) {
	t.Helper()
	for _, k := range wechatEnv {
		t.Setenv(k, "")
	}
}

// fakeWechat answers every request to the WeChat Pay API host through
// http.DefaultTransport (pay.Native uses a client without its own transport) and counts
// them; other hosts go to the real transport.
type fakeWechat struct {
	calls  atomic.Int32
	status int
	body   string
	next   http.RoundTripper
}

func (f *fakeWechat) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.mch.weixin.qq.com" {
		return f.next.RoundTrip(r)
	}
	f.calls.Add(1)
	return &http.Response{
		StatusCode: f.status, Status: fmt.Sprintf("%d", f.status), Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(f.body)), Request: r,
	}, nil
}

func installFakeWechat(t *testing.T, status int, body string) *fakeWechat {
	t.Helper()
	f := &fakeWechat{status: status, body: body, next: http.DefaultTransport}
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = f.next })
	return f
}

type wechatKeys struct {
	key     *rsa.PrivateKey
	privPEM string
	pubPEM  string
}

func newWechatKeys(t *testing.T) wechatKeys {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return wechatKeys{
		key:     key,
		privPEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		pubPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})),
	}
}

const apiV3 = "01234567890123456789012345678901"

// wescanOrder adds wescan row 2 (the given merchant id / APIv3 key / private key) and an
// unpaid order on a manual product paying with it.
func wescanOrder(t *testing.T, app *App, mch, v3, priv string) store.Order {
	t.Helper()
	ctx := context.Background()
	if _, err := app.live().Pool.Exec(ctx, `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_id,merchant_key,merchant_pem,pay_handleroute) VALUES(2,'微信扫码','wescan',1,3,$1,$2,$3,'/pay/wepay')`, mch, v3, priv); err != nil {
		t.Fatal(err)
	}
	if _, err := app.live().Pool.Exec(ctx, `INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(3,1,'manual','','',10,5,2)`); err != nil {
		t.Fatal(err)
	}
	o, err := app.live().CreateOrder(ctx, store.CreateInput{GID: 3, PayID: 2, Amount: 1, Email: "wx@example.com"}, store.Site{})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// A3-1 (#5): the cashier runs the full Validate before calling WeChat Pay: an order half
// that is complete is not enough when the callback could not be verified or decrypted.
// The customer sees one generic sentence, the owner's log names the channel and the gap.
func TestWechatGatewayValidatesBeforeCallingWechat(t *testing.T) {
	keys := newWechatKeys(t)
	cases := []struct {
		name     string
		platform string
		v3       string
		priv     string
		logWant  string
	}{
		{"platform key empty", "", apiV3, keys.privPEM, "平台公钥（WECHAT_PAY_PLATFORM_KEY）"},
		{"31-byte APIv3 key", keys.pubPEM, apiV3[:31], keys.privPEM, "32 字节"},
		{"unparsable private key", keys.pubPEM, apiV3, "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----", "商户私钥无法解析"},
		{"unparsable platform key", "not a pem", apiV3, keys.privPEM, "平台公钥无法解析"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearWechatEnv(t)
			t.Setenv("WECHAT_PAY_APP_ID", "wx-app")
			t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "CERT01")
			t.Setenv("WECHAT_PAY_PLATFORM_KEY", c.platform)
			fake := installFakeWechat(t, 200, `{"code_url":"weixin://wxpay/bizpayurl?pr=x"}`)
			app, _ := auditOrder(t)
			o := wescanOrder(t, app, "1900000000", c.v3, c.priv)
			h := installed(t, app)
			logs := captureLog(t)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/pay/wescan/scan/"+o.SN, nil))
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, "微信支付暂未配置完整，请联系店主") {
				t.Fatalf("gateway: %d %s", w.Code, body)
			}
			if strings.Contains(body, "WECHAT_PAY") || strings.Contains(body, "无法解析") || strings.Contains(body, "32 字节") {
				t.Fatalf("configuration details shown to the customer")
			}
			if fake.calls.Load() != 0 {
				t.Fatalf("WeChat Pay called %d times with an unusable config", fake.calls.Load())
			}
			if !strings.Contains(logs.String(), "渠道 2") || !strings.Contains(logs.String(), c.logWant) {
				t.Fatalf("log lacks the channel or the reason: %q", logs.String())
			}
		})
	}
}

// A3-1 + A3-2: a config that validates reaches WeChat Pay — here with 商户号 and 商户私钥
// supplied only by the deprecated environment variables, as a shop configured before the
// admin form may be. A WeChat Pay error is logged with the channel and never shown.
func TestWechatGatewayCallsWechatWhenReady(t *testing.T) {
	keys := newWechatKeys(t)
	clearWechatEnv(t)
	t.Setenv("WECHAT_PAY_APP_ID", "wx-app")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "CERT01")
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", keys.pubPEM)
	t.Setenv("WECHAT_PAY_MCH_ID", "1900000009")
	t.Setenv("WECHAT_PAY_PRIVATE_KEY", keys.privPEM)
	fake := installFakeWechat(t, 200, `{"code_url":"weixin://wxpay/bizpayurl?pr=ok"}`)
	app, _ := auditOrder(t)
	o := wescanOrder(t, app, "", apiV3, "")
	h := installed(t, app)
	logs := captureLog(t)
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/pay/wescan/scan/"+o.SN, nil))
		return w
	}
	if w := get(); w.Code != 200 || strings.Contains(w.Body.String(), "暂未配置完整") || fake.calls.Load() != 1 {
		t.Fatalf("ready config: %d calls=%d %q", w.Code, fake.calls.Load(), logs.String())
	}
	fake.status, fake.body = 400, `{"code":"PARAM_ERROR","message":"商户号 1900000009 与 appid 不匹配 INTERNAL-DETAIL"}`
	logs.Reset()
	w := get()
	body := w.Body.String()
	if !strings.Contains(body, "微信支付下单失败，请稍后再试或联系店主") || strings.Contains(body, "INTERNAL-DETAIL") || strings.Contains(body, "1900000009") {
		t.Fatalf("WeChat error surfaced to the customer: %s", body)
	}
	if fake.calls.Load() != 2 || !strings.Contains(logs.String(), "渠道 2") || !strings.Contains(logs.String(), "INTERNAL-DETAIL") || !strings.Contains(logs.String(), o.SN) {
		t.Fatalf("WeChat error not logged: calls=%d %q", fake.calls.Load(), logs.String())
	}
}

// signedNotify builds a WeChat Pay callback for plain, encrypted with v3 and signed with key.
func signedNotify(t *testing.T, key *rsa.PrivateKey, v3, plain string) *http.Request {
	t.Helper()
	block, _ := aes.NewCipher([]byte(v3))
	gcm, _ := cipher.NewGCM(block)
	nonce, aad := "123456789012", "transaction"
	sealed := gcm.Seal(nil, []byte(nonce), []byte(plain), []byte(aad))
	raw, _ := json.Marshal(map[string]any{"resource": map[string]string{"nonce": nonce, "associated_data": aad, "ciphertext": base64.StdEncoding.EncodeToString(sealed)}})
	ts := fmt.Sprint(time.Now().Unix())
	sum := sha256.Sum256([]byte(ts + "\n" + "round3" + "\n" + string(raw) + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/pay/wepay/notify_url", strings.NewReader(string(raw)))
	r.Header.Set("Wechatpay-Timestamp", ts)
	r.Header.Set("Wechatpay-Nonce", "round3")
	r.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
	return r
}

// A3-4: a verified callback the configured APIv3 key cannot open answers 500 so WeChat
// retries once the owner fixes the key; a payment that lands on sold-out stock is
// recorded (status 6) and acknowledged with SUCCESS instead of being retried forever.
func TestWechatNotifyDecryptFailureAndPaidShort(t *testing.T) {
	keys := newWechatKeys(t)
	clearWechatEnv(t)
	t.Setenv("WECHAT_PAY_APP_ID", "wx-app")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "CERT01")
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", keys.pubPEM)
	app, o := auditOrder(t)
	if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_id,merchant_key,merchant_pem,pay_handleroute) VALUES(2,'微信扫码','wescan',1,3,'1900000000',$1,$2,'/pay/wepay')`, apiV3, keys.privPEM); err != nil {
		t.Fatal(err)
	}
	plain := fmt.Sprintf(`{"out_trade_no":%q,"trade_state":"SUCCESS","transaction_id":"wx-short","amount":{"total":1000}}`, o.SN)
	logs := captureLog(t)
	// Encrypted under another key: the signature holds, decryption fails → 500 + log.
	w := httptest.NewRecorder()
	app.wechatNotify(w, signedNotify(t, keys.key, strings.Repeat("x", 32), plain))
	if w.Code != 500 || !strings.Contains(logs.String(), "解密失败") || !strings.Contains(logs.String(), "渠道 2") {
		t.Fatalf("undecryptable callback: %d %q", w.Code, logs.String())
	}
	if strings.Contains(logs.String(), apiV3) {
		t.Fatal("APIv3 key leaked into the log")
	}
	// The card was sold elsewhere meanwhile: the payment is recorded as 异常 and WeChat is
	// told SUCCESS.
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE carmis SET deleted_at=now()`); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	w = httptest.NewRecorder()
	app.wechatNotify(w, signedNotify(t, keys.key, apiV3, plain))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"SUCCESS"`) {
		t.Fatalf("paid-short callback: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(logs.String(), "库存不足") || !strings.Contains(logs.String(), o.SN) {
		t.Fatalf("paid-short not logged for the owner: %q", logs.String())
	}
	done, _ := app.live().OrderBySN(context.Background(), o.SN)
	if done.Status != 6 || done.TradeNo != "wx-short" {
		t.Fatalf("payment not recorded: %+v", done)
	}
}

// A3-5 (#16): a database failure while reading the order keeps the cashier polling
// (400000) and is logged with the order number while the caller is still there; only an
// unknown order answers 400001.
func TestPollLookupFailure(t *testing.T) {
	logs := captureLog(t)
	got := pollLookupFailed(context.Background(), "ABCDEF0123456789", errors.New("connection refused"))
	if got["code"] != 400000 || !strings.Contains(logs.String(), "ABCDEF0123456789") || !strings.Contains(logs.String(), "connection refused") {
		t.Fatalf("db error: %v %q", got, logs.String())
	}
	logs.Reset()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if got := pollLookupFailed(gone, "ABCDEF0123456789", errors.New("canceled")); got["code"] != 400000 || logs.Len() != 0 {
		t.Fatalf("gone caller: %v %q", got, logs.String())
	}
	if got := pollLookupFailed(context.Background(), "X", store.RuleError{Msg: "订单不存在"}); got["code"] != 400001 || logs.Len() != 0 {
		t.Fatalf("not found: %v %q", got, logs.String())
	}
	long := strings.Repeat("S", 500)
	pollLookupFailed(context.Background(), long, errors.New("x"))
	if strings.Contains(logs.String(), long) {
		t.Fatal("request-supplied order number not clipped in the log")
	}
	// Through the handler, an unknown order still ends the polling.
	app, _ := auditOrder(t)
	h := installed(t, app)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/check-order-status/FFFFFFFFFFFFFFFF", nil))
	if !strings.Contains(w.Body.String(), `"code":400001`) {
		t.Fatalf("unknown order: %s", w.Body.String())
	}
}

// A3-5 (#16): when the tick deadline cuts the read of the waiting orders, the pass says so.
func TestSyncCldxLogsWaitingSNsTimeout(t *testing.T) {
	app, o := auditOrder(t)
	app.wallet.Base, app.wallet.Secret, app.wallet.MerchantID = "http://127.0.0.1:1", "test-only", "shop-test"
	if _, _, err := app.live().LockCldxQuote(context.Background(), o.SN, 15000, time.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	old := syncCldxTimeout
	syncCldxTimeout = 300 * time.Millisecond
	t.Cleanup(func() { syncCldxTimeout = old })
	ctx := context.Background()
	tx, err := app.live().Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	app.SyncCldx(ctx)
	if !strings.Contains(logs.String(), "cldx 同步：读取待支付订单时超时") {
		t.Fatalf("WaitingSNs timeout not logged: %q", logs.String())
	}
}

// A3-9: the per-address limiter: a fixed budget per window, a fresh budget after it, IPv6
// addresses of one /64 share a budget. A full table never evicts a live counter, and it
// never locks new addresses out either (repair pass: a flood of /64s filling the table
// must not 429 every new buyer): the lookup limiter routes them into one shared overflow
// budget, the poll limiter lets them through untracked. Each overflow period is logged.
func TestIPLimiter(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	l := newIPLimiter("test", 3, 0, time.Minute)
	l.now = clock.Now
	for i := 0; i < 3; i++ {
		if !l.allow("a") {
			t.Fatalf("request %d refused", i)
		}
	}
	if l.allow("a") {
		t.Fatal("over-budget request allowed")
	}
	if !l.allow("b") {
		t.Fatal("another address throttled")
	}
	clock.Advance(time.Minute)
	if !l.allow("a") {
		t.Fatal("budget not renewed after the window")
	}
	logs := captureLog(t)
	// Full table, poll style (no overflow budget): new addresses pass untracked, live
	// counters stay.
	open := newIPLimiter("轮询", 1, 0, time.Minute)
	open.now = clock.Now
	open.max = 2
	open.allow("x")
	open.allow("y")
	for i := 0; i < 5; i++ {
		if !open.allow("z") {
			t.Fatal("new address refused while the poll table is full")
		}
	}
	if open.allow("x") {
		t.Fatal("live counter was reset")
	}
	if len(open.hits) != 2 || strings.Count(logs.String(), "地址表已满") != 1 {
		t.Fatalf("overflow tracked or logged per request: %d %q", len(open.hits), logs.String())
	}
	// Full table, lookup style: new addresses share one overflow budget.
	spill := newIPLimiter("查询", 1, 2, time.Minute)
	spill.now = clock.Now
	spill.max = 2
	spill.allow("x")
	spill.allow("y")
	if !spill.allow("n1") || !spill.allow("n2") || spill.allow("n3") {
		t.Fatal("overflow budget not shared or not bounded")
	}
	if spill.allow("x") {
		t.Fatal("live counter was reset")
	}
	clock.Advance(time.Minute + time.Second)
	if !spill.allow("z") || len(spill.hits) != 1 {
		t.Fatalf("expired counters not swept: %d", len(spill.hits))
	}
	// A new overflow period, one window later, is logged again.
	logs.Reset()
	open.allow("x")
	open.allow("y")
	open.allow("q")
	if !strings.Contains(logs.String(), "地址表已满") {
		t.Fatalf("second overflow period not logged: %q", logs.String())
	}
	// Keys come from the shared rule: one IPv6 /64 is one budget.
	req := func(remote string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		return r
	}
	if limiterKey(req("[2001:db8:1:2::1]:5")) != limiterKey(req("[2001:db8:1:2:ffff::9]:5")) || limiterKey(req("198.51.100.7:1")) != "198.51.100.7" {
		t.Fatal("limiter keys")
	}
}

// A3-9 (#12): order lookups get 30 requests per 5 minutes per address, then a 429 page;
// the cashier poll has its own budget of 120, enough for a page polling every 5 seconds.
func TestLookupAndPollLimits(t *testing.T) {
	app, o := auditOrder(t)
	h := installed(t, app)
	do := func(method, path, form, remote string) *httptest.ResponseRecorder {
		var body io.Reader
		if form != "" {
			body = strings.NewReader(form)
		}
		r := httptest.NewRequest(method, "http://shop.test"+path, body)
		if form != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// The three lookup routes share one budget.
	for i := 0; i < lookupLimit; i++ {
		var w *httptest.ResponseRecorder
		switch i % 3 {
		case 0:
			w = do("GET", "/detail-order-sn/"+o.SN, "", "198.51.100.1:1")
		case 1:
			w = do("POST", "/search-order-by-sn", "order_sn="+o.SN, "198.51.100.1:1")
		default:
			w = do("POST", "/search-order-by-email", "email=nobody%40example.com", "198.51.100.1:1")
		}
		if w.Code != 200 {
			t.Fatalf("lookup %d: %d", i, w.Code)
		}
	}
	for _, try := range []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return do("GET", "/detail-order-sn/"+o.SN, "", "198.51.100.1:2") },
		func() *httptest.ResponseRecorder {
			return do("POST", "/search-order-by-email", "email=audit%40example.com", "198.51.100.1:3")
		},
	} {
		w := try()
		if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "查询过于频繁，请稍后再试") || strings.Contains(w.Body.String(), o.SN) {
			t.Fatalf("over budget: %d", w.Code)
		}
	}
	// Another address is unaffected, and so is the poll budget of the throttled one.
	if w := do("GET", "/detail-order-sn/"+o.SN, "", "198.51.100.2:1"); w.Code != 200 {
		t.Fatalf("other address throttled: %d", w.Code)
	}
	// A cashier polling every 5 s for the whole 5-minute window (60 polls, twice over for
	// a second tab) is never throttled.
	for i := 0; i < pollLimit; i++ {
		w := do("GET", "/check-order-status/"+o.SN, "", "198.51.100.1:4")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":400000`) {
			t.Fatalf("poll %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	w := do("GET", "/check-order-status/"+o.SN, "", "198.51.100.1:4")
	var res map[string]any
	if w.Code != http.StatusTooManyRequests || json.Unmarshal(w.Body.Bytes(), &res) != nil || res["code"] != float64(429) {
		t.Fatalf("poll over budget: %d %s", w.Code, w.Body.String())
	}
}

// A3-10 (#19/#5): the product page offers cldx only with a configured wallet and wescan
// only with a config that passes Validate (hidden channels are logged once); checkout
// refuses a channel that is not ready before any card is reserved.
func TestStorefrontOffersOnlyReadyChannels(t *testing.T) {
	keys := newWechatKeys(t)
	clearWechatEnv(t)
	t.Setenv("WECHAT_PAY_APP_ID", "wx-app")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "CERT01")
	app, _ := auditOrder(t)
	ctx := context.Background()
	if _, err := app.live().Pool.Exec(ctx, `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_id,merchant_key,merchant_pem,pay_handleroute) VALUES(2,'微信扫码','wescan',1,3,'1900000000',$1,$2,'/pay/wepay')`, apiV3, keys.privPEM); err != nil {
		t.Fatal(err)
	}
	if _, err := app.live().Pool.Exec(ctx, `INSERT INTO carmis(goods_id,carmi) VALUES(1,'SPARE-1')`); err != nil {
		t.Fatal(err)
	}
	h := installed(t, app)
	logs := captureLog(t)
	page := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/buy/1", nil))
		return w.Body.String()
	}
	offers := func(body string, id int) bool {
		return strings.Contains(body, fmt.Sprintf(`name="payway" value="%d"`, id))
	}
	// Neither ready: wallet secret unset, platform key missing.
	body := page()
	if offers(body, 1) || offers(body, 2) || !strings.Contains(body, "暂无可用支付方式") {
		t.Fatal("unready channels offered")
	}
	page()
	if strings.Count(logs.String(), "支付渠道 1（cldx）未就绪") != 1 || strings.Count(logs.String(), "支付渠道 2（wescan）未就绪") != 1 || !strings.Contains(logs.String(), "平台公钥") {
		t.Fatalf("hidden channels not logged exactly once: %q", logs.String())
	}
	reserved := func() int {
		var n int
		if err := app.live().Pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE reserved_order_id IS NOT NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	orders := func() int {
		var n int
		if err := app.live().Pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	beforeReserved, beforeOrders := reserved(), orders()
	checkout := func(payway int) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://shop.test/create-order", strings.NewReader(fmt.Sprintf("gid=1&payway=%d&by_amount=1&email=ready%%40example.com", payway)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, payway := range []int{1, 2} {
		if w := checkout(payway); w.Code != 200 || !strings.Contains(w.Body.String(), "支付方式不可用") {
			t.Fatalf("payway %d: %d", payway, w.Code)
		}
	}
	if reserved() != beforeReserved || orders() != beforeOrders {
		t.Fatal("stock reserved or order created for an unready channel")
	}
	// Both ready: both offered, checkout goes through.
	app.wallet.Secret, app.wallet.MerchantID = "test-only", "shop-test"
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", keys.pubPEM)
	body = page()
	if !offers(body, 1) || !offers(body, 2) {
		t.Fatal("ready channels hidden")
	}
	if w := checkout(2); w.Code != http.StatusFound {
		t.Fatalf("ready checkout: %d %s", w.Code, w.Body.String())
	}
}

// A3-5 repair: a real database failure behind the handler (the orders table is gone for a
// moment) keeps the cashier polling with 400000 and logs the order number; the payment
// pages show a generic sentence, never SQL or driver text, instead of 订单不存在.
func TestPollHandlerKeepsPollingOnDatabaseError(t *testing.T) {
	app, o := auditOrder(t)
	h := installed(t, app)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test"+path, nil))
		return w
	}
	if w := get("/check-order-status/" + o.SN); !strings.Contains(w.Body.String(), `"code":400000`) {
		t.Fatalf("healthy poll: %s", w.Body.String())
	}
	if _, err := app.live().Pool.Exec(context.Background(), `ALTER TABLE orders RENAME TO orders_gone`); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	w := get("/check-order-status/" + o.SN)
	if !strings.Contains(w.Body.String(), `"code":400000`) {
		t.Fatalf("database error ended the polling: %s", w.Body.String())
	}
	if !strings.Contains(logs.String(), o.SN) {
		t.Fatalf("database error not logged with the order number: %q", logs.String())
	}
	for _, path := range []string{"/bill/" + o.SN, "/pay/cldx/1/" + o.SN, "/detail-order-sn/" + o.SN} {
		body := get(path).Body.String()
		if !strings.Contains(body, genericFailure) || strings.Contains(body, "订单不存在") || strings.Contains(body, "orders") || strings.Contains(body, "SQLSTATE") {
			t.Fatalf("%s: %s", path, body)
		}
	}
	// The pool going away entirely is the same story.
	if _, err := app.live().Pool.Exec(context.Background(), `ALTER TABLE orders_gone RENAME TO orders`); err != nil {
		t.Fatal(err)
	}
	app.live().Pool.Close()
	if w := get("/check-order-status/" + o.SN); !strings.Contains(w.Body.String(), `"code":400000`) {
		t.Fatalf("closed pool ended the polling: %s", w.Body.String())
	}
}

// A3-9 repair: a loopback proxy that forwards no client address would put every buyer on
// 127.0.0.1 and one shared budget, so limiting is switched off (and logged) instead.
func TestLookupLimitSkipsUnforwardedProxy(t *testing.T) {
	app, o := auditOrder(t)
	h := installed(t, app)
	logs := captureLog(t)
	unforwardedOnce = sync.Once{}
	for i := 0; i < lookupLimit+10; i++ {
		r := httptest.NewRequest("GET", "http://shop.test/detail-order-sn/"+o.SN, nil)
		r.RemoteAddr = "127.0.0.1:40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("buyer %d behind a header-less proxy throttled: %d", i, w.Code)
		}
	}
	if strings.Count(logs.String(), "反代未传 X-Real-IP，查询限流已停用") != 1 {
		t.Fatalf("missing or repeated warning: %q", logs.String())
	}
	// With the header the proxy's buyers are told apart and limited again.
	for i := 0; i <= lookupLimit; i++ {
		r := httptest.NewRequest("GET", "http://shop.test/detail-order-sn/"+o.SN, nil)
		r.RemoteAddr = "127.0.0.1:40000"
		r.Header.Set("X-Real-IP", "198.51.100.77")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if (i < lookupLimit) != (w.Code == 200) {
			t.Fatalf("forwarded lookup %d: %d", i, w.Code)
		}
	}
}

// A3-9 repair: the buyer's own order (signed browser cookie) is never throttled, neither
// the cashier's poll nor the delivery page it opens after payment, even when the address
// has spent its lookup budget.
func TestOwnedOrderSkipsLookupLimit(t *testing.T) {
	t.Setenv("DUFAKA_SESSION_KEY", "test-session-key-0123456789abcdef")
	app, o := auditOrder(t)
	h := installed(t, app)
	raw, _ := json.Marshal([]string{o.SN})
	own := &http.Cookie{Name: "dujiaoka_orders", Value: orderCookie(base64.RawURLEncoding.EncodeToString(raw))}
	do := func(path string, cookie bool) int {
		r := httptest.NewRequest("GET", "http://shop.test"+path, nil)
		r.RemoteAddr = "198.51.100.9:1"
		if cookie {
			r.AddCookie(own)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < lookupLimit; i++ {
		do("/detail-order-sn/FFFFFFFFFFFFFFFF", false)
	}
	if c := do("/detail-order-sn/"+o.SN, false); c != http.StatusTooManyRequests {
		t.Fatalf("budget not spent: %d", c)
	}
	if c := do("/detail-order-sn/"+o.SN, true); c != 200 {
		t.Fatalf("buyer's own delivery page throttled: %d", c)
	}
	for i := 0; i < pollLimit+5; i++ {
		if c := do("/check-order-status/"+o.SN, true); c != 200 {
			t.Fatalf("buyer's own poll %d throttled: %d", i, c)
		}
	}
	// A forged or foreign cookie does not buy an exemption.
	r := httptest.NewRequest("GET", "http://shop.test/detail-order-sn/"+o.SN, nil)
	r.RemoteAddr = "198.51.100.9:1"
	r.AddCookie(&http.Cookie{Name: "dujiaoka_orders", Value: base64.RawURLEncoding.EncodeToString(raw) + ".forged"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("forged cookie exempted: %d", w.Code)
	}
}

// Repair pass 2: an order number carrying NUL + newline in the URL or form used to reach
// PostgreSQL (SQLSTATE 22021), count as a database failure and be logged unquoted, so
// anyone could write a forged line into the owner's log. Now it is 订单不存在 and silent.
func TestCraftedOrderNumberCannotForgeLogLines(t *testing.T) {
	app, _ := auditOrder(t)
	h := installed(t, app)
	logs := captureLog(t)
	const forged = "2026/09/25 12:00:00 FORGED"
	for _, path := range []string{
		"/bill/" + url.PathEscape("a\x00\n"+forged),
		"/detail-order-sn/" + url.PathEscape("\x00\n"+forged),
		"/pay/cldx/1/b%00%0AFORGED",
		"/pay-gateway/cldx/1/%FF%0AFORGED",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test"+path, nil))
		if body := w.Body.String(); !strings.Contains(body, "订单不存在") || strings.Contains(body, genericFailure) {
			t.Fatalf("%s: %d %s", path, w.Code, body)
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://shop.test/search-order-by-sn", strings.NewReader("order_sn=b%00%0AFORGED-2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "订单不存在") {
		t.Fatalf("form lookup: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/check-order-status/a%00%0AFORGED", nil))
	if !strings.Contains(w.Body.String(), `"code":400001`) {
		t.Fatalf("poll: %s", w.Body.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("crafted order numbers were logged: %q", logs.String())
	}
}

// Whatever reaches failErr, one call writes exactly one log line: the order number is
// Go-quoted and any leftover control character in the text or the error is escaped.
func TestFailErrWritesOneLogLine(t *testing.T) {
	app, _ := auditOrder(t)
	logs := captureLog(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://shop.test/", nil)
	app.failErr(w, r, orderWhat("x\x00\nFORGED-A"), errors.New("boom\nFORGED-B"))
	app.failErr(w, r, "raw\nFORGED-C", errors.New("\xff"))
	lines := strings.Split(strings.TrimRight(logs.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d: %q", len(lines), logs.String())
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "FORGED") || strings.Contains(l, "\x00") {
			t.Fatalf("forged line: %q", l)
		}
	}
	if !strings.Contains(lines[0], `"x\x00\nFORGED-A"`) || !strings.Contains(lines[0], `boom\nFORGED-B`) {
		t.Fatalf("detail lost: %q", lines[0])
	}
	if !strings.Contains(lines[1], `raw\nFORGED-C`) || !strings.Contains(lines[1], `\xff`) {
		t.Fatalf("detail lost: %q", lines[1])
	}
}

// Round-3 closeout (A3-2 blocker): the live build decrypted callbacks only with
// WECHAT_PAY_API_V3_KEY and ignored the admin 商户 KEY, so a live row may carry a stale
// 32-byte value. A verified callback must still be decrypted with the env key, the order
// delivered, and the stale admin value reported once.
func TestWechatNotifyFallsBackToEnvAPIv3Key(t *testing.T) {
	staleKeyOnce = sync.Once{}
	t.Cleanup(func() { staleKeyOnce = sync.Once{} })
	keys := newWechatKeys(t)
	clearWechatEnv(t)
	t.Setenv("WECHAT_PAY_APP_ID", "wx-app")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "CERT01")
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", keys.pubPEM)
	t.Setenv("WECHAT_PAY_API_V3_KEY", apiV3)
	app, o := auditOrder(t)
	stale := strings.Repeat("v", 32)
	if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_id,merchant_key,merchant_pem,pay_handleroute) VALUES(2,'微信扫码','wescan',1,3,'1900000000',$1,$2,'/pay/wepay')`, stale, keys.privPEM); err != nil {
		t.Fatal(err)
	}
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE orders SET pay_id=2 WHERE order_sn=$1`, o.SN); err != nil {
		t.Fatal(err)
	}
	plain := fmt.Sprintf(`{"out_trade_no":%q,"trade_state":"SUCCESS","transaction_id":"wx-env","amount":{"total":1000}}`, o.SN)
	logs := captureLog(t)
	w := httptest.NewRecorder()
	app.wechatNotify(w, signedNotify(t, keys.key, apiV3, plain))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"SUCCESS"`) {
		t.Fatalf("env-key callback: %d %s log=%q", w.Code, w.Body.String(), logs.String())
	}
	done, _ := app.live().OrderBySN(context.Background(), o.SN)
	if done.Status != 4 || done.TradeNo != "wx-env" {
		t.Fatalf("order not delivered via env key: %+v", done)
	}
	if !strings.Contains(logs.String(), "WECHAT_PAY_API_V3_KEY") || strings.Contains(logs.String(), apiV3) || strings.Contains(logs.String(), stale) {
		t.Fatalf("stale-key hint missing or key leaked: %q", logs.String())
	}
	cfg := wechatConfig(store.Pay{MerchantID: "1900000000", MerchantKey: stale, MerchantPem: keys.privPEM})
	if len(cfg.Warnings()) != 1 {
		t.Fatalf("differing keys must warn: %v", cfg.Warnings())
	}
}

// A live wescan row may carry a leftover merchant_key of the wrong length (dujiaoka kept
// the 10-digit mch_id there and the pre-audit build never read it). With a valid env
// key the channel must stay ready (listed, QR issued) and callbacks must decrypt.
func TestWechatMalformedRowKeyUsesEnvKey(t *testing.T) {
	keys := newWechatKeys(t)
	clearWechatEnv(t)
	t.Setenv("WECHAT_PAY_APP_ID", "wx-app")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "CERT01")
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", keys.pubPEM)
	t.Setenv("WECHAT_PAY_API_V3_KEY", apiV3)
	row := store.Pay{ID: 2, Check: "wescan", Open: 1, MerchantID: "1900000000", MerchantKey: "1900000000", MerchantPem: keys.privPEM}
	cfg := wechatConfig(row)
	if v := cfg.Validate(); len(v) != 0 {
		t.Fatalf("malformed row key with a valid env key must be ready, got %v", v)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "不是 32 字节") {
		t.Fatalf("malformed row key must be reported as a warning: %v", w)
	}
	// Without a valid env key the malformed row key is still a readiness error.
	t.Setenv("WECHAT_PAY_API_V3_KEY", "")
	if v := wechatConfig(row).Validate(); len(v) == 0 {
		t.Fatal("malformed row key without an env key must not be ready")
	}
	t.Setenv("WECHAT_PAY_API_V3_KEY", apiV3)

	app, o := auditOrder(t)
	if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_id,merchant_key,merchant_pem,pay_handleroute) VALUES(2,'微信扫码','wescan',1,3,'1900000000','1900000000',$1,'/pay/wepay')`, keys.privPEM); err != nil {
		t.Fatal(err)
	}
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE orders SET pay_id=2 WHERE order_sn=$1`, o.SN); err != nil {
		t.Fatal(err)
	}
	plain := fmt.Sprintf(`{"out_trade_no":%q,"trade_state":"SUCCESS","transaction_id":"wx-malformed","amount":{"total":1000}}`, o.SN)
	w := httptest.NewRecorder()
	app.wechatNotify(w, signedNotify(t, keys.key, apiV3, plain))
	if w.Code != 200 {
		t.Fatalf("callback with malformed row key: %d %s", w.Code, w.Body.String())
	}
	done, _ := app.live().OrderBySN(context.Background(), o.SN)
	if done.Status != 4 || done.TradeNo != "wx-malformed" {
		t.Fatalf("order not delivered: %+v", done)
	}
}
