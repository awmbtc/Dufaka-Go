package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dufaka/internal/pay"
	"dufaka/internal/store"
)

// captureLog routes the standard logger into a buffer for the test's lifetime.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

// A-1 (#4): create() stores netx.ClientIP: the proxy's X-Real-IP when the peer is local,
// the peer itself when the header is garbage or over-long, and never a SQLSTATE error.
func TestCreateOrderStoresClientIP(t *testing.T) {
	app, _ := auditOrder(t)
	// The order pays with cldx, which checkout only accepts while the wallet is configured (A3-10).
	app.wallet.Secret, app.wallet.MerchantID = "test-only", "shop-test"
	if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(2,1,'manual','','',10,50,2)`); err != nil {
		t.Fatal(err)
	}
	h := installed(t, app)
	create := func(remote, real string) (string, *httptest.ResponseRecorder) {
		req := httptest.NewRequest("POST", "http://shop.test/create-order", strings.NewReader("gid=2&payway=1&by_amount=1&email=ip%40example.com"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = remote
		if real != "" {
			req.Header.Set("X-Real-IP", real)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return strings.TrimPrefix(w.Header().Get("Location"), "/bill/"), w
	}
	cases := []struct{ remote, real, want string }{
		{"127.0.0.1:43210", "198.51.100.7", "198.51.100.7"},
		{"127.0.0.1:43211", strings.Repeat("1", 60), "127.0.0.1"},
		{"127.0.0.1:43212", "not an ip <script>", "127.0.0.1"},
		{"203.0.113.9:43213", "198.51.100.7", "203.0.113.9"},
	}
	for _, c := range cases {
		sn, w := create(c.remote, c.real)
		if w.Code != http.StatusFound || sn == "" {
			t.Fatalf("remote=%q real=%q: create answered %d %q", c.remote, c.real, w.Code, w.Body.String())
		}
		if body := w.Body.String(); strings.Contains(body, "SQLSTATE") || strings.Contains(body, "buy_ip") {
			t.Fatalf("remote=%q real=%q: database error surfaced to the buyer: %s", c.remote, c.real, body)
		}
		var got string
		if err := app.live().Pool.QueryRow(context.Background(), `SELECT buy_ip FROM orders WHERE order_sn=$1`, sn).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Fatalf("remote=%q real=%q: buy_ip %q want %q", c.remote, c.real, got, c.want)
		}
	}
}

// fakeClock is a mutex-guarded clock for receiptThrottle tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// A-6 (#18): the window is 10s, stamped after the fetch returns; concurrent callers share
// one fetch and only the one that started it reports fetched; a genuine wallet failure is
// cached; a cancellation error from the fetch itself is not.
func TestReceiptThrottle(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	th := newReceiptThrottle()
	th.now = clock.Now
	var hits atomic.Int32
	fetch := func(context.Context) (pay.Receipt, error) {
		hits.Add(1)
		return pay.Receipt{Paid: true, Amount: 7}, nil
	}
	ctx := context.Background()
	if th.window != 10*time.Second || receiptWindow != 10*time.Second {
		t.Fatalf("window %s", th.window)
	}
	// Three cashier polls 5s apart (0s, 5s, 9s) hit the wallet once; only the first fetched.
	for i, step := range []time.Duration{0, 5 * time.Second, 4 * time.Second} {
		clock.Advance(step)
		r, fetched, err := th.get(ctx, "SN1", fetch)
		if err != nil || !r.Paid || r.Amount != 7 || fetched != (i == 0) {
			t.Fatalf("poll %d: %+v fetched=%v %v", i, r, fetched, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("wallet asked %d times inside the window", hits.Load())
	}
	clock.Advance(time.Second) // 10s after the stamp: expired
	if _, fetched, err := th.get(ctx, "SN1", fetch); err != nil || !fetched || hits.Load() != 2 {
		t.Fatalf("expired entry not refreshed: %v fetched=%v hits=%d", err, fetched, hits.Load())
	}

	// A slow wallet (2s) followed by polls: the stamp is taken after the answer, so a poll
	// 11s after the request started (9s after the answer) is still served from the cache.
	hits.Store(0)
	slow := func(context.Context) (pay.Receipt, error) {
		hits.Add(1)
		clock.Advance(2 * time.Second)
		return pay.Receipt{Paid: false}, nil
	}
	if _, _, err := th.get(ctx, "SN2", slow); err != nil {
		t.Fatal(err)
	}
	clock.Advance(9 * time.Second)
	if _, _, err := th.get(ctx, "SN2", slow); err != nil || hits.Load() != 1 {
		t.Fatalf("slow wallet re-asked: hits=%d", hits.Load())
	}
	clock.Advance(time.Second)
	if _, _, err := th.get(ctx, "SN2", slow); err != nil || hits.Load() != 2 {
		t.Fatalf("window did not expire after 10s: hits=%d", hits.Load())
	}

	// Two concurrent polls share one in-flight fetch; exactly one reports fetched.
	hits.Store(0)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	blocking := func(context.Context) (pay.Receipt, error) {
		hits.Add(1)
		entered <- struct{}{}
		<-release
		return pay.Receipt{Paid: true, Amount: 9}, nil
	}
	var wg sync.WaitGroup
	results := make([]pay.Receipt, 2)
	fetchedBy := make([]bool, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], fetchedBy[i], errs[i] = th.get(ctx, "SN3", blocking)
		}(i)
	}
	<-entered // the leader is inside fetch; the follower must now be waiting, not fetching
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Amount != 9 {
			t.Fatalf("caller %d: %+v %v", i, results[i], errs[i])
		}
	}
	if hits.Load() != 1 || fetchedBy[0] == fetchedBy[1] {
		t.Fatalf("concurrent polls: hits=%d fetched=%v", hits.Load(), fetchedBy)
	}

	// A fetch that itself fails with a cancellation says nothing about the wallet and is
	// not cached.
	hits.Store(0)
	cancelling := func(context.Context) (pay.Receipt, error) {
		hits.Add(1)
		return pay.Receipt{}, fmt.Errorf("wallet: %w", context.Canceled)
	}
	if _, _, err := th.get(ctx, "SN4", cancelling); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fetch got %v", err)
	}
	if r, fetched, err := th.get(ctx, "SN4", fetch); err != nil || !r.Paid || !fetched || hits.Load() != 2 {
		t.Fatalf("cancelled answer poisoned the cache: %+v %v hits=%d", r, err, hits.Load())
	}
	// A caller that is already cancelled never fetches at all.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, fetched, err := th.get(cancelled, "SN5", fetch); !errors.Is(err, context.Canceled) || fetched || hits.Load() != 2 {
		t.Fatalf("dead caller fetched: %v fetched=%v hits=%d", err, fetched, hits.Load())
	}
	// A genuine wallet failure is cached like any other answer.
	hits.Store(0)
	failing := func(context.Context) (pay.Receipt, error) {
		hits.Add(1)
		return pay.Receipt{}, fmt.Errorf("钱包返回 502")
	}
	th.get(ctx, "SN6", failing)
	th.get(ctx, "SN6", failing)
	if hits.Load() != 1 {
		t.Fatalf("wallet failure not throttled: %d", hits.Load())
	}
}

// A3-6: a caller whose context ends mid-fetch returns at once (still counted as the one
// that fetched), the detached fetch fills the cache, and the next poll inside the window
// costs no wallet call. The fetch runs on a context that the caller's cancellation does
// not reach, bounded by its own timeout.
func TestReceiptThrottleAbortedCallerFillsCache(t *testing.T) {
	th := newReceiptThrottle()
	var hits atomic.Int32
	release := make(chan struct{})
	started := make(chan context.Context, 1)
	fetch := func(fctx context.Context) (pay.Receipt, error) {
		hits.Add(1)
		started <- fctx
		<-release
		return pay.Receipt{Paid: true, Amount: 5}, fctx.Err()
	}
	caller, cancel := context.WithCancel(context.Background())
	type result struct {
		fetched bool
		err     error
	}
	out := make(chan result, 1)
	go func() {
		_, fetched, err := th.get(caller, "SN7", fetch)
		out <- result{fetched, err}
	}()
	fctx := <-started
	if dl, ok := fctx.Deadline(); !ok || time.Until(dl) > receiptFetchTimeout || time.Until(dl) < receiptFetchTimeout-time.Second {
		t.Fatalf("fetch deadline %v %v", dl, ok)
	}
	cancel()
	select {
	case r := <-out:
		if !errors.Is(r.err, context.Canceled) || !r.fetched {
			t.Fatalf("aborted caller: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("aborted caller did not return on its own context")
	}
	if fctx.Err() != nil {
		t.Fatal("the caller's cancellation reached the wallet fetch")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		r, fetched, err := th.get(context.Background(), "SN7", fetch)
		if err == nil && r.Paid && r.Amount == 5 && !fetched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache not filled after the aborted poll: %+v fetched=%v %v", r, fetched, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() != 1 {
		t.Fatalf("wallet asked %d times", hits.Load())
	}
}

// A3-6: a panic inside the wallet fetch is contained: the leader gets an error, the
// waiters are released, nothing is cached and a later caller fetches again.
func TestReceiptThrottlePanicDoesNotWedge(t *testing.T) {
	logs := captureLog(t)
	th := newReceiptThrottle()
	entered := make(chan struct{})
	release := make(chan struct{})
	boom := func(context.Context) (pay.Receipt, error) {
		close(entered)
		<-release
		panic("wallet client bug")
	}
	errs := make(chan error, 2)
	go func() { _, _, err := th.get(context.Background(), "SN8", boom); errs <- err }()
	<-entered
	go func() {
		_, _, err := th.get(context.Background(), "SN8", func(context.Context) (pay.Receipt, error) {
			return pay.Receipt{Paid: true, Amount: 3}, nil
		})
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond) // the second caller is now waiting on the first fetch
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-errs:
		case <-time.After(2 * time.Second):
			t.Fatal("caller wedged behind a panicking fetch")
		}
	}
	var hits atomic.Int32
	r, fetched, err := th.get(context.Background(), "SN8", func(context.Context) (pay.Receipt, error) {
		hits.Add(1)
		return pay.Receipt{Paid: true, Amount: 4}, nil
	})
	if err != nil || r.Amount != 4 || !fetched || hits.Load() != 1 {
		// The waiter may have become the next leader and cached Amount 3; accept that too.
		if !(err == nil && r.Amount == 3 && !fetched) {
			t.Fatalf("after the panic: %+v fetched=%v %v", r, fetched, err)
		}
	}
	if !strings.Contains(logs.String(), "查询钱包时出现异常") || !strings.Contains(logs.String(), "SN8") {
		t.Fatalf("panic not logged: %q", logs.String())
	}
}

// A-6: the cashier poll (/check-order-status) serves the cached receipt inside the window
// without a wallet call, using real time so nothing about the clock is faked.
func TestCldxPollServesCachedReceipt(t *testing.T) {
	app, o := auditOrder(t)
	var hits atomic.Int32
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"paid":false,"amount":15000,"refunded":0}`)
	}))
	defer wallet.Close()
	app.wallet.Base, app.wallet.Secret, app.wallet.MerchantID = wallet.URL, "test-only", "shop-test"
	if _, _, err := app.live().LockCldxQuote(context.Background(), o.SN, 15000, time.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	h := installed(t, app)
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/check-order-status/"+o.SN, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":400000`) {
			t.Fatalf("poll %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("three polls asked the wallet %d times", hits.Load())
	}
}

// cldxOrders creates n unpaid cldx orders with locked quotes and returns their numbers.
func cldxOrders(t *testing.T, app *App, n int) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := app.live().Pool.Exec(ctx, `INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(9,1,'manual','','',10,1000,2)`); err != nil {
		t.Fatal(err)
	}
	var sns []string
	for i := 0; i < n; i++ {
		o, err := app.live().CreateOrder(ctx, store.CreateInput{GID: 9, PayID: 1, Amount: 1, Email: "pool@example.com"}, store.Site{})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := app.live().LockCldxQuote(ctx, o.SN, 15000, time.Now().Add(10*time.Minute).Unix()); err != nil {
			t.Fatal(err)
		}
		sns = append(sns, o.SN)
	}
	return sns
}

func polledCount(t *testing.T, app *App) int {
	t.Helper()
	var n int
	if err := app.live().Pool.QueryRow(context.Background(), `SELECT count(*) FROM orders WHERE cldx_polled_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A-4 (#16): a wallet that hangs past the wallet client's own timeout is a real failure
// and is logged with the order number, even though the error wraps a deadline.
func TestSyncCldxLogsHangingWallet(t *testing.T) {
	app, o := auditOrder(t)
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer wallet.Close()
	app.wallet.Base, app.wallet.Secret, app.wallet.MerchantID = wallet.URL, "test-only", "shop-test"
	app.wallet.HTTP = &http.Client{Timeout: 150 * time.Millisecond}
	if _, _, err := app.live().LockCldxQuote(context.Background(), o.SN, 15000, time.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	app.SyncCldx(context.Background())
	if !strings.Contains(logs.String(), "cldx 订单 "+o.SN) || !strings.Contains(logs.String(), "查询钱包到账失败") {
		t.Fatalf("hanging wallet not logged with the order number: %q", logs.String())
	}
	if strings.Contains(logs.String(), "test-only") {
		t.Fatalf("secret leaked into the log: %q", logs.String())
	}
	// The order still counts as polled so the rotation moves on.
	if polledCount(t, app) != 1 {
		t.Fatal("hanging order was not stamped")
	}
	// The same failure through the cashier poll, with the caller's context live, is logged too.
	logs.Reset()
	app.receipts.forget(o.SN)
	req := httptest.NewRequest("GET", "/check-order-status/"+o.SN, nil)
	req.SetPathValue("sn", o.SN)
	w := httptest.NewRecorder()
	app.poll(w, req)
	if !strings.Contains(w.Body.String(), `"code":400000`) || !strings.Contains(logs.String(), o.SN) {
		t.Fatalf("poll failure not logged: %s %q", w.Body.String(), logs.String())
	}
	// But a caller that already went away stays silent.
	logs.Reset()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	logCldx(gone, o.SN, "查询钱包到账失败", fmt.Errorf("x: %w", context.DeadlineExceeded))
	if logs.Len() != 0 {
		t.Fatalf("cancelled caller logged: %q", logs.String())
	}
}

// A-4 (#16) + A-5: when the tick deadline cuts the pass, the remainder is logged, the
// orders that were asked about are stamped, and the next tick starts with the others.
func TestSyncCldxDeadlineRotates(t *testing.T) {
	app, _ := auditOrder(t)
	var mu sync.Mutex
	var asked []string
	var slow atomic.Bool
	unblock := make(chan struct{})
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("order"))
		mu.Unlock()
		if slow.Load() {
			select {
			case <-r.Context().Done():
				return
			case <-unblock:
			case <-time.After(2 * time.Second):
			}
		}
		fmt.Fprint(w, `{"paid":false,"amount":15000,"refunded":0}`)
	}))
	defer wallet.Close()
	app.wallet.Base, app.wallet.Secret, app.wallet.MerchantID = wallet.URL, "test-only", "shop-test"
	sns := cldxOrders(t, app, 2)
	oldTimeout, oldWorkers := syncCldxTimeout, syncCldxWorkers
	syncCldxTimeout, syncCldxWorkers = 300*time.Millisecond, 1
	t.Cleanup(func() { syncCldxTimeout, syncCldxWorkers = oldTimeout, oldWorkers })
	logs := captureLog(t)
	slow.Store(true)
	app.SyncCldx(context.Background())
	mu.Lock()
	first := append([]string(nil), asked...)
	mu.Unlock()
	if len(first) != 1 {
		t.Fatalf("first tick asked about %v", first)
	}
	if !strings.Contains(logs.String(), "本轮超过") || !strings.Contains(logs.String(), "剩余 1 单") || !strings.Contains(logs.String(), "用时") {
		t.Fatalf("deadline cut not logged: %q", logs.String())
	}
	if polledCount(t, app) != 1 {
		t.Fatal("cut-off order was not stamped")
	}
	other := sns[0]
	if other == first[0] {
		other = sns[1]
	}
	// The cut-off fetch runs detached from the tick (A3-6): let it answer so its result is
	// cached rather than still in flight when the next tick reaches that order.
	slow.Store(false)
	close(unblock)
	waitIdle(t, app.receipts, first[0])
	// Next tick: the never-polled order goes first.
	logs.Reset()
	app.SyncCldx(context.Background())
	mu.Lock()
	second := append([]string(nil), asked[1:]...)
	mu.Unlock()
	if len(second) == 0 || second[0] != other {
		t.Fatalf("second tick did not start with the other order: %v (first was %s)", second, first[0])
	}
	if polledCount(t, app) != 2 {
		t.Fatal("second order was not stamped")
	}
	if strings.Contains(logs.String(), "本轮超过") {
		t.Fatalf("complete tick logged a cut: %q", logs.String())
	}
}

// A-5 (#3): the pass runs through a bounded pool (≤ 8 in flight), polls every waiting
// order and stamps each one.
func TestSyncCldxWorkerPool(t *testing.T) {
	app, _ := auditOrder(t)
	var inFlight, maxInFlight atomic.Int32
	var mu sync.Mutex
	asked := map[string]int{}
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		asked[r.URL.Query().Get("order")]++
		mu.Unlock()
		fmt.Fprint(w, `{"paid":false,"amount":15000,"refunded":0}`)
	}))
	defer wallet.Close()
	app.wallet.Base, app.wallet.Secret, app.wallet.MerchantID = wallet.URL, "test-only", "shop-test"
	sns := cldxOrders(t, app, 24)
	logs := captureLog(t)
	start := time.Now()
	app.SyncCldx(context.Background())
	elapsed := time.Since(start)
	if got := maxInFlight.Load(); got > 8 || got < 2 {
		t.Fatalf("max concurrency %d (want 2..8)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, sn := range sns {
		if asked[sn] != 1 {
			t.Fatalf("order %s asked %d times", sn, asked[sn])
		}
	}
	if polledCount(t, app) != len(sns) {
		t.Fatalf("stamped %d of %d", polledCount(t, app), len(sns))
	}
	if elapsed > time.Duration(len(sns))*40*time.Millisecond {
		t.Fatalf("pass took %s, looks serial", elapsed)
	}
	if strings.Contains(logs.String(), "本轮超过") || strings.Contains(logs.String(), "失败") {
		t.Fatalf("unexpected log: %q", logs.String())
	}
}

// A-7 (#23): trade_no = "cldx:<SN>:<from>", bounded to 200 characters; junk in optional
// fields and an over-long payer still complete the order.
func TestCldxTradeNoFromWallet(t *testing.T) {
	if got, ok := cldxTradeNo("ABCDEF0123456789", ""); got != "cldx:ABCDEF0123456789" || !ok {
		t.Fatalf("empty from: %q %v", got, ok)
	}
	if got, ok := cldxTradeNo("ABCDEF0123456789", " payer "); got != "cldx:ABCDEF0123456789:payer" || !ok {
		t.Fatalf("from: %q %v", got, ok)
	}
	if got, ok := cldxTradeNo("ABCDEF0123456789", strings.Repeat("f", 300)); got != "cldx:ABCDEF0123456789" || ok {
		t.Fatalf("long from: %q %v", got, ok)
	}
	// A3-7: at most 150 characters of [A-Za-z0-9._@-]; anything else falls back to the SN.
	exact := strings.Repeat("f", 150)
	if got, ok := cldxTradeNo("ABCDEF0123456789", exact); got != "cldx:ABCDEF0123456789:"+exact || !ok || len(got) > tradeNoLimit {
		t.Fatalf("boundary: %d %v", len(got), ok)
	}
	if got, ok := cldxTradeNo("ABCDEF0123456789", exact+"f"); got != "cldx:ABCDEF0123456789" || ok {
		t.Fatalf("boundary+1: %q %v", got, ok)
	}
	if got, ok := cldxTradeNo("ABCDEF0123456789", "Cldx1.payer_x@wallet-2"); got != "cldx:ABCDEF0123456789:Cldx1.payer_x@wallet-2" || !ok {
		t.Fatalf("allowed characters: %q %v", got, ok)
	}
	for _, bad := range []string{"a\u0000b", ":", "a:b", "payer name", "付款人", "a\nb", "<x>"} {
		if got, ok := cldxTradeNo("ABCDEF0123456789", bad); got != "cldx:ABCDEF0123456789" || ok {
			t.Fatalf("from %q: %q %v", bad, got, ok)
		}
	}

	for name, body := range map[string]string{
		"junk":     `{"to":"shop-test","order":"x","from":123,"at":456,"amount":15000,"refunded":0,"paid":true}`,
		"longFrom": `{"from":"` + strings.Repeat("f", 300) + `","amount":15000,"refunded":0,"paid":true,"at":"never"}`,
	} {
		t.Run(name, func(t *testing.T) {
			app, o := auditOrder(t)
			wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer wallet.Close()
			app.wallet.Base, app.wallet.Secret, app.wallet.MerchantID = wallet.URL, "test-only", "shop-test"
			if _, _, err := app.live().LockCldxQuote(context.Background(), o.SN, 15000, time.Now().Add(10*time.Minute).Unix()); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("GET", "/check-order-status/"+o.SN, nil)
			req.SetPathValue("sn", o.SN)
			w := httptest.NewRecorder()
			app.poll(w, req)
			if !strings.Contains(w.Body.String(), `"code":200`) {
				t.Fatalf("payment not picked up: %s", w.Body.String())
			}
			done, _ := app.live().OrderBySN(context.Background(), o.SN)
			if done.Status != 4 || done.Info != "AUDIT-SECRET" || done.TradeNo != "cldx:"+o.SN {
				t.Fatalf("delivered order: %+v", done)
			}
		})
	}
}

// A-9 (#6): /cldx/pay refuses malformed order numbers before touching the database, a
// disabled wallet, a closed pay row and an order that is no longer payable.
func TestCldxLaunchRejects(t *testing.T) {
	// No database at all: a malformed number must be refused before any lookup (a lookup
	// would dereference the nil store and panic).
	noDB, err := New(nil, "http://shop.test", "")
	if err != nil {
		t.Fatal(err)
	}
	noDB.wallet.Secret, noDB.wallet.MerchantID = "test-only", "shop-test"
	for _, sn := range []string{"", "abcdef0123456789", "ABCDEF012345678", "ABCDEF01234567890", "ABCDEFGHIJKLMNOP", "ABCDEF012345678%20", "%00BCDEF012345678", "ABCDEF0123456789%0A"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://shop.test/cldx/pay?order="+sn, nil)
		noDB.cldxLaunch(w, r)
		if w.Code != 404 {
			t.Fatalf("order=%q answered %d", sn, w.Code)
		}
	}
	if !validSN("ABCDEF0123456789") || validSN("abcdef0123456789") || validSN("ABCDEF012345678G") {
		t.Fatal("validSN shape")
	}

	app, o := auditOrder(t)
	exp := time.Now().Add(10 * time.Minute).Unix()
	if _, _, err := app.live().LockCldxQuote(context.Background(), o.SN, 15000, exp); err != nil {
		t.Fatal(err)
	}
	h := installed(t, app)
	get := func() int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/cldx/pay?order="+o.SN, nil))
		return w.Code
	}
	// Wallet disabled (no merchant secret): nothing to launch.
	app.wallet.Secret, app.wallet.MerchantID = "", "shop-test"
	if code := get(); code != 404 {
		t.Fatalf("disabled wallet answered %d", code)
	}
	app.wallet.Secret = "test-only"
	if code := get(); code != 302 {
		t.Fatalf("enabled wallet answered %d", code)
	}
	// Closed pay row: 404 even with a valid quote.
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE pays SET is_open=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if code := get(); code != 404 {
		t.Fatalf("closed channel answered %d", code)
	}
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE pays SET is_open=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	// A paid (status 2) order that still carries its quote must not relaunch payment.
	if _, err := app.live().Pool.Exec(context.Background(), `UPDATE orders SET status=2 WHERE order_sn=$1`, o.SN); err != nil {
		t.Fatal(err)
	}
	if code := get(); code != 404 {
		t.Fatalf("paid order answered %d", code)
	}
}

// A-11 (#1): golden output of the rich-text sanitizer (bluemonday UGC policy): links keep
// their href with rel="nofollow" (target is dropped), javascript: links lose the tag,
// images keep src/alt, every on* attribute and <script> vanish, an unclosed tag stays
// unclosed but harmless.
func TestRichGolden(t *testing.T) {
	in := `<p>说明 <a href="https://example.com/x?a=1&b=2" target="_blank">链接</a> <a href="javascript:alert(1)">坏链接</a></p>` +
		`<img src="/assets/brand/logo.svg" alt="logo" onerror="alert(1)">` +
		`<ul><li>一</li><li onclick="steal()">二</li></ul>` +
		`<script>alert(1)</script>` +
		`<div onclick="steal()">点我</div>` +
		`<b>未闭合 <i>斜体`
	want := `<p>说明 <a href="https://example.com/x?a=1&amp;b=2" rel="nofollow">链接</a> 坏链接</p>` +
		`<img src="/assets/brand/logo.svg" alt="logo">` +
		`<ul><li>一</li><li>二</li></ul>` +
		`<div>点我</div>` +
		`<b>未闭合 <i>斜体`
	if got := string(rich(in)); got != want {
		t.Fatalf("rich():\n got %s\nwant %s", got, want)
	}
}

// A-2 (#5): the customer sees only a generic message when the cashier is not configured;
// the field list goes to the log.
func TestWechatGatewayHidesConfigFromCustomers(t *testing.T) {
	for _, k := range []string{"WECHAT_PAY_APP_ID", "WECHAT_PAY_MCH_ID", "WECHAT_PAY_CERT_SERIAL_NO", "WECHAT_PAY_API_V3_KEY", "WECHAT_PAY_PRIVATE_KEY", "WECHAT_PAY_PLATFORM_KEY", "WECHAT_PAY_PLATFORM_SERIAL", "WECHAT_PAY_PUBLIC_KEY_ID"} {
		t.Setenv(k, "")
	}
	app, _ := auditOrder(t)
	if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO pays(id,pay_name,pay_check,pay_method,pay_client,merchant_id,merchant_key,merchant_pem,pay_handleroute) VALUES(2,'微信扫码','wescan',1,3,'1900000000','','','/pay/wepay')`); err != nil {
		t.Fatal(err)
	}
	o, err := app.live().CreateOrder(context.Background(), store.CreateInput{GID: 1, PayID: 2, Amount: 1, Email: "wx@example.com"}, store.Site{})
	if err != nil {
		// The single card is reserved by auditOrder's order; a manual product works too.
		if _, err := app.live().Pool.Exec(context.Background(), `INSERT INTO goods(id,group_id,gd_name,gd_description,gd_keywords,actual_price,in_stock,type) VALUES(3,1,'manual','','',10,5,2)`); err != nil {
			t.Fatal(err)
		}
		if o, err = app.live().CreateOrder(context.Background(), store.CreateInput{GID: 3, PayID: 2, Amount: 1, Email: "wx@example.com"}, store.Site{}); err != nil {
			t.Fatal(err)
		}
	}
	h := installed(t, app)
	logs := captureLog(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://shop.test/pay/wescan/scan/"+o.SN, nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "微信支付暂未配置完整，请联系店主") {
		t.Fatalf("gateway: %d %s", w.Code, body)
	}
	if strings.Contains(body, "WECHAT_PAY") || strings.Contains(body, "缺少") || strings.Contains(body, "商户密钥") {
		t.Fatalf("configuration details shown to the customer: %s", body)
	}
	if !strings.Contains(logs.String(), "WECHAT_PAY_APP_ID") || !strings.Contains(logs.String(), "商户密钥") {
		t.Fatalf("details missing from the log: %q", logs.String())
	}
}

// waitIdle waits until no wallet fetch for sn is in flight.
func waitIdle(t *testing.T, th *receiptThrottle, sn string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		th.mu.Lock()
		e := th.last[sn]
		busy := e != nil && e.done != nil
		th.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fetch for %s still in flight", sn)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
