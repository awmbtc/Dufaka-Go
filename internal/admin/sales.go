package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type couponRow struct {
	ID, Ret, Use, Open             int
	Code, Discount, Goods, Created string
	Trashed                        bool
}

type couponForm struct {
	ID, Ret, Use, Open int
	Code, Discount     string
	Goods              []int
}

type couponsPage struct {
	View
	Rows     []couponRow
	Goods    []opt
	Form     couponForm
	Selected map[int]bool
	Action   string
	Trashed  bool
}

func (s *Server) allGoods(r *http.Request) ([]opt, error) {
	return s.nameOptions(r, `SELECT id, gd_name FROM goods WHERE deleted_at IS NULL ORDER BY id DESC`)
}

func (s *Server) couponsPage(w http.ResponseWriter, r *http.Request, u session) {
	s.renderCoupons(w, r, u, couponForm{Ret: 1, Use: 1, Open: 1}, "/admin/coupons", "", http.StatusOK)
}

func (s *Server) couponEdit(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var f couponForm
	err := s.db().QueryRow(r.Context(), `SELECT id, coupon, discount::text, ret, is_use, is_open FROM coupons WHERE id=$1 AND deleted_at IS NULL`, id).
		Scan(&f.ID, &f.Code, &f.Discount, &f.Ret, &f.Use, &f.Open)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, u, "coupons", "记录不存在")
		return
	}
	if err != nil {
		s.fail(w, u, "coupons", dbErr(err))
		return
	}
	rows, err := s.db().Query(r.Context(), `SELECT goods_id FROM coupons_goods WHERE coupons_id=$1`, id)
	if err != nil {
		s.fail(w, u, "coupons", dbErr(err))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var gid int
		if err := rows.Scan(&gid); err != nil {
			s.fail(w, u, "coupons", dbErr(err))
			return
		}
		f.Goods = append(f.Goods, gid)
	}
	s.renderCoupons(w, r, u, f, "/admin/coupons/"+strconv.Itoa(id), "", http.StatusOK)
}

func selectedGoods(ids []int) map[int]bool {
	m := map[int]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func (s *Server) renderCoupons(w http.ResponseWriter, r *http.Request, u session, form couponForm, action, msg string, status int) {
	if !s.ready(w, u) {
		return
	}
	goods, err := s.allGoods(r)
	if err != nil {
		s.fail(w, u, "coupons", dbErr(err))
		return
	}
	where := "c.deleted_at IS NULL"
	if trashed(r) {
		where = "c.deleted_at IS NOT NULL"
	}
	rows, err := s.db().Query(r.Context(), `
		SELECT c.id, c.coupon, c.discount::text, c.ret, c.is_use, c.is_open,
			to_char(COALESCE(c.created_at, now()), 'YYYY-MM-DD HH24:MI'),
			c.deleted_at IS NOT NULL,
			COALESCE((SELECT string_agg(g.gd_name, '、') FROM coupons_goods cg JOIN goods g ON g.id=cg.goods_id WHERE cg.coupons_id=c.id), '')
		FROM coupons c WHERE `+where+` ORDER BY c.id DESC LIMIT 200`)
	if err != nil {
		s.fail(w, u, "coupons", dbErr(err))
		return
	}
	defer rows.Close()
	var list []couponRow
	for rows.Next() {
		var row couponRow
		if err := rows.Scan(&row.ID, &row.Code, &row.Discount, &row.Ret, &row.Use, &row.Open, &row.Created, &row.Trashed, &row.Goods); err != nil {
			s.fail(w, u, "coupons", dbErr(err))
			return
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, u, "coupons", dbErr(err))
		return
	}
	title := "优惠码"
	if form.ID > 0 {
		title = "编辑优惠码"
	}
	p := couponsPage{
		View: s.shell(u, title, "coupons", r), Rows: list, Goods: goods, Form: form,
		Selected: selectedGoods(form.Goods), Action: action, Trashed: trashed(r),
	}
	p.Err = msg
	s.render(w, status, "coupons", p)
}

func readCoupon(r *http.Request) (couponForm, error) {
	discount, err := money(r.FormValue("discount"), false)
	f := couponForm{
		Code:     strings.TrimSpace(r.FormValue("coupon")),
		Discount: discount,
		Ret:      mustInt(r, "ret", -1),
		Use:      mustInt(r, "is_use", 1),
		Open:     mustInt(r, "is_open", 1),
	}
	if err != nil {
		return f, errors.New("优惠金额" + err.Error())
	}
	if f.Code == "" {
		return f, errors.New("请填写优惠码")
	}
	if runeLen(f.Code) > 150 {
		return f, errors.New("优惠码超过 150 字")
	}
	if strings.TrimSpace(r.FormValue("ret")) == "" || f.Ret < 0 {
		return f, errors.New("请填写剩余使用次数")
	}
	if f.Use != 1 && f.Use != 2 {
		return f, errors.New("使用状态不正确")
	}
	if f.Open != 0 && f.Open != 1 {
		return f, errors.New("是否启用不正确")
	}
	for _, raw := range r.Form["goods_id"] {
		id, err := strconv.Atoi(raw)
		if err != nil || id < 1 {
			return f, errors.New("可用商品不正确")
		}
		f.Goods = append(f.Goods, id)
	}
	return f, nil
}

func (s *Server) couponSave(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	id, _ := pathID(r)
	f, err := readCoupon(r)
	f.ID = id
	action := "/admin/coupons"
	if id > 0 {
		action = "/admin/coupons/" + strconv.Itoa(id)
	}
	if err != nil {
		s.renderCoupons(w, r, u, f, action, err.Error(), http.StatusBadRequest)
		return
	}
	tx, err := s.db().Begin(r.Context())
	if err != nil {
		s.renderCoupons(w, r, u, f, action, dbErr(err), http.StatusBadRequest)
		return
	}
	defer tx.Rollback(r.Context())
	goodsIDs := uniqueInts(f.Goods)
	for _, gid := range goodsIDs {
		var one int
		err = tx.QueryRow(r.Context(), `SELECT id FROM goods WHERE id=$1 AND deleted_at IS NULL`, gid).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			s.renderCoupons(w, r, u, f, action, "可用商品不存在", http.StatusBadRequest)
			return
		}
		if err != nil {
			s.renderCoupons(w, r, u, f, action, dbErr(err), http.StatusBadRequest)
			return
		}
	}
	if id == 0 {
		err = tx.QueryRow(r.Context(), `
			INSERT INTO coupons (discount, is_use, is_open, coupon, ret, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,now(),now()) RETURNING id`, f.Discount, f.Use, f.Open, f.Code, f.Ret).Scan(&id)
	} else {
		var tag int64
		tag, err = execTx(tx, r, `UPDATE coupons SET discount=$1, is_use=$2, is_open=$3, coupon=$4, ret=$5, updated_at=now() WHERE id=$6 AND deleted_at IS NULL`, f.Discount, f.Use, f.Open, f.Code, f.Ret, id)
		if err == nil && tag == 0 {
			err = errors.New("记录不存在或已删除")
		}
	}
	if err != nil {
		s.renderCoupons(w, r, u, f, action, dbErr(err), http.StatusBadRequest)
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM coupons_goods WHERE coupons_id=$1`, id); err != nil {
		s.renderCoupons(w, r, u, f, action, dbErr(err), http.StatusBadRequest)
		return
	}
	for _, gid := range goodsIDs {
		if _, err = tx.Exec(r.Context(), `INSERT INTO coupons_goods (goods_id, coupons_id) VALUES ($1,$2)`, gid, id); err != nil {
			s.renderCoupons(w, r, u, f, action, dbErr(err), http.StatusBadRequest)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.renderCoupons(w, r, u, f, action, dbErr(err), http.StatusBadRequest)
		return
	}
	redirectOK(w, r, "/admin/coupons", "保存成功")
}

func uniqueInts(in []int) []int {
	seen := map[int]struct{}{}
	var out []int
	for _, n := range in {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func execTx(tx pgx.Tx, r *http.Request, sql string, args ...any) (int64, error) {
	tag, err := tx.Exec(r.Context(), sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Server) couponDelete(w http.ResponseWriter, r *http.Request, u session) {
	s.markDeleted(w, r, u, "coupons", "coupons", false)
}

func (s *Server) couponRestore(w http.ResponseWriter, r *http.Request, u session) {
	s.markDeleted(w, r, u, "coupons", "coupons", true)
}

type orderRow struct {
	ID, Amount, Status, Type                                     int
	SN, Title, Email, Actual, Pwd, Trade, Pay, Created, Info, IP string
	GoodsPrice, Total, CouponOff, WholesaleOff                   string
}

type ordersPage struct {
	View
	Rows                            []orderRow
	SN, Title, Email, Trade, Status string
}

type orderPage struct {
	View
	Row    orderRow
	Action string
}

func (s *Server) ordersList(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	where := []string{"o.deleted_at IS NULL"}
	args := []any{}
	add := func(cond string, val string) {
		args = append(args, val)
		where = append(where, cond+"$"+strconv.Itoa(len(args)))
	}
	sn := strings.TrimSpace(r.URL.Query().Get("order_sn"))
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	trade := strings.TrimSpace(r.URL.Query().Get("trade_no"))
	status := r.URL.Query().Get("status")
	if sn != "" {
		add("o.order_sn=", sn)
	}
	if title != "" {
		args = append(args, "%"+title+"%")
		where = append(where, "o.title ILIKE $"+strconv.Itoa(len(args)))
	}
	if email != "" {
		add("o.email=", email)
	}
	if trade != "" {
		add("o.trade_no=", trade)
	}
	switch status {
	case "1", "2", "3", "4", "5", "6", "-1":
		args = append(args, status)
		where = append(where, "o.status=$"+strconv.Itoa(len(args)))
	}
	page, limit := pageArgs(r)
	args = append(args, limit+1, (page-1)*limit)
	q := `SELECT o.id, o.order_sn, o.title, o.email, o.buy_amount, o.actual_price::text,
		o.status, COALESCE(o.search_pwd,''), COALESCE(o.trade_no,''), COALESCE(p.pay_name,''),
		to_char(COALESCE(o.created_at, now()), 'YYYY-MM-DD HH24:MI'), COALESCE(o.info,''), o.type
		FROM orders o LEFT JOIN pays p ON p.id=o.pay_id
		WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY o.id DESC LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := s.db().Query(r.Context(), q, args...)
	if err != nil {
		s.fail(w, u, "orders", dbErr(err))
		return
	}
	defer rows.Close()
	var list []orderRow
	for rows.Next() {
		var row orderRow
		if err := rows.Scan(&row.ID, &row.SN, &row.Title, &row.Email, &row.Amount, &row.Actual, &row.Status, &row.Pwd, &row.Trade, &row.Pay, &row.Created, &row.Info, &row.Type); err != nil {
			s.fail(w, u, "orders", dbErr(err))
			return
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, u, "orders", dbErr(err))
		return
	}
	hasNext := len(list) > limit
	if hasNext {
		list = list[:limit]
	}
	prev, next := pageLinks(r, page, hasNext)
	p := ordersPage{View: s.shell(u, "订单列表", "orders", r), Rows: list, SN: sn, Title: title, Email: email, Trade: trade, Status: status}
	p.Prev, p.Next = prev, next
	s.render(w, http.StatusOK, "orders", p)
}

func (s *Server) loadOrder(r *http.Request, id int) (orderRow, error) {
	var row orderRow
	err := s.db().QueryRow(r.Context(), `
		SELECT o.id, o.order_sn, o.title, o.email, o.buy_amount, o.goods_price::text, o.total_price::text,
			o.coupon_discount_price::text, o.wholesale_discount_price::text, o.actual_price::text,
			o.status, COALESCE(o.search_pwd,''), COALESCE(o.trade_no,''), COALESCE(p.pay_name,''),
			to_char(COALESCE(o.created_at, now()), 'YYYY-MM-DD HH24:MI'), COALESCE(o.info,''), o.buy_ip, o.type
		FROM orders o LEFT JOIN pays p ON p.id=o.pay_id
		WHERE o.id=$1 AND o.deleted_at IS NULL`, id).Scan(
		&row.ID, &row.SN, &row.Title, &row.Email, &row.Amount, &row.GoodsPrice, &row.Total,
		&row.CouponOff, &row.WholesaleOff, &row.Actual, &row.Status, &row.Pwd, &row.Trade, &row.Pay,
		&row.Created, &row.Info, &row.IP, &row.Type)
	return row, err
}

func (s *Server) orderDetail(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	row, err := s.loadOrder(r, id)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, u, "orders", "记录不存在")
		return
	}
	if err != nil {
		s.fail(w, u, "orders", dbErr(err))
		return
	}
	p := orderPage{View: s.shell(u, "订单详情", "orders", r), Row: row, Action: "/admin/orders/" + strconv.Itoa(id)}
	s.render(w, http.StatusOK, "order_detail", p)
}

func validOrderStatus(v int) bool {
	switch v {
	case -1, 1, 2, 3, 4, 5, 6:
		return true
	default:
		return false
	}
}

func (s *Server) orderSave(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	row, err := s.loadOrder(r, id)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, u, "orders", "记录不存在")
		return
	}
	if err != nil {
		s.fail(w, u, "orders", dbErr(err))
		return
	}
	prevStatus := row.Status
	row.Title = strings.TrimSpace(r.FormValue("title"))
	row.Info = normalizeNL(r.FormValue("info"))
	row.Pwd = strings.TrimSpace(r.FormValue("search_pwd"))
	row.Status = mustInt(r, "status", 0)
	action := "/admin/orders/" + strconv.Itoa(id)
	if row.Title == "" || runeLen(row.Title) > 200 {
		s.render(w, http.StatusBadRequest, "order_detail", orderPage{View: View{Title: "订单详情", User: u.Name, Nav: "orders", Err: "订单名称不正确"}, Row: row, Action: action})
		return
	}
	if runeLen(row.Pwd) > 200 {
		s.render(w, http.StatusBadRequest, "order_detail", orderPage{View: View{Title: "订单详情", User: u.Name, Nav: "orders", Err: "查询密码过长"}, Row: row, Action: action})
		return
	}
	if !validOrderStatus(row.Status) {
		s.render(w, http.StatusBadRequest, "order_detail", orderPage{View: View{Title: "订单详情", User: u.Name, Nav: "orders", Err: "订单状态不正确"}, Row: row, Action: action})
		return
	}
	if (row.Status == 2 || row.Status == 4) && row.Status != prevStatus && prevStatus != 2 && prevStatus != 3 && prevStatus != 4 {
		s.render(w, http.StatusBadRequest, "order_detail", orderPage{View: View{Title: "订单详情", User: u.Name, Nav: "orders", Err: "不能直接改成已支付或已完成。未发货的卡不能标成已售。"}, Row: row, Action: action})
		return
	}
	n, err := execCount(s, r, `UPDATE orders SET title=$1, info=$2, search_pwd=$3, status=$4, updated_at=now() WHERE id=$5 AND deleted_at IS NULL`, row.Title, row.Info, row.Pwd, row.Status, id)
	if err != nil || n == 0 {
		msg := "记录不存在"
		if err != nil {
			msg = dbErr(err)
		}
		s.fail(w, u, "orders", msg)
		return
	}
	redirectOK(w, r, action, "保存成功")
}

type payForm struct {
	ID, Method, Client, Open                 int
	Name, Check, MerchantID, Key, Pem, Route string
}

type payRow struct {
	ID, Method, Client, Open                 int
	Name, Check, MerchantID, Key, Pem, Route string
	Created                                  string
}

type paysPage struct {
	View
	Rows   []payRow
	Form   payForm
	Action string
}

func (s *Server) paysPage(w http.ResponseWriter, r *http.Request, u session) {
	s.renderPays(w, r, u, payForm{Method: 1, Client: 1, Open: 1}, "/admin/pays", "", http.StatusOK)
}

func (s *Server) payEdit(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := s.loadPay(r, id)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, u, "pays", "记录不存在")
		return
	}
	if err != nil {
		s.fail(w, u, "pays", dbErr(err))
		return
	}
	s.renderPays(w, r, u, f, "/admin/pays/"+strconv.Itoa(id), "", http.StatusOK)
}

func (s *Server) loadPay(r *http.Request, id int) (payForm, error) {
	var f payForm
	err := s.db().QueryRow(r.Context(), `
		SELECT id, pay_name, pay_check, pay_method, pay_client, COALESCE(merchant_id,''),
			COALESCE(merchant_key,''), merchant_pem, pay_handleroute, is_open
		FROM pays WHERE id=$1 AND deleted_at IS NULL`, id).Scan(
		&f.ID, &f.Name, &f.Check, &f.Method, &f.Client, &f.MerchantID, &f.Key, &f.Pem, &f.Route, &f.Open)
	return f, err
}

func (s *Server) renderPays(w http.ResponseWriter, r *http.Request, u session, form payForm, action, msg string, status int) {
	if !s.ready(w, u) {
		return
	}
	rows, err := s.db().Query(r.Context(), `
		SELECT id, pay_name, pay_check, pay_method, pay_client, COALESCE(merchant_id,''),
			COALESCE(merchant_key,''), merchant_pem, pay_handleroute, is_open,
			to_char(COALESCE(created_at, now()), 'YYYY-MM-DD HH24:MI')
		FROM pays WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		s.fail(w, u, "pays", dbErr(err))
		return
	}
	defer rows.Close()
	var list []payRow
	for rows.Next() {
		var row payRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Check, &row.Method, &row.Client, &row.MerchantID, &row.Key, &row.Pem, &row.Route, &row.Open, &row.Created); err != nil {
			s.fail(w, u, "pays", dbErr(err))
			return
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, u, "pays", dbErr(err))
		return
	}
	title := "支付通道"
	if form.ID > 0 {
		title = "编辑支付通道"
	}
	p := paysPage{View: s.shell(u, title, "pays", r), Rows: list, Form: form, Action: action}
	p.Err = msg
	s.render(w, status, "pays", p)
}

func readPay(r *http.Request) (payForm, error) {
	f := payForm{
		Name:       strings.TrimSpace(r.FormValue("pay_name")),
		Check:      strings.TrimSpace(r.FormValue("pay_check")),
		Method:     mustInt(r, "pay_method", 0),
		Client:     mustInt(r, "pay_client", 0),
		MerchantID: strings.TrimSpace(r.FormValue("merchant_id")),
		Key:        r.FormValue("merchant_key"),
		Pem:        r.FormValue("merchant_pem"),
		Route:      strings.TrimSpace(r.FormValue("pay_handleroute")),
		Open:       mustInt(r, "is_open", 0),
	}
	if f.Name == "" || runeLen(f.Name) > 200 {
		return f, errors.New("请填写支付名称")
	}
	if f.Check == "" || runeLen(f.Check) > 50 {
		return f, errors.New("请填写支付标识")
	}
	if f.Method != 1 && f.Method != 2 {
		return f, errors.New("支付方式请选择跳转或扫码")
	}
	if f.Client != 1 && f.Client != 2 && f.Client != 3 {
		return f, errors.New("支付场景请选择电脑、手机或通用")
	}
	if f.MerchantID == "" || runeLen(f.MerchantID) > 200 {
		return f, errors.New("请填写商户 ID")
	}
	if strings.TrimSpace(f.Pem) == "" {
		return f, errors.New("请填写商户密钥")
	}
	if f.Route == "" || runeLen(f.Route) > 200 {
		return f, errors.New("请填写支付处理路由")
	}
	if f.Open != 0 && f.Open != 1 {
		return f, errors.New("是否启用不正确")
	}
	return f, nil
}

func (s *Server) paySave(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	id, _ := pathID(r)
	f, err := readPay(r)
	f.ID = id
	action := "/admin/pays"
	if id > 0 {
		action = "/admin/pays/" + strconv.Itoa(id)
	}
	if err != nil {
		s.renderPays(w, r, u, f, action, err.Error(), http.StatusBadRequest)
		return
	}
	if id == 0 {
		_, err = s.db().Exec(r.Context(), `
			INSERT INTO pays (pay_name, pay_check, pay_method, pay_client, merchant_id, merchant_key, merchant_pem, pay_handleroute, is_open, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),now())`,
			f.Name, f.Check, f.Method, f.Client, f.MerchantID, f.Key, f.Pem, f.Route, f.Open)
	} else {
		var n int64
		n, err = execCount(s, r, `
			UPDATE pays SET pay_name=$1, pay_check=$2, pay_method=$3, pay_client=$4, merchant_id=$5,
				merchant_key=$6, merchant_pem=$7, pay_handleroute=$8, is_open=$9, updated_at=now()
			WHERE id=$10 AND deleted_at IS NULL`,
			f.Name, f.Check, f.Method, f.Client, f.MerchantID, f.Key, f.Pem, f.Route, f.Open, id)
		if err == nil && n == 0 {
			err = errors.New("记录不存在或已删除")
		}
	}
	if err != nil {
		s.renderPays(w, r, u, f, action, dbErr(err), http.StatusBadRequest)
		return
	}
	redirectOK(w, r, "/admin/pays", "保存成功")
}
