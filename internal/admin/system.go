package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type mailForm struct {
	ID                   int
	Name, Token, Content string
}

type mailPage struct {
	View
	Rows   []mailForm
	Form   mailForm
	Action string
}

func (s *Server) mailPage(w http.ResponseWriter, r *http.Request, u session) {
	s.renderMail(w, r, u, mailForm{}, "/admin/emailtpls", "", http.StatusOK)
}

func (s *Server) mailEdit(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var f mailForm
	err := s.pool.QueryRow(r.Context(), `SELECT id, tpl_name, tpl_token, tpl_content FROM emailtpls WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&f.ID, &f.Name, &f.Token, &f.Content)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, u, "email", "记录不存在")
		return
	}
	if err != nil {
		s.fail(w, u, "email", dbErr(err))
		return
	}
	s.renderMail(w, r, u, f, "/admin/emailtpls/"+strconv.Itoa(id), "", http.StatusOK)
}

func (s *Server) renderMail(w http.ResponseWriter, r *http.Request, u session, form mailForm, action, msg string, status int) {
	if !s.ready(w, u) {
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT id, tpl_name, tpl_token, tpl_content FROM emailtpls WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		s.fail(w, u, "email", dbErr(err))
		return
	}
	defer rows.Close()
	var list []mailForm
	for rows.Next() {
		var row mailForm
		if err := rows.Scan(&row.ID, &row.Name, &row.Token, &row.Content); err != nil {
			s.fail(w, u, "email", dbErr(err))
			return
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, u, "email", dbErr(err))
		return
	}
	title := "邮件模板"
	if form.ID > 0 {
		title = "编辑邮件模板"
	}
	p := mailPage{View: s.shell(u, title, "email", r), Rows: list, Form: form, Action: action}
	p.Err = msg
	s.render(w, status, "emailtpls", p)
}

func readMail(r *http.Request, editing bool) (mailForm, error) {
	f := mailForm{
		Name:    strings.TrimSpace(r.FormValue("tpl_name")),
		Token:   strings.TrimSpace(r.FormValue("tpl_token")),
		Content: r.FormValue("tpl_content"),
	}
	if f.Name == "" || runeLen(f.Name) > 150 {
		return f, errors.New("请填写邮件标题")
	}
	if !editing {
		if f.Token == "" || runeLen(f.Token) > 50 || strings.ContainsAny(f.Token, " \t\r\n") {
			return f, errors.New("请填写邮件标识")
		}
	}
	if strings.TrimSpace(f.Content) == "" {
		return f, errors.New("请填写邮件内容")
	}
	if runeLen(f.Content) > 200000 {
		return f, errors.New("邮件内容过长")
	}
	return f, nil
}

func (s *Server) mailSave(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	id, _ := pathID(r)
	f, err := readMail(r, id > 0)
	f.ID = id
	action := "/admin/emailtpls"
	if id > 0 {
		action = "/admin/emailtpls/" + strconv.Itoa(id)
	}
	if err != nil {
		s.renderMail(w, r, u, f, action, err.Error(), http.StatusBadRequest)
		return
	}
	if id == 0 {
		_, err = s.pool.Exec(r.Context(), `INSERT INTO emailtpls (tpl_name, tpl_content, tpl_token, created_at, updated_at) VALUES ($1,$2,$3,now(),now())`, f.Name, f.Content, f.Token)
	} else {
		var n int64
		n, err = execCount(s, r, `UPDATE emailtpls SET tpl_name=$1, tpl_content=$2, updated_at=now() WHERE id=$3 AND deleted_at IS NULL`, f.Name, f.Content, id)
		if err == nil && n == 0 {
			err = errors.New("记录不存在或已删除")
		}
	}
	if err != nil {
		s.renderMail(w, r, u, f, action, dbErr(err), http.StatusBadRequest)
		return
	}
	redirectOK(w, r, "/admin/emailtpls", "保存成功")
}

type settingField struct {
	Key, Label, Value, Help, Kind string
	Options                       []opt
}

type settingTab struct {
	Name   string
	Fields []settingField
}

type settingsPage struct {
	View
	Tabs []settingTab
}

func settingValue(m map[string]string, key, def string) string {
	if m == nil {
		return def
	}
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return def
}

func settingTabs(m map[string]string) []settingTab {
	text := func(key, label, def, help string) settingField {
		return settingField{Key: key, Label: label, Value: settingValue(m, key, def), Help: help, Kind: "text"}
	}
	area := func(key, label, def string) settingField {
		return settingField{Key: key, Label: label, Value: settingValue(m, key, def), Kind: "textarea"}
	}
	sw := func(key, label string) settingField {
		return settingField{Key: key, Label: label, Value: settingValue(m, key, "0"), Kind: "switch"}
	}
	return []settingTab{
		{Name: "基本设置", Fields: []settingField{
			text("title", "网站标题", "", ""),
			text("img_logo", "图片LOGO", "", "填写图片地址"),
			text("text_logo", "文字LOGO", "", ""),
			text("keywords", "网站关键词", "", ""),
			area("description", "网站描述", ""),
			{Key: "template", Label: "站点模板", Kind: "select", Value: settingValue(m, "template", "unicorn"), Options: []opt{
				{Value: "unicorn", Label: "官方[unicorn-独角兽]"},
				{Value: "luna", Label: "官方[luna-露娜]"},
				{Value: "hyper", Label: "官方[hyper-极光]"},
			}},
			{Key: "language", Label: "站点语言", Kind: "select", Value: settingValue(m, "language", "zh_CN"), Options: []opt{
				{Value: "zh_CN", Label: "简体中文"},
				{Value: "zh_TW", Label: "繁体中文"},
			}},
			text("manage_email", "管理员邮箱", "", ""),
			text("order_expire_time", "订单过期时间(分钟)", "5", ""),
			sw("is_open_anti_red", "是否开启微信/QQ防红"),
			sw("is_open_img_code", "是否开启图形验证码"),
			sw("is_open_search_pwd", "是否开启查询密码"),
			sw("is_open_google_translate", "是否开启google翻译"),
			area("notice", "站点公告", ""),
			area("footer", "页脚自定义代码", ""),
		}},
		{Name: "订单推送配置", Fields: []settingField{
			sw("is_open_server_jiang", "是否开启server酱"),
			text("server_jiang_token", "server酱通讯token", "", ""),
			sw("is_open_telegram_push", "是否开启Telegram推送"),
			text("telegram_bot_token", "Telegram通讯token", "", ""),
			text("telegram_userid", "Telegram用户id", "", ""),
			sw("is_open_bark_push", "是否开启Bark推送"),
			sw("is_open_bark_push_url", "是否推送订单URL"),
			text("bark_server", "Bark服务器", "", ""),
			text("bark_token", "Bark通讯Token", "", ""),
			sw("is_open_qywxbot_push", "是否开启企业微信Bot推送"),
			text("qywxbot_key", "企业微信Bot通讯Key", "", ""),
		}},
		{Name: "邮件服务", Fields: []settingField{
			text("driver", "邮件驱动", "smtp", ""),
			text("host", "smtp服务器地址", "", ""),
			text("port", "端口", "587", ""),
			text("username", "账号", "", ""),
			{Key: "password", Label: "密码", Kind: "password", Help: "留空则不修改"},
			text("encryption", "协议", "", ""),
			text("from_address", "发件地址", "", ""),
			text("from_name", "发件名称", "", ""),
		}},
		{Name: "极验验证", Fields: []settingField{
			text("geetest_id", "极验id", "", ""),
			text("geetest_key", "极验key", "", ""),
			sw("is_open_geetest", "是否开启极验"),
		}},
	}
}

func (s *Server) loadSettings(r *http.Request) (map[string]string, error) {
	rows, err := s.pool.Query(r.Context(), `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func applySettings(old map[string]string, r *http.Request) (map[string]string, error) {
	out := map[string]string{}
	for _, tab := range settingTabs(nil) {
		for _, f := range tab.Fields {
			raw := r.FormValue(f.Key)
			val := strings.TrimSpace(raw)
			switch f.Kind {
			case "password":
				// Blank keeps the stored SMTP password. A non-empty value is kept as entered.
				if raw == "" {
					val = old[f.Key]
				} else {
					val = raw
				}
			case "textarea":
				val = normalizeNL(raw)
			case "switch":
				if val != "1" {
					val = "0"
				}
			}
			out[f.Key] = val
		}
	}
	if strings.TrimSpace(out["title"]) == "" {
		return out, errors.New("请填写网站标题")
	}
	switch out["template"] {
	case "unicorn", "luna", "hyper":
	default:
		return out, errors.New("请选择站点模板")
	}
	switch out["language"] {
	case "zh_CN", "zh_TW":
	default:
		return out, errors.New("请选择站点语言")
	}
	n, err := strconv.Atoi(out["order_expire_time"])
	if err != nil || n <= 0 || n > 100000 {
		return out, errors.New("订单过期时间必须是正整数")
	}
	if out["driver"] == "" {
		return out, errors.New("请填写邮件驱动")
	}
	if out["port"] != "" {
		p, err := strconv.Atoi(out["port"])
		if err != nil || p < 1 || p > 65535 {
			return out, errors.New("端口不正确")
		}
	}
	return out, nil
}

func (s *Server) settingsForm(w http.ResponseWriter, r *http.Request, u session) {
	s.renderSettings(w, r, u, nil, "", http.StatusOK)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, u session, posted map[string]string, msg string, status int) {
	if !s.ready(w, u) {
		return
	}
	vals := posted
	if vals == nil {
		var err error
		vals, err = s.loadSettings(r)
		if err != nil {
			s.fail(w, u, "settings", dbErr(err))
			return
		}
	}
	p := settingsPage{View: s.shell(u, "系统设置", "settings", r), Tabs: settingTabs(vals)}
	p.Err = msg
	s.render(w, status, "settings", p)
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	old, err := s.loadSettings(r)
	if err != nil {
		s.fail(w, u, "settings", dbErr(err))
		return
	}
	vals, err := applySettings(old, r)
	if err != nil {
		s.renderSettings(w, r, u, vals, err.Error(), http.StatusBadRequest)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.renderSettings(w, r, u, vals, dbErr(err), http.StatusBadRequest)
		return
	}
	defer tx.Rollback(r.Context())
	for k, v := range vals {
		if _, err = tx.Exec(r.Context(), `
			INSERT INTO settings (key, value, updated_at) VALUES ($1,$2,now())
			ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, k, v); err != nil {
			s.renderSettings(w, r, u, vals, dbErr(err), http.StatusBadRequest)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.renderSettings(w, r, u, vals, dbErr(err), http.StatusBadRequest)
		return
	}
	redirectOK(w, r, "/admin/settings", "系统配置保存成功")
}

type channelStat struct {
	Name  string
	Count int
	Ratio string
}

type popularGood struct {
	Name, Price string
	Sales       int
}

type dashPage struct {
	View
	Sales, Week, Ratio                     string
	Success, Paid, Total, Wait, Pending    int
	Processing, Failure, Abnormal, Expired int
	Channels                               []channelStat
	Popular                                []popularGood
}

func percent(part, total int) string {
	if total <= 0 {
		return "0%"
	}
	return strconv.FormatFloat(float64(part)*100/float64(total), 'f', 1, 64) + "%"
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	p := dashPage{View: s.shell(u, "仪表盘", "home", r)}
	err := s.pool.QueryRow(r.Context(), `
		SELECT
			COALESCE(SUM(actual_price) FILTER (WHERE status NOT IN (1, -1)), 0)::text,
			COALESCE(SUM(actual_price) FILTER (WHERE status NOT IN (1, -1) AND created_at >= now() - interval '7 days'), 0)::text,
			COUNT(*) FILTER (WHERE status = 4),
			COUNT(*) FILTER (WHERE status NOT IN (1, -1)),
			COUNT(*),
			COUNT(*) FILTER (WHERE status = 1),
			COUNT(*) FILTER (WHERE status = 2),
			COUNT(*) FILTER (WHERE status = 3),
			COUNT(*) FILTER (WHERE status = 5),
			COUNT(*) FILTER (WHERE status = 6),
			COUNT(*) FILTER (WHERE status = -1)
		FROM orders WHERE deleted_at IS NULL`).Scan(
		&p.Sales, &p.Week, &p.Success, &p.Paid, &p.Total, &p.Wait, &p.Pending, &p.Processing, &p.Failure, &p.Abnormal, &p.Expired)
	if err != nil {
		s.fail(w, u, "home", dbErr(err))
		return
	}
	p.Ratio = percent(p.Paid, p.Total)
	rows, err := s.pool.Query(r.Context(), `
		SELECT COALESCE(p.pay_name, '未指定'), COUNT(*)::int
		FROM orders o LEFT JOIN pays p ON p.id=o.pay_id
		WHERE o.deleted_at IS NULL AND o.status NOT IN (1, -1)
		GROUP BY 1 ORDER BY COUNT(*) DESC`)
	if err != nil {
		s.fail(w, u, "home", dbErr(err))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c channelStat
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			s.fail(w, u, "home", dbErr(err))
			return
		}
		c.Ratio = percent(c.Count, p.Paid)
		p.Channels = append(p.Channels, c)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, u, "home", dbErr(err))
		return
	}
	prows, err := s.pool.Query(r.Context(), `
		SELECT gd_name, COALESCE(sales_volume,0), COALESCE(actual_price,0)::text
		FROM goods WHERE deleted_at IS NULL
		ORDER BY COALESCE(sales_volume,0) DESC, id DESC LIMIT 5`)
	if err != nil {
		s.fail(w, u, "home", dbErr(err))
		return
	}
	defer prows.Close()
	for prows.Next() {
		var g popularGood
		if err := prows.Scan(&g.Name, &g.Sales, &g.Price); err != nil {
			s.fail(w, u, "home", dbErr(err))
			return
		}
		p.Popular = append(p.Popular, g)
	}
	if err := prows.Err(); err != nil {
		s.fail(w, u, "home", dbErr(err))
		return
	}
	s.render(w, http.StatusOK, "dashboard", p)
}
