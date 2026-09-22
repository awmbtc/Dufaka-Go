package order

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// ErrPrice is a customer-facing quote failure. Callers must not place the order.
var ErrPrice = errors.New("购买数量或金额不正确")

const maxCents int64 = 9999999999 // numeric(10,2)

// Money is a decimal amount in yuan with two fractional digits, stored as cents.
type Cents int64

func ParseYuan(s string) (Cents, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, strconv.ErrSyntax
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	whole, frac, ok := strings.Cut(s, ".")
	if !ok {
		frac = "00"
	}
	if len(frac) > 2 {
		frac = frac[:2]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, err
	}
	c := w*100 + f
	if neg {
		c = -c
	}
	return Cents(c), nil
}

func (c Cents) Yuan() string {
	sign := ""
	v := int64(c)
	if v < 0 {
		sign = "-"
		v = -v
	}
	return sign + strconv.FormatInt(v/100, 10) + "." + strconv.FormatInt(v%100/10, 10) + strconv.FormatInt(v%10, 10)
}

func mul(unit Cents, qty int) (Cents, bool) {
	if qty < 0 || unit < 0 {
		return 0, false
	}
	if qty == 0 || unit == 0 {
		return 0, true
	}
	if Cents(qty) > Cents(math.MaxInt64)/unit {
		return 0, false
	}
	v := unit * Cents(qty)
	if int64(v) > maxCents {
		return 0, false
	}
	return v, true
}

// Wholesale tier lines are "quantity=unitPrice". The matching tier is the
// highest quantity threshold the purchase still reaches, not the last line.
func WholesaleOff(raw string, qty int, unit Cents) Cents {
	if qty <= 0 || raw == "" {
		return 0
	}
	var best Cents
	bestNeed := 0
	matched := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, p, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		need, err1 := strconv.Atoi(strings.TrimSpace(n))
		price, err2 := ParseYuan(strings.TrimSpace(p))
		if err1 != nil || err2 != nil || need <= 0 || price <= 0 {
			continue
		}
		if qty < need {
			continue
		}
		if !matched || need > bestNeed || (need == bestNeed && price < best) {
			best = price
			bestNeed = need
			matched = true
		}
	}
	if !matched {
		return 0
	}
	total, ok1 := mul(unit, qty)
	next, ok2 := mul(best, qty)
	if !ok1 || !ok2 || next >= total {
		return 0
	}
	return total - next
}

// Quote is the price pipeline: total, coupon, wholesale, payable.
// err is set when the quantity overflows or exceeds the money column.
func Quote(unit Cents, qty int, coupon, wholesaleRaw string) (total, couponOff, wholesaleOff, actual Cents, err error) {
	if qty < 1 {
		qty = 1
	}
	var ok bool
	total, ok = mul(unit, qty)
	if !ok {
		return 0, 0, 0, 0, ErrPrice
	}
	wholesaleOff = WholesaleOff(wholesaleRaw, qty, unit)
	if strings.TrimSpace(coupon) != "" {
		if c, errParse := ParseYuan(coupon); errParse == nil && c > 0 {
			couponOff = c
		}
	}
	actual = total - couponOff - wholesaleOff
	if actual < 0 {
		actual = 0
	}
	return total, couponOff, wholesaleOff, actual, nil
}

// TakeCoupon accepts one use only when the row was open, not marked used,
// still had a remaining use, and the conditional update changed that row.
func TakeCoupon(isOpen, isUse, ret int, rowsAffected int64) error {
	if isOpen != 1 {
		return errors.New("优惠码不存在")
	}
	if isUse == 2 {
		return errors.New("优惠码已使用")
	}
	if ret <= 0 || rowsAffected != 1 {
		return errors.New("优惠码可用次数不足")
	}
	return nil
}
