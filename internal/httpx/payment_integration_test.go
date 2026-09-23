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

func TestWeChatSignedNotificationIntegration(t *testing.T) {
	app, o := auditOrder(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})))
	aesKey := "01234567890123456789012345678901"
	t.Setenv("WECHAT_PAY_API_V3_KEY", aesKey)
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
}
