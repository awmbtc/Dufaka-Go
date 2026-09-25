package pay

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
)

// A3-1: Validate is Missing plus usability: the private key and the platform key must
// parse and the APIv3 key must be exactly 32 bytes after trimming. Each problem is one
// short item; a ready config yields nothing.
func TestWechatConfigValidate(t *testing.T) {
	key, pub, priv := testKeyPair(t)
	ready := WechatConfig{
		AppID: "wx-app", MchID: "1900000000", SerialNo: "CERT01",
		APIv3Key: strings.Repeat("k", 32), PrivateKeyPEM: priv, PlatformKeyPEM: pub,
	}
	if v := ready.Validate(); len(v) != 0 {
		t.Fatalf("ready config: %v", v)
	}
	// PKCS#8 private key, base64-wrapped PEM, a platform certificate and a key padded with
	// whitespace are all usable.
	p8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	alt := ready
	alt.PrivateKeyPEM = base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8}))
	alt.PlatformKeyPEM = selfSignedCert(t, key, big.NewInt(42))
	alt.APIv3Key = " " + strings.Repeat("k", 32) + "\n"
	if v := alt.Validate(); len(v) != 0 {
		t.Fatalf("usable variants rejected: %v", v)
	}
	cases := []struct {
		name string
		edit func(*WechatConfig)
		want string
	}{
		{"platform key empty", func(c *WechatConfig) { c.PlatformKeyPEM = "" }, "平台公钥（WECHAT_PAY_PLATFORM_KEY）"},
		{"31-byte key", func(c *WechatConfig) { c.APIv3Key = strings.Repeat("k", 31) }, "微信支付 APIv3 密钥必须是 32 字节（ASCII）"},
		{"33-byte key", func(c *WechatConfig) { c.APIv3Key = strings.Repeat("k", 33) }, "微信支付 APIv3 密钥必须是 32 字节（ASCII）"},
		{"private key garbage", func(c *WechatConfig) { c.PrivateKeyPEM = "not a key" }, "商户私钥无法解析（后台 wescan 渠道的商户密钥）"},
		{"private key truncated", func(c *WechatConfig) { c.PrivateKeyPEM = priv[:len(priv)/2] }, "商户私钥无法解析（后台 wescan 渠道的商户密钥）"},
		{"private key is a public key", func(c *WechatConfig) { c.PrivateKeyPEM = pub }, "商户私钥无法解析（后台 wescan 渠道的商户密钥）"},
		{"platform key garbage", func(c *WechatConfig) { c.PlatformKeyPEM = "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----" }, "平台公钥无法解析（WECHAT_PAY_PLATFORM_KEY）"},
		{"platform key is the private key", func(c *WechatConfig) { c.PlatformKeyPEM = priv }, "平台公钥无法解析（WECHAT_PAY_PLATFORM_KEY）"},
		{"appid empty", func(c *WechatConfig) { c.AppID = " " }, "appid（WECHAT_PAY_APP_ID）"},
	}
	for _, c := range cases {
		cfg := ready
		c.edit(&cfg)
		v := cfg.Validate()
		if len(v) != 1 || v[0] != c.want {
			t.Fatalf("%s: %q, want [%q]", c.name, v, c.want)
		}
	}
	if v := (WechatConfig{}).Validate(); len(v) != 6 {
		t.Fatalf("empty config: %v", v)
	}
}
