package httpx

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

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
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{db: db, tpl: tpl, base: strings.TrimRight(base, "/"), configPath: configPath}, nil
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
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /buy/{id}", a.buy)
	mux.HandleFunc("POST /create-order", a.create)
	mux.HandleFunc("GET /bill/{sn}", a.bill)
	mux.HandleFunc("GET /detail-order-sn/{sn}", a.detail)
	mux.HandleFunc("GET /order-search", a.searchPage)
	mux.HandleFunc("GET /check-order-status/{sn}", a.poll)
	mux.HandleFunc("POST /search-order-by-sn", a.searchSN)
	mux.HandleFunc("POST /search-order-by-email", a.searchEmail)
	mux.HandleFunc("POST /search-order-by-browser", a.searchBrowser)
	mux.HandleFunc("GET /pay-gateway/{handle}/{payway}/{sn}", a.gateway)
	mux.HandleFunc("GET /pay/{channel}/{payway}/{sn}", a.gateway)
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
	})
	return a.guard(mux)
}

func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	groups, err := a.live().Home(r.Context())
	if err != nil {
		a.fail(w, r, err.Error())
		return
	}
	a.view(w, "home.html", map[string]any{"Site": a.live().Site(r.Context()), "Groups": groups})
}

func (a *App) buy(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	g, err := a.live().Good(r.Context(), id)
	if err != nil {
		a.fail(w, r, err.Error())
		return
	}
	pays, _ := a.live().Pays(r.Context(), clientKind(r))
	a.view(w, "buy.html", map[string]any{"Site": a.live().Site(r.Context()), "Good": g, "Pays": pays})
}

func (a *App) create(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, r, "请求不正确")
		return
	}
	gid, _ := strconv.Atoi(r.FormValue("gid"))
	payID, _ := strconv.Atoi(r.FormValue("payway"))
	amt, _ := strconv.Atoi(r.FormValue("by_amount"))
	extra := map[string]string{}
	for k, v := range r.Form {
		if len(v) > 0 {
			extra[k] = v[0]
		}
	}
	o, err := a.live().CreateOrder(r.Context(), store.CreateInput{
		GID: gid, PayID: payID, Amount: amt, Email: r.FormValue("email"),
		SearchPwd: r.FormValue("search_pwd"), Coupon: r.FormValue("coupon_code"),
		IP: r.RemoteAddr, Extra: extra,
	}, a.live().Site(r.Context()))
	if err != nil {
		a.fail(w, r, err.Error())
		return
	}
	remember(w, r, o.SN)
	http.Redirect(w, r, "/bill/"+o.SN, http.StatusFound)
}

func (a *App) bill(w http.ResponseWriter, r *http.Request) {
	o, err := a.live().OrderBySN(r.Context(), r.PathValue("sn"))
	if err != nil {
		a.fail(w, r, err.Error())
		return
	}
	if o.Status == -1 {
		a.fail(w, r, "订单已过期")
		return
	}
	p, _ := a.live().Pay(r.Context(), o.PayID)
	a.view(w, "bill.html", map[string]any{"Site": a.live().Site(r.Context()), "Order": o, "Pay": p})
}

func (a *App) detail(w http.ResponseWriter, r *http.Request) {
	o, err := a.live().OrderBySN(r.Context(), r.PathValue("sn"))
	if err != nil {
		a.fail(w, r, err.Error())
		return
	}
	a.view(w, "orders.html", map[string]any{"Site": a.live().Site(r.Context()), "Orders": []store.Order{o}})
}

func (a *App) searchPage(w http.ResponseWriter, r *http.Request) {
	a.view(w, "search.html", map[string]any{"Site": a.live().Site(r.Context())})
}

func (a *App) poll(w http.ResponseWriter, r *http.Request) {
	o, err := a.live().OrderBySN(r.Context(), r.PathValue("sn"))
	w.Header().Set("Content-Type", "application/json")
	if err != nil || o.Status == -1 {
		_ = json.NewEncoder(w).Encode(map[string]any{"msg": "expired", "code": 400001})
		return
	}
	if o.Status == 1 {
		_ = json.NewEncoder(w).Encode(map[string]any{"msg": "wait....", "code": 400000})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"msg": "success", "code": 200})
}

func (a *App) searchSN(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	http.Redirect(w, r, "/detail-order-sn/"+r.FormValue("order_sn"), http.StatusFound)
}

func (a *App) searchEmail(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	site := a.live().Site(r.Context())
	if r.FormValue("email") == "" || (site.SearchPwd && r.FormValue("search_pwd") == "") {
		a.fail(w, r, "请求不合法")
		return
	}
	list, err := a.live().OrdersByEmail(r.Context(), r.FormValue("email"), r.FormValue("search_pwd"), site.SearchPwd)
	if err != nil || len(list) == 0 {
		a.fail(w, r, "未找到相关订单")
		return
	}
	a.view(w, "orders.html", map[string]any{"Site": site, "Orders": list})
}

func (a *App) searchBrowser(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("dujiaoka_orders")
	if err != nil || c.Value == "" {
		a.fail(w, r, "浏览器没有相关订单")
		return
	}
	var sns []string
	_ = json.Unmarshal([]byte(c.Value), &sns)
	var list []store.Order
	for _, sn := range sns {
		if o, err := a.live().OrderBySN(r.Context(), sn); err == nil {
			list = append(list, o)
		}
	}
	if len(list) == 0 {
		a.fail(w, r, "浏览器没有相关订单")
		return
	}
	a.view(w, "orders.html", map[string]any{"Site": a.live().Site(r.Context()), "Orders": list})
}

func (a *App) gateway(w http.ResponseWriter, r *http.Request) {
	o, err := a.live().OrderBySN(r.Context(), r.PathValue("sn"))
	if err != nil {
		a.fail(w, r, err.Error())
		return
	}
	p, err := a.live().Pay(r.Context(), o.PayID)
	if err != nil {
		a.fail(w, r, err.Error())
		return
	}
	if p.Check != "wescan" && !strings.Contains(p.Check, "wx") {
		a.fail(w, r, "该支付渠道页面与原站路径一致，当前这一版先接通微信扫码。标识："+p.Check)
		return
	}
	notify := a.base + "/pay/wepay/notify_url"
	code, err := pay.Native(r.Context(), p.MerchantID, p.MerchantKey, "", "", p.MerchantPem, "", notify, o.SN, o.Title, int64(o.Actual))
	if err != nil {
		a.fail(w, r, err.Error())
		return
	}
	png, err := qrcode.Encode(code, qrcode.Medium, 256)
	if err != nil {
		a.fail(w, r, "二维码生成失败")
		return
	}
	a.view(w, "qrpay.html", map[string]any{
		"Site": a.live().Site(r.Context()), "Order": o, "Pay": p,
		"QR": base64.StdEncoding.EncodeToString(png),
	})
}

func (a *App) wechatNotify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Resource struct {
			Nonce, Ciphertext, AssociatedData string
		} `json:"resource"`
	}
	raw, _ := readAll(r)
	if err := pay.VerifyNotify("", r.Header.Get("Wechatpay-Timestamp"), r.Header.Get("Wechatpay-Nonce"), string(raw), r.Header.Get("Wechatpay-Signature"), time.Now()); err != nil {
		http.Error(w, "fail", http.StatusUnauthorized)
		return
	}
	if json.Unmarshal(raw, &body) != nil {
		http.Error(w, "bad", 400)
		return
	}
	plain, err := pay.DecryptResource(os.Getenv("WECHAT_PAY_API_V3_KEY"), body.Resource.Nonce, body.Resource.Ciphertext, body.Resource.AssociatedData)
	if err != nil {
		http.Error(w, "fail", 400)
		return
	}
	var n struct {
		OutTradeNo, TradeState, TransactionID string
		Amount                                struct {
			Total int64 `json:"total"`
		} `json:"amount"`
	}
	if json.Unmarshal(plain, &n) != nil || n.TradeState != "SUCCESS" {
		w.WriteHeader(204)
		return
	}
	_, err = a.live().Complete(r.Context(), n.OutTradeNo, order.Cents(n.Amount.Total), n.TransactionID)
	if err != nil {
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
	a.view(w, "error.html", map[string]any{"Site": a.live().Site(r.Context()), "Message": msg})
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
		_ = json.Unmarshal([]byte(c.Value), &sns)
	}
	sns = append(sns, sn)
	raw, _ := json.Marshal(sns)
	http.SetCookie(w, &http.Cookie{Name: "dujiaoka_orders", Value: string(raw), Path: "/", MaxAge: 86400 * 30, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}
