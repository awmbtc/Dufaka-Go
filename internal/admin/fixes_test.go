package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"dufaka/internal/store"
	"dufaka/internal/testdb"
)

func formReq(t *testing.T, method, path string, v url.Values) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return req
}

func payValues(check, merchant, pem, open string) url.Values {
	return url.Values{
		"pay_name": {"通道"}, "pay_check": {check}, "pay_method": {"2"}, "pay_client": {"3"},
		"merchant_id": {merchant}, "merchant_key": {""}, "merchant_pem": {pem},
		"pay_handleroute": {"/pay/" + check}, "is_open": {open},
	}
}

// #7 + #19: cldx may be saved with empty merchant fields; other channels still
// need them; only wescan/cldx may be enabled.
func TestReadPayCldxAndCashierGate(t *testing.T) {
	if _, err := readPay(formReq(t, "POST", "/admin/pays/1", payValues("cldx", "", "", "0"))); err != nil {
		t.Fatalf("cldx with empty merchant fields: %v", err)
	}
	if _, err := readPay(formReq(t, "POST", "/admin/pays/1", payValues("cldx", "", "", "1"))); err != nil {
		t.Fatalf("cldx enabled: %v", err)
	}
	if _, err := readPay(formReq(t, "POST", "/admin/pays", payValues("wescan", "m", "", "1"))); err == nil || err.Error() != "请填写商户密钥" {
		t.Fatalf("wescan with empty pem: %v", err)
	}
	if _, err := readPay(formReq(t, "POST", "/admin/pays", payValues("wescan", "", "pem", "1"))); err == nil || err.Error() != "请填写商户 ID" {
		t.Fatalf("wescan with empty merchant id: %v", err)
	}
	if _, err := readPay(formReq(t, "POST", "/admin/pays", payValues("wescan", "m", "pem", "1"))); err != nil {
		t.Fatalf("wescan complete: %v", err)
	}
	if _, err := readPay(formReq(t, "POST", "/admin/pays", payValues("alipay", "m", "pem", "1"))); err == nil || err.Error() != payNotReady {
		t.Fatalf("alipay enabled must be rejected: %v", err)
	}
	if _, err := readPay(formReq(t, "POST", "/admin/pays", payValues("alipay", "m", "pem", "0"))); err != nil {
		t.Fatalf("alipay disabled: %v", err)
	}
}

func TestPaysListMarksUnimplemented(t *testing.T) {
	var buf strings.Builder
	err := pages.ExecuteTemplate(&buf, "pays", paysPage{
		View: View{Title: "支付通道", User: "管理员", Nav: "pays"}, Form: payForm{Method: 1, Client: 3}, Action: "/admin/pays",
		Rows: []payRow{
			{ID: 1, Name: "微信", Check: "wescan", Open: 1}, {ID: 2, Name: "cldx", Check: "cldx", Open: 1},
			{ID: 3, Name: "支付宝", Check: "alipay", Open: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if n := strings.Count(body, ">未接通</em>"); n != 1 {
		t.Fatalf("expected exactly one 未接通 tag (alipay), got %d:\n%s", n, body)
	}
	if strings.Index(body, "alipay") > strings.Index(body, ">未接通</em>") {
		t.Fatal("tag is not next to the unimplemented channel")
	}
	for _, s := range []string{`name="merchant_id" value="" maxlength="200">`, `name="merchant_pem">`} {
		if !strings.Contains(body, s) {
			t.Fatalf("merchant fields must not be required: missing %q", s)
		}
	}
}

func TestPaySaveSeededCldxIntegration(t *testing.T) {
	pool := testdb.Open(t)
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	ctx := context.Background()
	var id int
	err := pool.QueryRow(ctx, `INSERT INTO pays (pay_name, pay_check, pay_method, pay_client, merchant_id, merchant_key, merchant_pem, pay_handleroute, is_open, created_at, updated_at)
		VALUES ('cldx','cldx',2,3,'','','','/pay/cldx',1,now(),now()) RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, func() *pgxpool.Pool { return pool })
	u := session{UID: 1, Exp: time.Now().Add(time.Hour).Unix(), Name: "店主"}
	cookie := &http.Cookie{Name: cookieName, Value: signSession(u.UID, u.Name, time.Unix(u.Exp, 0))}

	// Disable the seeded row without inventing merchant values.
	v := payValues("cldx", "", "", "0")
	v.Set("_token", csrfToken(u))
	req := formReq(t, "POST", "/admin/pays/"+strconv.Itoa(id), v)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), url.QueryEscape("保存成功")) {
		t.Fatalf("cldx save %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	var open int
	if err := pool.QueryRow(ctx, `SELECT is_open FROM pays WHERE id=$1`, id).Scan(&open); err != nil || open != 0 {
		t.Fatalf("is_open=%d err=%v", open, err)
	}

	// wescan with an empty pem is still refused.
	v = payValues("wescan", "m", "", "1")
	v.Set("_token", csrfToken(u))
	req = formReq(t, "POST", "/admin/pays", v)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "请填写商户密钥") {
		t.Fatalf("wescan empty pem %d %s", rec.Code, rec.Body.String())
	}

	// An unimplemented channel cannot be enabled.
	v = payValues("alipay", "m", "pem", "1")
	v.Set("_token", csrfToken(u))
	req = formReq(t, "POST", "/admin/pays", v)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), payNotReady) {
		t.Fatalf("alipay enable %d %s", rec.Code, rec.Body.String())
	}
}

// #8: unwired switches are disabled in the UI and applySettings forces both
// off on every save, whatever the form or the stored row says (a stored
// geetest=1 rejects every order and the disabled control could not clear it).
func TestApplySettingsUnwiredSwitches(t *testing.T) {
	base := url.Values{
		"title": {"店"}, "template": {"unicorn"}, "language": {"zh_CN"}, "order_expire_time": {"5"}, "driver": {"smtp"},
		"is_open_img_code": {"1"}, "is_open_geetest": {"1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/settings", nil)
	req.Form = base
	got, err := applySettings(map[string]string{"is_open_geetest": "0"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if got["is_open_img_code"] != "0" || got["is_open_geetest"] != "0" {
		t.Fatalf("form values must not switch them on: img=%q geetest=%q", got["is_open_img_code"], got["is_open_geetest"])
	}
	got, err = applySettings(map[string]string{"is_open_geetest": "1", "is_open_img_code": "1"}, req)
	if err != nil || got["is_open_geetest"] != "0" || got["is_open_img_code"] != "0" {
		t.Fatalf("stored geetest=1 must be cleared on save: %v %v", got, err)
	}
	req.Form.Del("is_open_geetest")
	got, err = applySettings(map[string]string{"is_open_geetest": "1"}, req)
	if err != nil || got["is_open_geetest"] != "0" {
		t.Fatalf("a form that omits the disabled control must still clear it: %v %v", got, err)
	}

	var buf strings.Builder
	if err := pages.ExecuteTemplate(&buf, "settings", settingsPage{View: View{Title: "系统设置", User: "管理员", Nav: "settings"}, Tabs: settingTabs(map[string]string{"is_open_geetest": "1"})}); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, s := range []string{
		`<select id="set-is_open_img_code" name="is_open_img_code" disabled`,
		`<select id="set-is_open_geetest" name="is_open_geetest" disabled`,
		helpImgCode, helpGeetest, "保存设置时会清零",
	} {
		if !strings.Contains(body, s) {
			t.Errorf("settings page missing %q", s)
		}
	}
	if !strings.Contains(body, `name="is_open_search_pwd">`) {
		t.Error("wired switches must stay enabled")
	}
}

// #20: the goods list counts only cards the storefront could still sell.
func TestGoodsListStockExcludesReservedIntegration(t *testing.T) {
	pool := testdb.Open(t)
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	_, err := pool.Exec(context.Background(), `INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type,ord,sales_volume) VALUES(7,1,'card','d','k',10,1,5,0);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status) VALUES(1,'SN1',7,'card','a@example.com','local',1);
 INSERT INTO carmis(goods_id,carmi) VALUES(7,'free');
 INSERT INTO carmis(goods_id,carmi,reserved_order_id) VALUES(7,'held',1);
 INSERT INTO carmis(goods_id,carmi,deleted_at) VALUES(7,'gone',now());
 INSERT INTO carmis(goods_id,carmi,status) VALUES(7,'sold',2);`)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.goodsList(w, httptest.NewRequest("GET", "/admin/goods", nil), session{Name: "audit"})
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "<td>1</td>") || strings.Contains(body, "<td>2</td>") || strings.Contains(body, "<td>3</td>") {
		t.Fatalf("stock must be 1 (only the free card): %d\n%s", w.Code, body)
	}
}

// #11: 重新发货 for status-6 orders.
func TestOrderRedeliverIntegration(t *testing.T) {
	pool := testdb.Open(t)
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type) VALUES(1,1,'card','','',10,1);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,buy_amount,actual_price,trade_no,info) VALUES(1,'SHORT',1,'card','a@example.com','local',6,1,1,10,'wx-1','库存不足');
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,buy_amount,actual_price) VALUES(2,'UNPAID',1,'card','a@example.com','local',1,1,1,10);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,buy_amount,actual_price,trade_no) VALUES(3,'MANUAL',1,'card','a@example.com','local',6,2,1,10,'wx-3');`)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, func() *pgxpool.Pool { return pool })
	u := session{UID: 1, Exp: time.Now().Add(time.Hour).Unix(), Name: "店主"}
	cookie := &http.Cookie{Name: cookieName, Value: signSession(u.UID, u.Name, time.Unix(u.Exp, 0))}
	post := func(path string) *httptest.ResponseRecorder {
		req := formReq(t, "POST", path, url.Values{"_token": {csrfToken(u)}})
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Still no stock: RuleError message shown, order untouched.
	rec := post("/admin/orders/1/redeliver")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "库存不足") {
		t.Fatalf("short stock %d %s", rec.Code, rec.Body.String())
	}
	var status int
	pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=1`).Scan(&status)
	if status != 6 {
		t.Fatalf("status changed to %d without delivery", status)
	}

	// Not a status-6 order.
	rec = post("/admin/orders/2/redeliver")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "才能重新发货") {
		t.Fatalf("unpaid order %d %s", rec.Code, rec.Body.String())
	}

	// A 人工处理 (type 2) order in status 6 has no cards to send: refused
	// with the rule message before the delivery path is even called.
	calls := 0
	oldRedeliver := redeliver
	redeliver = func(ctx context.Context, pool *pgxpool.Pool, sn string) (bool, error) {
		calls++
		return oldRedeliver(ctx, pool, sn)
	}
	rec = post("/admin/orders/3/redeliver")
	redeliver = oldRedeliver
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), redeliverManualOnly) || calls != 0 {
		t.Fatalf("type-2 order %d calls=%d %s", rec.Code, calls, rec.Body.String())
	}

	// Detail page shows the action only for status 6 + type 1.
	get := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if body := get("/admin/orders/1"); !strings.Contains(body, `action="/admin/orders/1/redeliver"`) || !strings.Contains(body, "重新发货") {
		t.Fatal("detail page of a status-6 order lacks the 重新发货 form")
	}
	if body := get("/admin/orders/2"); strings.Contains(body, "/redeliver") {
		t.Fatal("unpaid order must not offer 重新发货")
	}
	if body := get("/admin/orders/3"); strings.Contains(body, "/redeliver") {
		t.Fatal("人工处理 order must not offer 重新发货")
	}

	// Restock, then redeliver succeeds.
	if _, err := pool.Exec(ctx, `INSERT INTO carmis(id,goods_id,carmi) VALUES(1,1,'CODE-1')`); err != nil {
		t.Fatal(err)
	}
	rec = post("/admin/orders/1/redeliver")
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), url.QueryEscape("已重新发货")) {
		t.Fatalf("redeliver %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	var info, trade string
	var cardStatus, sales int
	pool.QueryRow(ctx, `SELECT status, info, COALESCE(trade_no,'') FROM orders WHERE id=1`).Scan(&status, &info, &trade)
	pool.QueryRow(ctx, `SELECT status FROM carmis WHERE id=1`).Scan(&cardStatus)
	pool.QueryRow(ctx, `SELECT COALESCE(sales_volume,0) FROM goods WHERE id=1`).Scan(&sales)
	if status != 4 || info != "CODE-1" || trade != "wx-1" || cardStatus != 2 || sales != 1 {
		t.Fatalf("after redeliver: status=%d info=%q trade=%q card=%d sales=%d", status, info, trade, cardStatus, sales)
	}

	// Unexpected errors from the delivery path surface as dbErr.
	old := redeliver
	redeliver = func(context.Context, *pgxpool.Pool, string) (bool, error) { return false, errors.New("boom") }
	defer func() { redeliver = old }()
	pool.Exec(ctx, `UPDATE orders SET status=6 WHERE id=1`)
	rec = post("/admin/orders/1/redeliver")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("plain error %d %s", rec.Code, rec.Body.String())
	}
	var rule store.RuleError
	if !errors.As(error(store.RuleError{Msg: "x"}), &rule) {
		t.Fatal("RuleError must be matchable with errors.As")
	}
}

// #14a: unknown usernames still pay for one bcrypt comparison. The check is
// structural (dummy hash cost + a counted compare func), not a wall-clock one.
func TestUnknownUserStillRunsBcrypt(t *testing.T) {
	if cost, err := bcrypt.Cost([]byte(dummyHash)); err != nil || cost != bcrypt.DefaultCost {
		t.Fatalf("dummyHash cost=%d err=%v, want %d", cost, err, bcrypt.DefaultCost)
	}
	var calls int
	var lastHash string
	old := compareHash
	compareHash = func(hash, pw []byte) error {
		calls++
		lastHash = string(hash)
		return old(hash, pw)
	}
	defer func() { compareHash = old }()
	if checkLogin(false, "", "whatever") {
		t.Fatal("unknown user accepted")
	}
	if calls != 1 || lastHash != dummyHash {
		t.Fatalf("unknown-user path ran %d compares against %q; want one against dummyHash", calls, lastHash)
	}
	if checkLogin(true, "", "whatever") || calls != 2 || lastHash != dummyHash {
		t.Fatalf("empty stored hash must still cost one dummy compare: calls=%d", calls)
	}
	raw, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if !checkLogin(true, string(raw), "pw") || checkLogin(true, string(raw), "no") {
		t.Fatal("real comparison broken")
	}
	if calls != 4 || lastHash != string(raw) {
		t.Fatalf("real path must compare against the stored hash exactly once each: calls=%d", calls)
	}
}

// #14b: limiter semantics with a fake clock. reserve takes an address
// attempt up front (hard, 5 per window); username keys only record failures
// and turn "slow" at 20, they never refuse. (Round 3 replaced the combined
// allow(ip, user) with reserve/fail/slow.)
func TestLoginLimiter(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	l := &loginLimiter{now: func() time.Time { return now }}
	for i := 0; i < loginMaxFails; i++ {
		if !l.reserve("ip:1.2.3.4") {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if l.reserve("ip:1.2.3.4") {
		t.Fatal("6th attempt from the same IP must be refused")
	}
	if !l.blocked("ip:1.2.3.4") {
		t.Fatal("the IP key must be blocked")
	}
	if !l.reserve("ip:9.9.9.9") {
		t.Fatal("unrelated address refused")
	}
	now = now.Add(loginBlock - time.Second)
	if l.reserve("ip:1.2.3.4") {
		t.Fatal("block lifted too early")
	}
	now = now.Add(2 * time.Second)
	if !l.reserve("ip:1.2.3.4") {
		t.Fatal("block must expire after 15 minutes")
	}
	// Attempts spread over more than the window do not accumulate.
	l = &loginLimiter{now: func() time.Time { return now }}
	for i := 0; i < loginMaxFails; i++ {
		l.reserve("ip:slow")
	}
	now = now.Add(loginWindow + time.Second)
	if !l.reserve("ip:slow") {
		t.Fatal("old attempts outside the window must not count")
	}
	// A successful login clears the reservations; release hands one back.
	l = &loginLimiter{now: func() time.Time { return now }}
	for i := 0; i < loginMaxFails; i++ {
		l.reserve("ip:ok")
	}
	l.release("ip:ok")
	if !l.reserve("ip:ok") || l.reserve("ip:ok") {
		t.Fatal("release must hand back exactly one attempt")
	}
	l.clear("ip:ok")
	for i := 0; i < loginMaxFails; i++ {
		if !l.reserve("ip:ok") {
			t.Fatalf("counter not cleared on success (attempt %d)", i+1)
		}
	}
	// Username soft limit: failures only, slow from the 20th on, never blocked.
	l = &loginLimiter{now: func() time.Time { return now }}
	for i := 0; i < loginUserMaxFails-1; i++ {
		l.fail("user:owner")
	}
	if l.slow("user:owner") {
		t.Fatal("19 failures must not slow the account down")
	}
	l.fail("user:owner")
	if !l.slow("user:owner") || l.blocked("user:owner") {
		t.Fatal("20 failures slow the account down but never block it")
	}
	now = now.Add(loginWindow + time.Second)
	if l.slow("user:owner") {
		t.Fatal("the soft limit must expire with the window")
	}
}

func TestLoginRateLimitIntegration(t *testing.T) {
	pool := testdb.Open(t)
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	raw, _ := bcrypt.GenerateFromPassword([]byte("right-pw"), bcrypt.MinCost)
	if _, err := pool.Exec(context.Background(), `INSERT INTO admin_users(username,password,name) VALUES('owner',$1,'店主'),('second',$1,'二号')`, string(raw)); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, func() *pgxpool.Pool { return pool })
	try := func(ip, user, pw string) *httptest.ResponseRecorder {
		req := formReq(t, "POST", "/admin/login", url.Values{"username": {user}, "password": {pw}})
		req.RemoteAddr = ip + ":5000"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < loginMaxFails; i++ {
		if rec := try("203.0.113.1", "owner", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, rec.Code)
		}
	}
	rec := try("203.0.113.1", "owner", "right-pw")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), loginBlocked) {
		t.Fatalf("blocked login %d %s", rec.Code, rec.Body.String())
	}
	// The username key is soft (20): five stranger guesses do not lock the
	// owner out from their own address. The IP key is hard (5).
	if rec := try("203.0.113.2", "owner", "right-pw"); rec.Code != http.StatusFound {
		t.Fatalf("owner must still log in from another IP after five stranger guesses: %d %s", rec.Code, rec.Body.String())
	}
	if rec := try("203.0.113.1", "second", "right-pw"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("ip key must block another username: %d", rec.Code)
	}
	if rec := try("203.0.113.2", "second", "right-pw"); rec.Code != http.StatusFound {
		t.Fatalf("unrelated ip+user must still log in: %d %s", rec.Code, rec.Body.String())
	}
	// A success clears the counters: 4 failures, success, then 4 more failures do not block.
	for i := 0; i < loginMaxFails-1; i++ {
		try("203.0.113.3", "second", "wrong")
	}
	if rec := try("203.0.113.3", "second", "right-pw"); rec.Code != http.StatusFound {
		t.Fatalf("4 failures must not block: %d", rec.Code)
	}
	for i := 0; i < loginMaxFails-1; i++ {
		try("203.0.113.3", "second", "wrong")
	}
	if rec := try("203.0.113.3", "second", "right-pw"); rec.Code != http.StatusFound {
		t.Fatalf("counter was not cleared by the earlier success: %d", rec.Code)
	}
	// Unknown username and wrong password look the same.
	if rec := try("203.0.113.4", "nobody", "x"); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "账号或密码错误") {
		t.Fatalf("unknown user %d %s", rec.Code, rec.Body.String())
	}
}

// #22: every authenticated POST needs the session-derived token.
func TestCSRFTokenRequiredOnAdminPost(t *testing.T) {
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	mux := http.NewServeMux()
	Mount(mux, nil)
	u := session{UID: 3, Exp: time.Now().Add(time.Hour).Unix(), Name: "店主"}
	cookie := &http.Cookie{Name: cookieName, Value: signSession(u.UID, u.Name, time.Unix(u.Exp, 0))}
	post := func(v url.Values) *httptest.ResponseRecorder {
		req := formReq(t, "POST", "/admin/goods/1/delete", v)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(url.Values{}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), csrfExpired) {
		t.Fatalf("missing token %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(url.Values{"_token": {"nope"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token %d", rec.Code)
	}
	other := session{UID: 4, Exp: u.Exp}
	if rec := post(url.Values{"_token": {csrfToken(other)}}); rec.Code != http.StatusForbidden {
		t.Fatalf("another session's token %d", rec.Code)
	}
	// The right token passes the wrapper and reaches the handler (nil pool -> 503).
	if rec := post(url.Values{"_token": {csrfToken(u)}}); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "数据库未连接") {
		t.Fatalf("valid token %d %s", rec.Code, rec.Body.String())
	}
	// Logout is a state change too: without the layout's _token it is refused
	// and the session cookie stays untouched.
	req := formReq(t, "POST", "/admin/logout", url.Values{})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("logout without token %d %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	req = formReq(t, "POST", "/admin/logout", url.Values{"_token": {csrfToken(u)}})
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout with token %d %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	// A token in the query string does not count.
	req = httptest.NewRequest("POST", "/admin/goods/1/delete?_token="+csrfToken(u), nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("query token accepted: %d", rec.Code)
	}
	// GET pages are untouched and the login form keeps working without a token.
	req = httptest.NewRequest("GET", "/admin/goods", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, formReq(t, "POST", "/admin/login", url.Values{"username": {"a"}, "password": {"b"}}))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login without token must reach the handler: %d", rec.Code)
	}
	if csrfToken(session{}) != "" || csrfToken(u) == csrfToken(other) {
		t.Fatal("token derivation")
	}
}

func TestEveryPostFormCarriesToken(t *testing.T) {
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	u := session{UID: 5, Exp: time.Now().Add(time.Hour).Unix(), Name: "店主"}
	s := &Server{}
	view := s.shell(u, "t", "goods", nil)
	tok := csrfToken(u)
	if view.CSRF != tok || tok == "" {
		t.Fatal("shell must expose the token")
	}
	hidden := `<input type="hidden" name="_token" value="` + tok + `">`
	cases := map[string]any{
		"goods_list":    goodsPage{View: view, Rows: []goodRow{{ID: 1}, {ID: 2, Trashed: true}}},
		"goods_form":    goodsFormPage{View: view, Action: "/admin/goods"},
		"groups":        groupsPage{View: view, Rows: []groupRow{{ID: 1}, {ID: 2, Trashed: true}}, Action: "/admin/groups"},
		"carmis_list":   carmisPage{View: view, Rows: []carmiRow{{ID: 1}, {ID: 2, Trashed: true}}},
		"carmis_import": carmiImportPage{View: view},
		"carmi_form":    carmiFormPage{View: view, Action: "/admin/carmis/1"},
		"coupons":       couponsPage{View: view, Rows: []couponRow{{ID: 1}, {ID: 2, Trashed: true}}, Action: "/admin/coupons"},
		"order_detail":  orderPage{View: view, Row: orderRow{ID: 1, Status: 6}, Action: "/admin/orders/1"},
		"pays":          paysPage{View: view, Action: "/admin/pays"},
		"emailtpls":     mailPage{View: view, Action: "/admin/emailtpls"},
		"settings":      settingsPage{View: view, Tabs: settingTabs(nil)},
		"dashboard":     dashPage{View: view},
	}
	for name, data := range cases {
		var buf strings.Builder
		if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body := buf.String()
		forms := strings.Count(body, `method="post"`)
		if forms == 0 {
			t.Fatalf("%s: expected at least the logout form", name)
		}
		if got := strings.Count(body, hidden); got != forms {
			t.Errorf("%s: %d post forms but %d carry the token\n%s", name, forms, got, body)
		}
	}
}
