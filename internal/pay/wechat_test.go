package pay

import (
	"bytes"
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
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testKeyPair(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	priv := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return key, pub, priv
}

func signNotice(t *testing.T, key *rsa.PrivateKey, ts, nonce, body string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(ts + "\n" + nonce + "\n" + body + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func TestVerifyNotify(t *testing.T) {
	key, pub, _ := testKeyPair(t)
	body := `{"id":"1"}`
	ts := "1700000000"
	nonce := "nonce"
	sign := signNotice(t, key, ts, nonce, body)
	now := time.Unix(1700000000, 0)
	cfg := WechatConfig{PlatformKeyPEM: pub}
	n := Notice{Timestamp: ts, Nonce: nonce, Signature: sign, Body: body}
	if err := cfg.VerifyNotify(n, now); err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyNotify(n, now.Add(10*time.Minute)); err == nil {
		t.Fatal("stale notice accepted")
	}
	if err := (WechatConfig{}).VerifyNotify(n, now); err == nil {
		t.Fatal("missing platform key accepted")
	}
	bad := n
	bad.Body = `{"id":"2"}`
	if err := cfg.VerifyNotify(bad, now); err == nil {
		t.Fatal("tampered body accepted")
	}
}

// #21 (round 3): the signature decides. A verified callback whose serial differs from the
// configured one is accepted (warning only); a failed signature under a different serial is
// reported as a probable platform-key rotation, distinct from a plain forgery.
func TestVerifyNotifyPlatformSerial(t *testing.T) {
	key, pub, _ := testKeyPair(t)
	body := `{"id":"1"}`
	ts := "1700000000"
	nonce := "nonce"
	now := time.Unix(1700000000, 0)
	n := Notice{Timestamp: ts, Nonce: nonce, Signature: signNotice(t, key, ts, nonce, body), Body: body, Serial: "PUB_KEY_ID_1"}
	cfg := WechatConfig{PlatformKeyPEM: pub, PlatformSerial: "PUB_KEY_ID_1"}
	if err := cfg.VerifyNotify(n, now); err != nil {
		t.Fatalf("matching serial rejected: %v", err)
	}
	logs := captureLog(t)
	n.Serial = "PUB_KEY_ID_2"
	if err := cfg.VerifyNotify(n, now); err != nil {
		t.Fatalf("verified callback rejected over its serial: %v", err)
	}
	if !strings.Contains(logs.String(), "序列号不一致") || !strings.Contains(logs.String(), "PUB_KEY_ID_2") {
		t.Fatalf("serial mismatch not logged: %q", logs.String())
	}
	n.Serial = ""
	if err := cfg.VerifyNotify(n, now); err != nil {
		t.Fatalf("verified callback without a serial rejected: %v", err)
	}
	// No configured serial: any (or no) header is fine.
	if err := (WechatConfig{PlatformKeyPEM: pub}).VerifyNotify(n, now); err != nil {
		t.Fatalf("serial enforced without configuration: %v", err)
	}
	// Signed by another key (WeChat rotated): the error names both serials.
	other, _, _ := testKeyPair(t)
	rotated := Notice{Timestamp: ts, Nonce: nonce, Signature: signNotice(t, other, ts, nonce, body), Body: body, Serial: "PUB_KEY_ID_NEW"}
	err := cfg.VerifyNotify(rotated, now)
	if err == nil || !strings.Contains(err.Error(), "微信支付平台证书可能已轮换：通知序列号 PUB_KEY_ID_NEW，已配置 PUB_KEY_ID_1") {
		t.Fatalf("rotation not told apart: %v", err)
	}
	// Same serial, bad signature: a forgery, not a rotation.
	rotated.Serial = "pub_key_id_1"
	if err := cfg.VerifyNotify(rotated, now); err == nil || err.Error() != "微信支付通知签名无效" {
		t.Fatalf("forgery reported as %v", err)
	}
	// No serial header on a bad signature: a forgery too.
	rotated.Serial = ""
	if err := cfg.VerifyNotify(rotated, now); err == nil || err.Error() != "微信支付通知签名无效" {
		t.Fatalf("headerless forgery reported as %v", err)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// #5: the payment row wins; the environment only fills fields the row leaves empty,
// including the deprecated WECHAT_PAY_MCH_ID / WECHAT_PAY_PRIVATE_KEY; Missing names what
// is absent.
func TestWechatConfigEnvFallbackAndMissing(t *testing.T) {
	t.Setenv("WECHAT_PAY_APP_ID", "env-app")
	t.Setenv("WECHAT_PAY_MCH_ID", "env-mch")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "")
	t.Setenv("WECHAT_PAY_API_V3_KEY", "env-key")
	t.Setenv("WECHAT_PAY_PRIVATE_KEY", "env-pem")
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", "")
	t.Setenv("WECHAT_PAY_PLATFORM_SERIAL", "env-serial")
	t.Setenv("WECHAT_PAY_PUBLIC_KEY_ID", "")
	rowKey := "row-key-row-key-row-key-row-key-"
	cfg := WechatConfig{MchID: "row-mch", APIv3Key: rowKey, PrivateKeyPEM: "row-pem"}.WithEnv()
	if cfg.MchID != "row-mch" || cfg.APIv3Key != rowKey || cfg.PrivateKeyPEM != "row-pem" {
		t.Fatalf("environment overrode the payment row: %+v", cfg)
	}
	if cfg.AppID != "env-app" || cfg.PlatformSerial != "env-serial" {
		t.Fatalf("environment fallback not applied: %+v", cfg)
	}
	missing := cfg.Missing()
	if len(missing) != 2 || !strings.Contains(missing[0], "证书序列号") || !strings.Contains(missing[1], "平台公钥") {
		t.Fatalf("unexpected missing list: %v", missing)
	}
	if err := cfg.MissingError(); err == nil || !strings.Contains(err.Error(), "WECHAT_PAY_CERT_SERIAL_NO") {
		t.Fatalf("missing error does not name the piece: %v", err)
	}
	cfg.SerialNo, cfg.PlatformKeyPEM = "s", "p"
	if cfg.Missing() != nil || cfg.MissingError() != nil {
		t.Fatal("complete config reported as missing")
	}
	if _, err := Native(context.Background(), WechatConfig{}, "", "SN", "x", 1); err == nil || !strings.Contains(err.Error(), "配置不完整") {
		t.Fatalf("Native ran with an incomplete config: %v", err)
	}
}

// A3-2: a shop configured only through the deprecated WECHAT_PAY_MCH_ID and
// WECHAT_PAY_PRIVATE_KEY keeps working: an empty row field is filled from them, a filled
// one is not, and surrounding whitespace is trimmed.
func TestWechatConfigDeprecatedEnvFallback(t *testing.T) {
	t.Setenv("WECHAT_PAY_MCH_ID", " 1900000001\n")
	t.Setenv("WECHAT_PAY_PRIVATE_KEY", "\nenv-pem\n")
	bare := WechatConfig{}.WithEnv()
	if bare.MchID != "1900000001" || bare.PrivateKeyPEM != "env-pem" {
		t.Fatalf("deprecated variables ignored: %+v", bare)
	}
	blank := WechatConfig{MchID: "  ", PrivateKeyPEM: "\n"}.WithEnv()
	if blank.MchID != "1900000001" || blank.PrivateKeyPEM != "env-pem" {
		t.Fatalf("whitespace-only row fields not filled: %+v", blank)
	}
	row := WechatConfig{MchID: "row-mch", PrivateKeyPEM: "row-pem"}.WithEnv()
	if row.MchID != "row-mch" || row.PrivateKeyPEM != "row-pem" {
		t.Fatalf("environment overrode the row: %+v", row)
	}
	if m := strings.Join(bare.MissingForOrder(), "、"); strings.Contains(m, "商户号") || strings.Contains(m, "商户私钥") {
		t.Fatalf("filled fields still reported missing: %s", m)
	}
}

// Native sends every field from the config, never from the environment, and returns code_url.
func TestNativeSendsConfiguredCredentials(t *testing.T) {
	t.Setenv("WECHAT_PAY_APP_ID", "env-app")
	t.Setenv("WECHAT_PAY_MCH_ID", "env-mch")
	_, pub, priv := testKeyPair(t)
	var got struct {
		body   map[string]any
		auth   string
		serial string
		path   string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		got.auth = r.Header.Get("Authorization")
		got.serial = r.Header.Get("Wechatpay-Serial")
		got.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code_url":"weixin://wxpay/bizpayurl?pr=test"}`))
	}))
	defer srv.Close()
	old := wechatAPIBase
	wechatAPIBase = srv.URL
	defer func() { wechatAPIBase = old }()
	cfg := WechatConfig{AppID: "row-app", MchID: "row-mch", SerialNo: "CERT01", APIv3Key: "01234567890123456789012345678901", PrivateKeyPEM: priv, PlatformKeyPEM: pub, PublicKeyID: "PUB_KEY_ID_9"}
	code, err := Native(context.Background(), cfg, "https://shop.test/pay/wepay/notify_url", "SN1", "card", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if code != "weixin://wxpay/bizpayurl?pr=test" {
		t.Fatalf("code url %q", code)
	}
	if got.path != "/v3/pay/transactions/native" || got.body["appid"] != "row-app" || got.body["mchid"] != "row-mch" || got.body["out_trade_no"] != "SN1" {
		t.Fatalf("request not built from config: %s %v", got.path, got.body)
	}
	if !strings.Contains(got.auth, `mchid="row-mch"`) || !strings.Contains(got.auth, `serial_no="CERT01"`) || got.serial != "PUB_KEY_ID_9" {
		t.Fatalf("headers not built from config: %s / %s", got.auth, got.serial)
	}
	if amount, _ := got.body["amount"].(map[string]any); amount["total"] != float64(1000) {
		t.Fatalf("amount %v", got.body["amount"])
	}
}

func TestDecryptResource(t *testing.T) {
	key := "01234567890123456789012345678901"
	block, _ := aes.NewCipher([]byte(key))
	gcm, _ := cipher.NewGCM(block)
	nonce := "123456789012"
	sealed := gcm.Seal(nil, []byte(nonce), []byte(`{"ok":1}`), []byte("transaction"))
	cipherText := base64.StdEncoding.EncodeToString(sealed)
	plain, err := (WechatConfig{APIv3Key: key}).DecryptResource(nonce, cipherText, "transaction")
	if err != nil || string(plain) != `{"ok":1}` {
		t.Fatalf("decrypt: %v %s", err, plain)
	}
	if _, err := (WechatConfig{}).DecryptResource(nonce, cipherText, "transaction"); err == nil {
		t.Fatal("decrypted without a key")
	}
	if _, err := (WechatConfig{APIv3Key: "wrong-key-wrong-key-wrong-key-00"}).DecryptResource(nonce, cipherText, "transaction"); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

// #23: the wallet contract is {"to","order","from","amount","refunded","paid","at"} with no
// payment id. paid / amount / refunded must be well typed; from / at are optional and any
// junk in them must not fail the receipt.
func TestReceiptParsesWalletContract(t *testing.T) {
	var r Receipt
	if err := json.Unmarshal([]byte(`{"to":"shop","order":"SN1","from":" cldx1payer ","amount":15000,"refunded":0,"paid":true,"at":"2026-09-25T08:00:00Z"}`), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Paid || r.Amount != 15000 || r.Refunded != 0 || r.From != "cldx1payer" || !r.At.Equal(time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", r)
	}
	for _, raw := range []string{
		`{"paid":true,"amount":1,"refunded":0}`,
		`{"paid":true,"amount":1,"refunded":0,"from":123,"at":456}`,
		`{"paid":true,"amount":1,"refunded":0,"from":null,"at":"yesterday"}`,
		`{"paid":true,"amount":1,"refunded":0,"from":{"x":1},"at":["y"]}`,
		`{"paid":true,"amount":1,"refunded":0,"id":"pay_1","paymentId":"pay_2"}`,
	} {
		var r Receipt
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !r.Paid || r.Amount != 1 || r.From != "" || !r.At.IsZero() {
			t.Fatalf("%s: %+v", raw, r)
		}
	}
	for _, raw := range []string{`{"paid":"yes","amount":1}`, `{"paid":true,"amount":"1"}`, `{"paid":true,"amount":1,"refunded":"0"}`, `[]`} {
		var r Receipt
		if err := json.Unmarshal([]byte(raw), &r); err == nil {
			t.Fatalf("%s parsed as %+v", raw, r)
		}
	}
	long := strings.Repeat("f", 300)
	if err := json.Unmarshal([]byte(`{"paid":true,"amount":1,"refunded":0,"from":"`+long+`"}`), &r); err != nil || r.From != long {
		t.Fatalf("long from: %v %d", err, len(r.From))
	}
}

// #5: every field is trimmed, including values taken from the environment, so "key\n"
// pasted into the admin form or exported with a trailing newline still counts as 32 bytes.
func TestWechatConfigTrimsEveryField(t *testing.T) {
	key := "01234567890123456789012345678901"
	t.Setenv("WECHAT_PAY_APP_ID", " env-app\n")
	t.Setenv("WECHAT_PAY_MCH_ID", "")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "\tCERT01\n")
	t.Setenv("WECHAT_PAY_API_V3_KEY", "")
	t.Setenv("WECHAT_PAY_PRIVATE_KEY", "")
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", " pub \n")
	t.Setenv("WECHAT_PAY_PLATFORM_SERIAL", "")
	t.Setenv("WECHAT_PAY_PUBLIC_KEY_ID", " PUB_1 ")
	cfg := WechatConfig{MchID: " row-mch \r\n", APIv3Key: key + "\n", PrivateKeyPEM: "\n row-pem \n"}.WithEnv()
	want := WechatConfig{AppID: "env-app", MchID: "row-mch", SerialNo: "CERT01", APIv3Key: key, PrivateKeyPEM: "row-pem", PlatformKeyPEM: "pub", PublicKeyID: "PUB_1"}
	if cfg != want {
		t.Fatalf("got %+v\nwant %+v", cfg, want)
	}
	if m := cfg.Missing(); m != nil {
		t.Fatalf("trimmed config reported missing: %v", m)
	}
	// Trimmed on its own, without the environment.
	if got := (WechatConfig{APIv3Key: key + "\n"}).Trimmed().APIv3Key; got != key {
		t.Fatalf("Trimmed: %q", got)
	}
	// Decryption uses the trimmed key too.
	block, _ := aes.NewCipher([]byte(key))
	gcm, _ := cipher.NewGCM(block)
	nonce := "123456789012"
	sealed := base64.StdEncoding.EncodeToString(gcm.Seal(nil, []byte(nonce), []byte(`{"ok":1}`), []byte("transaction")))
	if plain, err := (WechatConfig{APIv3Key: key + "\n"}).DecryptResource(nonce, sealed, "transaction"); err != nil || string(plain) != `{"ok":1}` {
		t.Fatalf("decrypt with untrimmed key: %v %s", err, plain)
	}
}

// #5: the self-check is split; the APIv3 key must be exactly 32 bytes; the private key is
// labelled 商户密钥 (the admin column), never 商户 PEM.
func TestWechatConfigMissingSplit(t *testing.T) {
	for _, k := range []string{"WECHAT_PAY_APP_ID", "WECHAT_PAY_MCH_ID", "WECHAT_PAY_CERT_SERIAL_NO", "WECHAT_PAY_API_V3_KEY", "WECHAT_PAY_PRIVATE_KEY", "WECHAT_PAY_PLATFORM_KEY", "WECHAT_PAY_PLATFORM_SERIAL", "WECHAT_PAY_PUBLIC_KEY_ID"} {
		t.Setenv(k, "")
	}
	empty := WechatConfig{}.WithEnv()
	order, notify := empty.MissingForOrder(), empty.MissingForNotify()
	if len(order) != 4 || len(notify) != 2 || len(empty.Missing()) != 6 {
		t.Fatalf("order %v notify %v union %v", order, notify, empty.Missing())
	}
	joined := strings.Join(empty.Missing(), "、")
	if !strings.Contains(joined, "商户密钥") || strings.Contains(joined, "商户 PEM") {
		t.Fatalf("private key label: %s", joined)
	}
	short := WechatConfig{AppID: "a", MchID: "m", SerialNo: "s", PrivateKeyPEM: "p", PlatformKeyPEM: "k", APIv3Key: "too-short"}
	if m := short.MissingForOrder(); m != nil {
		t.Fatalf("order half complained: %v", m)
	}
	if m := short.MissingForNotify(); len(m) != 1 || m[0] != "微信支付 APIv3 密钥必须是 32 字节（ASCII）" {
		t.Fatalf("31-byte key accepted: %v", m)
	}
	if err := short.MissingForNotifyError(); err == nil || !strings.Contains(err.Error(), "必须是 32 字节") {
		t.Fatalf("notify error: %v", err)
	}
	if _, err := short.DecryptResource("123456789012", "AAAA", "x"); err == nil || !strings.Contains(err.Error(), "32 字节") {
		t.Fatalf("decrypt with a short key: %v", err)
	}
	long := short
	long.APIv3Key = strings.Repeat("k", 33)
	if m := long.MissingForNotify(); len(m) != 1 || !strings.Contains(m[0], "32 字节") {
		t.Fatalf("33-byte key accepted: %v", m)
	}
	ok := short
	ok.APIv3Key = strings.Repeat("k", 32)
	if ok.Missing() != nil || ok.MissingError() != nil || ok.MissingForOrderError() != nil || ok.MissingForNotifyError() != nil {
		t.Fatalf("complete config reported missing: %v", ok.Missing())
	}
	// The cashier half does not need the notify half: Native fails only on its own fields.
	if _, err := Native(context.Background(), WechatConfig{APIv3Key: "x", PlatformKeyPEM: "y"}, "", "SN", "x", 1); err == nil || strings.Contains(err.Error(), "APIv3") || strings.Contains(err.Error(), "平台公钥") {
		t.Fatalf("Native complained about notify fields: %v", err)
	}
}

// selfSignedCert returns a certificate PEM with the given serial number.
func selfSignedCert(t *testing.T, key *rsa.PrivateKey, serial *big.Int) string {
	t.Helper()
	tpl := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// #21: expected serial = PlatformSerial, else the certificate serial of PlatformKeyPEM,
// else PublicKeyID; compared case-insensitively ignoring leading zeros; nothing configured
// means unchecked (logged once). A mismatch under a valid signature is a warning only.
func TestVerifyNotifySerialFallbacks(t *testing.T) {
	key, pub, _ := testKeyPair(t)
	// First byte 0x0A (< 0x10): the whole-byte form keeps the leading zero.
	certPEM := selfSignedCert(t, key, new(big.Int).SetBytes([]byte{0x0A, 0xBC, 0xDE, 0xF1, 0x23}))
	const certSerial = "0ABCDEF123"
	body := `{"id":"1"}`
	ts := "1700000000"
	nonce := "nonce"
	now := time.Unix(1700000000, 0)
	sig := signNotice(t, key, ts, nonce, body)
	notice := func(serial string) Notice {
		return Notice{Timestamp: ts, Nonce: nonce, Signature: sig, Body: body, Serial: serial}
	}
	cases := []struct {
		name string
		cfg  WechatConfig
		want string
	}{
		{"platform serial wins", WechatConfig{PlatformKeyPEM: certPEM, PlatformSerial: "SER_A", PublicKeyID: "PUB_B"}, "SER_A"},
		{"certificate serial beats public key id", WechatConfig{PlatformKeyPEM: certPEM, PublicKeyID: "PUB_B"}, certSerial},
		{"certificate serial alone", WechatConfig{PlatformKeyPEM: certPEM}, certSerial},
		{"public key id for a bare key", WechatConfig{PlatformKeyPEM: pub, PublicKeyID: "PUB_B"}, "PUB_B"},
		{"bare public key has none", WechatConfig{PlatformKeyPEM: pub}, ""},
	}
	logs := captureLog(t)
	for _, c := range cases {
		if got := c.cfg.ExpectedSerial(); got != c.want {
			t.Fatalf("%s: expected serial %q want %q", c.name, got, c.want)
		}
		for _, header := range []string{c.want, " " + strings.ToLower(c.want) + " ", "OTHER", "", "anything"} {
			logs.Reset()
			if err := c.cfg.VerifyNotify(notice(header), now); err != nil {
				t.Fatalf("%s: verified callback with serial %q refused: %v", c.name, header, err)
			}
			warned := strings.Contains(logs.String(), "序列号不一致")
			if wantWarn := c.want != "" && !sameSerial(header, c.want); warned != wantWarn {
				t.Fatalf("%s: serial %q warned=%v: %q", c.name, header, warned, logs.String())
			}
		}
	}
	// WeChat Pay prints serials without the leading zero of the first byte, or with
	// extra ones; both still match.
	for _, header := range []string{"ABCDEF123", "000abcdef123", "0ABCDEF123"} {
		if !sameSerial(header, certSerial) {
			t.Fatalf("%q does not match %q", header, certSerial)
		}
	}
	if sameSerial("ABCDEF124", certSerial) || sameSerial("", certSerial) {
		t.Fatal("different serials matched")
	}
	// Order: the timestamp gate comes first; a bad signature under the configured serial is
	// a forgery; a bad signature under another serial is a probable rotation; the
	// attacker-controlled header is clipped and stripped of control characters.
	cfg := WechatConfig{PlatformKeyPEM: certPEM, PlatformSerial: "SER_A"}
	forged := notice("ser_a")
	forged.Body = `{"id":"2"}`
	if err := cfg.VerifyNotify(forged, now); err == nil || err.Error() != "微信支付通知签名无效" {
		t.Fatalf("forged body: %v", err)
	}
	forged.Serial = "SER_B"
	if err := cfg.VerifyNotify(forged, now); err == nil || !strings.Contains(err.Error(), "可能已轮换") || !strings.Contains(err.Error(), "SER_B") || !strings.Contains(err.Error(), "SER_A") {
		t.Fatalf("rotation not reported: %v", err)
	}
	stale := notice("OTHER")
	if err := cfg.VerifyNotify(stale, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "时间无效") {
		t.Fatalf("stale notice reached the signature check: %v", err)
	}
	huge := "坏\n" + strings.Repeat("坏", 300)
	forged.Serial = huge
	err := cfg.VerifyNotify(forged, now)
	if err == nil || strings.Contains(err.Error(), huge[:20]) || strings.Contains(err.Error(), "\n") || len([]rune(err.Error())) > 200 {
		t.Fatalf("header not clipped: %v", err)
	}
	logs.Reset()
	if err := cfg.VerifyNotify(notice(huge), now); err != nil || strings.Contains(logs.String(), "坏\n") || len([]rune(logs.String())) > 300 {
		t.Fatalf("warning not clipped: %v %q", err, logs.String())
	}
	// CheckTimestamp is the standalone first gate the handler runs before any database work.
	if err := CheckTimestamp("1700000100", now); err != nil {
		t.Fatal(err)
	}
	if err := CheckTimestamp("1700000000", now.Add(6*time.Minute)); err == nil {
		t.Fatal("stale timestamp passed")
	}
	if err := CheckTimestamp("soon", now); err == nil {
		t.Fatal("garbage timestamp passed")
	}
}
