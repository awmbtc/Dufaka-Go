package admin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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

	"dufaka/internal/pay"
	"dufaka/internal/testdb"
)

func testRSAPEMs(t *testing.T) (priv, pub string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// loginRig is a Server wired to a real database with one admin "owner"
// (password "right-pw"), plus a counted, stubbed soft delay.
type loginRig struct {
	s      *Server
	mux    *http.ServeMux
	pool   *pgxpool.Pool
	mu     sync.Mutex
	pauses []time.Duration
}

func newLoginRig(t *testing.T, maxKeys int) *loginRig {
	t.Helper()
	pool := testdb.Open(t)
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	raw, _ := bcrypt.GenerateFromPassword([]byte("right-pw"), bcrypt.MinCost)
	if _, err := pool.Exec(context.Background(), `INSERT INTO admin_users(username,password,name) VALUES('owner',$1,'店主')`, string(raw)); err != nil {
		t.Fatal(err)
	}
	g := &loginRig{pool: pool}
	g.s = &Server{poolFn: func() *pgxpool.Pool { return pool }}
	g.s.logins.maxKeys = maxKeys
	g.s.loginPause = func(_ context.Context, d time.Duration) {
		g.mu.Lock()
		g.pauses = append(g.pauses, d)
		g.mu.Unlock()
	}
	g.mux = http.NewServeMux()
	g.mux.HandleFunc("POST /admin/login", g.s.login)
	return g
}

func (g *loginRig) try(t *testing.T, remote, user, pw string) *httptest.ResponseRecorder {
	t.Helper()
	req := formReq(t, "POST", "/admin/login", url.Values{"username": {user}, "password": {pw}})
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	g.mux.ServeHTTP(rec, req)
	return rec
}

func (g *loginRig) pauseCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pauses)
}

// C3-1 (c): requests that are refused (blocked address, full map) never
// create entries and never disturb the counter on the owner's username.
func TestLoginRefusedFloodKeepsUserCounter(t *testing.T) {
	g := newLoginRig(t, 8)
	// Ten failures on the owner's name from two addresses (5 + 5): the
	// first address is then blocked, the second has used its five.
	for i := 0; i < 5; i++ {
		g.try(t, "198.51.100.1:1", "owner", "wrong")
		g.try(t, "198.51.100.2:1", "owner", "wrong")
	}
	if n := g.s.logins.count("user:owner"); n != 10 {
		t.Fatalf("user:owner=%d want 10", n)
	}
	// Fill the map with live entries up to its cap (user:owner + 7 addresses).
	for i := 3; i <= 7; i++ {
		if rec := g.try(t, "198.51.100."+strconv.Itoa(i)+":1", "owner", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("filler %d: %d", i, rec.Code)
		}
	}
	if n := g.s.logins.count("user:owner"); n != 15 {
		t.Fatalf("user:owner=%d want 15", n)
	}
	before := g.s.logins.size()
	if before != 8 {
		t.Fatalf("setup: %d entries, want the cap of 8", before)
	}
	// Flood: refused requests from the exhausted address and from 500 new addresses.
	for i := 0; i < 50; i++ {
		if rec := g.try(t, "198.51.100.1:1", "owner", "wrong"); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("blocked address answered %d", rec.Code)
		}
	}
	for i := 0; i < 500; i++ {
		rec := g.try(t, "203.0.113."+strconv.Itoa(i%250)+":"+strconv.Itoa(i), "owner", "wrong")
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), loginBlocked) {
			t.Fatalf("new address on a full map answered %d", rec.Code)
		}
	}
	if n := g.s.logins.count("user:owner"); n != 15 {
		t.Fatalf("refused flood changed user:owner to %d", n)
	}
	if after := g.s.logins.size(); after != before {
		t.Fatalf("refused requests created entries: %d -> %d", before, after)
	}
	if !g.s.logins.blocked("ip:198.51.100.1") {
		t.Fatal("the blocked address must stay blocked")
	}
}

// C3-1 (a): the address key of an IPv6 client is its /64.
func TestLoginIPv6RotationIsOneKey(t *testing.T) {
	g := newLoginRig(t, 0)
	for i := 1; i <= loginMaxFails; i++ {
		remote := "[2001:db8:1:2::" + strconv.FormatInt(int64(i), 16) + "]:443"
		if rec := g.try(t, remote, "owner", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	if rec := g.try(t, "[2001:db8:1:2:ffff:ffff:ffff:ffff]:443", "owner", "right-pw"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("another address in the same /64 must share the limit: %d", rec.Code)
	}
	if rec := g.try(t, "[2001:db8:1:3::1]:443", "owner", "right-pw"); rec.Code != http.StatusFound {
		t.Fatalf("the next /64 is a different client: %d", rec.Code)
	}
	if got := keysOf(&g.s.logins); len(got) != 1 || got[0] != "ip:2001:db8:1:2::/64" {
		t.Fatalf("keys %v", got)
	}
}

// C3-1 (d): strangers can slow the owner's account down but never lock the
// owner out; the delay runs before bcrypt and a success clears both keys.
func TestLoginOwnerNotLockedOutByUsernameFailures(t *testing.T) {
	g := newLoginRig(t, 0)
	var compares int
	var cmu sync.Mutex
	old := compareHash
	compareHash = func(hash, pw []byte) error {
		cmu.Lock()
		compares++
		cmu.Unlock()
		return old(hash, pw)
	}
	defer func() { compareHash = old }()
	for i := 0; i < 25; i++ {
		ip := "198.51.100." + strconv.Itoa(i+1) + ":1"
		if rec := g.try(t, ip, "owner", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d answered %d", i+1, rec.Code)
		}
	}
	if n := g.s.logins.count("user:owner"); n != 25 {
		t.Fatalf("user:owner=%d want 25", n)
	}
	// Attempts 21..25 were delayed (20 failures already on record).
	if n := g.pauseCount(); n != 25-loginUserMaxFails {
		t.Fatalf("%d delays, want %d", n, 25-loginUserMaxFails)
	}
	for _, d := range g.pauses {
		if d != loginUserDelay {
			t.Fatalf("delay %v want %v", d, loginUserDelay)
		}
	}
	if compares != 25 {
		t.Fatalf("delayed attempts must still be evaluated: %d compares", compares)
	}
	rec := g.try(t, "192.0.2.77:1", "owner", "right-pw")
	if rec.Code != http.StatusFound {
		t.Fatalf("owner locked out: %d %s", rec.Code, rec.Body.String())
	}
	if n := g.pauseCount(); n != 25-loginUserMaxFails+1 {
		t.Fatalf("the owner's own attempt is delayed too, not refused: %d", n)
	}
	if g.s.logins.count("user:owner") != 0 || g.s.logins.count("ip:192.0.2.77") != 0 {
		t.Fatal("success must clear both keys")
	}
	// Unknown usernames never get a username key.
	g.try(t, "192.0.2.78:1", "ghost", "x")
	for _, k := range keysOf(&g.s.logins) {
		if strings.HasPrefix(k, "user:") {
			t.Fatalf("unexpected username key %q", k)
		}
	}
}

// C3-1 (b): the 503 paths answer before anything is reserved.
func TestLoginUnavailableDoesNotConsumeAttempts(t *testing.T) {
	g := newLoginRig(t, 0)
	t.Setenv("DUFAKA_SESSION_KEY", "")
	for i := 0; i < 10; i++ {
		if rec := g.try(t, "198.51.100.9:1", "owner", "wrong"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("missing session key: %d", rec.Code)
		}
	}
	if n := g.s.logins.size(); n != 0 {
		t.Fatalf("503 reserved %d keys", n)
	}
	noDB := &Server{}
	req := formReq(t, "POST", "/admin/login", url.Values{"username": {"owner"}, "password": {"x"}})
	req.RemoteAddr = "198.51.100.9:1"
	rec := httptest.NewRecorder()
	noDB.login(rec, req)
	if rec.Code != http.StatusServiceUnavailable || noDB.logins.size() != 0 {
		t.Fatalf("nil db: %d keys=%d", rec.Code, noDB.logins.size())
	}
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	for i := 0; i < loginMaxFails-1; i++ {
		g.try(t, "198.51.100.9:1", "owner", "wrong")
	}
	if rec := g.try(t, "198.51.100.9:1", "owner", "right-pw"); rec.Code != http.StatusFound {
		t.Fatalf("503s must not have used up attempts: %d", rec.Code)
	}
}

// C3-1 (b): a database error during the lookup hands the reservation back.
func TestLoginDBErrorReleasesReservation(t *testing.T) {
	g := newLoginRig(t, 0)
	if _, err := g.pool.Exec(context.Background(), `ALTER TABLE admin_users RENAME TO admin_users_gone`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if rec := g.try(t, "198.51.100.10:1", "owner", "x"); rec.Code != http.StatusInternalServerError {
			t.Fatalf("db error: %d", rec.Code)
		}
	}
	if n := g.s.logins.count("ip:198.51.100.10"); n != 0 {
		t.Fatalf("db errors left %d attempts reserved", n)
	}
}

// C3-1 (b): invalid usernames get the normal 401 and never become keys.
func TestLoginInvalidUsernamesNeverKeyed(t *testing.T) {
	g := newLoginRig(t, 0)
	for _, u := range []string{"", "   ", strings.Repeat("a", loginMaxUser+1), "own\xffer", "own\x00er"} {
		rec := g.try(t, "198.51.100.11:1", u, "x")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("username %q: %d", u, rec.Code)
		}
	}
	if n := g.s.logins.size(); n != 0 {
		t.Fatalf("invalid usernames created %d keys: %v", n, keysOf(&g.s.logins))
	}
	if !validLoginUser(strings.Repeat("名", loginMaxUser)) || validLoginUser("a\x00") || validLoginUser("\xc3") {
		t.Fatal("validLoginUser")
	}
}

// C3-2: the wescan status follows WechatConfig.Validate exactly: "配置完整"
// only when it reports nothing, otherwise every reported item is listed.
func TestPayConfigStatusFollowsValidate(t *testing.T) {
	for _, k := range []string{"WECHAT_PAY_APP_ID", "WECHAT_PAY_CERT_SERIAL_NO", "WECHAT_PAY_API_V3_KEY", "WECHAT_PAY_PLATFORM_KEY", "WECHAT_PAY_PLATFORM_SERIAL", "WECHAT_PAY_PUBLIC_KEY_ID"} {
		t.Setenv(k, "")
	}
	priv, pub := testRSAPEMs(t)
	t.Setenv("WECHAT_PAY_APP_ID", "app")
	t.Setenv("WECHAT_PAY_CERT_SERIAL_NO", "serial")
	t.Setenv("WECHAT_PAY_PLATFORM_KEY", pub)
	rows := []payRow{
		{Check: "wescan", MerchantID: "mch", Pem: priv, Key: strings.Repeat("k", 32)},
		{Check: "wescan", MerchantID: "mch", Pem: "not a key", Key: strings.Repeat("k", 32)},
		{Check: "wescan", MerchantID: "mch", Pem: priv, Key: "short"},
		{Check: "wescan", MerchantID: "", Pem: "", Key: ""},
	}
	for i, row := range rows {
		st, ok := payConfigStatus(row)
		want := pay.WechatConfig{MchID: row.MerchantID, PrivateKeyPEM: row.Pem, APIv3Key: row.Key}.WithEnv().Validate()
		if !ok || st.OK != (len(want) == 0) {
			t.Fatalf("row %d: %+v vs Validate %v", i, st, want)
		}
		if st.OK && st.Text != "配置完整" {
			t.Fatalf("row %d: %q", i, st.Text)
		}
		for _, w := range want {
			if !strings.Contains(st.Text, w) {
				t.Fatalf("row %d: %q lacks %q", i, st.Text, w)
			}
		}
	}
	t.Setenv("WALLET_MERCHANT_SECRET", " \n\t")
	if st, _ := payConfigStatus(payRow{Check: "cldx"}); st.OK {
		t.Fatal("a whitespace-only wallet secret is not configured")
	}
}

// C3-3: secrets never reach the pays page; the edit form keeps them unless a
// new value is submitted, and 清空商户 KEY clears the key.
func TestPaySecretsHiddenAndKeptIntegration(t *testing.T) {
	pool := testdb.Open(t)
	t.Setenv("DUFAKA_SESSION_KEY", "test-key")
	ctx := context.Background()
	const oldKey = "OLDKEYOLDKEYOLDKEYOLDKEYOLDKEY12"
	const oldPem = "-----BEGIN PRIVATE KEY-----SECRETPEMBODY"
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO pays (pay_name, pay_check, pay_method, pay_client, merchant_id, merchant_key, merchant_pem, pay_handleroute, is_open, created_at, updated_at)
		VALUES ('微信','wescan',2,3,'mch',$1,$2,'/pay/wescan',0,now(),now()) RETURNING id`, oldKey, oldPem).Scan(&id); err != nil {
		t.Fatal(err)
	}
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	u := session{UID: 1, Exp: time.Now().Add(time.Hour).Unix(), Name: "店主"}
	noSecrets := func(label, body string) {
		t.Helper()
		for _, secret := range []string{oldKey, "OLDKEY", "SECRETPEM", "BEGIN PRIVATE"} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s leaks %q", label, secret)
			}
		}
	}
	w := httptest.NewRecorder()
	s.paysPage(w, httptest.NewRequest("GET", "/admin/pays", nil), u)
	noSecrets("list", w.Body.String())
	if strings.Count(w.Body.String(), "<td>已填写</td>") != 2 {
		t.Fatalf("list must show 已填写 for both secrets:\n%s", w.Body.String())
	}
	req := httptest.NewRequest("GET", "/admin/pays/"+strconv.Itoa(id)+"/edit", nil)
	req.SetPathValue("id", strconv.Itoa(id))
	w = httptest.NewRecorder()
	s.payEdit(w, req, u)
	body := w.Body.String()
	noSecrets("edit form", body)
	for _, want := range []string{`name="merchant_key"></textarea>`, `name="merchant_pem"></textarea>`, "留空表示不修改", `name="clear_merchant_key"`, "清空商户 KEY"} {
		if !strings.Contains(body, want) {
			t.Fatalf("edit form lacks %q", want)
		}
	}
	stored := func() (key, pemText string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT COALESCE(merchant_key,''), merchant_pem FROM pays WHERE id=$1`, id).Scan(&key, &pemText); err != nil {
			t.Fatal(err)
		}
		return
	}
	save := func(v url.Values) *httptest.ResponseRecorder {
		req := formReq(t, "POST", "/admin/pays/"+strconv.Itoa(id), v)
		req.SetPathValue("id", strconv.Itoa(id))
		rec := httptest.NewRecorder()
		s.paySave(rec, req, u)
		return rec
	}
	// Blank fields keep both secrets; a pem is not required when one is stored.
	v := payValues("wescan", "mch-2", "", "0")
	if rec := save(v); rec.Code != http.StatusFound {
		t.Fatalf("keep save: %d %s", rec.Code, rec.Body.String())
	}
	if k, p := stored(); k != oldKey || p != oldPem {
		t.Fatalf("blank submit changed secrets: %q %q", k, p)
	}
	// A stored key of the wrong length is not re-checked when left blank; a
	// newly submitted one is.
	pool.Exec(ctx, `UPDATE pays SET merchant_key='legacy-short' WHERE id=$1`, id)
	if rec := save(payValues("wescan", "mch", "", "0")); rec.Code != http.StatusFound {
		t.Fatalf("stored short key blocked an unrelated save: %d %s", rec.Code, rec.Body.String())
	}
	v = payValues("wescan", "mch", "", "0")
	v.Set("merchant_key", "short")
	if rec := save(v); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), payKeyLen) {
		t.Fatalf("new short key: %d", rec.Code)
	} else {
		noSecrets("error re-render", rec.Body.String())
		if strings.Contains(rec.Body.String(), ">short</textarea>") {
			t.Fatal("submitted key echoed back")
		}
	}
	// A new key and pem replace the stored ones.
	newKey := strings.Repeat("n", 32)
	v = payValues("wescan", "mch", "NEWPEM", "0")
	v.Set("merchant_key", newKey)
	if rec := save(v); rec.Code != http.StatusFound {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
	}
	if k, p := stored(); k != newKey || p != "NEWPEM" {
		t.Fatalf("replace: %q %q", k, p)
	}
	// 清空商户 KEY clears the key and keeps the pem.
	v = payValues("wescan", "mch", "", "0")
	v.Set("clear_merchant_key", "1")
	if rec := save(v); rec.Code != http.StatusFound {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if k, p := stored(); k != "" || p != "NEWPEM" {
		t.Fatalf("clear: %q %q", k, p)
	}
	// Clearing and submitting a key at once is refused.
	v.Set("merchant_key", newKey)
	if rec := save(v); rec.Code != http.StatusBadRequest {
		t.Fatalf("clear+new: %d", rec.Code)
	}
	// A new channel still needs a pem.
	req = formReq(t, "POST", "/admin/pays", payValues("wescan2", "m", "", "0"))
	rec := httptest.NewRecorder()
	s.paySave(rec, req, u)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "请填写商户密钥") {
		t.Fatalf("new channel without pem: %d", rec.Code)
	}
}

// C3-4: moving a 人工处理 order out of 异常 takes its stock in the same
// transaction, or is refused when stock is short.
func TestOrderSaveManualShortTakesStockIntegration(t *testing.T) {
	pool := testdb.Open(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type,in_stock) VALUES(1,1,'manual','','',10,2,1);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,buy_amount,trade_no) VALUES
  (1,'M1',1,'manual','a@example.com','local',6,2,2,'wx-1'),
  (2,'M2',1,'manual','a@example.com','local',6,2,1,'wx-2'),
  (3,'M3',1,'manual','a@example.com','local',2,2,1,'wx-3');
 UPDATE orders SET info=E'库存不足\n充值账号:QQ1', stock_owed=true WHERE id IN (1,2);
 INSERT INTO carmis(id,goods_id,carmi,status,reserved_order_id,updated_at) VALUES(1,1,'c',1,4,'2020-01-01');
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,buy_amount,trade_no) VALUES
  (4,'A4',1,'auto','a@example.com','local',6,1,1,'wx-4');`)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	save := func(id int, status string) *httptest.ResponseRecorder {
		req := formReq(t, "POST", "/admin/orders/"+strconv.Itoa(id), url.Values{"title": {"manual"}, "status": {status}})
		req.SetPathValue("id", strconv.Itoa(id))
		w := httptest.NewRecorder()
		s.orderSave(w, req, session{Name: "店主"})
		return w
	}
	state := func(id int) (status, stock int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT o.status, g.in_stock FROM orders o JOIN goods g ON g.id=o.goods_id WHERE o.id=$1`, id).Scan(&status, &stock); err != nil {
			t.Fatal(err)
		}
		return
	}
	// Needs 2, only 1 in stock: refused, nothing changes.
	rec := save(1, "2")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), orderManualShort) {
		t.Fatalf("short stock: %d %s", rec.Code, rec.Body.String())
	}
	if st, stock := state(1); st != 6 || stock != 1 {
		t.Fatalf("refused change left status=%d stock=%d", st, stock)
	}
	// Needs 1: taken together with the status change.
	if rec := save(2, "3"); rec.Code != http.StatusFound {
		t.Fatalf("6->3: %d %s", rec.Code, rec.Body.String())
	}
	if st, stock := state(2); st != 3 || stock != 0 {
		t.Fatalf("6->3 status=%d stock=%d", st, stock)
	}
	// Restocked: 6 -> 4 takes 2.
	pool.Exec(ctx, `UPDATE goods SET in_stock=5 WHERE id=1`)
	if rec := save(1, "4"); rec.Code != http.StatusFound {
		t.Fatalf("6->4: %d", rec.Code)
	}
	if st, stock := state(1); st != 4 || stock != 3 {
		t.Fatalf("6->4 status=%d stock=%d", st, stock)
	}
	// Progress between handled states never touches stock again, and 6 cannot
	// be set by hand (it would let the order take its stock twice).
	if rec := save(3, "4"); rec.Code != http.StatusFound {
		t.Fatalf("2->4: %d", rec.Code)
	}
	if _, stock := state(3); stock != 3 {
		t.Fatalf("2->4 changed stock to %d", stock)
	}
	if rec := save(2, "6"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), orderNoManualShort) {
		t.Fatalf("3->6 must be refused: %d", rec.Code)
	}
	// 6 -> 5 (处理失败) is refused for a 人工处理 order: from 5 it could later
	// move to 2/3/4 without its stock ever being taken (repair pass 1).
	pool.Exec(ctx, `UPDATE orders SET status=6, info=E'库存不足\n', stock_owed=true WHERE id=3`)
	if rec := save(3, "5"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), orderManualShortFailed) {
		t.Fatalf("6->5 must be refused: %d %s", rec.Code, rec.Body.String())
	}
	if st, stock := state(3); st != 6 || stock != 3 {
		t.Fatalf("refused 6->5 left status=%d stock=%d", st, stock)
	}
	// Type 1: reservation release stamps updated_at; no stock arithmetic.
	if rec := save(4, "4"); rec.Code != http.StatusFound {
		t.Fatalf("type 1 6->4: %d", rec.Code)
	}
	var fresh bool
	if err := pool.QueryRow(ctx, `SELECT reserved_order_id IS NULL AND updated_at > now() - interval '1 minute' FROM carmis WHERE id=1`).Scan(&fresh); err != nil || !fresh {
		t.Fatalf("released card not stamped: %v %v", fresh, err)
	}
	if _, stock := state(4); stock != 3 {
		t.Fatalf("type 1 changed in_stock to %d", stock)
	}
	for _, c := range []struct {
		typ, from, to int
		want          bool
	}{{2, 6, 2, true}, {2, 6, 3, true}, {2, 6, 4, true}, {2, 6, 5, false}, {2, 2, 4, false}, {1, 6, 4, false}, {2, 6, 6, false}} {
		if got := takesManualStock(c.typ, c.from, c.to); got != c.want {
			t.Errorf("takesManualStock(%d,%d->%d)=%v", c.typ, c.from, c.to, got)
		}
	}
}

// C3-5: couponSave locks goods before the coupon, so it cannot deadlock with
// a checkout that holds the goods row and then updates the coupon.
func TestCouponSaveLockOrderIntegration(t *testing.T) {
	pool := testdb.Open(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type) VALUES(1,1,'a','','',10,1),(2,1,'b','','',10,1);
 INSERT INTO coupons(id,discount,is_use,is_open,coupon,ret,created_at,updated_at) VALUES(1,1,1,1,'C',50,now(),now());
 INSERT INTO coupons_goods(goods_id,coupons_id) VALUES(1,1);`); err != nil {
		t.Fatal(err)
	}
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	saveCoupon := func(ret string, goods ...string) *httptest.ResponseRecorder {
		req := formReq(t, "POST", "/admin/coupons/1", url.Values{"coupon": {"C"}, "discount": {"2"}, "ret": {ret}, "is_use": {"1"}, "is_open": {"1"}, "goods_id": goods})
		req.SetPathValue("id", "1")
		w := httptest.NewRecorder()
		s.couponSave(w, req, session{Name: "店主"})
		return w
	}

	// lockGoods is the other transaction's first step: the storefront's
	// SELECT … FOR UPDATE (conflicts with KEY SHARE), or a plain UPDATE goods
	// of a non-key column (does not).
	for _, lockGoods := range []string{
		`SELECT id FROM goods WHERE id=2 FOR UPDATE`,
		`UPDATE goods SET in_stock=in_stock+1, updated_at=now() WHERE id=2`,
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, lockGoods); err != nil {
			t.Fatal(err)
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- saveCoupon("40", "2", "1") }()
		// Give couponSave time to either finish or block on the goods row.
		deadline := time.Now().Add(10 * time.Second)
		var finished *httptest.ResponseRecorder
		for finished == nil {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				break
			}
			select {
			case finished = <-done:
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: couponSave neither finished nor blocked", lockGoods)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if finished == nil && strings.Contains(lockGoods, "UPDATE goods") {
			// A non-key UPDATE does not conflict with KEY SHARE; it must not block.
			t.Fatalf("%s: couponSave blocked unexpectedly", lockGoods)
		}
		// The other transaction now takes the coupon. With the goods lock taken
		// first, couponSave either already committed or waits on goods without
		// holding the coupon, so this goes through (no deadlock, no timeout).
		if _, err := tx.Exec(ctx, `UPDATE coupons SET ret=ret-1, updated_at=now() WHERE id=1`); err != nil {
			t.Fatalf("%s: checkout blocked on the coupon (lock order inverted): %v", lockGoods, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if finished == nil {
			select {
			case finished = <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: couponSave did not finish", lockGoods)
			}
		}
		if finished.Code != http.StatusFound {
			t.Fatalf("%s: couponSave %d %s", lockGoods, finished.Code, finished.Body.String())
		}
	}
	var links int
	pool.QueryRow(ctx, `SELECT count(*) FROM coupons_goods WHERE coupons_id=1`).Scan(&links)
	if links != 2 {
		t.Fatalf("links=%d", links)
	}
	// A missing or deleted goods id is still refused.
	pool.Exec(ctx, `UPDATE goods SET deleted_at=now() WHERE id=2`)
	if w := saveCoupon("4", "2"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "可用商品不存在") {
		t.Fatalf("deleted goods: %d", w.Code)
	}
	if w := saveCoupon("4", "99"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "可用商品不存在") {
		t.Fatalf("missing goods: %d", w.Code)
	}
}

// C3-6: the 重新发货 help no longer claims it handles 人工处理 goods; a
// 人工处理 order in 异常 gets accurate guidance instead.
func TestOrderDetailShortHelpAccurate(t *testing.T) {
	render := func(typ int) string {
		var buf strings.Builder
		if err := pages.ExecuteTemplate(&buf, "order_detail", orderPage{View: View{Title: "订单详情"}, Row: orderRow{ID: 1, Status: 6, Type: typ, Owes: typ == 2}, Action: "/admin/orders/1"}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	auto, manual := render(1), render(2)
	if strings.Contains(auto, "人工处理商品改为待处理") || !strings.Contains(auto, "重新发货") {
		t.Fatal("stale 重新发货 hint")
	}
	if strings.Contains(manual, "/redeliver") || !strings.Contains(manual, "扣减库存") || !strings.Contains(manual, "不能直接改为处理失败") {
		t.Fatal("manual 异常 order needs the stock hint, not the redeliver form")
	}
	// A legacy manual 异常 order that owes no stock must not claim it does.
	var buf strings.Builder
	if err := pages.ExecuteTemplate(&buf, "order_detail", orderPage{View: View{Title: "订单详情"}, Row: orderRow{ID: 1, Status: 6, Type: 2}, Action: "/admin/orders/1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "扣减库存") {
		t.Fatal("legacy manual 异常 order must not show the stock hint")
	}
}

// Repair pass 1 (C3-4): a 人工处理 order in 异常 cannot reach 已处理 through
// 处理失败 without taking its stock. The reviewers' probe: in_stock=0,
// buy_amount=2, 6->2 refused, then 6->5 and 5->2 used to succeed and leave
// status=2 with in_stock=0.
func TestOrderSaveManualShortNoDetourViaFailedIntegration(t *testing.T) {
	pool := testdb.Open(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type,in_stock) VALUES(1,1,'manual','','',10,2,0),(2,1,'auto','','',10,1,0);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,buy_amount,trade_no) VALUES
  (1,'M1',1,'manual','a@example.com','local',6,2,2,'wx-1'),
  (2,'M2',1,'manual','a@example.com','local',2,2,1,'wx-2'),
  (3,'A3',2,'auto','a@example.com','local',6,1,1,'wx-3');
 UPDATE orders SET info=E'库存不足\n充值账号:QQ1', stock_owed=true WHERE id=1;`)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	save := func(id int, status string) *httptest.ResponseRecorder {
		req := formReq(t, "POST", "/admin/orders/"+strconv.Itoa(id), url.Values{"title": {"t"}, "status": {status}})
		req.SetPathValue("id", strconv.Itoa(id))
		w := httptest.NewRecorder()
		s.orderSave(w, req, session{Name: "店主"})
		return w
	}
	state := func(id int) (status, stock int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT o.status, g.in_stock FROM orders o JOIN goods g ON g.id=o.goods_id WHERE o.id=$1`, id).Scan(&status, &stock); err != nil {
			t.Fatal(err)
		}
		return
	}
	if rec := save(1, "2"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), orderManualShort) {
		t.Fatalf("6->2 short: %d", rec.Code)
	}
	if rec := save(1, "5"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), orderManualShortFailed) {
		t.Fatalf("6->5: %d %s", rec.Code, rec.Body.String())
	}
	// The detour's second step can no longer be reached.
	if rec := save(1, "2"); rec.Code != http.StatusBadRequest {
		t.Fatalf("second 6->2: %d", rec.Code)
	}
	if st, stock := state(1); st != 6 || stock != 0 {
		t.Fatalf("manual 异常 order ended status=%d in_stock=%d", st, stock)
	}
	// After restocking the only exit still takes the stock.
	pool.Exec(ctx, `UPDATE goods SET in_stock=2 WHERE id=1`)
	if rec := save(1, "2"); rec.Code != http.StatusFound {
		t.Fatalf("restocked 6->2: %d", rec.Code)
	}
	if st, stock := state(1); st != 2 || stock != 0 {
		t.Fatalf("restocked 6->2 status=%d in_stock=%d", st, stock)
	}
	// An ordinary 人工处理 order (stock taken at payment) may still fail and
	// be put back without any stock arithmetic.
	if rec := save(2, "5"); rec.Code != http.StatusFound {
		t.Fatalf("2->5: %d", rec.Code)
	}
	if rec := save(2, "2"); rec.Code != http.StatusFound {
		t.Fatalf("5->2: %d", rec.Code)
	}
	if st, stock := state(2); st != 2 || stock != 0 {
		t.Fatalf("2->5->2 status=%d in_stock=%d", st, stock)
	}
	// Auto-delivery orders keep 6 -> 5 (it releases their reserved cards).
	if rec := save(3, "5"); rec.Code != http.StatusFound {
		t.Fatalf("type 1 6->5: %d", rec.Code)
	}
	for _, c := range []struct {
		typ, from, to int
		want          bool
	}{{2, 6, 5, true}, {1, 6, 5, false}, {2, 2, 5, false}, {2, 6, 2, false}, {2, 5, 5, false}} {
		if got := manualShortToFailed(c.typ, c.from, c.to); got != c.want {
			t.Errorf("manualShortToFailed(%d,%d->%d)=%v", c.typ, c.from, c.to, got)
		}
	}
}

// Repair pass 2: the hand-set 异常 guard applies to 人工处理 orders only. A
// 自动发卡 order moved 6 -> 5 (which releases its reserved cards) must be able
// to go back to 6 so that 重新发货 is offered again; otherwise the owner would
// hand out cards via info without ever marking them sold.
func TestOrderSaveAutoOrderBackToShortIntegration(t *testing.T) {
	pool := testdb.Open(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO goods_group(id,gp_name) VALUES(1,'g');
 INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,type,in_stock) VALUES(1,1,'auto','','',10,1,0),(2,1,'manual','','',10,2,5);
 INSERT INTO orders(id,order_sn,goods_id,title,email,buy_ip,status,type,buy_amount,trade_no) VALUES
  (1,'A1',1,'auto','a@example.com','local',6,1,1,'wx-1'),
  (2,'M2',2,'manual','a@example.com','local',5,2,1,'wx-2');
 INSERT INTO carmis(id,goods_id,carmi,status,reserved_order_id) VALUES(1,1,'c',1,1);`); err != nil {
		t.Fatal(err)
	}
	s := &Server{poolFn: func() *pgxpool.Pool { return pool }}
	save := func(id int, status string) *httptest.ResponseRecorder {
		req := formReq(t, "POST", "/admin/orders/"+strconv.Itoa(id), url.Values{"title": {"t"}, "status": {status}})
		req.SetPathValue("id", strconv.Itoa(id))
		w := httptest.NewRecorder()
		s.orderSave(w, req, session{Name: "店主"})
		return w
	}
	status := func(id int) (st int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, id).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return
	}
	detail := func(id int) string {
		t.Helper()
		req := httptest.NewRequest("GET", "/admin/orders/"+strconv.Itoa(id), nil)
		req.SetPathValue("id", strconv.Itoa(id))
		w := httptest.NewRecorder()
		s.orderDetail(w, req, session{Name: "店主"})
		if w.Code != http.StatusOK {
			t.Fatalf("detail %d: %d %s", id, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	redeliverForm := `action="/admin/orders/1/redeliver"`

	if !strings.Contains(detail(1), redeliverForm) {
		t.Fatal("status-6 auto order must offer 重新发货")
	}
	// 6 -> 5 releases the reserved card and hides 重新发货.
	if rec := save(1, "5"); rec.Code != http.StatusFound {
		t.Fatalf("auto 6->5: %d %s", rec.Code, rec.Body.String())
	}
	var released bool
	if err := pool.QueryRow(ctx, `SELECT reserved_order_id IS NULL FROM carmis WHERE id=1`).Scan(&released); err != nil || !released {
		t.Fatalf("6->5 did not release the card: %v %v", released, err)
	}
	if strings.Contains(detail(1), redeliverForm) {
		t.Fatal("status-5 order must not offer 重新发货")
	}
	// 5 -> 6 is allowed again for 自动发卡, and 重新发货 comes back.
	if rec := save(1, "6"); rec.Code != http.StatusFound {
		t.Fatalf("auto 5->6: %d %s", rec.Code, rec.Body.String())
	}
	if st := status(1); st != 6 {
		t.Fatalf("auto 5->6 left status=%d", st)
	}
	if !strings.Contains(detail(1), redeliverForm) {
		t.Fatal("auto order back in 异常 must offer 重新发货 again")
	}
	// A 人工处理 order still cannot be set to 异常 by hand.
	if rec := save(2, "6"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), orderNoManualShort) {
		t.Fatalf("manual 5->6 must be refused: %d", rec.Code)
	}
	if st := status(2); st != 5 {
		t.Fatalf("refused manual 5->6 left status=%d", st)
	}

	for _, c := range []struct {
		typ, from, to int
		want          bool
	}{{2, 5, 6, true}, {2, 2, 6, true}, {2, 3, 6, true}, {2, 6, 6, false}, {1, 5, 6, false}, {1, 2, 6, false}, {2, 6, 2, false}} {
		if got := manualToShort(c.typ, c.from, c.to); got != c.want {
			t.Errorf("manualToShort(%d,%d->%d)=%v", c.typ, c.from, c.to, got)
		}
	}
}
