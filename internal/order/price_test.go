package order

import "testing"

func TestQuoteWholesaleAndCoupon(t *testing.T) {
	total, coupon, wholesale, actual := Quote(1000, 5, "1.00", "3=8.00\n10=7.00")
	if total != 5000 || coupon != 100 || wholesale != 1000 || actual != 3900 {
		t.Fatalf("got %d %d %d %d", total, coupon, wholesale, actual)
	}
}

func TestLoopPriceFloor(t *testing.T) {
	_, _, _, actual := Quote(100, 1, "9.00", "")
	if actual != 0 {
		t.Fatalf("actual %d", actual)
	}
}
