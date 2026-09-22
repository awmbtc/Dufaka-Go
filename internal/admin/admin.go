// Package admin is the shop back office. Pages and field names follow the
// original dujiaoka menus; HTML and SQL here are written for this service.
package admin

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/*.html
var templateFiles embed.FS

const cookieName = "dufaka_admin"

const sessionTTL = 7 * 24 * time.Hour

type Server struct {
	poolFn func() *pgxpool.Pool
}

func (s *Server) db() *pgxpool.Pool {
	if s.poolFn == nil {
		return nil
	}
	return s.poolFn()
}

// View is the shell data for every authenticated page.
type View struct {
	Title string
	User  string
	Nav   string
	Ok    string
	Err   string
	Prev  string
	Next  string
}

type session struct {
	UID  int64  `json:"uid"`
	Exp  int64  `json:"exp"`
	Name string `json:"name"`
}

var pages = template.Must(template.New("admin").Funcs(template.FuncMap{
	"goodsType": func(v int) string {
		switch v {
		case 1:
			return "自动发货"
		case 2:
			return "人工处理"
		default:
			return "未知"
		}
	},
	"shelf": func(v int) string {
		if v == 1 {
			return "上架"
		}
		return "下架"
	},
	"openName": func(v int) string {
		if v == 1 {
			return "启用"
		}
		return "禁用"
	},
	"orderStatus": orderStatus,
	"payMethod": func(v int) string {
		if v == 2 {
			return "扫码"
		}
		return "跳转"
	},
	"payClient": func(v int) string {
		switch v {
		case 2:
			return "手机"
		case 3:
			return "通用"
		default:
			return "电脑PC"
		}
	},
	"carmiStatus": func(v int) string {
		if v == 2 {
			return "已售出"
		}
		return "未售出"
	},
	"yesNo": func(v int) string {
		if v == 1 {
			return "是"
		}
		return "否"
	},
	"short": func(s string, n int) string {
		if n <= 0 {
			return ""
		}
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "…"
	},
}).ParseFS(templateFiles, "templates/*.html"))

// Mount registers the /admin site on mux. pool may be nil in tests that only
// render public pages; data pages then report that the database is unavailable.
func Mount(mux *http.ServeMux, poolFn func() *pgxpool.Pool) {
	s := &Server{poolFn: poolFn}
	mux.HandleFunc("GET /admin/login", s.loginForm)
	mux.HandleFunc("POST /admin/login", s.login)
	mux.HandleFunc("GET /admin/logout", s.logout)
	mux.HandleFunc("POST /admin/logout", s.logout)

	mux.HandleFunc("GET /admin", s.authed(s.dashboard))
	mux.HandleFunc("GET /admin/{$}", s.authed(s.dashboard))

	mux.HandleFunc("GET /admin/goods", s.authed(s.goodsList))
	mux.HandleFunc("GET /admin/goods/create", s.authed(s.goodsCreate))
	mux.HandleFunc("POST /admin/goods", s.authed(s.goodsSave))
	mux.HandleFunc("GET /admin/goods/{id}/edit", s.authed(s.goodsEdit))
	mux.HandleFunc("POST /admin/goods/{id}", s.authed(s.goodsSave))
	mux.HandleFunc("POST /admin/goods/{id}/delete", s.authed(s.goodsDelete))
	mux.HandleFunc("POST /admin/goods/{id}/restore", s.authed(s.goodsRestore))

	mux.HandleFunc("GET /admin/groups", s.authed(s.groupsPage))
	mux.HandleFunc("GET /admin/groups/{id}/edit", s.authed(s.groupEdit))
	mux.HandleFunc("POST /admin/groups", s.authed(s.groupSave))
	mux.HandleFunc("POST /admin/groups/{id}", s.authed(s.groupSave))
	mux.HandleFunc("POST /admin/groups/{id}/delete", s.authed(s.groupDelete))
	mux.HandleFunc("POST /admin/groups/{id}/restore", s.authed(s.groupRestore))

	mux.HandleFunc("GET /admin/carmis", s.authed(s.carmisList))
	mux.HandleFunc("GET /admin/carmis/import", s.authed(s.carmisImportForm))
	mux.HandleFunc("POST /admin/carmis/import", s.authed(s.carmisImport))
	mux.HandleFunc("GET /admin/carmis/{id}/edit", s.authed(s.carmiEdit))
	mux.HandleFunc("POST /admin/carmis/{id}", s.authed(s.carmiSave))
	mux.HandleFunc("POST /admin/carmis/{id}/delete", s.authed(s.carmiDelete))
	mux.HandleFunc("POST /admin/carmis/{id}/restore", s.authed(s.carmiRestore))

	mux.HandleFunc("GET /admin/coupons", s.authed(s.couponsPage))
	mux.HandleFunc("GET /admin/coupons/{id}/edit", s.authed(s.couponEdit))
	mux.HandleFunc("POST /admin/coupons", s.authed(s.couponSave))
	mux.HandleFunc("POST /admin/coupons/{id}", s.authed(s.couponSave))
	mux.HandleFunc("POST /admin/coupons/{id}/delete", s.authed(s.couponDelete))
	mux.HandleFunc("POST /admin/coupons/{id}/restore", s.authed(s.couponRestore))

	mux.HandleFunc("GET /admin/orders", s.authed(s.ordersList))
	mux.HandleFunc("GET /admin/orders/{id}", s.authed(s.orderDetail))
	mux.HandleFunc("POST /admin/orders/{id}", s.authed(s.orderSave))

	mux.HandleFunc("GET /admin/pays", s.authed(s.paysPage))
	mux.HandleFunc("GET /admin/pays/{id}/edit", s.authed(s.payEdit))
	mux.HandleFunc("POST /admin/pays", s.authed(s.paySave))
	mux.HandleFunc("POST /admin/pays/{id}", s.authed(s.paySave))

	mux.HandleFunc("GET /admin/emailtpls", s.authed(s.mailPage))
	mux.HandleFunc("GET /admin/emailtpls/{id}/edit", s.authed(s.mailEdit))
	mux.HandleFunc("POST /admin/emailtpls", s.authed(s.mailSave))
	mux.HandleFunc("POST /admin/emailtpls/{id}", s.authed(s.mailSave))

	mux.HandleFunc("GET /admin/settings", s.authed(s.settingsForm))
	mux.HandleFunc("POST /admin/settings", s.authed(s.settingsSave))
}

func (s *Server) authed(next func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r)
		if !ok {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		next(w, r, u)
	}
}

func (s *Server) ready(w http.ResponseWriter, u session) bool {
	if s.db() != nil {
		return true
	}
	s.render(w, http.StatusServiceUnavailable, "message", messagePage{
		View: View{Title: "错误", User: u.Name, Err: "数据库未连接"},
		Back: "/admin/login",
	})
	return false
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, u session, nav, msg string) {
	s.render(w, http.StatusInternalServerError, "message", messagePage{
		View: View{Title: "错误", User: u.Name, Nav: nav, Err: msg},
		Back: "/admin",
	})
}

func (s *Server) shell(u session, title, nav string, r *http.Request) View {
	v := View{Title: title, User: u.Name, Nav: nav}
	if r != nil {
		ok := r.URL.Query().Get("ok")
		if len(ok) <= 80 {
			v.Ok = ok
		}
	}
	return v
}

type messagePage struct {
	View
	Back string
}

func sessionKey() ([]byte, bool) {
	k := os.Getenv("DUFAKA_SESSION_KEY")
	if k == "" {
		return nil, false
	}
	return []byte(k), true
}

func signSession(uid int64, name string, exp time.Time) string {
	key, ok := sessionKey()
	if !ok {
		return ""
	}
	raw, _ := json.Marshal(session{UID: uid, Exp: exp.Unix(), Name: name})
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + hex.EncodeToString(mac.Sum(nil))
}

func verifySession(token string, now time.Time) (session, bool) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || payload == "" || sig == "" {
		return session{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return session{}, false
	}
	key, ok := sessionKey()
	if !ok {
		return session{}, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	sum, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(sum, mac.Sum(nil)) {
		return session{}, false
	}
	var s session
	if json.Unmarshal(raw, &s) != nil || s.UID <= 0 {
		return session{}, false
	}
	if !now.Before(time.Unix(s.Exp, 0)) {
		return session{}, false
	}
	return s, true
}

func currentUser(r *http.Request) (session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return session{}, false
	}
	return verifySession(c.Value, time.Now())
}

func setSession(w http.ResponseWriter, r *http.Request, uid int64, name string) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    signSession(uid, name, exp),
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
	})
}

func passwordOK(hash, plain string) bool {
	return bcryptCompare(hash, plain)
}

func orderStatus(v int) string {
	switch v {
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
}

func parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求不正确", http.StatusBadRequest)
		return false
	}
	return true
}

func pathID(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	return id, err == nil && id > 0
}

func formInt(r *http.Request, key string) (int, bool) {
	v := strings.TrimSpace(r.FormValue(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}

func mustInt(r *http.Request, key string, fallback int) int {
	n, ok := formInt(r, key)
	if !ok {
		return fallback
	}
	return n
}

func normalizeNL(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

func runeLen(s string) int { return len([]rune(s)) }

const pageSize = 50

func pageArgs(r *http.Request) (page, limit int) {
	page, ok := formIntQuery(r, "page")
	if !ok || page < 1 {
		page = 1
	}
	if page > 10000 {
		page = 10000
	}
	return page, pageSize
}

func formIntQuery(r *http.Request, key string) (int, bool) {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}

func pageLinks(r *http.Request, page int, hasNext bool) (prev, next string) {
	q := r.URL.Query()
	if page > 1 {
		q.Set("page", strconv.Itoa(page-1))
		prev = r.URL.Path + "?" + q.Encode()
	}
	if hasNext {
		q.Set("page", strconv.Itoa(page+1))
		next = r.URL.Path + "?" + q.Encode()
	}
	return prev, next
}

func redirectOK(w http.ResponseWriter, r *http.Request, path, msg string) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	http.Redirect(w, r, path+sep+"ok="+url.QueryEscape(msg), http.StatusFound)
}

func dbErr(err error) string {
	if err == nil {
		return ""
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505":
			return "已存在相同的记录"
		case "23503":
			return "关联数据不存在或仍被引用"
		default:
			return "数据库错误：" + pe.Message
		}
	}
	return err.Error()
}

func trashed(r *http.Request) bool {
	return r.URL.Query().Get("trashed") == "1"
}

type opt struct {
	ID    int
	Name  string
	Value string
	Label string
}

func (s *Server) nameOptions(r *http.Request, query string, args ...any) ([]opt, error) {
	rows, err := s.db().Query(r.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []opt
	for rows.Next() {
		var o opt
		if err := rows.Scan(&o.ID, &o.Name); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
