package store

import (
	"context"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// PayByCheck loads the payment channel with the given pay_check slug ("wescan", "cldx"),
// ignoring soft-deleted rows. pays.pay_check is UNIQUE in every schema, so there is at most
// one row. The Open field reflects is_open so callers decide whether a closed channel
// still counts: the cashier refuses it, the WeChat notify handler still needs the keys to
// decrypt a callback for an order that was paid before the channel was closed.
func (db *DB) PayByCheck(ctx context.Context, check string) (Pay, error) {
	var p Pay
	err := db.Pool.QueryRow(ctx, `
		SELECT id, pay_name, pay_check, pay_method, pay_client, COALESCE(merchant_id,''), COALESCE(merchant_key,''), merchant_pem, pay_handleroute, is_open
		FROM pays WHERE pay_check=$1 AND deleted_at IS NULL`, check).Scan(
		&p.ID, &p.Name, &p.Check, &p.Method, &p.Client, &p.MerchantID, &p.MerchantKey, &p.MerchantPem, &p.Route, &p.Open)
	if err != nil {
		return Pay{}, RuleError{Msg: "支付方式不存在"}
	}
	return p, nil
}

// CldxQuote returns the locked cldx amount and unix expiry of an order. Both are 0 when the
// order has no locked quote yet. It lives here so the storefront can rebuild the wallet
// deep link from stored values only, never from request parameters.
func (db *DB) CldxQuote(ctx context.Context, sn string) (minor, expires int64, err error) {
	var m, e *int64
	err = db.Pool.QueryRow(ctx, `SELECT cldx_minor, cldx_expires_at FROM orders WHERE order_sn=$1 AND deleted_at IS NULL`, sn).Scan(&m, &e)
	if err != nil {
		return 0, 0, err
	}
	if m != nil {
		minor = *m
	}
	if e != nil {
		expires = *e
	}
	return minor, expires, nil
}

// OrderBySNChecked is OrderBySN for callers that must tell an unknown order apart from a
// database failure (the cashier poll, the payment pages). OrderBySN answers 订单不存在 for
// any failed read, so its not-found answer is confirmed with a second, cheap existence
// query: only when that query succeeds and finds no row is the RuleError returned. A failed
// probe, or a row that exists after all, comes back as an ordinary error wrapping the
// cause, so store.IsNotFound is false for it. Once OrderBySN itself tells the two cases
// apart, the probe simply never runs for a database error.
//
// The order number usually comes straight from a URL or form field. A value that cannot
// be a stored order number (invalid UTF-8, a NUL or other control character, or longer
// than the varchar(150) column) is answered 订单不存在 without touching the database:
// PostgreSQL would reject NUL / invalid UTF-8 with SQLSTATE 22021, which would otherwise
// surface as a database failure, get logged, and let anyone write crafted text into the
// owner's log. The check is deliberately loose (not the 16-hex newSN shape) because
// order numbers imported from dujiaoka have other alphanumeric forms.
func (db *DB) OrderBySNChecked(ctx context.Context, sn string) (Order, error) {
	if !plausibleOrderSN(sn) {
		return Order{}, RuleError{Msg: msgNotFound}
	}
	o, err := db.OrderBySN(ctx, sn)
	if err == nil || !IsNotFound(err) {
		return o, err
	}
	var exists bool
	if perr := db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE order_sn=$1 AND deleted_at IS NULL)`, sn).Scan(&exists); perr != nil {
		return Order{}, fmt.Errorf("读取订单: %w", perr)
	}
	if exists {
		return Order{}, fmt.Errorf("读取订单: 订单存在但读取失败")
	}
	return Order{}, err
}

// maxOrderSNRunes is the width of orders.order_sn (varchar(150)): nothing longer is stored.
const maxOrderSNRunes = 150

// plausibleOrderSN reports whether sn could be a stored order number at all.
func plausibleOrderSN(sn string) bool {
	if sn == "" || !utf8.ValidString(sn) || utf8.RuneCountInString(sn) > maxOrderSNRunes {
		return false
	}
	for _, r := range sn {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
