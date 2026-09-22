package order

import (
	"strconv"
	"strings"
)

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

// Wholesale tier lines are "quantity=unitPrice", same as the original shop.
func WholesaleOff(raw string, qty int, unit Cents) Cents {
	if qty <= 0 || raw == "" {
		return 0
	}
	var best Cents
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
		if err1 != nil || err2 != nil || need <= 0 {
			continue
		}
		if qty >= need {
			best = price
			matched = true
		}
	}
	if !matched || best <= 0 {
		return 0
	}
	total := unit * Cents(qty)
	next := best * Cents(qty)
	if next >= total {
		return 0
	}
	return total - next
}

// Quote is the original price pipeline: total, coupon, wholesale, payable.
func Quote(unit Cents, qty int, coupon, wholesaleRaw string) (total, couponOff, wholesaleOff, actual Cents) {
	if qty < 1 {
		qty = 1
	}
	total = unit * Cents(qty)
	wholesaleOff = WholesaleOff(wholesaleRaw, qty, unit)
	if strings.TrimSpace(coupon) != "" {
		if c, err := ParseYuan(coupon); err == nil && c > 0 {
			couponOff = c
		}
	}
	actual = total - couponOff - wholesaleOff
	if actual < 0 {
		actual = 0
	}
	return
}
