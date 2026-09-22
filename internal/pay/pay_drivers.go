package pay

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

const (
	// PaysapiGateway is the paysapi form action.
	PaysapiGateway = "https://pay.bearsoftware.net.cn/"
	// PayjsNativeURL is the payjs native endpoint. Callers POST PayjsForm there.
	PayjsNativeURL = "https://payjs.cn/api/native"
)

// YipaySign is the yipay MD5: sorted non-empty k=v pairs, excluding sign and
// sign_type, with the key appended, as lowercase hex.
func YipaySign(params map[string]string, key string) string {
	raw := sortedKV(params, func(k, v string) bool {
		return k == "sign" || k == "sign_type" || v == ""
	})
	return md5hex(raw + key)
}

// YipayForm builds the yipay submit fields. cents is converted to money in yuan
// with two decimals. The form action is the channel merchant key; key is the
// merchant pem. sign_type is MD5.
func YipayForm(pid, payType, outTradeNo, notifyURL, returnURL, name string, cents int64, key string) map[string]string {
	fields := map[string]string{
		"pid":          pid,
		"type":         payType,
		"out_trade_no": outTradeNo,
		"notify_url":   notifyURL,
		"return_url":   returnURL,
		"name":         name,
		"money":        yuan(cents),
		"sign_type":    "MD5",
	}
	fields["sign"] = YipaySign(fields, key)
	return fields
}

// YipayVerify reports whether params carries the yipay sign for key.
func YipayVerify(params map[string]string, key string) bool {
	return signEq(params["sign"], YipaySign(params, key))
}

// MapaySign is the codepay MD5: sorted non-empty k=v pairs, excluding sign,
// with the key appended, as lowercase hex.
func MapaySign(params map[string]string, key string) string {
	raw := sortedKV(params, func(k, v string) bool {
		return k == "sign" || v == ""
	})
	return md5hex(raw + key)
}

// MapayURL appends the signed query to gateway, the channel merchant key.
// key is the channel merchant pem. payway is mqq, mzfb, or mwx.
// cents is the price in yuan with two decimals. sign is appended, not sorted in.
func MapayURL(gateway, id, payID, param, returnURL, notifyURL, payway string, cents int64, key string) string {
	fields := map[string]string{
		"id":         id,
		"price":      yuan(cents),
		"pay_id":     payID,
		"param":      param,
		"act":        "0",
		"outTime":    "120",
		"page":       "1",
		"return_url": returnURL,
		"notify_url": notifyURL,
		"pay_type":   "0",
		"chart":      "utf-8",
		"type":       mapayType(payway),
	}
	raw, query := sortedRawAndQuery(fields)
	sign := md5hex(raw + key)
	if query == "" {
		return gateway + "sign=" + sign
	}
	return gateway + query + "&sign=" + sign
}

// MapayVerify reports whether params carries the codepay sign for key.
func MapayVerify(params map[string]string, key string) bool {
	return signEq(params["sign"], MapaySign(params, key))
}

// PayjsSign is the payjs MD5: sorted k=v pairs, skipping sign, empty values,
// and "0" the way the original client filters them, then "&key=" and the key.
// The hex digest is uppercase. total_fee is fen.
func PayjsSign(params map[string]string, key string) string {
	raw := sortedKV(params, func(k, v string) bool {
		return k == "sign" || v == "" || v == "0"
	})
	return strings.ToUpper(md5hex(raw + "&key=" + key))
}

// PayjsForm builds the native POST fields for PayjsNativeURL. cents is
// total_fee in fen, the same integer the order stores. key is the merchant pem.
func PayjsForm(mchid, body, outTradeNo, notifyURL string, cents int64, key string) map[string]string {
	src := map[string]string{
		"mchid":        mchid,
		"body":         body,
		"total_fee":    strconv.FormatInt(cents, 10),
		"out_trade_no": outTradeNo,
		"notify_url":   notifyURL,
	}
	fields := make(map[string]string, len(src)+1)
	for k, v := range src {
		if v == "" || v == "0" {
			continue
		}
		fields[k] = v
	}
	fields["sign"] = PayjsSign(fields, key)
	return fields
}

// PayjsVerify reports whether params carries the payjs sign for key.
func PayjsVerify(params map[string]string, key string) bool {
	return signEq(params["sign"], PayjsSign(params, key))
}

// PaysapiSign is md5(goodsName + isType + notifyURL + orderID + orderUID + price + returnURL + token + uid).
func PaysapiSign(goodsName, isType, notifyURL, orderID, orderUID, price, returnURL, token, uid string) string {
	return md5hex(goodsName + isType + notifyURL + orderID + orderUID + price + returnURL + token + uid)
}

// PaysapiNotifySign is md5(orderID + orderUID + paysapiID + price + realPrice + token).
func PaysapiNotifySign(orderID, orderUID, paysapiID, price, realPrice, token string) string {
	return md5hex(orderID + orderUID + paysapiID + price + realPrice + token)
}

// PaysapiForm builds the POST fields for PaysapiGateway. uid is the merchant id
// and token is the merchant pem. payway is pszfb or pswx. cents is price in
// yuan with two decimals. The signature is the field named key.
func PaysapiForm(uid, token, orderID, orderUID, notifyURL, returnURL, payway string, cents int64) map[string]string {
	isType := paysapiType(payway)
	price := yuan(cents)
	goodsName := orderID
	return map[string]string{
		"goodsname":  goodsName,
		"istype":     isType,
		"key":        PaysapiSign(goodsName, isType, notifyURL, orderID, orderUID, price, returnURL, token, uid),
		"notify_url": notifyURL,
		"orderid":    orderID,
		"orderuid":   orderUID,
		"price":      price,
		"return_url": returnURL,
		"uid":        uid,
	}
}

// PaysapiVerify reports whether params["key"] matches the paysapi notify sign.
func PaysapiVerify(params map[string]string, key string) bool {
	want := PaysapiNotifySign(params["orderid"], params["orderuid"], params["paysapi_id"], params["price"], params["realprice"], key)
	return signEq(params["key"], want)
}

// VpayCreateSign is md5(payID + param + payType + price + key), with no separators.
func VpayCreateSign(payID, param, payType, price, key string) string {
	return md5hex(payID + param + payType + price + key)
}

// VpayNotifySign is md5(payID + param + payType + price + reallyPrice + key).
func VpayNotifySign(payID, param, payType, price, reallyPrice, key string) string {
	return md5hex(payID + param + payType + price + reallyPrice + key)
}

// VpayURL appends "createOrder?" to gateway, the channel merchant pem.
// key is the channel merchant id. payway is vzfb or vwx. cents is price in
// yuan with two decimals. The original driver does not insert a missing slash.
func VpayURL(gateway, payID, param, returnURL, notifyURL, payway string, cents int64, key string) string {
	price := yuan(cents)
	payType := vpayType(payway)
	sign := VpayCreateSign(payID, param, payType, price, key)
	query := phpQuery([][2]string{
		{"payId", payID},
		{"price", price},
		{"param", param},
		{"returnUrl", returnURL},
		{"notifyUrl", notifyURL},
		{"isHtml", "1"},
		{"type", payType},
		{"sign", sign},
	})
	return gateway + "createOrder?" + query
}

// VpayVerify reports whether params carries the vpay notify sign for key.
func VpayVerify(params map[string]string, key string) bool {
	want := VpayNotifySign(params["payId"], params["param"], params["type"], params["price"], params["reallyPrice"], key)
	return signEq(params["sign"], want)
}

func mapayType(payway string) string {
	switch payway {
	case "mzfb":
		return "1"
	case "mqq":
		return "2"
	default:
		return "3"
	}
}

func paysapiType(payway string) string {
	if payway == "pszfb" {
		return "1"
	}
	return "2"
}

func vpayType(payway string) string {
	if payway == "vzfb" {
		return "2"
	}
	return "1"
}

func yuan(cents int64) string {
	neg := false
	if cents < 0 {
		neg = true
		cents = -cents
	}
	frac := cents % 100
	s := strconv.FormatInt(cents/100, 10) + "." + strconv.FormatInt(frac/10, 10) + strconv.FormatInt(frac%10, 10)
	if neg {
		return "-" + s
	}
	return s
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func signEq(got, want string) bool {
	if got == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func sortedKV(params map[string]string, skip func(k, v string) bool) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if skip(k, v) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}

func sortedRawAndQuery(params map[string]string) (raw, query string) {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var rawB, queryB strings.Builder
	for i, k := range keys {
		if i > 0 {
			rawB.WriteByte('&')
			queryB.WriteByte('&')
		}
		v := params[k]
		rawB.WriteString(k)
		rawB.WriteByte('=')
		rawB.WriteString(v)
		queryB.WriteString(phpURLEncode(k))
		queryB.WriteByte('=')
		queryB.WriteString(phpURLEncode(v))
	}
	return rawB.String(), queryB.String()
}

func phpQuery(pairs [][2]string) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(phpURLEncode(p[0]))
		b.WriteByte('=')
		b.WriteString(phpURLEncode(p[1]))
	}
	return b.String()
}

// phpURLEncode matches PHP urlencode: space is +, and ~ is percent-encoded.
func phpURLEncode(s string) string {
	const hexdigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
			continue
		}
		if c == ' ' {
			b.WriteByte('+')
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexdigits[c>>4])
		b.WriteByte(hexdigits[c&0x0F])
	}
	return b.String()
}
