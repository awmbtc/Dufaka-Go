package pay

import (
	"strings"
	"testing"
)

func TestYipaySignStable(t *testing.T) {
	const key = "fixed-test-key"
	base := map[string]string{
		"pid":          "10001",
		"type":         "alipay",
		"out_trade_no": "D202001010001",
		"notify_url":   "https://shop.example/pay/yipay/notify_url",
		"return_url":   "https://shop.example/pay/yipay/return_url",
		"name":         "D202001010001",
		"money":        "12.34",
	}
	const baseSign = "4dad72bb232a5b39690faf710a0e4d99"
	tests := []struct {
		name   string
		params map[string]string
		key    string
		want   string
	}{
		{name: "create fields", params: base, key: key, want: baseSign},
		{
			name: "sign and sign_type ignored",
			params: map[string]string{
				"pid": base["pid"], "type": base["type"], "out_trade_no": base["out_trade_no"],
				"notify_url": base["notify_url"], "return_url": base["return_url"],
				"name": base["name"], "money": base["money"],
				"sign": "ignored", "sign_type": "MD5",
			},
			key: key, want: baseSign,
		},
		{
			name: "empty value ignored",
			params: map[string]string{
				"pid": base["pid"], "type": base["type"], "out_trade_no": base["out_trade_no"],
				"notify_url": base["notify_url"], "return_url": base["return_url"],
				"name": base["name"], "money": base["money"], "sitename": "",
			},
			key: key, want: baseSign,
		},
		{
			name: "notify fields",
			params: map[string]string{
				"pid": base["pid"], "type": base["type"], "out_trade_no": base["out_trade_no"],
				"notify_url": base["notify_url"], "return_url": base["return_url"],
				"name": base["name"], "money": base["money"],
				"trade_no": "T100", "trade_status": "TRADE_SUCCESS",
			},
			key: key, want: "2889c02d0a638e442c88c823dba39ab4",
		},
		{
			name: "qqpay other amount",
			params: map[string]string{
				"pid": "42", "type": "qqpay", "out_trade_no": "SN2",
				"notify_url": "https://shop.example/n", "return_url": "https://shop.example/r",
				"name": "QQ", "money": "0.01",
			},
			key: "another-key", want: "59137014303a96f606c98ca1c6c84293",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := YipaySign(tc.params, tc.key)
			again := YipaySign(tc.params, tc.key)
			if got != again {
				t.Fatal("sign changed between calls")
			}
			if got != tc.want {
				t.Fatalf("sign mismatch")
			}
			signed := copyParams(tc.params)
			signed["sign"] = got
			signed["sign_type"] = "MD5"
			if !YipayVerify(signed, tc.key) {
				t.Fatal("verify rejected a stable sign")
			}
			if YipayVerify(signed, tc.key+"x") {
				t.Fatal("verify accepted a different key")
			}
			upper := copyParams(signed)
			upper["sign"] = strings.ToUpper(got)
			if YipayVerify(upper, tc.key) {
				t.Fatal("verify accepted uppercase sign")
			}
		})
	}
}

func TestYipayFormMoney(t *testing.T) {
	const key = "fixed-test-key"
	tests := []struct {
		cents int64
		money string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{10, "0.10"},
		{100, "1.00"},
		{1050, "10.50"},
		{1234, "12.34"},
	}
	for _, tc := range tests {
		fields := YipayForm("10001", "alipay", "D202001010001", "https://shop.example/pay/yipay/notify_url", "https://shop.example/pay/yipay/return_url", "D202001010001", tc.cents, key)
		if fields["money"] != tc.money || fields["sign_type"] != "MD5" {
			t.Fatalf("cents %d formatted incorrectly", tc.cents)
		}
		if strings.Contains(fields["sign"], key) || fields["pid"] == "" {
			t.Fatal("form leaked or dropped a field")
		}
		for _, v := range fields {
			if strings.Contains(v, key) {
				t.Fatal("form contains the signing key")
			}
		}
		if !YipayVerify(fields, key) {
			t.Fatal("form did not verify")
		}
	}
	fields := YipayForm("10001", "alipay", "D202001010001", "https://shop.example/pay/yipay/notify_url", "https://shop.example/pay/yipay/return_url", "D202001010001", 1234, key)
	if fields["sign"] != "4dad72bb232a5b39690faf710a0e4d99" {
		t.Fatal("form sign drifted from the fixed vector")
	}
	fields["money"] = "12.35"
	if YipayVerify(fields, key) {
		t.Fatal("tampered money verified")
	}
}

func TestMapayURL(t *testing.T) {
	const key = "mapay-key"
	got := MapayURL(
		"https://codepay.example/creat_order/?",
		"88", "SN1", "shop",
		"https://shop.example/detail-order-sn/SN1",
		"https://shop.example/pay/mapay/notify_url",
		"mzfb", 1050, key,
	)
	const want = "https://codepay.example/creat_order/?act=0&chart=utf-8&id=88&notify_url=https%3A%2F%2Fshop.example%2Fpay%2Fmapay%2Fnotify_url&outTime=120&page=1&param=shop&pay_id=SN1&pay_type=0&price=10.50&return_url=https%3A%2F%2Fshop.example%2Fdetail-order-sn%2FSN1&type=1&sign=28ef24734002f89118df64ec36b91c0c"
	if got != want {
		t.Fatal("mapay url mismatch")
	}
	if strings.Contains(got, key) {
		t.Fatal("mapay url contains the signing key")
	}
	if MapayURL("https://codepay.example/creat_order/?", "88", "SN1", "shop", "https://shop.example/detail-order-sn/SN1", "https://shop.example/pay/mapay/notify_url", "mwx", 1050, key) == got {
		t.Fatal("default payway did not change type")
	}
	notify := map[string]string{
		"pay_id": "SN1", "pay_no": "N1", "price": "10.50", "param": "shop",
	}
	notify["sign"] = MapaySign(notify, key)
	if !MapayVerify(notify, key) {
		t.Fatal("mapay notify did not verify")
	}
	notify["price"] = "10.51"
	if MapayVerify(notify, key) {
		t.Fatal("tampered mapay notify verified")
	}
}

func TestPayjsForm(t *testing.T) {
	const key = "yourkey"
	fields := PayjsForm("123456", "order_body", "123456789", "https://payjs.cn/notify", 1, key)
	if fields["total_fee"] != "1" || fields["sign"] != "185AE82DCCB084129AB16063889C5EE4" {
		t.Fatal("payjs sign mismatch")
	}
	if PayjsNativeURL != "https://payjs.cn/api/native" {
		t.Fatal("payjs endpoint changed")
	}
	if !PayjsVerify(fields, key) {
		t.Fatal("payjs form did not verify")
	}
	lower := copyParams(fields)
	lower["sign"] = strings.ToLower(fields["sign"])
	if PayjsVerify(lower, key) {
		t.Fatal("payjs accepted lowercase sign")
	}
	withZero := copyParams(fields)
	delete(withZero, "sign")
	withZero["attach"] = "0"
	if PayjsSign(withZero, key) != fields["sign"] {
		t.Fatal("payjs signed a zero value")
	}
	omitted := PayjsForm("123456", "order_body", "123456789", "", 0, key)
	if _, ok := omitted["notify_url"]; ok {
		t.Fatal("empty notify_url was posted")
	}
	if _, ok := omitted["total_fee"]; ok {
		t.Fatal("zero total_fee was posted")
	}
}

func TestPaysapiForm(t *testing.T) {
	const token = "paysapi-token"
	fields := PaysapiForm("9", token, "SN1", "a@b.c", "https://shop.example/pay/paysapi/notify_url", "https://shop.example/pay/paysapi/return_url", "pszfb", 1050)
	if fields["price"] != "10.50" || fields["istype"] != "1" || fields["uid"] != "9" {
		t.Fatal("paysapi fields mismatch")
	}
	if fields["key"] != "27a7fc542ed2ed8d6e407c7d6b99873c" {
		t.Fatal("paysapi sign mismatch")
	}
	for k, v := range fields {
		if k != "key" && strings.Contains(v, token) {
			t.Fatal("paysapi form contains the token")
		}
	}
	if PaysapiGateway != "https://pay.bearsoftware.net.cn/" {
		t.Fatal("paysapi gateway changed")
	}
	if PaysapiForm("9", token, "SN1", "a@b.c", "https://shop.example/pay/paysapi/notify_url", "https://shop.example/pay/paysapi/return_url", "pswx", 1050)["istype"] != "2" {
		t.Fatal("pswx type")
	}
	notify := map[string]string{
		"orderid": "SN1", "orderuid": "a@b.c", "paysapi_id": "P999",
		"price": "10.50", "realprice": "10.50",
		"key": "36549e2280ec2a7cd98c9f2319227291",
	}
	if !PaysapiVerify(notify, token) {
		t.Fatal("paysapi notify did not verify")
	}
	notify["realprice"] = "10.00"
	if PaysapiVerify(notify, token) {
		t.Fatal("tampered paysapi notify verified")
	}
}

func TestVpayURL(t *testing.T) {
	const key = "vpay-key"
	got := VpayURL(
		"https://vpay.example/api/",
		"202001011200001", "SN1",
		"https://shop.example/pay/vpay/return_url",
		"https://shop.example/pay/vpay/notify_url",
		"vzfb", 1050, key,
	)
	const want = "https://vpay.example/api/createOrder?payId=202001011200001&price=10.50&param=SN1&returnUrl=https%3A%2F%2Fshop.example%2Fpay%2Fvpay%2Freturn_url&notifyUrl=https%3A%2F%2Fshop.example%2Fpay%2Fvpay%2Fnotify_url&isHtml=1&type=2&sign=eb9966c5ac0b38dcc6f2eb9c182d1cbc"
	if got != want {
		t.Fatal("vpay url mismatch")
	}
	if strings.Contains(got, key) {
		t.Fatal("vpay url contains the signing key")
	}
	if !strings.Contains(VpayURL("https://vpay.example/api/", "202001011200001", "SN1", "https://shop.example/r", "https://shop.example/n", "vwx", 1050, key), "type=1&sign=") {
		t.Fatal("vwx type")
	}
	notify := map[string]string{
		"payId": "202001011200001", "param": "SN1", "type": "2",
		"price": "10.50", "reallyPrice": "10.00",
		"sign": "d8b635c8cbb70c82ff06cb72fb9617ff",
	}
	if !VpayVerify(notify, key) {
		t.Fatal("vpay notify did not verify")
	}
	if VpayVerify(notify, key+"x") {
		t.Fatal("vpay verify accepted a different key")
	}
}

func TestPHPURLEncode(t *testing.T) {
	if phpURLEncode("a b~c*") != "a+b%7Ec%2A" {
		t.Fatal("php urlencode mismatch")
	}
}

func copyParams(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
