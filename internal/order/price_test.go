package order

import "testing"

func TestQuoteWholesaleAndCoupon(t *testing.T) {
	total, coupon, wholesale, actual, err := Quote(1000, 5, "1.00", "3=8.00\n10=7.00")
	if err != nil || total != 5000 || coupon != 100 || wholesale != 1000 || actual != 3900 {
		t.Fatalf("got %d %d %d %d %v", total, coupon, wholesale, actual, err)
	}
}

func TestWholesaleUsesHighestTierNotLastLine(t *testing.T) {
	_, _, wholesale, actual, err := Quote(1000, 10, "", "10=8.00\n3=9.00")
	if err != nil || wholesale != 2000 || actual != 8000 {
		t.Fatalf("highest tier 8.00, got off %d actual %d err %v", wholesale, actual, err)
	}
}

func TestQuoteRejectsQuantityOverflow(t *testing.T) {
	_, _, _, actual, err := Quote(9999999999, 1_000_000_000, "", "")
	if err != ErrPrice || actual != 0 {
		t.Fatalf("overflow actual %d err %v", actual, err)
	}
}

func TestLastCouponUseIsSingleClaim(t *testing.T) {
	if err := TakeCoupon(1, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := TakeCoupon(1, 1, 1, 0); err == nil {
		t.Fatal("second claim of the last use was accepted")
	}
	if err := TakeCoupon(1, 2, 3, 1); err == nil {
		t.Fatal("used coupon was accepted")
	}
}

func TestLoopPriceFloor(t *testing.T) {
	_, _, _, actual, err := Quote(100, 1, "9.00", "")
	if err != nil || actual != 0 {
		t.Fatalf("actual %d err %v", actual, err)
	}
}
