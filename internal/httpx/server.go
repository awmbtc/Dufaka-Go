package httpx

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skip2/go-qrcode"

	"dufaka/internal/admin"
	"dufaka/internal/install"
	"dufaka/internal/order"
	"dufaka/internal/pay"
	"dufaka/internal/store"
)

//go:embed templates/*.html
var files embed.FS

type App struct {
	mu         sync.RWMutex
	db         *store.DB
	tpl        *template.Template
	base       string
	configPath string
	wallet     pay.Wallet
	receipts   *receiptThrottle
	lookups    *ipLimiter // order lookups by number / email: 30 per 5 minutes per address
	polls      *ipLimiter // cashier status polls: 120 per 5 minutes per address
	hiddenPays sync.Map   // pay id → struct{}: "channel hidden" already logged
}

func New(db *store.DB, base, configPath string) (*App, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"yuan": func(c order.Cents) string { return c.Yuan() },
		"status": func(s int) string {
			switch s {
			case 1:
				return "待支付"
			case 2:
				return "待处理"
			case 3:
				return "处理中"
			case 4:
				return "已完成"
			case 5:
				return "处理失败"
			case 6:
				return "异常"
			case -1:
				return "过期"
			default:
				return "未知"
			}
		},
		"rich":          rich,
		"maskSN":        maskSN,
		"lunaGoods":     lunaGoods,
		"stockPercent":  stockPercent,
		"wholesaleRows": wholesaleRows,
		"extraInputs":   extraInputs,
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, err
	}
	merchant := os.Getenv("WALLET_MERCHANT_ID")
	if merchant == "" {
		merchant = "shop"
	}
	return &App{
		db: db, tpl: tpl, base: strings.TrimRight(base, "/"), configPath: configPath,
		receipts: newReceiptThrottle(),
		lookups:  newIPLimiter("订单查询", lookupLimit, lookupOverflow, limitWindow),
		polls:    newIPLimiter("订单状态轮询", pollLimit, 0, limitWindow),
		wallet: pay.Wallet{
			Base:       envOr("WALLET_BASE_URL", "https://cldx-wallet-zh432gkopa-de.a.run.app"),
			MerchantID: merchant,
			Secret:     os.Getenv("WALLET_MERCHANT_SECRET"),
		},
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Live returns the database connected after install or startup.
func (a *App) Live() *store.DB { return a.live() }

func (a *App) live() *store.DB {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.db
}

func (a *App) setDB(db *store.DB) {
	a.mu.Lock()
	a.db = db
	a.mu.Unlock()
}

func (a *App) installed(r *http.Request) bool {
	db := a.live()
	return db != nil && db.Installed(r.Context())
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", staticFiles("web/assets")))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/assets/brand/favicon.ico", http.StatusFound)
	})
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /buy/{id}", a.buy)
	mux.HandleFunc("POST /create-order", a.create)
	mux.HandleFunc("GET /bill/{sn}", a.bill)
	mux.HandleFunc("GET /detail-order-sn/{sn}", a.limitLookups(a.detail))
	mux.HandleFunc("GET /order-search", a.searchPage)
	mux.HandleFunc("GET /check-order-status/{sn}", a.limitPolls(a.poll))
	mux.HandleFunc("POST /search-order-by-sn", a.limitLookups(a.searchSN))
	mux.HandleFunc("POST /search-order-by-email", a.limitLookups(a.searchEmail))
	mux.HandleFunc("POST /search-order-by-browser", a.searchBrowser)
	mux.HandleFunc("GET /pay-gateway/{handle}/{payway}/{sn}", a.gateway)
	mux.HandleFunc("GET /pay/{channel}/{payway}/{sn}", a.gateway)
	mux.HandleFunc("GET /cldx/pay", a.cldxLaunch)
	mux.HandleFunc("POST /pay/wepay/notify_url", a.wechatNotify)
	mux.HandleFunc("GET /install", a.installPage)
	mux.HandleFunc("POST /install/test", a.installTest)
	mux.HandleFunc("POST /do-install", a.doInstall)
	admin.Mount(mux, func() *pgxpool.Pool {
		db := a.live()
		if db == nil {
			return nil
		}
		return db.Pool
	}, admin.WithSettingsSaved(func() {
		if db := a.live(); db != nil {
			db.InvalidateSite()
		}
	}))
	return a.guard(mux)
}

func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w, r)
		if strings.HasPrefix(r.URL.Path, "/admin") && r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			u, err := url.Parse(origin)
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" || (origin != "" && (err != nil || !strings.EqualFold(u.Host, r.Host) || (u.Scheme != "http" && u.Scheme != "https"))) {
				http.Error(w, "请求来源不正确", http.StatusForbidden)
				return
			}
		}

		if r.URL.Query().Get("clodex_app") == "1" {
			http.SetCookie(w, &http.Cookie{
				Name: "clodex_app", Value: "1", Path: "/", MaxAge: 30 * 24 * 3600,
				HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
			})
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}
		if !a.installed(r) {
			if saved, err := install.LoadConfig(a.configPath); err == nil && saved.DatabaseURL != "" {
				http.Error(w, "数据库暂时不可用，请稍后重试", http.StatusServiceUnavailable)
				return
			}
		}
		if r.URL.Path == "/install" || r.URL.Path == "/install/test" || r.URL.Path == "/do-install" {
			next.ServeHTTP(w, r)
			return
		}
		if !a.installed(r) {
			http.Redirect(w, r, "/install", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) view(w http.ResponseWriter, name string, data any) {
	a.viewStatus(w, http.StatusOK, name, data)
}

func (a *App) viewStatus(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := a.render(&buf, name, data); err != nil {
		http.Error(w, "页面渲染失败", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (a *App) render(w io.Writer, name string, data any) error {
	chosen := name
	if m, ok := data.(map[string]any); ok {
		if site, ok := m["Site"].(store.Site); ok && (site.Template == "luna" || site.Template == "hyper") {
			alt := site.Template + "_" + name
			if a.tpl.Lookup(alt) != nil {
				chosen = alt
			}
		}
	}
	return a.tpl.ExecuteTemplate(w, chosen, data)
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	groups, err := a.live().Home(r.Context())
	if err != nil {
		a.failErr(w, r, "读取首页商品", err)
		return
	}
	a.view(w, "home.html", map[string]any{"Title": "首页", "Site": a.live().Site(r.Context()), "Groups": groups})
}

func (a *App) buy(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	g, err := a.live().Good(r.Context(), id)
	if err != nil || !g.Open {
		a.fail(w, r, "商品不存在或已下架")
		return
	}
	all, _ := a.live().Pays(r.Context(), clientKind(r))
	var pays []store.Pay
	for _, p := range all {
		if a.payReady(p) {
			pays = append(pays, p)
		}
	}
	a.view(w, "buy.html", map[string]any{"Title": g.Name, "Site": a.live().Site(r.Context()), "Good": g, "Pays": pays})
}

// payReady reports whether an open channel can actually take a payment right now: cldx
// needs the wallet credentials, wescan a configuration that passes Validate. A channel
// that is not ready is left off the product page (logged once per channel, with the
// reason, for the owner) and refused at checkout.
func (a *App) payReady(p store.Pay) bool {
	var why string
	switch p.Check {
	case "cldx":
		if !a.wallet.Enabled() {
			why = "钱包未配置（WALLET_MERCHANT_SECRET 等）"
		}
	case "wescan":
		cfg := wechatConfig(p)
		if v := cfg.Validate(); len(v) > 0 {
			why = strings.Join(v, "、")
		} else if w := cfg.Warnings(); len(w) > 0 {
			text := strings.Join(w, "；")
			if _, seen := wechatWarned.LoadOrStore(fmt.Sprintf("%d|%s", p.ID, text), struct{}{}); !seen {
				log.Printf("微信支付渠道 %d 提醒：%s", p.ID, text)
			}
		}
	default:
		if !store.CashierReady(p.Check) {
			why = "没有接通收银台"
		}
	}
	if why == "" {
		return true
	}
	if _, seen := a.hiddenPays.LoadOrStore(p.ID, struct{}{}); !seen {
		log.Printf("支付渠道 %d（%s）未就绪，已从商品页隐藏：%s", p.ID, p.Check, why)
	}
	return false
}

func (a *App) create(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, r, "请求不正确")
		return
	}
	gid, _ := strconv.Atoi(r.FormValue("gid"))
	payID, _ := strconv.Atoi(r.FormValue("payway"))
	amt, _ := strconv.Atoi(r.FormValue("by_amount"))
	if payID > 0 {
		// Refuse a channel that is not ready before any stock is reserved.
		if p, err := a.live().Pay(r.Context(), payID); err == nil && !a.payReady(p) {
			a.fail(w, r, "支付方式不可用")
			return
		}
	}
	extra := map[string]string{}
	for k, v := range r.Form {
		if len(v) > 0 {
			extra[k] = v[0]
		}
	}
	o, err := a.live().CreateOrder(r.Context(), store.CreateInput{
		GID: gid, PayID: payID, Amount: amt, Email: r.FormValue("email"),
		SearchPwd: r.FormValue("search_pwd"), Coupon: r.FormValue("coupon_code"),
		IP: clientIP(r), Extra: extra,
	}, a.live().Site(r.Context()))
	if err != nil {
		a.failErr(w, r, "创建订单", err)
		return
	}
	remember(w, r, o.SN)
	http.Redirect(w, r, "/bill/"+o.SN, http.StatusFound)
}

func (a *App) bill(w http.ResponseWriter, r *http.Request) {
	o, err := a.live().OrderBySNChecked(r.Context(), r.PathValue("sn"))
	if err != nil {
		a.failErr(w, r, orderWhat(r.PathValue("sn")), err)
		return
	}
	if o.Status == -1 {
		a.fail(w, r, "订单已过期")
		return
	}
	site := a.live().Site(r.Context())
	p, _ := a.live().Pay(r.Context(), o.PayID)
	deadline := o.Created.Add(time.Duration(site.ExpireMin) * time.Minute).Unix()
	a.view(w, "bill.html", map[string]any{"Title": "确认订单", "Site": site, "Order": o, "Pay": p, "PayDeadline": deadline})
}

// detail shows one order by number. The order number itself is the capability: it is 16
// hex characters from crypto/rand (store.newSN), never guessable, and it is only handed to
// the buyer's browser (redirect + cookie) or shown in full behind the query password, so
// whoever presents a full number is treated as the buyer. The query password is never
// read from the URL: the search form posts it in the body and renders the same page
// through renderDetail.
func (a *App) detail(w http.ResponseWriter, r *http.Request) {
	a.renderDetail(w, r, r.PathValue("sn"), "")
}

func (a *App) renderDetail(w http.ResponseWriter, r *http.Request, sn, pwd string) {
	o, err := a.live().OrderBySNChecked(r.Context(), sn)
	if err != nil {
		a.failErr(w, r, orderWhat(sn), err)
		return
	}
	site := a.live().Site(r.Context())
	if site.SearchPwd && o.SearchPwd != "" && pwd != o.SearchPwd && !ownsOrder(r, o.SN) {
		o.Info = ""
	}
	a.view(w, "orders.html", map[string]any{"Title": "订单详情", "Site": site, "Orders": []store.Order{o}})
}

func (a *App) searchPage(w http.ResponseWriter, r *http.Request) {
	a.view(w, "search.html", map[string]any{"Title": "订单查询", "Site": a.live().Site(r.Context())})
}

func (a *App) poll(w http.ResponseWriter, r *http.Request) {
	sn := r.PathValue("sn")
	o, err := a.live().OrderBySNChecked(r.Context(), sn)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(pollLookupFailed(r.Context(), sn, err))
		return
	}
	if o.Status == 1 || o.Status == -1 {
		o = a.syncCldxOrder(r, o)
	}
	if o.Status == -1 {
		_ = json.NewEncoder(w).Encode(map[string]any{"msg": "expired", "code": 400001})
		return
	}
	if o.Status == 1 {
		_ = json.NewEncoder(w).Encode(map[string]any{"msg": "wait....", "code": 400000})
		return
	}
	if o.Status != 2 && o.Status != 3 && o.Status != 4 {
		_ = json.NewEncoder(w).Encode(map[string]any{"msg": "exception", "code": 400002})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"msg": "success", "code": 200})
}

// pollLookupFailed is the cashier poll's answer when the order could not be read. Only a
// genuinely unknown order ends the polling (400001, shown as expired); a database error is
// logged with the order number (unless the caller already went away) and answered 400000
// so the page keeps polling instead of telling a paying customer the order expired.
func pollLookupFailed(ctx context.Context, sn string, err error) map[string]any {
	if store.IsNotFound(err) {
		return map[string]any{"msg": "expired", "code": 400001}
	}
	if ctx.Err() == nil {
		log.Printf("订单状态查询：读取订单 %q 失败: %v", clipText(sn, 64), err)
	}
	return map[string]any{"msg": "wait....", "code": 400000}
}

// clipText bounds request-supplied text before it reaches a log line.
func clipText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func (a *App) searchSN(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	sn := strings.TrimSpace(r.FormValue("order_sn"))
	if sn == "" {
		a.fail(w, r, "请输入订单号")
		return
	}
	a.renderDetail(w, r, sn, r.FormValue("search_pwd"))
}

func (a *App) searchEmail(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	site := a.live().Site(r.Context())
	if r.FormValue("email") == "" || (site.SearchPwd && r.FormValue("search_pwd") == "") {
		a.fail(w, r, "请求不合法")
		return
	}
	list, err := a.live().OrdersByEmail(r.Context(), r.FormValue("email"), r.FormValue("search_pwd"), site.SearchPwd)
	if err != nil {
		a.failErr(w, r, "按邮箱查询订单", err)
		return
	}
	if len(list) == 0 {
		a.fail(w, r, "未找到相关订单")
		return
	}
	mask := !site.SearchPwd
	if mask {
		// Without a query password an email alone must not hand out the order-number
		// capability: the full number opens /detail-order-sn/{sn} with the card, so the
		// listing shows only its last 4 characters and links nothing. Cards stay only for
		// the browser that bought them; the browser-owned search keeps full numbers.
		for i := range list {
			if !ownsOrder(r, list[i].SN) {
				list[i].Info = ""
			}
		}
	}
	a.view(w, "orders.html", map[string]any{"Site": site, "Orders": list, "MaskSN": mask})
}

// maskSN keeps only the last four characters of an order number ("…WXYZ") so a buyer can
// recognise an order without the listing handing out the number, or enough of it to
// narrow a guess.
func maskSN(sn string) string {
	r := []rune(sn)
	if len(r) <= 4 {
		return strings.Repeat("*", len(r))
	}
	return "…" + string(r[len(r)-4:])
}

func (a *App) searchBrowser(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("dujiaoka_orders")
	if err != nil || c.Value == "" {
		a.fail(w, r, "浏览器没有相关订单")
		return
	}
	sns := browserOrders(c.Value)
	var list []store.Order
	var lookupErr error
	for _, sn := range sns {
		o, err := a.live().OrderBySN(r.Context(), sn)
		if err == nil {
			list = append(list, o)
		} else if !store.IsNotFound(err) && lookupErr == nil {
			lookupErr = err
		}
	}
	if len(list) == 0 {
		if lookupErr != nil {
			a.failErr(w, r, "读取本机订单", lookupErr)
			return
		}
		a.fail(w, r, "浏览器没有相关订单")
		return
	}
	a.view(w, "orders.html", map[string]any{"Title": "订单详情", "Site": a.live().Site(r.Context()), "Orders": list})
}

func (a *App) gateway(w http.ResponseWriter, r *http.Request) {
	o, err := a.live().OrderBySNChecked(r.Context(), r.PathValue("sn"))
	if err != nil {
		a.failErr(w, r, orderWhat(r.PathValue("sn")), err)
		return
	}
	if o.Status != 1 {
		http.Redirect(w, r, "/detail-order-sn/"+url.PathEscape(o.SN), http.StatusFound)
		return
	}
	if !time.Now().Before(o.Created.Add(time.Duration(a.live().Site(r.Context()).ExpireMin) * time.Minute)) {
		a.fail(w, r, "订单已过期")
		return
	}
	p, err := a.live().Pay(r.Context(), o.PayID)
	if err != nil {
		a.failErr(w, r, orderWhat(o.SN)+" 的支付方式", err)
		return
	}
	if p.Open != 1 {
		a.fail(w, r, "支付方式已停用")
		return
	}
	if p.Check == "cldx" {
		a.cldxPay(w, r, o)
		return
	}
	if p.Check != "wescan" {
		a.fail(w, r, "该支付渠道还没有接通自己的收银台。标识："+p.Check)
		return
	}
	cfg := wechatConfig(p)
	if problems := cfg.Validate(); len(problems) > 0 {
		// The list names configuration fields; only the shop owner can act on it. Checked
		// before any call to WeChat Pay: a QR code whose callback could not be verified or
		// decrypted would take the customer's money without ever delivering.
		log.Printf("微信支付收银台（渠道 %d）：配置不可用：%s", p.ID, strings.Join(problems, "、"))
		a.fail(w, r, "微信支付暂未配置完整，请联系店主")
		return
	}
	notify := a.base + "/pay/wepay/notify_url"
	code, err := pay.Native(r.Context(), cfg, notify, o.SN, o.Title, int64(o.Actual))
	if err != nil {
		log.Printf("微信支付收银台（渠道 %d）：订单 %s 下单失败: %v", p.ID, o.SN, err)
		a.fail(w, r, "微信支付下单失败，请稍后再试或联系店主")
		return
	}
	png, err := qrcode.Encode(code, qrcode.Medium, 256)
	if err != nil {
		a.fail(w, r, "二维码生成失败")
		return
	}
	a.view(w, "qrpay.html", map[string]any{
		"Title": "扫码支付", "Site": a.live().Site(r.Context()), "Order": o, "Pay": p,
		"QR": base64.StdEncoding.EncodeToString(png),
	})
}

// wechatConfig builds the single WeChat Pay configuration from the "wescan" payment row,
// with environment variables only filling fields the row leaves empty.
// staleKeyOnce keeps the "admin APIv3 key is stale" hint to one log line per process;
// wechatWarned logs each storefront configuration warning once per channel and text.
var (
	staleKeyOnce sync.Once
	wechatWarned sync.Map
)

func wechatConfig(p store.Pay) pay.WechatConfig {
	return pay.WechatConfig{MchID: p.MerchantID, APIv3Key: p.MerchantKey, PrivateKeyPEM: p.MerchantPem}.WithEnv()
}

// wechatNotify handles the WeChat Pay callback. Checks run cheapest first: the timestamp
// window before any database work, then the platform signature (pay.WechatConfig.
// VerifyNotify; the Wechatpay-Serial header only explains a failure), and only then is
// the resource decrypted with the wescan row's APIv3 key (pays.pay_check is UNIQUE, so
// there is exactly one row). Status codes: 401 for a callback that is not WeChat's; 500
// whenever WeChat should retry — a configuration gap, a verified callback that the
// configured APIv3 key cannot decrypt (retried once the owner fixes the key), a database
// failure; 400 only for a verified body that is not JSON.
func (a *App) wechatNotify(w http.ResponseWriter, r *http.Request) {
	if err := pay.CheckTimestamp(r.Header.Get("Wechatpay-Timestamp"), time.Now()); err != nil {
		http.Error(w, "fail", http.StatusUnauthorized)
		return
	}
	row, err := a.live().PayByCheck(r.Context(), "wescan")
	if err != nil {
		log.Printf("微信支付通知：找不到 wescan 支付渠道: %v", err)
		http.Error(w, "fail", 500)
		return
	}
	cfg := wechatConfig(row)
	if strings.TrimSpace(cfg.PlatformKeyPEM) == "" {
		log.Printf("微信支付通知（渠道 %d）：%v", row.ID, cfg.MissingForNotifyError())
		http.Error(w, "fail", 500)
		return
	}
	raw, _ := readAll(r)
	notice := pay.Notice{
		Timestamp: r.Header.Get("Wechatpay-Timestamp"), Nonce: r.Header.Get("Wechatpay-Nonce"),
		Signature: r.Header.Get("Wechatpay-Signature"), Serial: r.Header.Get("Wechatpay-Serial"), Body: string(raw),
	}
	if err := cfg.VerifyNotify(notice, time.Now()); err != nil {
		log.Printf("微信支付通知验签失败: %v", err)
		http.Error(w, "fail", http.StatusUnauthorized)
		return
	}
	var body struct {
		Resource struct {
			Nonce          string `json:"nonce"`
			Ciphertext     string `json:"ciphertext"`
			AssociatedData string `json:"associated_data"`
		} `json:"resource"`
	}
	if json.Unmarshal(raw, &body) != nil {
		http.Error(w, "bad", 400)
		return
	}
	plain, usedEnv, err := cfg.DecryptWithEnvFallback(body.Resource.Nonce, body.Resource.Ciphertext, body.Resource.AssociatedData)
	if usedEnv {
		staleKeyOnce.Do(func() {
			log.Printf("微信支付通知（渠道 %d）已改用环境变量 WECHAT_PAY_API_V3_KEY 解密：后台商户 KEY 已失效，请在后台更新或清空", row.ID)
		})
	}
	if err != nil {
		// The callback is genuinely WeChat's, so the key is what is wrong: answer 500 and
		// let WeChat retry after the owner fixes it.
		if m := cfg.MissingForNotify(); len(m) > 0 {
			log.Printf("微信支付通知（渠道 %d）解密失败，配置不完整：%s", row.ID, strings.Join(m, "、"))
		} else {
			log.Printf("微信支付通知（渠道 %d）解密失败，请核对 APIv3 密钥（商户 KEY）", row.ID)
		}
		http.Error(w, "fail", 500)
		return
	}
	var n struct {
		OutTradeNo    string `json:"out_trade_no"`
		TradeState    string `json:"trade_state"`
		TransactionID string `json:"transaction_id"`
		Amount        struct {
			Total int64 `json:"total"`
		} `json:"amount"`
	}
	if json.Unmarshal(plain, &n) != nil || n.TradeState != "SUCCESS" {
		w.WriteHeader(204)
		return
	}
	_, err = a.live().Complete(r.Context(), n.OutTradeNo, order.Cents(n.Amount.Total), n.TransactionID)
	switch {
	case err == nil:
	case store.IsPaidShort(err):
		// The payment is recorded (status 6, 异常); retrying cannot conjure stock, so WeChat
		// is told SUCCESS and the owner restocks and redelivers from the back office.
		log.Printf("微信支付通知：订单 %q 已收款但库存不足，已记为异常（状态 6），请补货后在后台重新发货", clipText(n.OutTradeNo, 64))
	default:
		log.Printf("微信支付通知：订单 %q 完成失败: %v", clipText(n.OutTradeNo, 64), err)
		http.Error(w, "fail", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"code":"SUCCESS","message":"ok"}`))
}

func (a *App) installPage(w http.ResponseWriter, r *http.Request) {
	if a.installed(r) {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	page := install.DefaultPage(a.configPath, a.base)
	db := a.live()
	page.Connected = db != nil
	if db != nil {
		page.Tables = db.Installed(r.Context())
	}
	a.view(w, "install.html", page)
}

func (a *App) installTest(w http.ResponseWriter, r *http.Request) {
	if a.installed(r) {
		http.Error(w, "已经安装", http.StatusConflict)
		return
	}
	_ = r.ParseForm()
	err := install.Probe(r.Context(), formFrom(r))
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "数据库连接成功"})
}

func (a *App) doInstall(w http.ResponseWriter, r *http.Request) {
	if a.installed(r) {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	_ = r.ParseForm()
	form := formFrom(r)
	page := install.DefaultPage(a.configPath, a.base)
	page.Host, page.Port, page.Database, page.User = form.Host, form.Port, form.Database, form.User
	page.Title, page.AppURL = form.Title, form.AppURL
	result, err := install.Run(r.Context(), form, a.configPath)
	if err != nil {
		page.Err = err.Error()
		a.view(w, "install.html", page)
		return
	}
	os.Setenv("DUFAKA_SESSION_KEY", result.SessionKey)
	os.Setenv("DUFAKA_DATABASE_URL", result.DSN)
	db, err := store.Open(r.Context(), result.DSN)
	if err != nil {
		page.Err = "配置已写入，但进程没有连上新数据库，请用该配置重启"
		a.view(w, "install.html", page)
		return
	}
	a.setDB(db)
	if a.base == "" {
		a.base = result.AppURL
	}
	page.Done = &result
	a.view(w, "install.html", page)
}

func formFrom(r *http.Request) install.Form {
	return install.Form{
		Host: r.FormValue("db_host"), Port: r.FormValue("db_port"), Database: r.FormValue("db_database"),
		User: r.FormValue("db_username"), Password: r.FormValue("db_password"),
		Title: r.FormValue("title"), AppURL: r.FormValue("app_url"),
		Admin: r.FormValue("admin_username"), AdminPwd: r.FormValue("admin_password"), Confirm: r.FormValue("admin_password_confirm"),
	}
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, msg string) {
	a.failStatus(w, r, http.StatusOK, msg)
}

// genericFailure is what a customer sees when a storefront step fails for a reason that is
// not a business rule (database down, timeout): driver or SQL text never reaches a page.
const genericFailure = "系统繁忙，请稍后再试"

// failErr shows a store.RuleError's own message (it is written for customers) and, for any
// other error, logs it with what was being done and shows genericFailure.
func (a *App) failErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	var re store.RuleError
	if errors.As(err, &re) {
		a.fail(w, r, re.Msg)
		return
	}
	var ie store.InternalError
	if r.Context().Err() == nil && !errors.As(err, &ie) {
		log.Printf("%s失败: %s", logSafe(what), logSafe(err.Error()))
	}
	a.fail(w, r, genericFailure)
}

// orderWhat names an order lookup for failErr. The order number may come from the request
// (path or form), so it is clipped and Go-quoted: a NUL, newline or invalid UTF-8 byte is
// written as an escape and can never start a forged log line.
func orderWhat(sn string) string {
	return fmt.Sprintf("读取订单 %q", clipText(sn, 64))
}

// logSafe is failErr's last line of defence for text of unknown origin: control
// characters (newlines included) and invalid UTF-8 are replaced with Go escapes so one
// failErr call always writes exactly one log line.
func logSafe(s string) string {
	clean := true
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

func (a *App) failStatus(w http.ResponseWriter, r *http.Request, status int, msg string) {
	site := store.Site{Title: "Dufaka-Go", TextLogo: "Dufaka-Go", Template: "unicorn"}
	if db := a.live(); db != nil {
		site = db.Site(r.Context())
	}
	a.viewStatus(w, status, "error.html", map[string]any{"Title": "提示", "Site": site, "Message": msg})
}

func clientKind(r *http.Request) int {
	ua := strings.ToLower(r.UserAgent())
	if strings.Contains(ua, "mobile") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone") {
		return 2
	}
	return 1
}

func remember(w http.ResponseWriter, r *http.Request, sn string) {
	var sns []string
	if c, err := r.Cookie("dujiaoka_orders"); err == nil {
		sns = browserOrders(c.Value)
	}
	sns = append(sns, sn)
	if len(sns) > 40 {
		sns = sns[len(sns)-40:]
	}
	raw, _ := json.Marshal(sns)
	http.SetCookie(w, &http.Cookie{Name: "dujiaoka_orders", Value: orderCookie(base64.RawURLEncoding.EncodeToString(raw)), Path: "/", MaxAge: 86400 * 30, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}
