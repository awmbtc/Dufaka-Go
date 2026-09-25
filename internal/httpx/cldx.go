package httpx

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"

	"dufaka/internal/pay"
	"dufaka/internal/store"
)

// receiptThrottle keeps the last wallet answer per order so a cashier page polling every
// five seconds (templates/cldxpay.html), or several tabs of the same order, do not turn
// into one wallet call each. The window is 10 seconds, measured from the moment the wallet
// answered, so it always spans the next poll of a single tab even after a slow fetch. One
// fetch per order is in flight at a time: concurrent callers wait for it and share the
// result. The fetch runs detached from the caller (context.WithoutCancel, bounded by its
// own receiptFetchTimeout), so a closed tab or an ended tick still fills the cache for
// the next poll, while every caller returns as soon as its own context ends. The
// 30-second background SyncCldx shares the cache: its tick is far slower than the window,
// so sharing only ever saves a call that a poll made a moment ago.
type receiptThrottle struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time
	last   map[string]*receiptEntry
}

type receiptEntry struct {
	done chan struct{} // non-nil while a fetch is in flight
	at   time.Time
	item pay.Receipt
	err  error
}

const (
	receiptWindow       = 10 * time.Second
	receiptFetchTimeout = 8 * time.Second
)

func newReceiptThrottle() *receiptThrottle {
	return &receiptThrottle{window: receiptWindow, now: time.Now, last: map[string]*receiptEntry{}}
}

// get returns the cached answer for sn inside the window, otherwise starts fetch (once,
// however many callers arrive meanwhile) and caches its result, errors included, so a
// wallet outage is not hammered either. fetched reports whether this caller started the
// wallet call (and so should record the poll); a caller served from the cache or by
// another caller's fetch gets false. ctx is the caller's context: when it ends the caller
// gets its error back at once, and the fetch carries on to fill the cache.
func (t *receiptThrottle) get(ctx context.Context, sn string, fetch func(context.Context) (pay.Receipt, error)) (item pay.Receipt, fetched bool, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return pay.Receipt{}, false, err
		}
		t.mu.Lock()
		e := t.last[sn]
		if e != nil && e.done == nil && t.now().Sub(e.at) < t.window {
			t.mu.Unlock()
			return e.item, false, e.err
		}
		if e != nil && e.done != nil {
			wait := e.done
			t.mu.Unlock()
			select {
			case <-wait:
				continue // the leader cached its answer, or dropped it; re-evaluate
			case <-ctx.Done():
				return pay.Receipt{}, false, ctx.Err()
			}
		}
		e = &receiptEntry{done: make(chan struct{})}
		t.last[sn] = e
		done := e.done
		t.mu.Unlock()
		go t.lead(ctx, sn, e, fetch)
		select {
		case <-done:
			t.mu.Lock()
			item, err = e.item, e.err
			t.mu.Unlock()
			return item, true, err
		case <-ctx.Done():
			return pay.Receipt{}, true, ctx.Err()
		}
	}
}

// lead runs one wallet fetch for e. Its bookkeeping is deferred, so whatever happens —
// including a panic inside fetch, which is recovered and reported as an error — done is
// closed and waiters are released; an answer that says nothing about the wallet (a panic,
// a cancellation) is dropped instead of cached.
func (t *receiptThrottle) lead(ctx context.Context, sn string, e *receiptEntry, fetch func(context.Context) (pay.Receipt, error)) {
	var item pay.Receipt
	var err error
	finished := false
	defer func() {
		if !finished {
			log.Printf("cldx 订单 %s：查询钱包时出现异常: %v", sn, recover())
			item, err = pay.Receipt{}, errors.New("查询钱包时出现异常")
		}
		t.mu.Lock()
		e.item, e.err = item, err
		if t.last[sn] == e {
			if !finished || errors.Is(err, context.Canceled) {
				delete(t.last, sn)
			} else {
				e.at = t.now()
				t.prune()
			}
		}
		close(e.done)
		e.done = nil
		t.mu.Unlock()
	}()
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), receiptFetchTimeout)
	defer cancel()
	item, err = fetch(fctx)
	finished = true
}

// prune drops expired entries once the map grows large; called with mu held.
func (t *receiptThrottle) prune() {
	if len(t.last) <= 4096 {
		return
	}
	now := t.now()
	for k, e := range t.last {
		if e.done == nil && now.Sub(e.at) >= t.window {
			delete(t.last, k)
		}
	}
}

// forget drops the cached answer, used once an order is delivered so nothing stale lingers.
func (t *receiptThrottle) forget(sn string) {
	t.mu.Lock()
	delete(t.last, sn)
	t.mu.Unlock()
}

func (a *App) cldxPay(w http.ResponseWriter, r *http.Request, o store.Order) {
	if !a.wallet.Enabled() {
		a.fail(w, r, "cldx 支付还没有配置")
		return
	}
	ttl := time.Duration(a.live().Site(r.Context()).ExpireMin) * time.Minute
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	amount, err := a.live().CldxMinor(r.Context(), o.SN)
	if err != nil {
		a.fail(w, r, "cldx 报价失败")
		return
	}
	if amount <= 0 {
		amount, err = a.wallet.Quote(r.Context(), int64(o.Actual))
	}
	if err != nil || amount <= 0 {
		a.fail(w, r, "cldx 报价失败")
		return
	}
	amount, exp, err := a.live().LockCldxQuote(r.Context(), o.SN, amount, o.Created.Add(ttl).Unix())
	if err != nil {
		a.fail(w, r, "订单状态已改变，请重新查询")
		return
	}
	if exp <= time.Now().Unix() {
		a.fail(w, r, "订单已过期")
		return
	}
	// The QR only carries the order number; /cldx/pay rebuilds every wallet parameter from
	// the locked quote in the database, so a crafted link can never change payee or amount.
	link := a.base + "/cldx/pay?" + url.Values{"order": {o.SN}}.Encode()
	// This fixed scheme and encoded parameters are constructed here, never from arbitrary input.
	appLink := template.URL("clodex://pay?" + a.cldxQuery(o.SN, amount, exp))
	direct := appClient(r)
	var png string
	if !direct {
		raw, err := qrcode.Encode(link, qrcode.Medium, 256)
		if err != nil {
			a.fail(w, r, "二维码生成失败")
			return
		}
		png = encodePNG(raw)
	}
	a.view(w, "cldxpay.html", map[string]any{
		"Title": "cldx 支付", "Site": a.live().Site(r.Context()), "Order": o,
		"PayDeadline": exp, "Cldx": formatCldx(amount), "QR": png, "Direct": direct, "AppLink": appLink,
	})
}

// cldxQuery is the only place the wallet deep-link query is assembled: payee, amount and
// expiry always come from the shop's own configuration and the locked quote.
func (a *App) cldxQuery(sn string, amount, exp int64) string {
	return url.Values{
		"to": {a.wallet.MerchantID}, "amount": {strconv.FormatInt(amount, 10)},
		"order": {sn}, "exp": {strconv.FormatInt(exp, 10)},
	}.Encode()
}

// cldxLaunch turns GET /cldx/pay?order=SN into the app deep link. It accepts nothing but
// the order number (plus clodex_app=1 for the cookie): the payee, amount and expiry are
// rebuilt from the locked quote, and anything unknown, unquoted or expired is 404 so the
// shop domain can never be used to vouch for an attacker's parameters.
func (a *App) cldxLaunch(w http.ResponseWriter, r *http.Request) {
	sn := r.URL.Query().Get("order")
	if !validSN(sn) || !a.wallet.Enabled() {
		http.NotFound(w, r)
		return
	}
	db := a.live()
	o, err := db.OrderBySN(r.Context(), sn)
	if err != nil || (o.Status != 1 && o.Status != -1) {
		http.NotFound(w, r)
		return
	}
	p, err := db.Pay(r.Context(), o.PayID)
	if err != nil || p.Check != "cldx" || p.Open != 1 {
		http.NotFound(w, r)
		return
	}
	amount, exp, err := db.CldxQuote(r.Context(), o.SN)
	if err != nil || amount <= 0 || exp <= 0 || exp <= time.Now().Unix() {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "clodex://pay?"+a.cldxQuery(o.SN, amount, exp), http.StatusFound)
}

// validSN reports whether sn has the exact shape store.newSN produces: 16 upper-case hex
// characters. Anything else is refused before the database is touched.
func validSN(sn string) bool {
	if len(sn) != 16 {
		return false
	}
	for i := 0; i < len(sn); i++ {
		c := sn[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func (a *App) syncCldxOrder(r *http.Request, o store.Order) store.Order {
	if !a.wallet.Enabled() || (o.Status != 1 && o.Status != -1) {
		return o
	}
	ctx := r.Context()
	db := a.live()
	p, err := db.Pay(ctx, o.PayID)
	if err != nil {
		logCldx(ctx, o.SN, "读取支付渠道失败", err)
		return o
	}
	if p.Check != "cldx" {
		return o
	}
	want, err := db.CldxMinor(ctx, o.SN)
	if err != nil {
		logCldx(ctx, o.SN, "读取 cldx 报价失败", err)
		return o
	}
	if want <= 0 {
		return o
	}
	item, fetched, err := a.receipts.get(ctx, o.SN, func(fctx context.Context) (pay.Receipt, error) {
		return a.wallet.Receipt(fctx, a.wallet.MerchantID, o.SN)
	})
	// Every wallet call this caller started (paid, not paid, or an error) counts as a poll
	// so the background pass rotates through the waiting set instead of re-asking about the
	// same head. The stamp is written even when ctx already ended mid-fetch: that is
	// exactly the order that must move to the back of the queue. An answer served from the
	// cache was stamped by whoever fetched it.
	if fetched {
		a.markPolled(ctx, db, o.SN)
	}
	if err != nil {
		logCldx(ctx, o.SN, "查询钱包到账失败", err)
		return o
	}
	if !item.Paid || item.Refunded != 0 {
		return o
	}
	if item.Amount != want {
		log.Printf("cldx 订单 %s：钱包到账 %d 与锁定报价 %d 不一致，不发货，请人工核对", o.SN, item.Amount, want)
		return o
	}
	tradeNo, kept := cldxTradeNo(o.SN, item.From)
	if !kept {
		tradeNoFromOnce.Do(func() {
			log.Printf("cldx 订单 %s：钱包返回的付款方地址格式不符，交易号只记录订单号（此提示只打印一次）", o.SN)
		})
	}
	if _, err := db.Complete(ctx, o.SN, o.Actual, tradeNo); err != nil {
		logCldx(ctx, o.SN, "到账后完成订单失败", err)
		return o
	}
	a.receipts.forget(o.SN)
	next, err := db.OrderBySNChecked(ctx, o.SN)
	if err != nil {
		logCldx(ctx, o.SN, "完成后重新读取订单失败", err)
		return o
	}
	return next
}

// markPolled stamps orders.cldx_polled_at now. It runs on a context detached from the
// caller's cancellation (bounded on its own) so a tick deadline or a closed cashier tab
// still leaves the rotation mark behind; only a database failure is logged.
func (a *App) markPolled(ctx context.Context, db *store.DB, sn string) {
	stamp, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := db.MarkCldxPolled(stamp, sn, time.Now().Unix()); err != nil {
		log.Printf("cldx 订单 %s：记录轮询时间失败: %v", sn, err)
	}
}

// tradeNoLimit is the width of orders.trade_no.
const tradeNoLimit = 200

// cldxPayer is what a payer address may look like to be recorded in orders.trade_no: at
// most 150 characters of letters, digits and . _ @ -, so "cldx:<SN>:<from>" stays well
// inside tradeNoLimit and neither ":" nor control characters can make it ambiguous.
var cldxPayer = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,150}$`)

var tradeNoFromOnce sync.Once

// cldxTradeNo builds the trade number recorded for a cldx payment: "cldx:<SN>:<from>" when
// the wallet reported a well-formed payer (cldxPayer), else "cldx:<SN>". The second result
// is false when a non-empty payer had to be dropped.
func cldxTradeNo(sn, from string) (string, bool) {
	base := "cldx:" + sn
	from = strings.TrimSpace(from)
	if from == "" {
		return base, true
	}
	if !cldxPayer.MatchString(from) {
		return base, false
	}
	return base + ":" + from, true
}

// logCldx reports a wallet or database failure for one order. It stays silent only when
// the CALLER's context is already done (the cashier tab closed, the tick deadline passed):
// that is the caller going away, not the wallet. A timeout raised by the wallet client's
// own 8-second limit happens while ctx is still live and is logged with the order number.
func logCldx(ctx context.Context, sn, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	log.Printf("cldx 订单 %s：%s: %v", sn, what, err)
}

// syncCldxTimeout bounds one background tick so a slow wallet cannot stall the poller;
// syncCldxWorkers is how many orders one tick asks the wallet about at the same time.
// Both are variables so tests can shrink them.
var (
	syncCldxTimeout = 25 * time.Second
	syncCldxWorkers = 8
)

// SyncCldx walks the waiting cldx orders once. The whole pass shares one deadline and a
// bounded worker pool; orders left over when the deadline hits are logged and, because
// every polled order was stamped (markPolled) and store.WaitingSNs serves the least
// recently polled first, they are the head of the next tick.
func (a *App) SyncCldx(ctx context.Context) {
	if !a.wallet.Enabled() {
		return
	}
	db := a.live()
	if db == nil {
		return
	}
	if ctx.Err() != nil {
		return // the caller was already gone: nothing was attempted, nothing to report
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, syncCldxTimeout)
	defer cancel()
	sns, err := db.WaitingSNs(ctx, "cldx")
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("cldx 同步：读取待支付订单时超时（%s）: %v", time.Since(start).Round(time.Millisecond), err)
		} else {
			log.Printf("cldx 同步：读取待支付订单失败: %v", err)
		}
		return
	}
	workers := syncCldxWorkers
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	started := 0
	for _, sn := range sns {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		started++
		wg.Add(1)
		go func(sn string) {
			defer wg.Done()
			defer func() { <-sem }()
			o, err := db.OrderBySNChecked(ctx, sn)
			if err != nil {
				logCldx(ctx, sn, "读取订单失败", err)
				return
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			_ = a.syncCldxOrder(req, o)
		}(sn)
	}
	wg.Wait()
	if left := len(sns) - started; left > 0 || (ctx.Err() != nil && len(sns) > 0) {
		log.Printf("cldx 同步：本轮超过 %s（用时 %s），已轮询 %d 单，剩余 %d 单留到下一轮", syncCldxTimeout, time.Since(start).Round(time.Millisecond), started, left)
	}
}

func appClient(r *http.Request) bool {
	if c, err := r.Cookie("clodex_app"); err == nil && c.Value == "1" {
		return true
	}
	return r.URL.Query().Get("clodex_app") == "1"
}

func encodePNG(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}

func formatCldx(minor int64) string {
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	text := fmt.Sprintf("%d.%04d", minor/10000, minor%10000)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	return sign + text
}
