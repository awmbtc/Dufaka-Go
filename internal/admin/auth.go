package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func bcryptCompare(hash, plain string) bool {
	if hash == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

type loginPage struct {
	View
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentUser(r); ok {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	s.render(w, http.StatusOK, "login", loginPage{View: View{Title: "登录"}})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	user := strings.TrimSpace(r.FormValue("username"))
	pass := r.FormValue("password")
	page := loginPage{View: View{Title: "登录"}}
	if user == "" || pass == "" {
		page.Err = "请填写用户名和密码"
		s.render(w, http.StatusBadRequest, "login", page)
		return
	}
	if s.pool == nil {
		page.Err = "数据库未连接"
		s.render(w, http.StatusServiceUnavailable, "login", page)
		return
	}
	if _, ok := sessionKey(); !ok {
		page.Err = "服务器未配置 DUFAKA_SESSION_KEY"
		s.render(w, http.StatusServiceUnavailable, "login", page)
		return
	}
	var id int64
	var hash, name string
	err := s.pool.QueryRow(r.Context(), `SELECT id, password, name FROM admin_users WHERE username=$1`, user).Scan(&id, &hash, &name)
	if err != nil || !passwordOK(hash, pass) {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			page.Err = dbErr(err)
			s.render(w, http.StatusInternalServerError, "login", page)
			return
		}
		page.Err = "账号或密码错误"
		s.render(w, http.StatusUnauthorized, "login", page)
		return
	}
	if strings.TrimSpace(name) == "" {
		name = user
	}
	setSession(w, r, id, name)
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w)
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}
