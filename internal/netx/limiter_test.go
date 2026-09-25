package netx

import "testing"

func TestLimiterKey(t *testing.T) {
	cases := map[string]string{
		"198.51.100.7":            "198.51.100.7",
		"::ffff:198.51.100.7":     "198.51.100.7",
		"2001:db8:1:2:aaaa::1":    "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb::9999": "2001:db8:1:2::/64",
		"not-an-ip":               "not-an-ip",
	}
	for in, want := range cases {
		if got := LimiterKey(in); got != want {
			t.Errorf("LimiterKey(%q) = %q, want %q", in, got, want)
		}
	}
}
