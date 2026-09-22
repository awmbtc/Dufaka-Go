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
	"math"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Native creates a WeChat Pay API v3 Native order and returns the code URL.
// The shop page stays the original QR cashier. The merchant account is v3.
func Native(ctx context.Context, appid, mchid, serial, apiV3, privPEM, pubID, notify, sn, desc string, cents int64) (string, error) {
	if appid == "" {
		appid = os.Getenv("WECHAT_PAY_APP_ID")
	}
	if mchid == "" {
		mchid = os.Getenv("WECHAT_PAY_MCH_ID")
	}
	if serial == "" {
		serial = os.Getenv("WECHAT_PAY_CERT_SERIAL_NO")
	}
	if apiV3 == "" {
		apiV3 = os.Getenv("WECHAT_PAY_API_V3_KEY")
	}
	if privPEM == "" {
		privPEM = os.Getenv("WECHAT_PAY_PRIVATE_KEY")
	}
	if pubID == "" {
		pubID = os.Getenv("WECHAT_PAY_PUBLIC_KEY_ID")
	}
	key, err := loadKey(privPEM)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"appid": appid, "mchid": mchid, "description": desc, "out_trade_no": sn,
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.mch.weixin.qq.com"+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`,
		mchid, nonce, base64.StdEncoding.EncodeToString(sig), ts, serial))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Dufaka-Go/0.1")
	if pubID != "" {
		req.Header.Set("Wechatpay-Serial", pubID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
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
	_ = apiV3
	return out.CodeURL, nil
}

func loadKey(pemText string) (*rsa.PrivateKey, error) {
	if pemText == "" {
		return nil, fmt.Errorf("未配置微信商户私钥")
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		decoded, err := base64.StdEncoding.DecodeString(pemText)
		if err != nil {
			return nil, fmt.Errorf("商户私钥无法解析")
		}
		block, _ = pem.Decode(decoded)
	}
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

// DecryptResource opens a v3 notify resource. Unused fields stay out of logs.
// VerifyNotify checks the WeChat Pay platform signature on a callback body.
// A notice is rejected when the platform public key is missing or the signature is wrong.
func VerifyNotify(pubPEM, timestamp, nonce, body, signature string, now time.Time) error {
	if pubPEM == "" {
		pubPEM = os.Getenv("WECHAT_PAY_PLATFORM_KEY")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || math.Abs(float64(now.Unix()-ts)) > 300 {
		return fmt.Errorf("微信支付通知时间无效")
	}
	pub, err := loadPublic(pubPEM)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("微信支付通知签名无效")
	}
	sum := sha256.Sum256([]byte(timestamp + "\n" + nonce + "\n" + body + "\n"))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("微信支付通知签名无效")
	}
	return nil
}

func loadPublic(pemText string) (*rsa.PublicKey, error) {
	if pemText == "" {
		return nil, fmt.Errorf("未配置微信支付平台公钥")
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		decoded, err := base64.StdEncoding.DecodeString(pemText)
		if err != nil {
			return nil, fmt.Errorf("微信支付平台公钥无法解析")
		}
		block, _ = pem.Decode(decoded)
	}
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

func DecryptResource(apiV3, nonce, ciphertext, aad string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher([]byte(apiV3))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, []byte(nonce), raw, []byte(aad))
}
