package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dufaka/internal/order"
)

var ErrRule = errors.New("rule")

type RuleError struct{ Msg string }

func (e RuleError) Error() string { return e.Msg }

// msgBusy is what a customer sees for a failure that is not a business rule
// (database down, bad bytes rejected by Postgres, cancelled request …).
const msgBusy = "系统繁忙，请稍后再试"

// InternalError wraps an unexpected failure of a storefront-facing store call
// (OrderBySN, OrdersByEmail, CreateOrder, Home). Its Error() is the generic
// msgBusy so that handlers which print err.Error() on public pages never leak
// connection strings, SQLSTATEs or echoed input; the cause is logged once
// where it happens and stays reachable through errors.Is / errors.As
// (Unwrap) and Detail. It is never a RuleError, so IsNotFound / IsPaidShort
// stay false for it.
type InternalError struct {
	Op  string
	Err error
}

func (e InternalError) Error() string  { return msgBusy }
func (e InternalError) Unwrap() error  { return e.Err }
func (e InternalError) Detail() string { return e.Op + ": " + e.Err.Error() }

// internalErr logs err (unless it is just a cancelled request) and wraps it
// as an InternalError. RuleErrors and nil pass through unchanged, as does an
// error that is already an InternalError.
func internalErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var re RuleError
	var ie InternalError
	if errors.As(err, &re) || errors.As(err, &ie) {
		return err
	}
	if !errors.Is(err, context.Canceled) {
		log.Printf("%s 失败: %v", op, err)
	}
	return InternalError{Op: op, Err: err}
}

// clipSN makes a caller-supplied order number safe to put in a log line.
func clipSN(sn string) string {
	if len(sn) > 32 {
		sn = sn[:32]
	}
	return strconv.QuoteToASCII(sn)
}

// plausibleSN rejects order numbers Postgres could not even compare
// (invalid UTF-8, NUL) or that are far longer than any real one; such input
// is simply "no such order" and never reaches the database.
func plausibleSN(sn string) bool {
	return sn != "" && len(sn) <= 64 && utf8.ValidString(sn) && !strings.ContainsRune(sn, 0)
}

type DB struct {
	Pool *pgxpool.Pool

	// installed caches a positive Installed() answer: once the site has an
	// admin user it never becomes "not installed" again at runtime.
	installed atomic.Bool

	// schemaOK is set once EnsureCldxSchema has succeeded in this process.
	schemaOK atomic.Bool

	// siteMu guards the parsed Site cache below. siteGen counts invalidations:
	// Site only stores a freshly loaded value when nobody invalidated the cache
	// while the settings were being read, so an admin save can never be hidden
	// behind a stale load that finished a moment later.
	siteMu      sync.Mutex
	siteCached  Site
	siteExpires time.Time
	siteGen     uint64

	// siteLoadHook, when set, runs between the settings read and the cache
	// store inside Site. Tests use it to interleave an invalidation deterministically.
	siteLoadHook func()
}

// siteCacheTTL is how long a parsed Site is reused before the settings table
// is read again. PutSetting and InvalidateSite drop the cache immediately.
const siteCacheTTL = 10 * time.Second

func Open(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

// Installed reports whether an admin user exists. A positive answer is cached
// for the life of the process so request guards stop hitting the database.
func (db *DB) Installed(ctx context.Context) bool {
	if db.installed.Load() {
		return true
	}
	var n int
	err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&n)
	ok := err == nil && n > 0
	if ok {
		db.installed.Store(true)
	}
	return ok
}

type Site struct {
	Title, Logo, TextLogo, Keywords, Description, Notice, Footer, Template, Language string
	SearchPwd, GeeTest                                                               bool
	ExpireMin                                                                        int
}

// Site returns the parsed site settings, cached for siteCacheTTL. A failed
// read is never cached so the defaults do not stick after installation; on a
// failed read the built-in defaults are returned (see SiteOK).
func (db *DB) Site(ctx context.Context) Site {
	s, _ := db.SiteOK(ctx)
	return s
}

// SiteOK is Site plus whether the value really came from the settings table
// (fresh or cached). ok=false means the read failed and s holds only the
// built-in defaults, so callers that act on a setting (the expiry sweep's
// order_expire_time) can skip the work instead of using a guessed value.
func (db *DB) SiteOK(ctx context.Context) (Site, bool) {
	db.siteMu.Lock()
	if !db.siteExpires.IsZero() && time.Now().Before(db.siteExpires) {
		s := db.siteCached
		db.siteMu.Unlock()
		return s, true
	}
	gen := db.siteGen
	db.siteMu.Unlock()
	s, ok := db.loadSite(ctx)
	if db.siteLoadHook != nil {
		db.siteLoadHook()
	}
	if ok {
		db.siteMu.Lock()
		if db.siteGen == gen {
			db.siteCached = s
			db.siteExpires = time.Now().Add(siteCacheTTL)
		}
		db.siteMu.Unlock()
	}
	return s, ok
}

// DisableCaptchaSwitches clears the image-captcha and GeeTest switches
// (is_open_img_code, is_open_geetest): neither is wired in this build, so a
// stored 1 from the original site must not linger in the settings table. It
// returns how many rows changed and drops the Site cache.
func (db *DB) DisableCaptchaSwitches(ctx context.Context) (int64, error) {
	tag, err := db.Pool.Exec(ctx, `
		UPDATE settings SET value='0', updated_at=now()
		WHERE key IN ('is_open_geetest','is_open_img_code') AND value<>'0'`)
	db.InvalidateSite()
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// InvalidateSite drops the cached Site so the next Site call reads settings
// again, and bumps the generation so a load already in flight is not stored.
func (db *DB) InvalidateSite() {
	db.siteMu.Lock()
	db.siteGen++
	db.siteExpires = time.Time{}
	db.siteCached = Site{}
	db.siteMu.Unlock()
}

func (db *DB) loadSite(ctx context.Context) (Site, bool) {
	s := Site{Title: "Dufaka-Go", TextLogo: "Dufaka-Go", Template: "unicorn", Language: "zh_CN", ExpireMin: 5}
	rows, err := db.Pool.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return s, false
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
	if err := rows.Err(); err != nil {
		return s, false
	}
	return s, true
}

func (db *DB) PutSetting(ctx context.Context, key, value string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1,$2,now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	db.InvalidateSite()
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

// Home lists the storefront groups; database failures come back as an
// InternalError (see OrderBySN).
func (db *DB) Home(ctx context.Context) ([]Group, error) {
	out, err := db.home(ctx)
	return out, internalErr("读取首页商品", err)
}

func (db *DB) home(ctx context.Context) ([]Group, error) {
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
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		goods, err := db.goodsByGroup(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Goods = goods
	}
	return out, nil
}

func (db *DB) goodsByGroup(ctx context.Context, gid int) ([]Good, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, group_id, gd_name, COALESCE(picture,''), COALESCE(gd_description,''),
		       COALESCE(actual_price,0)::text, COALESCE(retail_price,0)::text,
 CASE WHEN type=1 THEN (SELECT count(*) FROM carmis WHERE goods_id=goods.id AND status=1 AND reserved_order_id IS NULL AND deleted_at IS NULL) ELSE in_stock END,
 COALESCE(sales_volume,0),
		       buy_limit_num, type, COALESCE(wholesale_price_cnf,''), COALESCE(other_ipu_cnf,''),
		       COALESCE(buy_prompt,''), COALESCE(description,''), is_open
		FROM goods WHERE group_id=$1 AND is_open=1 AND deleted_at IS NULL ORDER BY ord, id`, gid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGoods(rows)
}

// querier is what the read helpers need; both *pgxpool.Pool and pgx.Tx
// satisfy it, so a transaction can run the same reads on its own connection.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func scanGoods(rows pgx.Rows) ([]Good, error) {
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

		list = append(list, g)
	}
	return list, rows.Err()
}

func (db *DB) Good(ctx context.Context, id int) (Good, error) {
	return goodBy(ctx, db.Pool, id)
}

// goodBy reads one goods row through q. CreateOrder passes its transaction so
// a checkout never needs a second pool connection while it holds one.
func goodBy(ctx context.Context, q querier, id int) (Good, error) {
	rows, err := q.Query(ctx, `
		SELECT id, group_id, gd_name, COALESCE(picture,''), COALESCE(gd_description,''),
		       COALESCE(actual_price,0)::text, COALESCE(retail_price,0)::text,
 CASE WHEN type=1 THEN (SELECT count(*) FROM carmis WHERE goods_id=goods.id AND status=1 AND reserved_order_id IS NULL AND deleted_at IS NULL) ELSE in_stock END,
 COALESCE(sales_volume,0),
		       buy_limit_num, type, COALESCE(wholesale_price_cnf,''), COALESCE(other_ipu_cnf,''),
		       COALESCE(buy_prompt,''), COALESCE(description,''), is_open
		FROM goods WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return Good{}, err
	}
	defer rows.Close()
	list, err := scanGoods(rows)
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
		FROM pays WHERE is_open=1 AND deleted_at IS NULL AND (pay_client=3 OR pay_client=$1)
		  AND pay_check = ANY($2::text[]) ORDER BY id`, client, CashierChecks)
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

// CreateOrder validates a checkout, reserves its stock and writes the order in
// one transaction. site supplies the search-password rule; the image-captcha
// and GeeTest switches are not wired in this build and are ignored here (they
// are forced to 0 on save and at startup, see DisableCaptchaSwitches).
//
// Failures other than a RuleError come back as an InternalError, so the
// checkout page never shows a raw transaction error.
func (db *DB) CreateOrder(ctx context.Context, in CreateInput, site Site) (Order, error) {
	o, err := db.createOrder(ctx, in, site)
	if err != nil {
		return Order{}, internalErr("下单", err)
	}
	return o, nil
}

func (db *DB) createOrder(ctx context.Context, in CreateInput, site Site) (Order, error) {
	if in.Amount < 1 {
		return Order{}, RuleError{Msg: "购买数量不正确"}
	}
	address, emailErr := mail.ParseAddress(in.Email)
	if emailErr != nil || address.Address != in.Email {
		return Order{}, RuleError{Msg: "邮箱格式不正确"}
	}
	if site.SearchPwd && strings.TrimSpace(in.SearchPwd) == "" {
		return Order{}, RuleError{Msg: "请填写查询密码"}
	}
	if in.PayID <= 0 {
		return Order{}, RuleError{Msg: "请选择支付方式"}
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback(ctx)
	// Every read below goes through tx: asking the pool for a second
	// connection while this one is held wedges a small pool under load.
	g, err := goodBy(ctx, tx, in.GID)
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
	var openPay int
	var payCheck string
	if err = tx.QueryRow(ctx, `SELECT is_open, pay_check FROM pays WHERE id=$1 AND deleted_at IS NULL`, in.PayID).Scan(&openPay, &payCheck); err != nil || openPay != 1 || !CashierReady(payCheck) {
		return Order{}, RuleError{Msg: "支付方式不可用"}
	}
	// Serialise checkouts, payments and expiry of this goods (see settle for
	// the lock order). FOR NO KEY UPDATE, not FOR UPDATE: it still excludes the
	// other writers but not the FOR KEY SHARE that foreign-key inserts take
	// (the admin saving a coupon inserts coupons_goods rows pointing here).
	if _, err = tx.Exec(ctx, `SELECT id FROM goods WHERE id=$1 FOR NO KEY UPDATE`, g.ID); err != nil {
		return Order{}, err
	}
	var reserved []int
	if g.Type == 1 {
		rows, err := tx.Query(ctx, `
			SELECT id FROM carmis
			WHERE goods_id=$1 AND status=1 AND reserved_order_id IS NULL AND deleted_at IS NULL
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2`, g.ID, in.Amount)
		if err != nil {
			return Order{}, err
		}
		for rows.Next() {
			var id int
			if rows.Scan(&id) == nil {
				reserved = append(reserved, id)
			}
		}
		rows.Close()
		if len(reserved) != in.Amount {
			return Order{}, RuleError{Msg: "库存不足"}
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE goods SET in_stock=in_stock-$2, updated_at=now() WHERE id=$1 AND in_stock>=$2`, g.ID, in.Amount)
		if err != nil {
			return Order{}, err
		}
		if tag.RowsAffected() != 1 {
			return Order{}, RuleError{Msg: "库存不足"}
		}
	}
	var couponCents string
	var couponID int
	if strings.TrimSpace(in.Coupon) != "" {
		var ret, open, used int
		err = tx.QueryRow(ctx, `
			SELECT c.id, c.discount, c.ret, c.is_open, c.is_use FROM coupons c
			JOIN coupons_goods cg ON cg.coupons_id=c.id
			WHERE c.coupon=$1 AND cg.goods_id=$2 AND c.deleted_at IS NULL
			FOR NO KEY UPDATE OF c`, in.Coupon, g.ID).Scan(&couponID, &couponCents, &ret, &open, &used)
		if err != nil {
			return Order{}, RuleError{Msg: "优惠码不存在"}
		}
		tag, err := tx.Exec(ctx, `UPDATE coupons SET ret=ret-1, updated_at=now() WHERE id=$1 AND ret>0 AND is_open=1 AND is_use<>2`, couponID)
		if err != nil {
			return Order{}, err
		}
		if err = order.TakeCoupon(open, used, ret, tag.RowsAffected()); err != nil {
			return Order{}, RuleError{Msg: err.Error()}
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
	total, coff, woff, actual, err := order.Quote(g.Actual, in.Amount, couponCents, g.Wholesale)
	if err != nil {
		return Order{}, RuleError{Msg: err.Error()}
	}
	if actual <= 0 {
		return Order{}, RuleError{Msg: "实付金额必须大于 0"}
	}
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
	if g.Type == 1 {
		for _, cid := range reserved {
			if _, err = tx.Exec(ctx, `UPDATE carmis SET reserved_order_id=$2, updated_at=now() WHERE id=$1`, cid, id); err != nil {
				return Order{}, err
			}
		}
	}
	o, err := orderBySN(ctx, tx, sn)
	if err != nil {
		return Order{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Order{}, err
	}
	return o, nil
}

func newSN() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// OrderBySN reads one order. A missing (or soft-deleted) order, or an order
// number that cannot exist (invalid UTF-8, NUL, absurdly long), is the
// RuleError "订单不存在" (store.IsNotFound); any other failure is an
// InternalError wrapping the database error, so callers can tell "no such
// order" from "database down" while err.Error() stays safe to show.
func (db *DB) OrderBySN(ctx context.Context, sn string) (Order, error) {
	return orderBySN(ctx, db.Pool, sn)
}

func orderBySN(ctx context.Context, q querier, sn string) (Order, error) {
	if !plausibleSN(sn) {
		return Order{}, RuleError{Msg: msgNotFound}
	}
	var o Order
	var gp, co, wo, to, ac string
	err := q.QueryRow(ctx, `
		SELECT id, order_sn, goods_id, title, type, goods_price::text, buy_amount, coupon_discount_price::text,
		       wholesale_discount_price::text, total_price::text, actual_price::text, COALESCE(search_pwd,''),
		       email, COALESCE(info,''), COALESCE(pay_id,0), buy_ip, COALESCE(trade_no,''), status, created_at
		FROM orders WHERE order_sn=$1 AND deleted_at IS NULL`, sn).Scan(
		&o.ID, &o.SN, &o.GoodsID, &o.Title, &o.Type, &gp, &o.Amount, &co, &wo, &to, &ac, &o.SearchPwd, &o.Email, &o.Info, &o.PayID, &o.BuyIP, &o.TradeNo, &o.Status, &o.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, RuleError{Msg: msgNotFound}
	}
	if err != nil {
		return Order{}, internalErr("读取订单 "+clipSN(sn), err)
	}
	o.GoodsPrice, _ = order.ParseYuan(gp)
	o.CouponOff, _ = order.ParseYuan(co)
	o.WholesaleOff, _ = order.ParseYuan(wo)
	o.Total, _ = order.ParseYuan(to)
	o.Actual, _ = order.ParseYuan(ac)
	return o, nil
}

// OrdersByEmail lists a buyer's latest orders. Database failures come back
// as an InternalError (see OrderBySN).
func (db *DB) OrdersByEmail(ctx context.Context, email, pwd string, needPwd bool) ([]Order, error) {
	if !utf8.ValidString(email) || !utf8.ValidString(pwd) || strings.ContainsRune(email+pwd, 0) {
		return nil, nil
	}
	out, err := db.ordersByEmail(ctx, email, pwd, needPwd)
	return out, internalErr("按邮箱查询订单", err)
}

func (db *DB) ordersByEmail(ctx context.Context, email, pwd string, needPwd bool) ([]Order, error) {
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
	var sns []string
	for rows.Next() {
		var sn string
		if err := rows.Scan(&sn); err != nil {
			return nil, err
		}
		sns = append(sns, sn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var out []Order
	for _, sn := range sns {
		o, err := db.OrderBySN(ctx, sn)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// Complete marks a wait-pay order paid when the gateway amount matches, then delivers.
// A second notice with the same amount returns already=true and does not deliver again.
func (db *DB) Complete(ctx context.Context, sn string, paid order.Cents, tradeNo string) (already bool, err error) {
	_, already, err = db.settle(ctx, sn, paid, tradeNo, false)
	return already, err
}

// Redeliver re-runs delivery for an auto-delivery order that was paid while
// stock was short (status 6). It reuses the Complete code path with the stored
// actual_price and trade_no ("manual" when the gateway never wrote one; the
// placeholder is only written when the delivery succeeds). It returns
// delivered=true only when this call moved the order to 4; any other status is
// left alone and reports (false, nil). When stock is still short the RuleError
// "库存不足" is returned and nothing is written: the order stays at 6 with its
// info and trade_no untouched. Manual goods
// (type 2) are refused with "仅自动发卡商品支持重新发货": their stock was
// handed back by ExpireDue and the owner processes such an order by hand.
func (db *DB) Redeliver(ctx context.Context, sn string) (delivered bool, err error) {
	delivered, _, err = db.settle(ctx, sn, 0, "", true)
	return delivered, err
}

// couponRetBackUnrecoverable marks an order whose coupon use was handed back by
// ExpireDue (coupon_ret_back=1) and could not be taken again when the late
// payment arrived: the coupon had no uses left or was deleted ("已退回且无法再扣").
// Redeliver and a repeated Complete see the marker and do not retry the take.
const couponRetBackUnrecoverable = 2

// settle is the single payment-settlement path shared by Complete and Redeliver.
// With redeliver=true it only acts on status 6 and takes paid/tradeNo from the row.
//
// Lock order: settle and ExpireDue lock the orders row first; then every writer
// (CreateOrder, settle, ExpireDue) locks the goods row with FOR NO KEY UPDATE
// before touching any card (carmis) or coupon row. Holding the goods row
// serialises all work on that goods, so its card rows can never be contended
// in two orders; the coupon is the only row shared across goods and each
// transaction takes at most one, so whether cards or the coupon come next
// (CreateOrder: carmis → coupons, settle/ExpireDue: coupons → carmis) cannot
// form a cycle. The mode is FOR NO KEY UPDATE rather than FOR UPDATE so the
// FOR KEY SHARE taken by foreign-key inserts (the admin coupon save does
// UPDATE coupons, then INSERT coupons_goods referencing the goods row) is not
// blocked: with FOR UPDATE that admin transaction and a late payment on the
// same coupon deadlocked (40P01).
func (db *DB) settle(ctx context.Context, sn string, paid order.Cents, tradeNo string, redeliver bool) (delivered, already bool, err error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	var id, status, goodsID, amount, typ, couponID, couponRetBack int
	var actual, storedTradeNo string
	err = tx.QueryRow(ctx, `
		SELECT id, status, goods_id, buy_amount, type, actual_price::text,
		       COALESCE(coupon_id,0), coupon_ret_back, COALESCE(trade_no,'')
		FROM orders WHERE order_sn=$1 FOR UPDATE`, sn).
		Scan(&id, &status, &goodsID, &amount, &typ, &actual, &couponID, &couponRetBack, &storedTradeNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, RuleError{Msg: "订单不存在"}
	}
	if err != nil {
		return false, false, err
	}
	want, _ := order.ParseYuan(actual)
	// short records a payment that met short stock. For Redeliver the order is
	// already 6 and nothing may change (no "manual" placeholder trade_no, no
	// info rewrite), so the deferred rollback discards the attempt.
	short := func(retBack int) (bool, bool, error) {
		if redeliver {
			return false, false, RuleError{Msg: msgShortStock}
		}
		return db.settleShort(ctx, tx, id, typ, tradeNo, retBack)
	}
	if redeliver {
		if status != 6 {
			return false, false, nil
		}
		if typ != 1 {
			return false, false, RuleError{Msg: "仅自动发卡商品支持重新发货"}
		}
		paid = want
		tradeNo = storedTradeNo
		if tradeNo == "" {
			tradeNo = "manual"
		}
	}
	if paid != want {
		return false, false, RuleError{Msg: "金额不一致"}
	}
	if status == 4 || status == 2 || status == 3 {
		return false, true, tx.Commit(ctx)
	}
	if status != 1 && status != -1 && status != 6 {
		return false, false, RuleError{Msg: "订单状态不可支付"}
	}
	if typ != 1 && status == 6 {
		// A manual order already recorded as paid-but-short: the payment is on
		// file (trade_no) and the owner handles it by hand, so a repeated notice
		// is acknowledged without touching stock or sales again.
		return false, true, tx.Commit(ctx)
	}
	// Goods row first (see the lock-order note above), before any coupon or card work.
	if _, err = tx.Exec(ctx, `SELECT id FROM goods WHERE id=$1 FOR NO KEY UPDATE`, goodsID); err != nil {
		return false, false, err
	}
	// A late payment on an order ExpireDue already expired: ExpireDue handed the
	// coupon use back (coupon_ret_back=1), so take it again now that the order is
	// really paid. If the coupon has no uses left (or was deleted) we still
	// deliver — the customer has already paid the discounted price — and write
	// coupon_ret_back=2 plus a log line so the shop owner can reconcile that
	// coupon by hand and no later Redeliver or notice retries the take.
	retBack := couponRetBack
	if couponID > 0 && couponRetBack == 1 {
		tag, cErr := tx.Exec(ctx, `UPDATE coupons SET ret=ret-1, updated_at=now() WHERE id=$1 AND ret>0 AND deleted_at IS NULL`, couponID)
		if cErr != nil {
			return false, false, cErr
		}
		if tag.RowsAffected() == 1 {
			retBack = 0
		} else {
			retBack = couponRetBackUnrecoverable
			log.Printf("订单 %s 迟到付款，优惠码 %d 已无剩余次数，未扣回", sn, couponID)
		}
	}
	if typ == 1 {
		rows, err := tx.Query(ctx, `
			SELECT id, carmi, is_loop FROM carmis
			WHERE goods_id=$1 AND status=1 AND deleted_at IS NULL
			  AND (reserved_order_id=$3 OR reserved_order_id IS NULL)
			ORDER BY CASE WHEN reserved_order_id=$3 THEN 0 ELSE 1 END, id
			FOR UPDATE SKIP LOCKED LIMIT $2`, goodsID, amount, id)
		if err != nil {
			return false, false, err
		}
		type card struct {
			id, loop int
			text     string
		}
		var cards []card
		for rows.Next() {
			var c card
			if err := rows.Scan(&c.id, &c.text, &c.loop); err != nil {
				rows.Close()
				return false, false, err
			}
			cards = append(cards, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, false, err
		}
		if len(cards) != amount {
			return short(retBack)
		}
		var lines []string
		for _, c := range cards {
			lines = append(lines, c.text)
			if c.loop == 0 {
				if _, err = tx.Exec(ctx, `UPDATE carmis SET status=2, reserved_order_id=NULL, updated_at=now() WHERE id=$1`, c.id); err != nil {
					return false, false, err
				}
			} else if _, err = tx.Exec(ctx, `UPDATE carmis SET reserved_order_id=NULL, updated_at=now() WHERE id=$1`, c.id); err != nil {
				return false, false, err
			}
		}
		// Release any card still reserved for this order: SKIP LOCKED above may
		// have skipped a reserved card that another transaction held and shipped
		// a free one instead. Without this the skipped card would stay reserved
		// for a finished order and vanish from the storefront stock forever.
		if _, err = tx.Exec(ctx, `UPDATE carmis SET reserved_order_id=NULL, updated_at=now() WHERE reserved_order_id=$1 AND goods_id=$2 AND status=1`, id, goodsID); err != nil {
			return false, false, err
		}
		_, err = tx.Exec(ctx, `UPDATE orders SET status=4, info=$2, trade_no=$3, coupon_ret_back=$4, updated_at=now() WHERE id=$1`, id, strings.Join(lines, "\n"), tradeNo, retBack)
		if err != nil {
			return false, false, err
		}
	} else {
		// Manual goods: CreateOrder took the stock and only ExpireDue hands it
		// back, so it is taken again exactly when the order had expired (-1).
		if status == -1 {
			tag, stockErr := tx.Exec(ctx, `UPDATE goods SET in_stock=in_stock-$2, updated_at=now() WHERE id=$1 AND in_stock >= $2`, goodsID, amount)
			if stockErr != nil {
				return false, false, stockErr
			}
			if tag.RowsAffected() != 1 {
				return short(retBack)
			}
		}
		_, err = tx.Exec(ctx, `UPDATE orders SET status=2, trade_no=$2, coupon_ret_back=$3, updated_at=now() WHERE id=$1`, id, tradeNo, retBack)
		if err != nil {
			return false, false, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE goods SET sales_volume=COALESCE(sales_volume,0)+$2, updated_at=now() WHERE id=$1`, goodsID, amount); err != nil {
		return false, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// settleShort records a payment that arrived while stock was short: the order
// becomes 6 (异常) with the gateway's trade_no, the transaction is committed so
// the paid order is visible to the owner and is not polled again, and the
// caller gets RuleError "库存不足". The info column is the buyer's input for
// manual goods (type 2), so there "库存不足" is put in front of it instead of
// replacing it; auto-delivery orders (type 1) have no buyer input and info is
// set to "库存不足".
func (db *DB) settleShort(ctx context.Context, tx pgx.Tx, id, typ int, tradeNo string, retBack int) (bool, bool, error) {
	info := `$2::text`
	if typ == 2 {
		info = `$2::text || E'\n' || COALESCE(info,'')`
	}
	// stock_owed records, outside the editable info text, that a 人工处理 order has not
	// taken its stock: the owner's 异常 -> 待处理/处理中/已完成 takes it and clears the flag.
	_, err := tx.Exec(ctx, `UPDATE orders SET status=6, info=`+info+`, trade_no=$3, coupon_ret_back=$4, stock_owed=$5, updated_at=now() WHERE id=$1`, id, msgShortStock, tradeNo, retBack, typ == 2)
	if err != nil {
		return false, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, false, err
	}
	return false, false, RuleError{Msg: "库存不足"}
}

func (db *DB) ExpireDue(ctx context.Context, minutes int) error {
	if minutes <= 0 {
		minutes = 5
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT id, coupon_id FROM orders
		WHERE status=1 AND coupon_ret_back=0 AND created_at < now() - ($1 * interval '1 minute')`, minutes)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct{ id, coupon int }
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.coupon); err != nil {
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range list {
		if err := db.expireOne(ctx, r.id, r.coupon); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) expireOne(ctx context.Context, id, coupon int) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var goodsID int
	err = tx.QueryRow(ctx, `
		UPDATE orders SET status=-1, coupon_ret_back=1,
			updated_at=now()
		WHERE id=$1 AND status=1
		RETURNING goods_id`, id).Scan(&goodsID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Paid or expired by someone else meanwhile: nothing to hand back.
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	// Goods row before the coupon and card rows: the same order every
	// writer uses (see settle), so expiry never deadlocks a checkout. FOR
	// NO KEY UPDATE leaves foreign-key inserts (FOR KEY SHARE) unblocked.
	if _, err = tx.Exec(ctx, `SELECT g.id FROM goods g JOIN orders o ON o.goods_id=g.id WHERE o.id=$1 FOR NO KEY UPDATE OF g`, id); err != nil {
		return err
	}
	if coupon > 0 {
		if _, err = tx.Exec(ctx, `UPDATE coupons SET ret=ret+1, updated_at=now() WHERE id=$1`, coupon); err != nil {
			return err
		}
	}
	// Scoped to the order's goods like settle's release: a stray card of
	// another goods carrying this id is not this order's reservation.
	if _, err = tx.Exec(ctx, `UPDATE carmis SET reserved_order_id=NULL, updated_at=now() WHERE reserved_order_id=$1 AND goods_id=$2`, id, goodsID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		UPDATE goods g SET in_stock=g.in_stock+o.buy_amount, updated_at=now()
		FROM orders o WHERE o.id=$1 AND g.id=o.goods_id AND o.type=2`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (db *DB) Pay(ctx context.Context, id int) (Pay, error) {
	var p Pay
	err := db.Pool.QueryRow(ctx, `
		SELECT id, pay_name, pay_check, pay_method, pay_client, COALESCE(merchant_id,''), COALESCE(merchant_key,''), merchant_pem, pay_handleroute, is_open
		FROM pays WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&p.ID, &p.Name, &p.Check, &p.Method, &p.Client, &p.MerchantID, &p.MerchantKey, &p.MerchantPem, &p.Route, &p.Open)
	if err != nil {
		return Pay{}, RuleError{Msg: "支付方式不存在"}
	}
	return p, nil
}
