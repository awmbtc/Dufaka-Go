package pay

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
	"time"
)

func TestVerifyNotify(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	body := `{"id":"1"}`
	ts := "1700000000"
	nonce := "nonce"
	sum := sha256.Sum256([]byte(ts + "\n" + nonce + "\n" + body + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	sign := base64.StdEncoding.EncodeToString(sig)
	if err := VerifyNotify(pub, ts, nonce, body, sign, now); err != nil {
		t.Fatal(err)
	}
	if err := VerifyNotify(pub, ts, nonce, body, sign, now.Add(10*time.Minute)); err == nil {
		t.Fatal("stale notice accepted")
	}
	if err := VerifyNotify("", ts, nonce, body, sign, now); err == nil {
		t.Fatal("missing platform key accepted")
	}
}
