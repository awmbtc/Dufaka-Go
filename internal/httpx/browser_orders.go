package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

func orderCookie(payload string) string {
	key := os.Getenv("DUFAKA_SESSION_KEY")
	if key == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("browser-orders:" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func browserOrders(value string) []string {
	payload, _, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(value), []byte(orderCookie(payload))) {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var sns []string
	if json.Unmarshal(raw, &sns) != nil || len(sns) > 40 {
		return nil
	}
	return sns
}

func ownsOrder(r *http.Request, sn string) bool {
	c, err := r.Cookie("dujiaoka_orders")
	if err != nil {
		return false
	}
	for _, item := range browserOrders(c.Value) {
		if item == sn {
			return true
		}
	}
	return false
}
