package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dufaka/internal/order"
)

var ErrRule = errors.New("rule")

type RuleError struct{ Msg string }

func (e RuleError) Error() string { return e.Msg }

type DB struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

func (db *DB) Installed(ctx context.Context) bool {
	var n int
	err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&n)
	return err == nil && n > 0
}

type Site struct {
	Title, Logo, TextLogo, Keywords, Description, Notice, Footer, Template, Language string
	SearchPwd, GeeTest                                                               bool
	ExpireMin                                                                        int
}

func (db *DB) Site(ctx context.Context) Site {
	s := Site{Title: "Dufaka-Go", TextLogo: "Dufaka-Go", Template: "unicorn", Language: "zh_CN", ExpireMin: 5}
	rows, err := db.Pool.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return s
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			m[k] = v
		}
	}
	set := func(dst *string, key string) {
		if m[key] != "" {
			*dst = m[key]
		}
	}
	set(&s.Title, "title")
	set(&s.Logo, "img_logo")
	set(&s.TextLogo, "text_logo")
	set(&s.Keywords, "keywords")
	set(&s.Description, "description")
	set(&s.Notice, "notice")
	set(&s.Footer, "footer")
	set(&s.Template, "template")
	set(&s.Language, "language")
	s.SearchPwd = m["is_open_search_pwd"] == "1"
	s.GeeTest = m["is_open_geetest"] == "1"
	if m["order_expire_time"] != "" {
		fmt.Sscan(m["order_expire_time"], &s.ExpireMin)
	}
	if s.ExpireMin <= 0 {
		s.ExpireMin = 5
	}
	return s
}

func (db *DB) PutSetting(ctx context.Context, key, value string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1,$2,now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	return err
}

type Good struct {
	ID, GroupID, InStock, Sales, BuyLimit, Type, Ord                     int
	Name, Picture, Desc, Keywords, Prompt, Description, Wholesale, Other string
	Retail, Actual                                                       order.Cents
	Open                                                                 bool
}

type Group struct {
	ID    int
	Name  string
	Goods []Good
}

func (db *DB) Home(ctx context.Context) ([]Group, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT g.id, g.gp_name FROM goods_group g
		WHERE g.is_open = 1 AND g.deleted_at IS NULL ORDER BY g.ord, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		goods, err := db.goodsByGroup(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		g.Goods = goods
		out = append(out, g)
	}
	return out, rows.Err()
}

func (db *DB) goodsByGroup(ctx context.Context, gid int) ([]Good, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, group_id, gd_name, COALESCE(picture,''), COALESCE(gd_description,''),
		       COALESCE(actual_price,0)::text, COALESCE(retail_price,0)::text, in_stock, COALESCE(sales_volume,0),
		       buy_limit_num, type, COALESCE(wholesale_price_cnf,''), COALESCE(other_ipu_cnf,''),
		       COALESCE(buy_prompt,''), COALESCE(description,''), is_open
		FROM goods WHERE group_id=$1 AND is_open=1 AND deleted_at IS NULL ORDER BY ord, id`, gid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGoods(ctx, db, rows)
}

func scanGoods(ctx context.Context, db *DB, rows pgx.Rows) ([]Good, error) {
	var list []Good
	for rows.Next() {
		var g Good
		var actual, retail string
		var open int
		if err := rows.Scan(&g.ID, &g.GroupID, &g.Name, &g.Picture, &g.Desc, &actual, &retail, &g.InStock, &g.Sales, &g.BuyLimit, &g.Type, &g.Wholesale, &g.Other, &g.Prompt, &g.Description, &open); err != nil {
			return nil, err
		}
		g.Actual, _ = order.ParseYuan(actual)
		g.Retail, _ = order.ParseYuan(retail)
		g.Open = open == 1
		if g.Type == 1 {
			_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE goods_id=$1 AND status=1 AND deleted_at IS NULL`, g.ID).Scan(&g.InStock)
		}
		list = append(list, g)
	}
	return list, rows.Err()
}

func (db *DB) Good(ctx context.Context, id int) (Good, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, group_id, gd_name, COALESCE(picture,''), COALESCE(gd_description,''),
		       COALESCE(actual_price,0)::text, COALESCE(retail_price,0)::text, in_stock, COALESCE(sales_volume,0),
		       buy_limit_num, type, COALESCE(wholesale_price_cnf,''), COALESCE(other_ipu_cnf,''),
		       COALESCE(buy_prompt,''), COALESCE(description,''), is_open
		FROM goods WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return Good{}, err
	}
	defer rows.Close()
	list, err := scanGoods(ctx, db, rows)
	if err != nil || len(list) == 0 {
		return Good{}, RuleError{Msg: "商品不存在"}
	}
	return list[0], nil
}

type Pay struct {
	ID, Method, Client, Open                                 int
	Name, Check, MerchantID, MerchantKey, MerchantPem, Route string
}

func (db *DB) Pays(ctx context.Context, client int) ([]Pay, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, pay_name, pay_check, pay_method, pay_client, COALESCE(merchant_id,''), COALESCE(merchant_key,''), merchant_pem, pay_handleroute
		FROM pays WHERE is_open=1 AND deleted_at IS NULL AND (pay_client=3 OR pay_client=$1) ORDER BY id`, client)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Pay
	for rows.Next() {
		var p Pay
		if err := rows.Scan(&p.ID, &p.Name, &p.Check, &p.Method, &p.Client, &p.MerchantID, &p.MerchantKey, &p.MerchantPem, &p.Route); err != nil {
			return nil, err
		}
		p.Open = 1
		list = append(list, p)
	}
	return list, rows.Err()
}

type Order struct {
	ID, GoodsID, PayID, Amount, Type, Status           int
	SN, Title, Email, Info, SearchPwd, TradeNo, BuyIP  string
	GoodsPrice, CouponOff, WholesaleOff, Total, Actual order.Cents
	Created                                            time.Time
}

type CreateInput struct {
	GID, PayID, Amount           int
	Email, SearchPwd, Coupon, IP string
	Extra                        map[string]string
}

func (db *DB) CreateOrder(ctx context.Context, in CreateInput, site Site) (Order, error) {
	if in.Amount < 1 {
		return Order{}, RuleError{Msg: "购买数量不正确"}
	}
	if !strings.Contains(in.Email, "@") {
		return Order{}, RuleError{Msg: "邮箱格式不正确"}
	}
	if site.SearchPwd && strings.TrimSpace(in.SearchPwd) == "" {
		return Order{}, RuleError{Msg: "请填写查询密码"}
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback(ctx)
	g, err := db.Good(ctx, in.GID)
	if err != nil {
		return Order{}, err
	}
	if !g.Open {
		return Order{}, RuleError{Msg: "商品已下架"}
	}
	if g.BuyLimit > 0 && in.Amount > g.BuyLimit {
		return Order{}, RuleError{Msg: "已超过单次限购数量"}
	}
	var loops int
	_ = tx.QueryRow(ctx, `SELECT count(*) FROM carmis WHERE goods_id=$1 AND status=1 AND is_loop=1 AND deleted_at IS NULL`, g.ID).Scan(&loops)
	if loops > 0 && in.Amount > 1 {
		return Order{}, RuleError{Msg: "循环卡密一次只能买 1 件"}
	}
	if in.Amount > g.InStock {
		return Order{}, RuleError{Msg: "库存不足"}
	}
	var couponCents string
	var couponID int
	if strings.TrimSpace(in.Coupon) != "" {
		var ret, open int
		err = tx.QueryRow(ctx, `
			SELECT c.id, c.discount, c.ret, c.is_open FROM coupons c
			JOIN coupons_goods cg ON cg.coupons_id=c.id
			WHERE c.coupon=$1 AND cg.goods_id=$2 AND c.deleted_at IS NULL`, in.Coupon, g.ID).Scan(&couponID, &couponCents, &ret, &open)
		if err != nil || open != 1 {
			return Order{}, RuleError{Msg: "优惠码不存在"}
		}
		if ret <= 0 {
			return Order{}, RuleError{Msg: "优惠码可用次数不足"}
		}
		if _, err = tx.Exec(ctx, `UPDATE coupons SET ret=ret-1, updated_at=now() WHERE id=$1 AND ret>0`, couponID); err != nil {
			return Order{}, err
		}
	}
	info := ""
	if g.Type == 2 && g.Other != "" {
		for _, line := range strings.Split(g.Other, "\n") {
			parts := strings.Split(strings.TrimSpace(line), "=")
			if len(parts) < 3 {
				continue
			}
			val := in.Extra[parts[0]]
			if parts[2] == "1" || strings.EqualFold(parts[2], "true") {
				if strings.TrimSpace(val) == "" {
					return Order{}, RuleError{Msg: parts[1] + "不能为空"}
				}
			}
			info += parts[1] + ":" + val + "\n"
		}
	}
	total, coff, woff, actual := order.Quote(g.Actual, in.Amount, couponCents, g.Wholesale)
	sn := newSN()
	var id int
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (order_sn, goods_id, coupon_id, title, type, goods_price, buy_amount,
			coupon_discount_price, wholesale_discount_price, total_price, actual_price, search_pwd,
			email, info, pay_id, buy_ip, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,1,now(),now())
		RETURNING id`,
		sn, g.ID, couponID, g.Name, g.Type, g.Actual.Yuan(), in.Amount, coff.Yuan(), woff.Yuan(), total.Yuan(), actual.Yuan(),
		in.SearchPwd, in.Email, info, in.PayID, in.IP).Scan(&id)
	if err != nil {
		return Order{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Order{}, err
	}
	return db.OrderBySN(ctx, sn)
}

func newSN() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

func (db *DB) OrderBySN(ctx context.Context, sn string) (Order, error) {
	var o Order
	var gp, co, wo, to, ac string
	err := db.Pool.QueryRow(ctx, `
		SELECT id, order_sn, goods_id, title, type, goods_price::text, buy_amount, coupon_discount_price::text,
		       wholesale_discount_price::text, total_price::text, actual_price::text, COALESCE(search_pwd,''),
		       email, COALESCE(info,''), COALESCE(pay_id,0), buy_ip, COALESCE(trade_no,''), status, created_at
		FROM orders WHERE order_sn=$1 AND deleted_at IS NULL`, sn).Scan(
		&o.ID, &o.SN, &o.GoodsID, &o.Title, &o.Type, &gp, &o.Amount, &co, &wo, &to, &ac, &o.SearchPwd, &o.Email, &o.Info, &o.PayID, &o.BuyIP, &o.TradeNo, &o.Status, &o.Created)
	if err != nil {
		return Order{}, RuleError{Msg: "订单不存在"}
	}
	o.GoodsPrice, _ = order.ParseYuan(gp)
	o.CouponOff, _ = order.ParseYuan(co)
	o.WholesaleOff, _ = order.ParseYuan(wo)
	o.Total, _ = order.ParseYuan(to)
	o.Actual, _ = order.ParseYuan(ac)
	return o, nil
}

func (db *DB) OrdersByEmail(ctx context.Context, email, pwd string, needPwd bool) ([]Order, error) {
	q := `SELECT order_sn FROM orders WHERE email=$1 AND deleted_at IS NULL`
	args := []any{email}
	if needPwd {
		q += ` AND search_pwd=$2`
		args = append(args, pwd)
	}
	q += ` ORDER BY id DESC LIMIT 20`
	rows, err := db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var sn string
		if rows.Scan(&sn) == nil {
			if o, err := db.OrderBySN(ctx, sn); err == nil {
				out = append(out, o)
			}
		}
	}
	return out, nil
}

// Complete marks a wait-pay order paid when the gateway amount matches, then delivers.
// A second notice with the same amount returns already=true and does not deliver again.
func (db *DB) Complete(ctx context.Context, sn string, paid order.Cents, tradeNo string) (already bool, err error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, status, goodsID, amount, typ int
	var actual string
	err = tx.QueryRow(ctx, `SELECT id, status, goods_id, buy_amount, type, actual_price::text FROM orders WHERE order_sn=$1 FOR UPDATE`, sn).
		Scan(&id, &status, &goodsID, &amount, &typ, &actual)
	if err != nil {
		return false, RuleError{Msg: "订单不存在"}
	}
	want, _ := order.ParseYuan(actual)
	if paid != want {
		return false, RuleError{Msg: "金额不一致"}
	}
	if status == 4 || status == 2 || status == 3 {
		return true, tx.Commit(ctx)
	}
	if status != 1 && status != -1 && status != 6 {
		return false, RuleError{Msg: "订单状态不可支付"}
	}
	if typ == 1 {
		rows, err := tx.Query(ctx, `
			SELECT id, carmi, is_loop FROM carmis
			WHERE goods_id=$1 AND status=1 AND deleted_at IS NULL
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2`, goodsID, amount)
		if err != nil {
			return false, err
		}
		type card struct {
			id, loop int
			text     string
		}
		var cards []card
		for rows.Next() {
			var c card
			if rows.Scan(&c.id, &c.text, &c.loop) == nil {
				cards = append(cards, c)
			}
		}
		rows.Close()
		if len(cards) != amount {
			_, err = tx.Exec(ctx, `UPDATE orders SET status=6, info=$2, trade_no=$3, updated_at=now() WHERE id=$1`, id, "库存不足", tradeNo)
			if err != nil {
				return false, err
			}
			return false, tx.Commit(ctx)
		}
		var lines []string
		for _, c := range cards {
			lines = append(lines, c.text)
			if c.loop == 0 {
				if _, err = tx.Exec(ctx, `UPDATE carmis SET status=2, updated_at=now() WHERE id=$1`, c.id); err != nil {
					return false, err
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE orders SET status=4, info=$2, trade_no=$3, updated_at=now() WHERE id=$1`, id, strings.Join(lines, "\n"), tradeNo)
		if err != nil {
			return false, err
		}
	} else {
		_, err = tx.Exec(ctx, `UPDATE orders SET status=2, trade_no=$2, updated_at=now() WHERE id=$1`, id, tradeNo)
		if err != nil {
			return false, err
		}
		_, _ = tx.Exec(ctx, `UPDATE goods SET in_stock=GREATEST(in_stock-$2,0), updated_at=now() WHERE id=$1`, goodsID, amount)
	}
	_, _ = tx.Exec(ctx, `UPDATE goods SET sales_volume=COALESCE(sales_volume,0)+$2, updated_at=now() WHERE id=$1`, goodsID, amount)
	return false, tx.Commit(ctx)
}

func (db *DB) ExpireDue(ctx context.Context, minutes int) error {
	if minutes <= 0 {
		minutes = 5
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT id, coupon_id FROM orders
		WHERE status=1 AND coupon_ret_back=0 AND created_at < now() - ($1 || ' minutes')::interval`, minutes)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct{ id, coupon int }
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.coupon) == nil {
			list = append(list, r)
		}
	}
	for _, r := range list {
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE orders SET status=-1, coupon_ret_back=1, updated_at=now() WHERE id=$1 AND status=1`, r.id)
		if err != nil {
			tx.Rollback(ctx)
			return err
		}
		if tag.RowsAffected() == 1 && r.coupon > 0 {
			_, _ = tx.Exec(ctx, `UPDATE coupons SET ret=ret+1, updated_at=now() WHERE id=$1`, r.coupon)
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) Pay(ctx context.Context, id int) (Pay, error) {
	var p Pay
	err := db.Pool.QueryRow(ctx, `
		SELECT id, pay_name, pay_check, pay_method, pay_client, COALESCE(merchant_id,''), COALESCE(merchant_key,''), merchant_pem, pay_handleroute
		FROM pays WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&p.ID, &p.Name, &p.Check, &p.Method, &p.Client, &p.MerchantID, &p.MerchantKey, &p.MerchantPem, &p.Route)
	if err != nil {
		return Pay{}, RuleError{Msg: "支付方式不存在"}
	}
	return p, nil
}
