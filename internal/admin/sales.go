package admin

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"dufaka/internal/pay"
	"dufaka/internal/store"
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
	// Lock the referenced goods rows before the coupon row, in id order: the
	// storefront locks goods first and coupons second, and the coupons_goods
	// foreign key would otherwise take its KEY SHARE lock on goods only after
	// this transaction already holds the coupon, which can deadlock a checkout.
	if msg, ok := lockCouponGoods(tx, r, goodsIDs); !ok {
		s.renderCoupons(w, r, u, f, action, msg, http.StatusBadRequest)
		return
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

// lockCouponGoods takes FOR KEY SHARE on every goods row a coupon refers to,
// in id order, and checks that each one exists and is not deleted. It returns
// the message to show when it is not ok.
func lockCouponGoods(tx pgx.Tx, r *http.Request, ids []int) (string, bool) {
	if len(ids) == 0 {
		return "", true
	}
	rows, err := tx.Query(r.Context(), `SELECT id, deleted_at IS NULL FROM goods WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, ids)
	if err != nil {
		return dbErr(err), false
	}
	live := map[int]bool{}
	for rows.Next() {
		var id int
		var ok bool
		if err := rows.Scan(&id, &ok); err != nil {
			rows.Close()
			return dbErr(err), false
		}
		live[id] = ok
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return dbErr(err), false
	}
	for _, id := range ids {
		if !live[id] {
			return "可用商品不存在", false
		}
	}
	return "", true
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
	// Owes is orders.stock_owed: a 人工处理 order the payment path put in 异常 because
	// stock was short, which has not taken its stock yet.
	Owes bool
}

type ordersPage struct {
	View
	Rows                                  []orderRow
	SN, FilterTitle, Email, Trade, Status string
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
	p := ordersPage{View: s.shell(u, "订单列表", "orders", r), Rows: list, SN: sn, FilterTitle: title, Email: email, Trade: trade, Status: status}
	p.Prev, p.Next = prev, next
	s.render(w, http.StatusOK, "orders", p)
}

func (s *Server) loadOrder(r *http.Request, id int) (orderRow, error) {
	var row orderRow
	err := s.db().QueryRow(r.Context(), `
		SELECT o.id, o.order_sn, o.title, o.email, o.buy_amount, o.goods_price::text, o.total_price::text,
			o.coupon_discount_price::text, o.wholesale_discount_price::text, o.actual_price::text,
			o.status, COALESCE(o.search_pwd,''), COALESCE(o.trade_no,''), COALESCE(p.pay_name,''),
			to_char(COALESCE(o.created_at, now()), 'YYYY-MM-DD HH24:MI'), COALESCE(o.info,''), o.buy_ip, o.type,
			o.stock_owed
		FROM orders o LEFT JOIN pays p ON p.id=o.pay_id
		WHERE o.id=$1 AND o.deleted_at IS NULL`, id).Scan(
		&row.ID, &row.SN, &row.Title, &row.Email, &row.Amount, &row.GoodsPrice, &row.Total,
		&row.CouponOff, &row.WholesaleOff, &row.Actual, &row.Status, &row.Pwd, &row.Trade, &row.Pay,
		&row.Created, &row.Info, &row.IP, &row.Type, &row.Owes)
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
		s.renderOrder(w, u, row, action, "订单名称不正确", http.StatusBadRequest)
		return
	}
	if runeLen(row.Pwd) > 200 {
		s.renderOrder(w, u, row, action, "查询密码过长", http.StatusBadRequest)
		return
	}
	if !validOrderStatus(row.Status) {
		s.renderOrder(w, u, row, action, "订单状态不正确", http.StatusBadRequest)
		return
	}
	// On a 人工处理 order, 异常 (6) means "paid while stock was short, stock
	// not yet taken" and is only ever set by the payment path; setting it by
	// hand would let the order take its stock twice (see takesManualStock).
	// 自动发卡 orders keep base3's 5 -> 6 path: 重新发货 requires 异常, and it
	// is the only way cards get marked sold after 6 -> 5 released them.
	if manualToShort(row.Type, prevStatus, row.Status) {
		s.renderOrder(w, u, row, action, orderNoManualShort, http.StatusBadRequest)
		return
	}
	// A 人工处理 order in 异常 has not taken its stock. 处理失败 (5) may later
	// move on to 待处理/处理中/已完成, and at that point nothing records that
	// the stock is still owed, so the order must leave 异常 only through
	// takesManualStock (which takes the stock) or stay in 异常.
	owes := owesManualStock(row.Type, prevStatus, row.Owes)
	if owes && manualShortToFailed(row.Type, prevStatus, row.Status) {
		s.renderOrder(w, u, row, action, orderManualShortFailed, http.StatusBadRequest)
		return
	}
	// Paid orders may progress through fulfillment; payment/expiry transitions belong to the ledger.
	if row.Status != prevStatus && !((prevStatus == 2 || prevStatus == 3 || ((prevStatus == 5 || prevStatus == 6) && row.Trade != "")) && (row.Status == 2 || row.Status == 3 || row.Status == 4 || row.Status == 5 || row.Status == 6)) {
		s.renderOrder(w, u, row, action, "该状态不能手动变更；付款与过期由系统处理，已付款订单可更新发货进度。", http.StatusBadRequest)
		return
	}

	takeStock := owes && takesManualStock(row.Type, prevStatus, row.Status)
	submittedInfo := row.Info
	if takeStock {
		// The stock is being taken now, so the short-stock note the payment path put in
		// front of the buyer's input no longer applies.
		row.Info = stripShortMarker(row.Info)
	}
	tx, err := s.db().Begin(r.Context())
	if err != nil {
		s.fail(w, u, "orders", dbErr(err))
		return
	}
	defer tx.Rollback(r.Context())
	n, err := execTx(tx, r, `UPDATE orders SET title=$1, info=$2, search_pwd=$3, status=$4, updated_at=now() WHERE id=$5 AND status=$6 AND deleted_at IS NULL`, row.Title, row.Info, row.Pwd, row.Status, id, prevStatus)
	if err != nil || n == 0 {
		msg := "记录不存在"
		if err != nil {
			msg = dbErr(err)
		}
		s.fail(w, u, "orders", msg)
		return
	}
	if takeStock {
		// The payment arrived while stock was short, so none was taken; the
		// owner marking the order as being handled takes it now.
		n, err := execTx(tx, r, `
			UPDATE goods g SET in_stock=g.in_stock-o.buy_amount, updated_at=now()
			FROM orders o WHERE o.id=$1 AND g.id=o.goods_id AND g.in_stock>=o.buy_amount`, id)
		if err != nil {
			s.fail(w, u, "orders", dbErr(err))
			return
		}
		if n != 1 {
			row.Status, row.Info = prevStatus, submittedInfo
			s.renderOrder(w, u, row, action, orderManualShort, http.StatusBadRequest)
			return
		}
		if _, err := execTx(tx, r, `UPDATE orders SET stock_owed=false WHERE id=$1`, id); err != nil {
			s.fail(w, u, "orders", dbErr(err))
			return
		}
	}
	if releasesReservations(row.Type, prevStatus, row.Status) {
		if _, err := execTx(tx, r, `UPDATE carmis SET reserved_order_id=NULL, updated_at=now() WHERE reserved_order_id=$1 AND status=1`, id); err != nil {
			s.fail(w, u, "orders", dbErr(err))
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, u, "orders", dbErr(err))
		return
	}
	redirectOK(w, r, action, "保存成功")
}

const (
	orderManualShort   = "库存不足，无法改为已处理状态"
	orderNoManualShort = "人工处理订单的「异常」状态由系统在付款时库存不足时设置，不能手动改为异常"
	// orderManualShortFailed refuses 异常 -> 处理失败 on a 人工处理 order.
	orderManualShortFailed = "该人工处理订单付款时库存不足、尚未扣减库存，不能直接改为处理失败；请补货后改为待处理、处理中或已完成（会同时扣减库存），或保持异常状态"
)

// manualToShort reports a hand-set 异常 on a 人工处理 (type 2) order, which is
// refused: that order has already taken its stock (or never owed it), and
// leaving 异常 again would take it a second time. 自动发卡 orders may be set
// back to 异常 so that 重新发货 becomes available.
func manualToShort(typ, from, to int) bool {
	return typ == 2 && to == 6 && from != 6
}

// manualShortMarker is the first line store.settleShort writes in front of a 人工处理
// order's info when the payment arrived while stock was short. It is only a note for the
// owner; whether the stock is still owed is recorded in orders.stock_owed.
const manualShortMarker = "库存不足"

// owesManualStock reports whether a 人工处理 order in 异常 was parked there by the
// payment path and has not taken its stock yet (orders.stock_owed). Orders already in 异常
// before that column existed (the old back office allowed setting it by hand after the
// stock was taken; dujiaoka imports can carry it) stay false and are never charged stock
// a second time.
func owesManualStock(typ, from int, owed bool) bool {
	return typ == 2 && from == 6 && owed
}

// stripShortMarker removes the leading short-stock line settleShort added, keeping the
// buyer's own input below it.
func stripShortMarker(info string) string {
	if !strings.HasPrefix(info, manualShortMarker) {
		return info
	}
	rest := strings.TrimPrefix(info, manualShortMarker)
	return strings.TrimPrefix(strings.TrimPrefix(rest, "\r"), "\n")
}

// manualShortToFailed reports the one way a 人工处理 order could leave 异常
// without taking its stock: 6 -> 5. From 5 the order may later move to
// 待处理/处理中/已完成 (it has a trade_no), and by then no column says its
// stock was never taken, so the storefront would keep selling stock it does
// not have. Every other exit from 6 goes through takesManualStock.
func manualShortToFailed(typ, from, to int) bool {
	return typ == 2 && from == 6 && to == 5
}

// takesManualStock reports whether a manual status change on a 人工处理
// (type 2) order must take its stock now: the order sat in 异常 (6) because
// the payment arrived while stock was short, and the owner is moving it on to
// 待处理/处理中/已完成.
func takesManualStock(typ, from, to int) bool {
	return typ == 2 && from == 6 && (to == 2 || to == 3 || to == 4)
}

// releasesReservations reports whether a manual status change on an
// auto-delivery (type 1) order ends its claim on reserved cards: leaving the
// 异常 state, or settling a 待处理/处理中 order as 已完成/处理失败. The cards
// stay unsold and go back to the shelf in the same request.
func releasesReservations(typ, from, to int) bool {
	if typ != 1 || from == to {
		return false
	}
	if from == 6 {
		return true
	}
	return (from == 2 || from == 3) && (to == 4 || to == 5)
}

func (s *Server) renderOrder(w http.ResponseWriter, u session, row orderRow, action, msg string, status int) {
	p := orderPage{View: s.shell(u, "订单详情", "orders", nil), Row: row, Action: action}
	p.Err = msg
	s.render(w, status, "order_detail", p)
}

// orderRedeliver re-runs delivery for a paid order that ended in status 6
// (stock was short when the payment arrived) once the owner has restocked.
func (s *Server) orderRedeliver(w http.ResponseWriter, r *http.Request, u session) {
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
	action := "/admin/orders/" + strconv.Itoa(id)
	if row.Status != 6 {
		s.renderOrder(w, u, row, action, "只有「异常」（已付款但库存不足）的订单才能重新发货", http.StatusBadRequest)
		return
	}
	if row.Type != 1 {
		s.renderOrder(w, u, row, action, redeliverManualOnly, http.StatusBadRequest)
		return
	}
	delivered, err := redeliver(r.Context(), s.db(), row.SN)
	var rule store.RuleError
	if errors.As(err, &rule) {
		s.renderOrder(w, u, row, action, rule.Msg, http.StatusBadRequest)
		return
	}
	if err != nil {
		s.fail(w, u, "orders", dbErr(err))
		return
	}
	if !delivered {
		s.renderOrder(w, u, row, action, "订单当前状态无需重新发货", http.StatusBadRequest)
		return
	}
	redirectOK(w, r, action, "已重新发货")
}

// redeliverManualOnly is shown when 重新发货 is asked for a 人工处理 order:
// there are no cards to send, the owner fulfils it by hand and sets the status.
const redeliverManualOnly = "人工处理订单没有卡密可发，请手动处理后在上方修改订单状态"

// cashierReady reports whether the storefront has a working cashier for a
// pay_check; every other channel would fail at checkout, so it may not be
// enabled. The list itself lives in store so the storefront and the back
// office cannot drift apart.
func cashierReady(check string) bool {
	return store.CashierReady(check)
}

const (
	payNotReady    = "该支付渠道尚未接通收银台，暂不能启用"
	payKeyLen      = "微信支付 APIv3 密钥必须是 32 位"
	wechatKeyBytes = 32
)

// payForm is the pay channel form. Key and Pem hold only what was submitted
// in this request: the stored secrets are never sent back to the browser, a
// blank field keeps the stored value, and ClearKey empties merchant_key.
// HasKey/HasPem say whether a value is stored.
type payForm struct {
	ID, Method, Client, Open                 int
	Name, Check, MerchantID, Key, Pem, Route string
	ClearKey, HasKey, HasPem                 bool
}

// payRow is one pays row. Key and Pem are read only to compute the
// configuration status and are wiped before the list is rendered; the list
// shows HasKey/HasPem instead.
type payRow struct {
	ID, Method, Client, Open                 int
	Name, Check, MerchantID, Key, Pem, Route string
	Created                                  string
	HasKey, HasPem                           bool
}

// filled is the list label for a secret column.
func filled(v bool) string {
	if v {
		return "已填写"
	}
	return "未填写"
}

// payStatus is one line of the read-only configuration block on the pays
// page: whether a wired channel has everything it needs to take a payment.
type payStatus struct {
	Name, Check, Text string
	OK                bool
}

// payConfigStatus reports the configuration state of a channel row. wescan
// merges the row with the WECHAT_PAY_* environment exactly as the cashier
// does; cldx is configured through WALLET_MERCHANT_SECRET only. Other
// channels are not wired and get no status.
func payConfigStatus(row payRow) (payStatus, bool) {
	st := payStatus{Name: row.Name, Check: row.Check}
	switch row.Check {
	case "wescan":
		cfg := pay.WechatConfig{MchID: row.MerchantID, PrivateKeyPEM: row.Pem, APIv3Key: row.Key}.WithEnv()
		if problems := cfg.Validate(); len(problems) > 0 {
			st.Text = "配置不完整：" + strings.Join(problems, "、")
		} else {
			st.OK, st.Text = true, "配置完整"
		}
		if w := cfg.Warnings(); len(w) > 0 {
			st.Text += "（提醒：" + strings.Join(w, "；") + "）"
		}
	case "cldx":
		if strings.TrimSpace(os.Getenv("WALLET_MERCHANT_SECRET")) != "" {
			st.OK, st.Text = true, "钱包密钥已配置"
		} else {
			st.Text = "未配置 WALLET_MERCHANT_SECRET"
		}
	default:
		return st, false
	}
	return st, true
}

type paysPage struct {
	View
	Rows   []payRow
	Status []payStatus
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
	s.renderPays(w, r, u, f.withoutSecrets(), "/admin/pays/"+strconv.Itoa(id), "", http.StatusOK)
}

// withoutSecrets turns a stored row into the edit form: the secret fields are
// emptied and only their presence is kept.
func (f payForm) withoutSecrets() payForm {
	f.HasKey, f.HasPem = f.HasKey || f.Key != "", f.HasPem || f.Pem != ""
	f.Key, f.Pem = "", ""
	return f
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
	for i, row := range list {
		if st, ok := payConfigStatus(row); ok {
			p.Status = append(p.Status, st)
		}
		list[i].HasKey, list[i].HasPem = row.Key != "", row.Pem != ""
		list[i].Key, list[i].Pem = "", ""
	}
	p.Form.Key, p.Form.Pem = "", ""
	p.Err = msg
	s.render(w, status, "pays", p)
}

// readPay reads a new channel from the form.
func readPay(r *http.Request) (payForm, error) {
	return readPayOver(r, payForm{})
}

// readPayOver reads the form for an edit of the stored row old (the zero
// payForm for a new channel). Blank secret fields keep the stored value; the
// 32-byte APIv3 rule applies only to a newly submitted wescan merchant KEY.
func readPayOver(r *http.Request, old payForm) (payForm, error) {
	f := payForm{
		ClearKey:   r.FormValue("clear_merchant_key") == "1",
		HasKey:     old.Key != "",
		HasPem:     old.Pem != "",
		Name:       strings.TrimSpace(r.FormValue("pay_name")),
		Check:      strings.TrimSpace(r.FormValue("pay_check")),
		Method:     mustInt(r, "pay_method", 0),
		Client:     mustInt(r, "pay_client", 0),
		MerchantID: strings.TrimSpace(r.FormValue("merchant_id")),
		Key:        strings.TrimSpace(r.FormValue("merchant_key")),
		Pem:        strings.TrimSpace(r.FormValue("merchant_pem")),
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
	// The cldx wallet channel is configured through environment variables, so
	// its merchant fields may stay empty; every other channel needs them.
	if f.Check != "cldx" {
		if f.MerchantID == "" {
			return f, errors.New("请填写商户 ID")
		}
		if f.Pem == "" && old.Pem == "" {
			return f, errors.New("请填写商户密钥")
		}
	}
	if f.ClearKey && f.Key != "" {
		return f, errors.New("不能同时填写并清空商户 KEY")
	}
	// The wescan merchant KEY is the WeChat Pay APIv3 key, which is always
	// exactly 32 bytes; anything else would only fail when a notify arrives.
	if f.Check == "wescan" && f.Key != "" && len(f.Key) != wechatKeyBytes {
		return f, errors.New(payKeyLen)
	}
	if runeLen(f.MerchantID) > 200 {
		return f, errors.New("商户 ID 超过 200 字")
	}
	if f.Route == "" || runeLen(f.Route) > 200 {
		return f, errors.New("请填写支付处理路由")
	}
	if f.Open != 0 && f.Open != 1 {
		return f, errors.New("是否启用不正确")
	}
	if f.Open == 1 && !cashierReady(f.Check) {
		return f, errors.New(payNotReady)
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
	action := "/admin/pays"
	var old payForm
	if id > 0 {
		action = "/admin/pays/" + strconv.Itoa(id)
		var err error
		old, err = s.loadPay(r, id)
		if errors.Is(err, pgx.ErrNoRows) {
			s.fail(w, u, "pays", "记录不存在")
			return
		}
		if err != nil {
			s.fail(w, u, "pays", dbErr(err))
			return
		}
	}
	f, err := readPayOver(r, old)
	f.ID = id
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
				merchant_key=CASE WHEN $11 THEN '' WHEN $6='' THEN merchant_key ELSE $6 END,
				merchant_pem=CASE WHEN $7='' THEN merchant_pem ELSE $7 END,
				pay_handleroute=$8, is_open=$9, updated_at=now()
			WHERE id=$10 AND deleted_at IS NULL`,
			f.Name, f.Check, f.Method, f.Client, f.MerchantID, f.Key, f.Pem, f.Route, f.Open, id, f.ClearKey)
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
