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
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// wechatAPIBase is the WeChat Pay API origin. Tests point it at a local server.
var wechatAPIBase = "https://api.mch.weixin.qq.com"

// WechatConfig holds the WeChat Pay APIv3 credentials. The shop builds it from the
// "wescan" payment row (merchant_id = 商户号, merchant_pem = 商户密钥 / private key,
// merchant_key = 商户 KEY / APIv3 key) and fills every field the row leaves empty from the
// environment with WithEnv; the cashier and the notify handler both use the same value,
// so a key changed in the admin panel takes effect everywhere. The row always wins; the
// certificate-level fields (appid, certificate serial, platform key and its ids) exist
// only in the environment.
type WechatConfig struct {
	AppID          string // 公众号 / 小程序 / 开放平台 appid
	MchID          string // 商户号
	SerialNo       string // 商户 API 证书序列号
	APIv3Key       string // APIv3 密钥，32 字节，用于解密通知
	PrivateKeyPEM  string // 商户 API 私钥（PEM 或其 base64）
	PlatformKeyPEM string // 微信支付平台公钥或平台证书（PEM 或其 base64），用于验签通知
	PlatformSerial string // 平台公钥 ID / 平台证书序列号；非空时通知的 Wechatpay-Serial 必须与之一致
	PublicKeyID    string // 请求头 Wechatpay-Serial 用的平台公钥 ID（公钥模式商户需要）

	// rowKeyMalformed records that WithEnv replaced a wescan-row APIv3 key that was not
	// 32 bytes with a valid WECHAT_PAY_API_V3_KEY. Before the 2026-09-25 audit the row
	// value was never read, so a live row may carry a leftover (dujiaoka kept mch_id
	// there); that must not switch WeChat Pay off. Reported through Warnings.
	rowKeyMalformed bool
}

// apiV3KeyLen is the fixed length WeChat Pay assigns to an APIv3 key (AES-256-GCM).
const apiV3KeyLen = 32

// WithEnv returns a copy where every empty field is taken from the matching environment
// variable, then trimmed. Environment variables are only a fallback, never an override:
// a value on the wescan row always wins. WECHAT_PAY_MCH_ID and WECHAT_PAY_PRIVATE_KEY are
// deprecated but still read, because a shop configured before the admin form existed may
// rely on them. A trailing newline pasted into the admin form or left by
// `export KEY="$(cat file)"` is removed from every field so a 33-byte "key\n" cannot
// masquerade as a 32-byte key.
func (c WechatConfig) WithEnv() WechatConfig {
	fill := func(dst *string, key string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = os.Getenv(key)
		}
	}
	fill(&c.AppID, "WECHAT_PAY_APP_ID")
	fill(&c.MchID, "WECHAT_PAY_MCH_ID")              // deprecated, still supported
	fill(&c.PrivateKeyPEM, "WECHAT_PAY_PRIVATE_KEY") // deprecated, still supported
	fill(&c.SerialNo, "WECHAT_PAY_CERT_SERIAL_NO")
	if row := strings.TrimSpace(c.APIv3Key); row != "" && len(row) != apiV3KeyLen && len(envAPIv3Key()) == apiV3KeyLen {
		c.APIv3Key, c.rowKeyMalformed = "", true
	}
	fill(&c.APIv3Key, "WECHAT_PAY_API_V3_KEY")
	fill(&c.PlatformKeyPEM, "WECHAT_PAY_PLATFORM_KEY")
	fill(&c.PlatformSerial, "WECHAT_PAY_PLATFORM_SERIAL")
	fill(&c.PublicKeyID, "WECHAT_PAY_PUBLIC_KEY_ID")
	return c.Trimmed()
}

// Trimmed returns a copy with surrounding whitespace removed from every field.
func (c WechatConfig) Trimmed() WechatConfig {
	for _, f := range []*string{&c.AppID, &c.MchID, &c.SerialNo, &c.APIv3Key, &c.PrivateKeyPEM, &c.PlatformKeyPEM, &c.PlatformSerial, &c.PublicKeyID} {
		*f = strings.TrimSpace(*f)
	}
	return c
}

// MissingForOrder lists what the cashier needs to create a Native order and is still
// empty, in Chinese so the message can be shown to the shop owner as is.
func (c WechatConfig) MissingForOrder() []string {
	c = c.Trimmed()
	var out []string
	add := func(v, name string) {
		if v == "" {
			out = append(out, name)
		}
	}
	add(c.AppID, "appid（WECHAT_PAY_APP_ID）")
	add(c.MchID, "商户号（后台 wescan 渠道的商户 ID）")
	add(c.SerialNo, "商户证书序列号（WECHAT_PAY_CERT_SERIAL_NO）")
	add(c.PrivateKeyPEM, "商户私钥（后台 wescan 渠道的商户密钥）")
	return out
}

// MissingForNotify lists what the notify handler needs to verify and decrypt a callback:
// an APIv3 key of exactly 32 bytes and the platform public key. PlatformSerial and
// PublicKeyID are optional.
func (c WechatConfig) MissingForNotify() []string {
	c = c.Trimmed()
	var out []string
	switch {
	case c.APIv3Key == "":
		out = append(out, "APIv3 密钥（后台商户 KEY 或 WECHAT_PAY_API_V3_KEY）")
	case len(c.APIv3Key) != apiV3KeyLen:
		out = append(out, "微信支付 APIv3 密钥必须是 32 字节（ASCII）")
	}
	if c.PlatformKeyPEM == "" {
		out = append(out, "平台公钥（WECHAT_PAY_PLATFORM_KEY）")
	}
	return out
}

// Missing is the union of MissingForOrder and MissingForNotify: the admin page shows the
// whole picture, the cashier and the notify handler each check only their own half.
func (c WechatConfig) Missing() []string {
	return append(c.MissingForOrder(), c.MissingForNotify()...)
}

func missingError(m []string) error {
	if len(m) > 0 {
		return fmt.Errorf("微信支付配置不完整，缺少：%s", strings.Join(m, "、"))
	}
	return nil
}

// MissingError turns Missing into one error, or nil when the config is complete.
func (c WechatConfig) MissingError() error { return missingError(c.Missing()) }

// MissingForOrderError turns MissingForOrder into one error, or nil.
func (c WechatConfig) MissingForOrderError() error { return missingError(c.MissingForOrder()) }

// MissingForNotifyError turns MissingForNotify into one error, or nil.
func (c WechatConfig) MissingForNotifyError() error { return missingError(c.MissingForNotify()) }

// Native creates a WeChat Pay API v3 Native order and returns the code URL.
// The shop page stays the original QR cashier. The merchant account is v3.
func Native(ctx context.Context, cfg WechatConfig, notify, sn, desc string, cents int64) (string, error) {
	cfg = cfg.Trimmed()
	if err := cfg.MissingForOrderError(); err != nil {
		return "", err
	}
	key, err := loadKey(cfg.PrivateKeyPEM)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"appid": cfg.AppID, "mchid": cfg.MchID, "description": desc, "out_trade_no": sn,
		"notify_url": notify, "amount": map[string]any{"total": cents, "currency": "CNY"},
	})
	const path = "/v3/pay/transactions/native"
	nonce := randText(16)
	ts := fmt.Sprint(time.Now().Unix())
	msg := "POST\n" + path + "\n" + ts + "\n" + nonce + "\n" + string(body) + "\n"
	sum := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wechatAPIBase+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`,
		cfg.MchID, nonce, base64.StdEncoding.EncodeToString(sig), ts, cfg.SerialNo))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Dufaka-Go/0.1")
	if cfg.PublicKeyID != "" {
		req.Header.Set("Wechatpay-Serial", cfg.PublicKeyID)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		CodeURL string `json:"code_url"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &out)
	if res.StatusCode >= 300 || out.CodeURL == "" {
		if out.Message == "" {
			out.Message = res.Status
		}
		return "", fmt.Errorf("微信支付下单失败: %s", out.Message)
	}
	return out.CodeURL, nil
}

func loadKey(pemText string) (*rsa.PrivateKey, error) {
	if strings.TrimSpace(pemText) == "" {
		return nil, fmt.Errorf("未配置微信商户私钥")
	}
	block := pemBlock(pemText)
	if block == nil {
		return nil, fmt.Errorf("商户私钥无法解析")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func randText(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// Notice carries the signed parts of a WeChat Pay callback: the four Wechatpay-* headers
// and the raw request body.
type Notice struct {
	Timestamp string // Wechatpay-Timestamp
	Nonce     string // Wechatpay-Nonce
	Signature string // Wechatpay-Signature
	Serial    string // Wechatpay-Serial
	Body      string
}

// notifyWindow is how far a callback's Wechatpay-Timestamp may drift from the shop clock.
const notifyWindow = 300

// CheckTimestamp is the cheap first gate on a callback: the Wechatpay-Timestamp header
// must be a unix time within five minutes of now. The notify handler runs it before any
// database work so a replayed or garbage request costs nothing but a parse.
func CheckTimestamp(timestamp string, now time.Time) error {
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil || math.Abs(float64(now.Unix()-ts)) > notifyWindow {
		return fmt.Errorf("微信支付通知时间无效")
	}
	return nil
}

// ExpectedSerial is the platform key id a callback's Wechatpay-Serial header should
// carry: PlatformSerial when set (the owner said so explicitly), else the serial of
// PlatformKeyPEM when it is an X.509 platform certificate (the key the shop verifies with
// names itself), else PublicKeyID. A certificate serial is printed as whole bytes in
// upper-case hex, the form WeChat Pay uses. It is empty when nothing identifies the
// platform key, in which case the header goes unchecked.
func (c WechatConfig) ExpectedSerial() string {
	c = c.Trimmed()
	if c.PlatformSerial != "" {
		return c.PlatformSerial
	}
	if cert := loadCertificate(c.PlatformKeyPEM); cert != nil && cert.SerialNumber != nil {
		return certSerial(cert)
	}
	return c.PublicKeyID
}

// certSerial formats a certificate serial number as whole bytes of upper-case hex, so a
// serial whose first byte is below 0x10 keeps its leading zero ("0A1B…", not "A1B…").
func certSerial(cert *x509.Certificate) string {
	return fmt.Sprintf("%X", cert.SerialNumber.Bytes())
}

// sameSerial compares two serials case-insensitively, ignoring surrounding whitespace
// and leading zeros on both sides.
func sameSerial(a, b string) bool {
	norm := func(s string) string { return strings.TrimLeft(strings.ToUpper(strings.TrimSpace(s)), "0") }
	return norm(a) == norm(b)
}

var serialUncheckedOnce sync.Once

// VerifyNotify checks a callback in cost order: the timestamp window, then the RSA
// signature under the configured platform key. The signature decides; the
// Wechatpay-Serial header (see ExpectedSerial) only explains:
//
//   - signature valid, serial differs: accepted, with a warning in the log (the owner's
//     serial setting is stale, the key itself is right);
//   - signature invalid, serial differs: WeChat Pay has most likely rotated its platform
//     key, and the error says so, naming both serials;
//   - signature invalid otherwise: "签名无效", a forgery or a damaged body.
//
// Attacker-controlled header text is clipped to 64 runes and stripped of control
// characters before it reaches an error or a log line.
func (c WechatConfig) VerifyNotify(n Notice, now time.Time) error {
	c = c.Trimmed()
	if err := CheckTimestamp(n.Timestamp, now); err != nil {
		return err
	}
	pub, err := loadPublic(c.PlatformKeyPEM)
	if err != nil {
		return err
	}
	want := c.ExpectedSerial()
	got := strings.TrimSpace(n.Serial)
	mismatch := want != "" && !sameSerial(got, want)
	sum := sha256.Sum256([]byte(n.Timestamp + "\n" + n.Nonce + "\n" + n.Body + "\n"))
	sig, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(n.Signature))
	if decodeErr != nil || rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
		if mismatch && got != "" {
			return fmt.Errorf("微信支付平台证书可能已轮换：通知序列号 %s，已配置 %s", safeText(got), safeText(want))
		}
		return fmt.Errorf("微信支付通知签名无效")
	}
	switch {
	case want == "":
		serialUncheckedOnce.Do(func() {
			log.Printf("微信支付通知：未配置平台公钥 ID / 平台证书序列号，Wechatpay-Serial 头不做校验（可设置 WECHAT_PAY_PLATFORM_SERIAL 或 WECHAT_PAY_PUBLIC_KEY_ID）")
		})
	case mismatch:
		log.Printf("微信支付通知：验签通过，但序列号不一致（通知 %s，已配置 %s），请核对 WECHAT_PAY_PLATFORM_SERIAL / WECHAT_PAY_PUBLIC_KEY_ID", safeText(got), safeText(want))
	}
	return nil
}

// safeText makes header text fit for a log line: control characters become "?" and the
// result is clipped to 64 runes.
func safeText(s string) string {
	return clip(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s), 64)
}

// clip bounds attacker-controlled text before it reaches a log line or error message.
func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

// pemBlock decodes PEM text, or base64 wrapping PEM text, into its first block.
func pemBlock(pemText string) *pem.Block {
	pemText = strings.TrimSpace(pemText)
	if pemText == "" {
		return nil
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		decoded, err := base64.StdEncoding.DecodeString(pemText)
		if err != nil {
			return nil
		}
		block, _ = pem.Decode(decoded)
	}
	return block
}

// loadCertificate returns the X.509 certificate in pemText, or nil when the text holds a
// bare public key or nothing parsable.
func loadCertificate(pemText string) *x509.Certificate {
	block := pemBlock(pemText)
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert
}

func loadPublic(pemText string) (*rsa.PublicKey, error) {
	if strings.TrimSpace(pemText) == "" {
		return nil, fmt.Errorf("未配置微信支付平台公钥")
	}
	block := pemBlock(pemText)
	if block == nil {
		return nil, fmt.Errorf("微信支付平台公钥无法解析")
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rk, ok := pub.(*rsa.PublicKey); ok {
			return rk, nil
		}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("微信支付平台公钥无法解析")
	}
	rk, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("微信支付平台公钥无法解析")
	}
	return rk, nil
}

// DecryptResource opens a v3 notify resource with the configured APIv3 key.
// Unused fields stay out of logs.
func (c WechatConfig) DecryptResource(nonce, ciphertext, aad string) ([]byte, error) {
	key := strings.TrimSpace(c.APIv3Key)
	if key == "" {
		return nil, fmt.Errorf("未配置微信支付 APIv3 密钥")
	}
	if len(key) != apiV3KeyLen {
		return nil, fmt.Errorf("微信支付 APIv3 密钥必须是 32 字节（ASCII）")
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, []byte(nonce), raw, []byte(aad))
}

// Validate is the full readiness check used before issuing a WeChat QR code, by the
// storefront to decide whether wescan is offered, and by the admin status block: every
// missing piece (Missing) plus pieces that are present but unusable — a private key or a
// platform key that does not parse. The APIv3 key length (exactly 32 bytes after
// trimming) is already part of Missing. Each problem is one short Chinese item; an empty
// result means the shop can both create an order and verify/decrypt its callback.
func (c WechatConfig) Validate() []string {
	c = c.Trimmed()
	out := c.Missing()
	if c.PrivateKeyPEM != "" && !parses('k', c.PrivateKeyPEM, func(s string) error { _, err := loadKey(s); return err }) {
		out = append(out, "商户私钥无法解析（后台 wescan 渠道的商户密钥）")
	}
	if c.PlatformKeyPEM != "" && !parses('p', c.PlatformKeyPEM, func(s string) error { _, err := loadPublic(s); return err }) {
		out = append(out, "平台公钥无法解析（WECHAT_PAY_PLATFORM_KEY）")
	}
	return out
}

// parseMemo remembers whether a PEM text parsed, keyed by a hash of the text (never the
// text itself). Parsing and validating an RSA private key costs milliseconds and the
// storefront runs Validate on every product page, so each distinct key is parsed once;
// the map is reset when it grows past a few dozen entries.
var parseMemo struct {
	mu sync.Mutex
	ok map[[sha256.Size]byte]bool
}

func parses(kind byte, text string, parse func(string) error) bool {
	h := sha256.Sum256(append([]byte{kind}, text...))
	parseMemo.mu.Lock()
	v, hit := parseMemo.ok[h]
	parseMemo.mu.Unlock()
	if hit {
		return v
	}
	v = parse(text) == nil
	parseMemo.mu.Lock()
	if parseMemo.ok == nil || len(parseMemo.ok) >= 64 {
		parseMemo.ok = map[[sha256.Size]byte]bool{}
	}
	parseMemo.ok[h] = v
	parseMemo.mu.Unlock()
	return v
}

// envAPIv3Key is the deployment-level APIv3 key. Before the 2026-09-25 audit the
// notify handler decrypted only with this variable and ignored the admin 商户 KEY,
// so a live wescan row may still carry a stale value there.
func envAPIv3Key() string { return strings.TrimSpace(os.Getenv("WECHAT_PAY_API_V3_KEY")) }

// Warnings lists configuration that works but deserves the owner's attention.
func (c WechatConfig) Warnings() []string {
	if c.rowKeyMalformed {
		return []string{"后台商户 KEY 不是 32 字节，已改用环境变量 WECHAT_PAY_API_V3_KEY；请在后台清空或改正"}
	}
	env := envAPIv3Key()
	if env != "" && strings.TrimSpace(c.APIv3Key) != "" && env != strings.TrimSpace(c.APIv3Key) {
		return []string{"后台商户 KEY 与环境变量 WECHAT_PAY_API_V3_KEY 不一致：通知先用后台值解密，失败时再用环境变量"}
	}
	return nil
}

// DecryptWithEnvFallback decrypts a verified callback resource with the configured
// APIv3 key and, if that fails while a different WECHAT_PAY_API_V3_KEY is set, retries
// with the environment key. usedEnv reports that the fallback was needed, which means
// the admin value is stale. Only call it after VerifyNotify has succeeded.
func (c WechatConfig) DecryptWithEnvFallback(nonce, ciphertext, aad string) (plain []byte, usedEnv bool, err error) {
	plain, err = c.DecryptResource(nonce, ciphertext, aad)
	if err == nil {
		return plain, false, nil
	}
	env := envAPIv3Key()
	if env == "" || env == strings.TrimSpace(c.APIv3Key) {
		return nil, false, err
	}
	alt := c
	alt.APIv3Key = env
	if p, altErr := alt.DecryptResource(nonce, ciphertext, aad); altErr == nil {
		return p, true, nil
	}
	return nil, false, err
}
