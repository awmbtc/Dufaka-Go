package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"dufaka/internal/testdb"
)

// C-2 (a): the limiter reserves before bcrypt, so a burst of concurrent wrong
// passwords from one IP gets at most loginMaxFails evaluations; the rest are
// refused with 429 without touching the database.
func TestLoginConcurrentBurstReservesUpFront(t *testing.T) {
	pool := testdb.Open(t)
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	raw, _ := bcrypt.GenerateFromPassword([]byte("right-pw"), bcrypt.MinCost)
	if _, err := pool.Exec(context.Background(), `INSERT INTO admin_users(username,password,name) VALUES('owner',$1,'店主')`, string(raw)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	compares := 0
	old := compareHash
	compareHash = func(hash, pw []byte) error {
		mu.Lock()
		compares++
		mu.Unlock()
		return old(hash, pw)
	}
	defer func() { compareHash = old }()

	mux := http.NewServeMux()
	Mount(mux, func() *pgxpool.Pool { return pool })
	const burst = 40
	codes := make([]int, burst)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := formReq(t, "POST", "/admin/login", url.Values{"username": {"owner"}, "password": {"wrong-" + strconv.Itoa(i)}})
			req.RemoteAddr = "203.0.113.1:5000"
			rec := httptest.NewRecorder()
			<-start
			mux.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()
	var unauthorized, tooMany int
	for _, c := range codes {
		switch c {
		case http.StatusUnauthorized:
			unauthorized++
		case http.StatusTooManyRequests:
			tooMany++
		default:
			t.Fatalf("unexpected status %d in %v", c, codes)
		}
	}
	if unauthorized > loginMaxFails || unauthorized+tooMany != burst {
		t.Fatalf("401=%d 429=%d; at most %d attempts may be evaluated", unauthorized, tooMany, loginMaxFails)
	}
	mu.Lock()
	defer mu.Unlock()
	if compares > loginMaxFails {
		t.Fatalf("%d bcrypt comparisons ran for %d evaluated attempts", compares, unauthorized)
	}
}

// C-2 (c): an over-long username is refused on the normal 401 path and
// never becomes a limiter key.
func TestLoginOverlongUsernameNeverKeyed(t *testing.T) {
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	mux := http.NewServeMux()
	s := &Server{}
	mux.HandleFunc("POST /admin/login", s.login)
	long := strings.Repeat("名", loginMaxUser+1)
	req := formReq(t, "POST", "/admin/login", url.Values{"username": {long}, "password": {"x"}})
	req.RemoteAddr = "203.0.113.7:1"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), loginWrong) {
		t.Fatalf("over-long username %d %s", rec.Code, rec.Body.String())
	}
	if n := s.logins.size(); n != 0 {
		t.Fatalf("limiter tracked %d keys for an over-long username", n)
	}
	// Exactly loginMaxUser runes is still a normal attempt; without a
	// database it is answered 503 before anything is reserved (round 3).
	req = formReq(t, "POST", "/admin/login", url.Values{"username": {strings.Repeat("名", loginMaxUser)}, "password": {"x"}})
	req.RemoteAddr = "203.0.113.7:1"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || s.logins.size() != 0 {
		t.Fatalf("boundary username %d keys=%d", rec.Code, s.logins.size())
	}
}

// C-2 (b): the login limiter keys on the shared netx.ClientIP rule: proxy
// headers only from a loopback/private peer, X-Real-IP first, then the LAST
// X-Forwarded-For entry, and only values that parse as IPs.
func TestLoginUsesSharedClientIPRule(t *testing.T) {
	pool := testdb.Open(t)
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	post := func(remote, real, xff string) {
		req := formReq(t, "POST", "/admin/login", url.Values{"username": {"u"}, "password": {"p"}})
		req.RemoteAddr = remote
		if real != "" {
			req.Header.Set("X-Real-IP", real)
		}
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		s.login(httptest.NewRecorder(), req)
	}
	has := func(key string) bool {
		s.logins.mu.Lock()
		defer s.logins.mu.Unlock()
		_, ok := s.logins.fails[key]
		return ok
	}
	// Default mode: only X-Real-IP (which nginx overwrites) is read; a client-sent
	// X-Forwarded-For alone gives no address, so the shared capped key is used.
	post("127.0.0.1:4321", "", "203.0.113.9, 198.51.100.7")
	if !has(loginUnknownKey) || has("ip:198.51.100.7") || has("ip:203.0.113.9") {
		t.Fatalf("default mode must not read X-Forwarded-For: %v", keysOf(&s.logins))
	}
	post("127.0.0.1:4321", "198.51.100.8", "203.0.113.9")
	if !has("ip:198.51.100.8") {
		t.Fatalf("X-Real-IP wins: %v", keysOf(&s.logins))
	}
	// X-Forwarded-For mode: the LAST entry (the one the proxy appended) is the client.
	t.Setenv("DUFAKA_REAL_IP_HEADER", "X-Forwarded-For")
	post("127.0.0.1:4321", "6.6.6.6", "203.0.113.9, 198.51.100.7")
	if !has("ip:198.51.100.7") || has("ip:203.0.113.9") || has("ip:6.6.6.6") {
		t.Fatalf("X-Forwarded-For mode must key on the last entry only: %v", keysOf(&s.logins))
	}
	t.Setenv("DUFAKA_REAL_IP_HEADER", "")
	post("192.0.2.5:1", "198.51.100.9", "198.51.100.10")
	if !has("ip:192.0.2.5") || has("ip:198.51.100.9") || has("ip:198.51.100.10") {
		t.Fatalf("headers must be ignored for a public peer: %v", keysOf(&s.logins))
	}
	post("127.0.0.1:4321", "not-an-ip-"+strings.Repeat("x", 200), "")
	if has("ip:not-an-ip-" + strings.Repeat("x", 200)) {
		t.Fatal("a header that is not an IP must not become a key")
	}
	for k := range s.logins.fails {
		if strings.HasPrefix(k, "ip:") && len(k) > len("ip:")+45 {
			t.Fatalf("ip key too long: %q", k)
		}
	}
}

func keysOf(l *loginLimiter) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for k := range l.fails {
		out = append(out, k)
	}
	return out
}

// C-2 (c), reworked in round 3: live entries are never evicted; a full map
// refuses new addresses instead, and expired entries are swept. (The round-2
// version expected the oldest counters to be dropped, which let a flood of
// fresh addresses reset a real counter.)
func TestLoginLimiterFullMapRefusesNewKeys(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	l := &loginLimiter{now: func() time.Time { return now }, maxKeys: 50}
	for i := 0; i < loginMaxFails+1; i++ {
		l.reserve("ip:blocked")
	}
	if !l.blocked("ip:blocked") {
		t.Fatal("setup: key should be blocked")
	}
	l.reserve("ip:counting")
	for i := 0; i < 48; i++ {
		now = now.Add(time.Millisecond)
		if !l.reserve("ip:" + strconv.Itoa(i)) {
			t.Fatalf("key %d refused below the cap", i)
		}
	}
	if l.reserve("ip:new") {
		t.Fatal("a full map must refuse a new address")
	}
	if _, ok := l.fails["ip:new"]; ok {
		t.Fatal("a refused request must not create an entry")
	}
	if !l.blocked("ip:blocked") || l.count("ip:counting") != 1 || l.count("ip:0") != 1 {
		t.Fatal("live entries must survive a full map")
	}
	// An existing key keeps counting while the map is full.
	if !l.reserve("ip:counting") || l.count("ip:counting") != 2 {
		t.Fatal("existing keys must still be served when the map is full")
	}
	// Once the window is over the counters expire and are swept, making room.
	now = now.Add(loginWindow + time.Minute)
	if !l.reserve("ip:new") {
		t.Fatal("expired entries must be swept to make room")
	}
	if l.size() != 1 {
		t.Fatalf("expired entries not swept: %d keys", l.size())
	}
	// Periodic sweep without needing the cap.
	l = &loginLimiter{now: func() time.Time { return now }}
	for i := 0; i < 100; i++ {
		l.reserve("ip:old" + strconv.Itoa(i))
	}
	now = now.Add(loginWindow + time.Minute)
	for i := 0; i < loginSweepEvery; i++ {
		l.reserve("ip:new" + strconv.Itoa(i))
	}
	if _, ok := l.fails["ip:old1"]; ok {
		t.Fatalf("expired entries not swept: %d keys", l.size())
	}
}

// C-5: merchant fields are trimmed and a wescan APIv3 key must be 32 bytes.
func TestReadPayTrimsAndChecksWechatKey(t *testing.T) {
	v := payValues("wescan", "  mch-1  ", "\n  PEM  \r\n", "1")
	v.Set("merchant_key", "  "+strings.Repeat("k", 32)+"\t")
	f, err := readPay(formReq(t, "POST", "/admin/pays", v))
	if err != nil {
		t.Fatal(err)
	}
	if f.MerchantID != "mch-1" || f.Pem != "PEM" || f.Key != strings.Repeat("k", 32) {
		t.Fatalf("not trimmed: %+v", f)
	}
	v.Set("merchant_key", strings.Repeat("k", 31))
	if _, err := readPay(formReq(t, "POST", "/admin/pays", v)); err == nil || err.Error() != payKeyLen {
		t.Fatalf("31-byte key: %v", err)
	}
	v.Set("merchant_key", strings.Repeat("k", 33))
	if _, err := readPay(formReq(t, "POST", "/admin/pays", v)); err == nil || err.Error() != payKeyLen {
		t.Fatalf("33-byte key: %v", err)
	}
	// 32 runes of multibyte text are not 32 bytes.
	v.Set("merchant_key", strings.Repeat("密", 32))
	if _, err := readPay(formReq(t, "POST", "/admin/pays", v)); err == nil || err.Error() != payKeyLen {
		t.Fatalf("multibyte key: %v", err)
	}
	// Whitespace only counts as empty: the environment variable is used instead.
	v.Set("merchant_key", "   ")
	if f, err := readPay(formReq(t, "POST", "/admin/pays", v)); err != nil || f.Key != "" {
		t.Fatalf("blank key: %+v %v", f, err)
	}
	// The rule is wescan-only; other channels keep free-form keys.
	v = payValues("cldx", "", "", "1")
	v.Set("merchant_key", "short")
	if _, err := readPay(formReq(t, "POST", "/admin/pays", v)); err != nil {
		t.Fatalf("cldx key: %v", err)
	}
	// A pem that is only whitespace is still "empty".
	if _, err := readPay(formReq(t, "POST", "/admin/pays", payValues("wescan", "m", " \n ", "1"))); err == nil || err.Error() != "请填写商户密钥" {
		t.Fatalf("blank pem: %v", err)
	}
	if !cashierReady("wescan") || !cashierReady("cldx") || cashierReady("alipay") || cashierReady("") {
		t.Fatal("cashierReady must mirror store.CashierReady")
	}
}

// C-5: the pays page shows a read-only configuration status per wired channel.
func TestPaysPageConfigStatus(t *testing.T) {
	for _, k := range []string{"WECHAT_PAY_APP_ID", "WECHAT_PAY_MCH_ID", "WECHAT_PAY_CERT_SERIAL_NO", "WECHAT_PAY_API_V3_KEY", "WECHAT_PAY_PRIVATE_KEY", "WECHAT_PAY_PLATFORM_KEY", "WALLET_MERCHANT_SECRET"} {
		t.Setenv(k, "")
	}
	// Real key material: round 3 made Validate parse the private key, so a
	// placeholder PEM would be reported as unparsable rather than present.
	priv, pub := testRSAPEMs(t)
	st, ok := payConfigStatus(payRow{Name: "微信", Check: "wescan", MerchantID: "mch", Pem: priv, Key: strings.Repeat("k", 32)})
	if !ok || st.OK || !strings.HasPrefix(st.Text, "配置不完整：") || strings.Contains(st.Text, "商户号") || strings.Contains(st.Text, "APIv3") || strings.Contains(st.Text, "商户私钥") || !strings.Contains(st.Text, "appid") {
		t.Fatalf("wescan row fields must count, env pieces listed as missing: %+v ok=%v", st, ok)
	}
	t.Setenv("WECHAT_PAY_APP_ID", "app")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "serial")
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", pub)
	st, _ = payConfigStatus(payRow{Check: "wescan", MerchantID: "mch", Pem: priv, Key: strings.Repeat("k", 32)})
	if !st.OK || st.Text != "配置完整" {
		t.Fatalf("complete wescan: %+v", st)
	}
	st, _ = payConfigStatus(payRow{Check: "cldx"})
	if st.OK || st.Text != "未配置 WALLET_MERCHANT_SECRET" {
		t.Fatalf("cldx without secret: %+v", st)
	}
	t.Setenv("WALLET_MERCHANT_SECRET", "s")
	st, _ = payConfigStatus(payRow{Check: "cldx"})
	if !st.OK || st.Text != "钱包密钥已配置" {
		t.Fatalf("cldx with secret: %+v", st)
	}
	if _, ok := payConfigStatus(payRow{Check: "alipay"}); ok {
		t.Fatal("unwired channels have no status")
	}

	var buf strings.Builder
	err := pages.ExecuteTemplate(&buf, "pays", paysPage{
		View: View{Title: "支付通道", User: "管理员", Nav: "pays"}, Form: payForm{Method: 1, Client: 3}, Action: "/admin/pays",
		Status: []payStatus{{Name: "微信", Check: "wescan", Text: "缺少：appid（WECHAT_PAY_APP_ID）"}, {Name: "钱包", Check: "cldx", OK: true, Text: "钱包密钥已配置"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, s := range []string{`id="pay-config-status"`, "配置状态", `<span class="banner err">缺少：appid（WECHAT_PAY_APP_ID）</span>`, `<span class="banner ok">钱包密钥已配置</span>`,
		"wescan 渠道：微信支付 APIv3 密钥（32 位），填写后优先于环境变量 WECHAT_PAY_API_V3_KEY；cldx 渠道留空"} {
		if !strings.Contains(body, s) {
			t.Errorf("pays page missing %q", s)
		}
	}
	if strings.Contains(body, `name="config_status"`) {
		t.Fatal("status block must be read-only")
	}
}

// C-5: the status block is fed from the rows on a real page render.
func TestPaysPageConfigStatusIntegration(t *testing.T) {
	pool := testdb.Open(t)
	t.Setenv("WALLET_MERCHANT_SECRET", "")
	if _, err := pool.Exec(context.Background(), `INSERT INTO pays (pay_name, pay_check, pay_method, pay_client, merchant_id, merchant_key, merchant_pem, pay_handleroute, is_open, created_at, updated_at)
		VALUES ('钱包','cldx',2,3,'','','','/pay/cldx',1,now(),now()), ('支付宝','alipay',1,1,'m','k','p','/pay/alipay',0,now(),now())`); err != nil {
		t.Fatal(err)
	}
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	w := httptest.NewRecorder()
	s.paysPage(w, httptest.NewRequest("GET", "/admin/pays", nil), session{Name: "店主"})
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "未配置 WALLET_MERCHANT_SECRET") || strings.Count(body, "<li>") != 1 {
		t.Fatalf("status block: %d\n%s", w.Code, body)
	}
}

// C-3: a manual status change that ends a type-1 order's claim on reserved
// cards releases them in the same request; other transitions leave them.
func TestOrderSaveReleasesReservationsIntegration(t *testing.T) {
	pool := testdb.Open(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type,in_stock) VALUES(1,1,'card','','',10,1,1);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,trade_no) VALUES
  (1,'ABN',1,'card','a@example.com','local',6,1,'wx-1'),
  (2,'PROC',1,'card','a@example.com','local',2,1,'wx-2'),
  (3,'KEEP',1,'card','a@example.com','local',2,1,'wx-3'),
  (4,'MANUAL',1,'card','a@example.com','local',6,2,'wx-4');
 INSERT INTO carmis(id,goods_id,carmi,status,reserved_order_id) VALUES
  (1,1,'c1',1,1),(2,1,'c2',2,1),(3,1,'c3',1,2),(4,1,'c4',1,3),(5,1,'c5',1,4);`)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	save := func(id int, status string) int {
		req := formReq(t, "POST", "/admin/orders/"+strconv.Itoa(id), url.Values{"title": {"card"}, "status": {status}})
		req.SetPathValue("id", strconv.Itoa(id))
		w := httptest.NewRecorder()
		s.orderSave(w, req, session{Name: "店主"})
		return w.Code
	}
	reserved := func(cardID int) bool {
		var v *int
		if err := pool.QueryRow(ctx, `SELECT reserved_order_id FROM carmis WHERE id=$1`, cardID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v != nil
	}
	// 6 -> 4 on a type-1 order: the unsold reserved card is freed, the sold one untouched.
	if code := save(1, "4"); code != http.StatusFound {
		t.Fatalf("6->4: %d", code)
	}
	if reserved(1) || !reserved(2) {
		t.Fatalf("leaving status 6: card1 reserved=%v card2 reserved=%v", reserved(1), reserved(2))
	}
	// 2 -> 5 frees too.
	if code := save(2, "5"); code != http.StatusFound || reserved(3) {
		t.Fatalf("2->5: %d reserved=%v", code, reserved(3))
	}
	// 2 -> 3 keeps the claim; the order is still being fulfilled.
	if code := save(3, "3"); code != http.StatusFound || !reserved(4) {
		t.Fatalf("2->3: %d reserved=%v", code, reserved(4))
	}
	// Type-2 orders never held cards; the rule is type-1 only. (Round 3: the
	// type-2 6->4 change now takes buy_amount from in_stock, hence in_stock=1 above.)
	if code := save(4, "4"); code != http.StatusFound || !reserved(5) {
		t.Fatalf("type 2: %d reserved=%v", code, reserved(5))
	}
	var status int
	pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=1`).Scan(&status)
	if status != 4 {
		t.Fatalf("order status %d", status)
	}
	for _, c := range []struct {
		typ, from, to int
		want          bool
	}{
		{1, 6, 4, true}, {1, 6, 2, true}, {1, 6, 6, false}, {1, 2, 4, true}, {1, 3, 5, true}, {1, 2, 3, false}, {1, 3, 6, false}, {2, 6, 4, false}, {1, 5, 4, false},
	} {
		if got := releasesReservations(c.typ, c.from, c.to); got != c.want {
			t.Errorf("releasesReservations(%d,%d->%d)=%v want %v", c.typ, c.from, c.to, got, c.want)
		}
	}
}
