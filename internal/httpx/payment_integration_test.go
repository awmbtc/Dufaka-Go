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
	"dufaka/internal/store"
	"dufaka/internal/testdb"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func auditOrder(t *testing.T) (*App, store.Order) {
	t.Helper()
	pool := testdb.Open(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'test');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type) VALUES(1,1,'card','','',10,1);
 INSERT INTO carmis(goods_id,carmi) VALUES(1,'AUDIT-SECRET');
 INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_pem,pay_handleroute) VALUES(1,'cldx','cldx',2,3,'','/pay/cldx');`)
	if err != nil {
		t.Fatal(err)
	}
	db := &store.DB{Pool: pool}
	o, err := db.CreateOrder(ctx, store.CreateInput{GID: 1, PayID: 1, Amount: 1, Email: "audit@example.com", SearchPwd: "query-secret"}, store.Site{})
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(db, "http://shop.test", "")
	if err != nil {
		t.Fatal(err)
	}
	return app, o
}

func TestWalletPaymentIntegration(t *testing.T) {
	app, o := auditOrder(t)
	quotes := 0
	paid := false
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/spend-quote" {
			quotes++
			fmt.Fprint(w, `{"amount":15000}`)
			return
		}
		if r.URL.Path == "/v1/payments" {
			fmt.Fprintf(w, `{"paid":%t,"amount":15000,"refunded":0}`, paid)
			return
		}
		http.NotFound(w, r)
	}))
	defer wallet.Close()
	app.wallet.Base = wallet.URL
	app.wallet.Secret = "test-only"
	r := httptest.NewRequest("GET", "http://shop.test/pay?clodex_app=1", nil)
	var first string
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		app.cldxPay(w, r, o)
		if w.Code != 200 || strings.Contains(w.Body.String(), "ZgotmplZ") {
			t.Fatal(w.Body.String())
		}
		if i == 0 {
			first = w.Body.String()
		} else if first != w.Body.String() {
			t.Fatal("refresh changed payment")
		}
	}
	if quotes != 1 {
		t.Fatalf("quoted %d times", quotes)
	}
	app.live().Pool.Exec(r.Context(), `UPDATE orders SET created_at=now()-interval '1 hour' WHERE id=$1`, o.ID)
	if err := app.live().ExpireDue(r.Context(), 5); err != nil {
		t.Fatal(err)
	}
	paid = true
	req := httptest.NewRequest("GET", "/check-order-status/"+o.SN, nil)
	req.SetPathValue("sn", o.SN)
	w := httptest.NewRecorder()
	app.poll(w, req)
	if !strings.Contains(w.Body.String(), `"code":200`) {
		t.Fatal(w.Body.String())
	}
	done, _ := app.live().OrderBySN(r.Context(), o.SN)
	if done.Status != 4 || done.Info != "AUDIT-SECRET" {
		t.Fatal("expired payment not delivered")
	}
	app.live().PutSetting(r.Context(), "is_open_search_pwd", "1")
	req = httptest.NewRequest("GET", "/detail-order-sn/"+o.SN, nil)
	req.SetPathValue("sn", o.SN)
	w = httptest.NewRecorder()
	app.detail(w, req)
	if strings.Contains(w.Body.String(), "AUDIT-SECRET") {
		t.Fatal("disclosed protected card")
	}
}

// #5: the notify handler reads the APIv3 key and merchant private key from the wescan
// payment row; only appid / serial / platform key come from the environment here.
func TestWeChatSignedNotificationIntegration(t *testing.T) {
	app, o := auditOrder(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})))
	t.Setenv("WECHAT_PAY_APP_ID", "wx-test-app")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "CERT-TEST")
	t.Setenv("WECHAT_PAY_API_V3_KEY", "environment-key-must-not-be-used!")
	t.Setenv("WECHAT_PAY_PLATFORM_SERIAL", "")
	aesKey := "01234567890123456789012345678901"
	priv := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	// No wescan row yet: the handler must fail loudly instead of guessing a key.
	early := httptest.NewRecorder()
	earlyReq := httptest.NewRequest("POST", "/pay/wepay/notify_url", strings.NewReader("{}"))
	earlyReq.Header.Set("Wechatpay-Timestamp", fmt.Sprint(time.Now().Unix()))
	app.wechatNotify(early, earlyReq)
	if early.Code != 500 {
		t.Fatalf("notify without a wescan row answered %d", early.Code)
	}
	// A-12: the timestamp window is checked before any database work, so a stale or
	// missing timestamp is refused (401) even when nothing is configured.
	stale := httptest.NewRecorder()
	app.wechatNotify(stale, httptest.NewRequest("POST", "/pay/wepay/notify_url", strings.NewReader("{}")))
	if stale.Code != 401 {
		t.Fatalf("notify without a timestamp answered %d", stale.Code)
	}
	_, err = app.live().Pool.Exec(context.Background(), `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_id,merchant_key,merchant_pem,pay_handleroute) VALUES(2,'微信扫码','wescan',1,3,'1900000000',$1,$2,'/pay/wepay')`, aesKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher([]byte(aesKey))
	gcm, _ := cipher.NewGCM(block)
	nonce := "123456789012"
	aad := "transaction"
	plain := fmt.Sprintf(`{"out_trade_no":%q,"trade_state":"SUCCESS","transaction_id":"wx-audit","amount":{"total":1000}}`, o.SN)
	encrypted := gcm.Seal(nil, []byte(nonce), []byte(plain), []byte(aad))
	raw, _ := json.Marshal(map[string]any{"resource": map[string]string{"nonce": nonce, "associated_data": aad, "ciphertext": base64.StdEncoding.EncodeToString(encrypted)}})
	ts := fmt.Sprint(time.Now().Unix())
	sigNonce := "audit-signature"
	sum := sha256.Sum256([]byte(ts + "\n" + sigNonce + "\n" + string(raw) + "\n"))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/pay/wepay/notify_url", strings.NewReader(string(raw)))
		r.Header.Set("Wechatpay-Timestamp", ts)
		r.Header.Set("Wechatpay-Nonce", sigNonce)
		r.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
		w := httptest.NewRecorder()
		app.wechatNotify(w, r)
		if w.Code != 200 {
			t.Fatalf("notify %d %s", w.Code, w.Body.String())
		}
	}
	done, _ := app.live().OrderBySN(context.Background(), o.SN)
	if done.Status != 4 || done.TradeNo != "wx-audit" {
		t.Fatalf("notification didn't deliver: %+v", done)
	}
	r := httptest.NewRequest("POST", "/pay/wepay/notify_url", strings.NewReader(string(raw)))
	w := httptest.NewRecorder()
	app.wechatNotify(w, r)
	if w.Code != 401 {
		t.Fatal("accepted unsigned notification")
	}
	// #21 (A3-3): the signature decides. With a platform serial configured, a verified
	// callback carrying another (or no) serial is still accepted — the key is right, only
	// the owner's serial setting is stale — and the mismatch is logged as a warning.
	t.Setenv("WECHAT_PAY_PLATFORM_SERIAL", "PUB_KEY_ID_LIVE")
	logs := captureLog(t)
	for _, serial := range []string{"PUB_KEY_ID_OLD", ""} {
		logs.Reset()
		r = httptest.NewRequest("POST", "/pay/wepay/notify_url", strings.NewReader(string(raw)))
		r.Header.Set("Wechatpay-Timestamp", ts)
		r.Header.Set("Wechatpay-Nonce", sigNonce)
		r.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
		r.Header.Set("Wechatpay-Serial", serial)
		w = httptest.NewRecorder()
		app.wechatNotify(w, r)
		if w.Code != 200 || !strings.Contains(logs.String(), "序列号不一致") {
			t.Fatalf("verified callback with serial %q: %d %q", serial, w.Code, logs.String())
		}
	}
	// A wescan row missing its key makes the handler answer 500 (WeChat retries) and log,
	// not 400. The key is only needed after the signature holds, so this is a fresh,
	// properly signed request.
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE pays SET merchant_key='' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WECHAT_PAY_API_V3_KEY", "")
	r = httptest.NewRequest("POST", "/pay/wepay/notify_url", strings.NewReader(string(raw)))
	r.Header.Set("Wechatpay-Timestamp", ts)
	r.Header.Set("Wechatpay-Nonce", sigNonce)
	r.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
	r.Header.Set("Wechatpay-Serial", "PUB_KEY_ID_LIVE")
	w = httptest.NewRecorder()
	app.wechatNotify(w, r)
	if w.Code != 500 {
		t.Fatalf("incomplete config answered %d", w.Code)
	}
}

// Exercise the actual HTTP cookie round trip before any payment is made.
func TestUnpaidBrowserSearchIntegration(t *testing.T) {
	t.Setenv("DUFAKA_SESSION_KEY", "browser-search-test-key")
	app, _ := auditOrder(t)
	// Checkout only accepts cldx while the wallet is configured (A3-10).
	app.wallet.Secret, app.wallet.MerchantID = "test-only", "shop-test"
	_, err := app.live().Pool.Exec(context.Background(), `INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(2,1,'Pending browser order','','',10,3,2)`)
	if err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/create-order", strings.NewReader("gid=2&payway=1&by_amount=1&email=browser%40example.com&search_pwd=query-secret"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		created := httptest.NewRecorder()
		app.create(created, req)
		if created.Code != http.StatusFound {
			t.Fatalf("create failed: %s", created.Body.String())
		}
		sn := strings.TrimPrefix(created.Header().Get("Location"), "/bill/")
		cookies := created.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("missing browser order cookie")
		}
		cookie = cookies[0]
		// AddCookie serializes the response cookie as a real browser request header.
		search := httptest.NewRequest("POST", "/search-order-by-browser", nil)
		search.AddCookie(cookie)
		found := httptest.NewRecorder()
		app.searchBrowser(found, search)
		if found.Code != http.StatusOK || !strings.Contains(found.Body.String(), sn) {
			t.Fatalf("unpaid order missing: %s", found.Body.String())
		}
		parsed, err := search.Cookie("dujiaoka_orders")
		if err != nil || len(browserOrders(parsed.Value)) != i+1 {
			t.Fatal("browser lost previous order")
		}
		order, err := app.live().OrderBySN(context.Background(), sn)
		if err != nil || order.Status != 1 {
			t.Fatal("expected unpaid order")
		}
	}
}
