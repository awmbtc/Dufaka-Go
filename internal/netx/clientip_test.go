package netx

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// withTrusted sets DUFAKA_TRUSTED_PROXIES for one test and re-reads it.
func withTrusted(t *testing.T, spec string) {
	t.Helper()
	t.Setenv("DUFAKA_REAL_IP_HEADER", "")
	t.Setenv("DUFAKA_TRUSTED_PROXIES", spec)
	trustedOnce, trustedNets = sync.Once{}, nil
	t.Cleanup(func() { trustedOnce, trustedNets = sync.Once{}, nil })
}

func request(remote, real string, xff ...string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	if real != "" {
		r.Header.Set("X-Real-IP", real)
	}
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	withTrusted(t, "")
	cases := []struct {
		name, remote, real string
		xff                []string
		want               string
	}{
		{"direct", "203.0.113.5:4433", "", nil, "203.0.113.5"},
		{"direct ignores headers", "203.0.113.5:4433", "10.0.0.9", []string{"10.0.0.8"}, "203.0.113.5"},
		{"proxy real ip", "127.0.0.1:5000", "198.51.100.7", nil, "198.51.100.7"},
		{"proxy real ip with port", "127.0.0.1:5000", "198.51.100.7:1234", nil, "198.51.100.7"},
		{"default mode ignores xff", "127.0.0.1:5000", "", []string{"1.2.3.4, 198.51.100.7"}, "127.0.0.1"},
		{"client-sent xff cannot override real ip", "127.0.0.1:5000", "198.51.100.7", []string{"6.6.6.6"}, "198.51.100.7"},
		{"garbage real ip falls back to peer, not xff", "127.0.0.1:5000", "not-an-ip<b>", []string{"198.51.100.7"}, "127.0.0.1"},
		{"garbage everywhere falls back to peer", "127.0.0.1:5000", strings.Repeat("9", 70), []string{"x,y"}, "127.0.0.1"},
		{"ipv6 peer", "[::1]:5000", "2001:db8::10", nil, "2001:db8::10"},
		{"ipv6 real ip bracketed", "[::1]:5000", "[2001:db8::10]:443", nil, "2001:db8::10"},
		{"ipv6 zone stripped from header", "[::1]:5000", "fe80::1%eth0", nil, "fe80::1"},
		{"ipv6 zone stripped from peer", "[fe80::2%en0]:5000", "", nil, "fe80::2"},
		{"public peer keeps port stripped", "[2001:db8::5]:9", "10.0.0.1", nil, "2001:db8::5"},
		// Private and unspecified peers are no longer trusted by default.
		{"private peer untrusted", "10.1.2.3:5000", "", []string{"198.51.100.7"}, "10.1.2.3"},
		{"lan peer untrusted", "192.168.1.2", "198.51.100.7", nil, "192.168.1.2"},
		{"unspecified peer untrusted", "0.0.0.0:1", "198.51.100.7", nil, "0.0.0.0"},
	}
	for _, c := range cases {
		r := request(c.remote, c.real, c.xff...)
		if got := ClientIP(r); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
		if len(ClientIP(r)) > 45 {
			t.Errorf("%s: address longer than an IPv6 literal", c.name)
		}
	}
}

// A3-8: non-local proxies are trusted only when listed in DUFAKA_TRUSTED_PROXIES.
func TestTrustedProxiesFromEnv(t *testing.T) {
	withTrusted(t, " 10.0.0.0/8 , 192.168.1.5, fd00::/8, bogus, 300.1.1.1/8")
	cases := []struct {
		remote string
		trust  bool
	}{
		{"127.0.0.1:1", true},
		{"[::1]:1", true},
		{"10.20.30.40:1", true},
		{"192.168.1.5:1", true},
		{"192.168.1.6:1", false},
		{"[fd00::7]:1", true},
		{"[fe80::1%en0]:1", false},
		{"203.0.113.9:1", false},
		{"0.0.0.0:1", false},
		{"garbage", false},
	}
	for _, c := range cases {
		r := request(c.remote, "198.51.100.7")
		if got := TrustsProxy(r); got != c.trust {
			t.Errorf("%s: TrustsProxy=%v want %v", c.remote, got, c.trust)
		}
		want := hostOnly(c.remote)
		if c.trust {
			want = "198.51.100.7"
		}
		if c.remote == "[fe80::1%en0]:1" {
			want = "fe80::1"
		}
		if got := ClientIP(r); got != want {
			t.Errorf("%s: ClientIP=%q want %q", c.remote, got, want)
		}
	}
	if n := len(trustedProxies()); n != 3 {
		t.Fatalf("parsed %d networks, want 3 (bad entries skipped)", n)
	}
}

// ClientAddr flags the one case where the address is not the end client's: a trusted
// proxy that forwarded no usable header. Every visitor behind it would share that address.
func TestClientAddrKnown(t *testing.T) {
	withTrusted(t, "10.0.0.0/8")
	cases := []struct {
		name, remote, real string
		xff                []string
		want               string
		known              bool
	}{
		{"direct client", "203.0.113.5:1", "", nil, "203.0.113.5", true},
		{"untrusted peer with headers", "192.168.1.2:1", "198.51.100.7", nil, "192.168.1.2", true},
		{"loopback proxy with real ip", "127.0.0.1:1", "198.51.100.7", nil, "198.51.100.7", true},
		{"loopback proxy with only xff (default mode)", "[::1]:1", "", []string{"198.51.100.7"}, "::1", false},
		{"loopback proxy without headers", "127.0.0.1:1", "", nil, "127.0.0.1", false},
		{"listed proxy with garbage headers", "10.2.3.4:1", "junk", []string{"x"}, "10.2.3.4", false},
	}
	for _, c := range cases {
		ip, known := ClientAddr(request(c.remote, c.real, c.xff...))
		if ip != c.want || known != c.known {
			t.Errorf("%s: got %q %v want %q %v", c.name, ip, known, c.want, c.known)
		}
	}
}

// DUFAKA_REAL_IP_HEADER=X-Forwarded-For: only the last entry (the one the proxy appended)
// counts, and X-Real-IP is ignored because that proxy does not overwrite it.
func TestClientIPForwardedForMode(t *testing.T) {
	withTrusted(t, "")
	t.Setenv("DUFAKA_REAL_IP_HEADER", "x-forwarded-for")
	cases := []struct {
		name, remote, real string
		xff                []string
		want               string
		known              bool
	}{
		{"last entry", "127.0.0.1:5000", "", []string{"1.2.3.4, 198.51.100.7"}, "198.51.100.7", true},
		{"across header lines", "127.0.0.1:5000", "", []string{"1.2.3.4", "198.51.100.7"}, "198.51.100.7", true},
		{"client real ip ignored", "127.0.0.1:5000", "6.6.6.6", []string{"198.51.100.7"}, "198.51.100.7", true},
		{"garbage last entry is not skipped to a client entry", "127.0.0.1:5000", "", []string{"6.6.6.6, junk"}, "127.0.0.1", false},
		{"bracketed ipv6", "[::1]:5000", "", []string{"[2001:db8::10]:443"}, "2001:db8::10", true},
		{"no header", "127.0.0.1:5000", "", nil, "127.0.0.1", false},
		{"direct client ignores headers", "203.0.113.5:1", "", []string{"198.51.100.7"}, "203.0.113.5", true},
	}
	for _, c := range cases {
		ip, known := ClientAddr(request(c.remote, c.real, c.xff...))
		if ip != c.want || known != c.known {
			t.Errorf("%s: got %q %v want %q %v", c.name, ip, known, c.want, c.known)
		}
	}
}
