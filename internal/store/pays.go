package store

import "errors"

import "context"

// CashierChecks lists the payment channels (pays.pay_check) that have a working
// cashier in this build. It is the single source for CashierReady, the
// storefront channel list (Pays), the checkout guard in CreateOrder and the
// startup sweep DisableUnwiredPays: a channel missing here can neither be
// offered to a customer nor accepted on an order.
var CashierChecks = []string{"wescan", "cldx"}

// CashierReady reports whether a payment channel has a working cashier in this
// build. Only these channels may be enabled or offered to customers.
func CashierReady(check string) bool {
	for _, c := range CashierChecks {
		if c == check {
			return true
		}
	}
	return false
}

// DisableUnwiredPays switches off every enabled channel that has no cashier
// (see CashierChecks) and returns how many rows it disabled. It runs once at
// startup so a row enabled by an older build, or by hand in the database, can
// never reach the storefront.
func (db *DB) DisableUnwiredPays(ctx context.Context) (int64, error) {
	tag, err := db.Pool.Exec(ctx, `
		UPDATE pays SET is_open=0, updated_at=now()
		WHERE is_open=1 AND deleted_at IS NULL AND NOT (pay_check = ANY($1::text[]))`, CashierChecks)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// IsPaidShort reports whether a Complete/Redeliver error means the payment is
// recorded (order moved to status 6) but stock was short. Payment callbacks
// should acknowledge it instead of asking the gateway to retry.
func IsPaidShort(err error) bool {
	var re RuleError
	return errors.As(err, &re) && re.Msg == msgShortStock
}

// IsNotFound reports whether an order lookup failed because the order does
// not exist (as opposed to a database error).
func IsNotFound(err error) bool {
	var re RuleError
	return errors.As(err, &re) && re.Msg == msgNotFound
}

const (
	msgShortStock = "库存不足"
	msgNotFound   = "订单不存在"
)
