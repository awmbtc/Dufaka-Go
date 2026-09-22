package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestParseCards(t *testing.T) {
	got, err := parseCards("a\nb\n\na\r\n c ", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("dedupe: %#v", got)
	}
	got, err = parseCards("a\na\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "a|a" {
		t.Fatalf("keep: %#v", got)
	}
	if _, err := parseCards(" \n\n", true); err == nil {
		t.Fatal("empty should fail")
	}
}

func TestSessionHMAC(t *testing.T) {
	if cookieName != "dufaka_admin" {
		t.Fatalf("cookie %s", cookieName)
	}
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	tok := signSession(7, "店主", time.Now().Add(time.Hour))
	got, ok := verifySession(tok, time.Now())
	if !ok || got.UID != 7 || got.Name != "店主" {
		t.Fatalf("session %+v ok=%v", got, ok)
	}
	if _, ok := verifySession(tok+"x", time.Now()); ok {
		t.Fatal("tampered token accepted")
	}
	expired := signSession(7, "店主", time.Now().Add(-time.Second))
	if _, ok := verifySession(expired, time.Now()); ok {
		t.Fatal("expired token accepted")
	}
	t.Setenv("DUFAKA_SESSION_KEY", "other-key")
	if _, ok := verifySession(tok, time.Now()); ok {
		t.Fatal("token accepted under a different key")
	}
}

func TestPasswordCheckUsesBcrypt(t *testing.T) {
	raw, err := bcrypt.GenerateFromPassword([]byte("secret12"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	hash := string(raw)
	if !passwordOK(hash, "secret12") {
		t.Fatal("expected match")
	}
	if passwordOK(hash, "wrong") || passwordOK("plain", "plain") {
		t.Fatal("plaintext or wrong password accepted")
	}
}

func TestReadGoodForm(t *testing.T) {
	body := url.Values{
		"group_id":            {"2"},
		"gd_name":             {"测试卡"},
		"gd_description":      {"描述"},
		"gd_keywords":         {"关键词"},
		"retail_price":        {"10"},
		"actual_price":        {"8.5"},
		"in_stock":            {"3"},
		"sales_volume":        {"1"},
		"ord":                 {"4"},
		"buy_limit_num":       {"2"},
		"type":                {"1"},
		"wholesale_price_cnf": {"5=3"},
		"other_ipu_cnf":       {"qq=QQ账号=true"},
		"is_open":             {"1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/goods", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	f, err := readGoodForm(req)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "测试卡" || f.Type != 1 || f.BuyLimit != 2 || f.Open != 1 || f.InStock != 3 || f.Actual != "8.50" || f.Wholesale != "5=3" || f.Other != "qq=QQ账号=true" {
		t.Fatalf("%+v", f)
	}
	req.Form.Set("type", "9")
	req.PostForm.Set("type", "9")
	if _, err := readGoodForm(req); err == nil {
		t.Fatal("type 9 should be rejected")
	}
}

func TestApplySettings(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/settings", nil)
	req.Form = url.Values{
		"title":              {"示例店"},
		"template":           {"hyper"},
		"language":           {"zh_CN"},
		"order_expire_time":  {"5"},
		"driver":             {"smtp"},
		"host":               {"smtp.example.com"},
		"is_open_search_pwd": {"1"},
		"is_open_geetest":    {"0"},
	}
	got, err := applySettings(map[string]string{"password": "kept"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if got["title"] != "示例店" || got["template"] != "hyper" || got["host"] != "smtp.example.com" || got["password"] != "kept" || got["is_open_search_pwd"] != "1" || got["is_open_geetest"] != "0" {
		t.Fatalf("%v", got)
	}
	req.Form.Set("title", "")
	if _, err := applySettings(nil, req); err == nil {
		t.Fatal("missing title")
	}
}

func TestPagesContainOriginalFields(t *testing.T) {
	var buf strings.Builder
	err := pages.ExecuteTemplate(&buf, "goods_form", goodsFormPage{
		View:   View{Title: "新增商品", User: "管理员", Nav: "goods"},
		Form:   goodForm{Type: 1, Open: 1, Ord: 1, Retail: "0.00", Actual: "0.00"},
		Action: "/admin/goods",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, field := range []string{"gd_name", "retail_price", "actual_price", "in_stock", "wholesale_price_cnf", "other_ipu_cnf", "buy_limit_num", "is_open", "1 自动发货", "2 人工处理"} {
		if !strings.Contains(body, field) {
			t.Errorf("goods form missing %s", field)
		}
	}
	if strings.Contains(body, "AI") {
		t.Fatal("goods form contains AI")
	}

	buf.Reset()
	err = pages.ExecuteTemplate(&buf, "settings", settingsPage{View: View{Title: "系统设置", User: "管理员", Nav: "settings"}, Tabs: settingTabs(nil)})
	if err != nil {
		t.Fatal(err)
	}
	body = buf.String()
	for _, field := range []string{"基本设置", "订单推送配置", "邮件服务", "极验验证", `name="title"`, `name="template"`, `name="order_expire_time"`, `name="is_open_search_pwd"`, `name="is_open_geetest"`, `name="driver"`, `name="host"`} {
		if !strings.Contains(body, field) {
			t.Errorf("settings missing %s", field)
		}
	}

	buf.Reset()
	err = pages.ExecuteTemplate(&buf, "carmis_import", carmiImportPage{View: View{Title: "导入卡密", User: "管理员", Nav: "import"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `name="carmis_list"`) || !strings.Contains(buf.String(), "一行一个") {
		t.Fatalf("import form: %s", buf.String())
	}

	buf.Reset()
	err = pages.ExecuteTemplate(&buf, "orders", ordersPage{
		View: View{Title: "订单列表", User: "管理员", Nav: "orders"},
		Rows: []orderRow{{ID: 1, SN: "AB12", Pwd: "secret-pwd", Title: "卡", Email: "a@b.c", Actual: "1.00", Status: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "查询密码") || !strings.Contains(buf.String(), `data-copy="secret-pwd"`) {
		t.Fatal("order search password is not copyable")
	}

	buf.Reset()
	err = pages.ExecuteTemplate(&buf, "pays", paysPage{
		View:   View{Title: "支付通道", User: "管理员", Nav: "pays"},
		Form:   payForm{Method: 1, Client: 3, Open: 1},
		Action: "/admin/pays",
		Rows:   []payRow{{ID: 1, Name: "微信扫码", Check: "wescan", Method: 2, Client: 3, MerchantID: "m", Key: "secret-key", Pem: "secret-pem", Route: "/pay/wepay", Open: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body = buf.String()
	for _, field := range []string{"pay_name", "pay_check", "pay_method", "pay_client", "merchant_id", "merchant_key", "merchant_pem", "pay_handleroute", "is_open", "跳转", "扫码", "通用"} {
		if !strings.Contains(body, field) {
			t.Errorf("pays missing %s", field)
		}
	}
}

func TestRoutesDoNotRequireDatabase(t *testing.T) {
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	mux := http.NewServeMux()
	Mount(mux, nil)

	login := httptest.NewRecorder()
	mux.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	if login.Code != http.StatusOK || !strings.Contains(login.Body.String(), "登录") {
		t.Fatalf("login %d %s", login.Code, login.Body.String())
	}

	home := httptest.NewRecorder()
	mux.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if home.Code != http.StatusFound || home.Header().Get("Location") != "/admin/login" {
		t.Fatalf("home redirect %d %s", home.Code, home.Header().Get("Location"))
	}

	for _, path := range []string{"/admin/goods", "/admin/goods/create", "/admin/groups", "/admin/carmis/import", "/admin/coupons", "/admin/orders", "/admin/pays", "/admin/settings", "/admin/emailtpls"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("%s -> %d", path, rec.Code)
		}
	}

	bad := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/goods", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "tampered"})
	mux.ServeHTTP(bad, req)
	if bad.Code != http.StatusFound {
		t.Fatalf("tampered cookie %d", bad.Code)
	}

	authed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: signSession(1, "管理员", time.Now().Add(time.Hour))})
	mux.ServeHTTP(authed, req)
	if authed.Code != http.StatusServiceUnavailable || !strings.Contains(authed.Body.String(), "数据库未连接") {
		t.Fatalf("nil pool %d %s", authed.Code, authed.Body.String())
	}

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, httptest.NewRequest(http.MethodPost, "/admin/logout", nil))
	if out.Code != http.StatusFound {
		t.Fatalf("logout %d", out.Code)
	}
	if ck := out.Header().Get("Set-Cookie"); !strings.Contains(ck, cookieName) {
		t.Fatalf("logout cookie %s", ck)
	}
}
