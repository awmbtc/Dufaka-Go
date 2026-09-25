package install

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

//go:embed schema.sql
var schemaFS embed.FS

var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Form is what the install page submits. Passwords stay out of logs.
type Form struct {
	Host     string
	Port     string
	Database string
	User     string
	Password string
	Title    string
	AppURL   string
	Admin    string
	AdminPwd string
	Confirm  string
}

// Result is shown on the finished step.
type Result struct {
	Title      string
	AppURL     string
	AdminUser  string
	ConfigPath string
	DSN        string
	SessionKey string
}

// Page is the wizard state.
type Page struct {
	ConfigPath string
	Writable   bool
	Connected  bool
	Tables     bool
	Host       string
	Port       string
	Database   string
	User       string
	Title      string
	AppURL     string
	Err        string
	Probe      string
	Done       *Result
}

func DefaultPage(configPath, base string) Page {
	return Page{
		ConfigPath: configPath,
		Writable:   writable(configPath),
		Host:       "127.0.0.1",
		Port:       "5432",
		Database:   "dufaka",
		User:       "dufaka",
		Title:      "Dufaka-Go",
		AppURL:     base,
	}
}

func writable(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false
	}
	info, err := f.Stat()
	_ = f.Close()
	if err != nil {
		return false
	}
	if info.Size() == 0 {
		_ = os.Remove(path)
	}
	return true
}

func (f Form) dsn(database string) (string, error) {
	if !ident.MatchString(f.Database) || !ident.MatchString(f.User) {
		return "", errors.New("数据库名和用户名只能使用字母、数字和下划线")
	}
	if f.Host == "" || f.Port == "" {
		return "", errors.New("请填写数据库主机和端口")
	}
	if _, err := net.LookupPort("tcp", f.Port); err != nil {
		return "", errors.New("数据库端口不正确")
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(f.User, f.Password),
		Host:   net.JoinHostPort(f.Host, f.Port),
		Path:   "/" + database,
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Probe checks that PostgreSQL accepts the account.
func Probe(ctx context.Context, f Form) error {
	dsn, err := f.dsn("postgres")
	if err != nil {
		return err
	}
	if err := ping(ctx, dsn); err != nil {
		return fmt.Errorf("数据库连接失败：%s", cleanErr(err))
	}
	return nil
}

// Run creates the database and tables, the admin, and the local config file.
func Run(ctx context.Context, f Form, configPath string) (Result, error) {
	if strings.TrimSpace(f.Admin) == "" || len(f.AdminPwd) < 6 {
		return Result{}, errors.New("请填写管理员账号，密码至少 6 位")
	}
	if f.AdminPwd != f.Confirm {
		return Result{}, errors.New("两次输入的密码不一致")
	}
	if strings.TrimSpace(f.Title) == "" {
		f.Title = "Dufaka-Go"
	}
	if strings.TrimSpace(f.AppURL) == "" {
		return Result{}, errors.New("请填写站点网址")
	}
	adminDSN, err := f.dsn("postgres")
	if err != nil {
		return Result{}, err
	}
	if err := ping(ctx, adminDSN); err != nil {
		return Result{}, fmt.Errorf("数据库连接失败：%s", cleanErr(err))
	}
	if err := ensureDatabase(ctx, adminDSN, f.Database); err != nil {
		return Result{}, err
	}
	appDSN, err := f.dsn(f.Database)
	if err != nil {
		return Result{}, err
	}
	conn, err := connect(ctx, appDSN)
	if err != nil {
		return Result{}, fmt.Errorf("无法打开业务库：%s", cleanErr(err))
	}
	defer conn.Close(ctx)
	var ready bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.admin_users') IS NOT NULL`).Scan(&ready); err != nil {
		return Result{}, err
	}
	if !ready {
		if err := applySchema(ctx, conn); err != nil {
			return Result{}, fmt.Errorf("创建数据表失败：%s", cleanErr(err))
		}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	var admins int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&admins); err != nil {
		return Result{}, err
	}
	if admins > 0 {
		return Result{}, errors.New("已经安装过了")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(f.AdminPwd), bcrypt.DefaultCost)
	if err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO admin_users (username, password, name, created_at, updated_at) VALUES ($1,$2,$3,now(),now())`, f.Admin, string(hash), f.Admin); err != nil {
		return Result{}, err
	}
	for _, kv := range siteSettings(f) {
		if _, err := tx.Exec(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ($1,$2,now()) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, kv[0], kv[1]); err != nil {
			return Result{}, err
		}
	}
	if err := seed(ctx, tx); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	sessionKey, err := randomKey()
	if err != nil {
		return Result{}, err
	}
	if err := SaveConfig(configPath, File{DatabaseURL: appDSN, SessionKey: sessionKey, BaseURL: strings.TrimRight(f.AppURL, "/")}); err != nil {
		return Result{}, fmt.Errorf("配置文件没有写成：%s", err.Error())
	}
	return Result{Title: f.Title, AppURL: strings.TrimRight(f.AppURL, "/"), AdminUser: f.Admin, ConfigPath: configPath, DSN: appDSN, SessionKey: sessionKey}, nil
}

// siteSettings is what a fresh install writes to the settings table. The
// query password is on from the start: without it an email alone would list
// a customer's orders, and the audit named switching it on as the first remedy.
func siteSettings(f Form) [][2]string {
	return [][2]string{
		{"title", f.Title},
		{"text_logo", f.Title},
		{"template", "unicorn"},
		{"language", "zh_CN"},
		{"order_expire_time", "5"},
		{"is_open_search_pwd", "1"},
		{"app_url", strings.TrimRight(f.AppURL, "/")},
		{"notice", "欢迎来到 Clodex小店。"},
	}
}

func ensureDatabase(ctx context.Context, adminDSN, name string) error {
	conn, err := connect(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = conn.Exec(ctx, `CREATE DATABASE `+name)
	if err != nil {
		return fmt.Errorf("创建数据库失败：%s。可以先手工建好同名库再安装", cleanErr(err))
	}
	return nil
}

func applySchema(ctx context.Context, conn *pgx.Conn) error {
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	for _, stmt := range splitSQL(string(raw)) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "42P07" {
				continue
			}
			return err
		}
	}
	return nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func seed(ctx context.Context, conn execer) error {
	_, err := conn.Exec(ctx, `
		INSERT INTO pays (pay_name, pay_check, pay_method, pay_client, merchant_id, merchant_key, merchant_pem, pay_handleroute, is_open, created_at, updated_at)
		SELECT '微信扫码', 'wescan', 2, 3, '', '', '', '/pay/wepay', 0, now(), now()
		WHERE NOT EXISTS (SELECT 1 FROM pays WHERE pay_check='wescan')`)
	if err != nil {
		return err
	}
	mails := []struct{ name, token, body string }{
		{"发货通知", "card_send_user_email", "您在 {webname} 购买的 {ord_title} 已发货。\n订单号：{order_id}\n数量：{buy_amount}\n金额：{ord_price}\n内容：\n{ord_info}"},
		{"人工处理通知", "manual_send_manage_mail", "有新的人工处理订单 {order_id}，商品 {ord_title}。"},
		{"待处理", "pending_order", "订单 {order_id} 待处理。商品 {ord_title}，金额 {ord_price}。"},
		{"已完成", "completed_order", "订单 {order_id} 已完成。商品 {ord_title}。"},
		{"失败", "failed_order", "订单 {order_id} 处理失败。商品 {ord_title}。"},
	}
	for _, m := range mails {
		if _, err := conn.Exec(ctx, `
			INSERT INTO emailtpls (tpl_name, tpl_content, tpl_token, created_at, updated_at)
			SELECT $1::text, $2::text, $3::text, now(), now()
			WHERE NOT EXISTS (SELECT 1 FROM emailtpls WHERE tpl_token=$3::text)`, m.name, m.body, m.token); err != nil {
			return err
		}
	}
	return nil
}

func ping(ctx context.Context, dsn string) error {
	conn, err := connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var one int
	return conn.QueryRow(ctx, `SELECT 1`).Scan(&one)
}

func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return pgx.Connect(c, dsn)
}

func splitSQL(raw string) []string {
	var out []string
	var b strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, part := range strings.Split(b.String(), ";") {
		stmt := strings.TrimSpace(part)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func cleanErr(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Message
	}
	msg := err.Error()
	if i := strings.Index(msg, "postgres://"); i >= 0 {
		return "连接失败"
	}
	return msg
}

func randomKey() (string, error) {
	var b [32]byte
	if _, err := randRead(&b); err != nil {
		return "", err
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, 64)
	for i, c := range b {
		out[i*2] = hexd[c>>4]
		out[i*2+1] = hexd[c&0x0f]
	}
	return string(out), nil
}

// File is the local config written by the wizard.
type File struct {
	DatabaseURL string `json:"database_url"`
	SessionKey  string `json:"session_key"`
	BaseURL     string `json:"base_url"`
	Addr        string `json:"addr,omitempty"`
}

func SaveConfig(path string, f File) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o600)
}

func LoadConfig(path string) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return File{}, err
	}
	return f, nil
}
