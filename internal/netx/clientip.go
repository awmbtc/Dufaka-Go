// Package netx holds the one client-address rule shared by the storefront and
// the back office.
package netx

import (
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// ClientIP returns the end client's address for a request.
//
// Proxy headers are trusted only when the TCP peer is a trusted reverse proxy
// (see TrustsProxy): a loopback address, or an address inside one of the
// CIDRs listed in DUFAKA_TRUSTED_PROXIES. In that case exactly one header is
// read, the one the proxy overwrites: DUFAKA_REAL_IP_HEADER, default X-Real-IP
// (nginx: proxy_set_header X-Real-IP $remote_addr), or X-Forwarded-For, where
// only the last entry counts (the one the proxy appended). The value must parse
// as an IP; otherwise, and for untrusted peers, the peer address is returned.
// The port and any IPv6 zone are stripped.
func ClientIP(r *http.Request) string {
	ip, _ := ClientAddr(r)
	return ip
}

// ClientAddr is ClientIP plus whether the address really names the end client. It is
// false only when the peer is a trusted proxy that sent no usable X-Real-IP or
// X-Forwarded-For: the address returned is then the proxy's own, shared by every visitor
// behind it, so per-client limits must not be keyed on it.
func ClientAddr(r *http.Request) (ip string, known bool) {
	peer := hostOnly(r.RemoteAddr)
	if p := parseIP(peer); p != "" {
		peer = p
	}
	if !TrustsProxy(r) {
		return peer, true
	}
	// Only the one header the proxy is configured to overwrite is read. Reading a second
	// header as a fallback would let a client supply it whenever the proxy does not set
	// it (nginx passes unknown request headers through unchanged).
	if realIPHeader() == "X-Forwarded-For" {
		var entries []string
		for _, line := range r.Header.Values("X-Forwarded-For") {
			entries = append(entries, strings.Split(line, ",")...)
		}
		// The proxy appends the address it saw, so only the last entry is its own.
		for i := len(entries) - 1; i >= 0; i-- {
			if strings.TrimSpace(entries[i]) == "" {
				continue
			}
			if v := parseIP(entries[i]); v != "" {
				return v, true
			}
			break
		}
		return peer, false
	}
	if v := parseIP(r.Header.Get("X-Real-IP")); v != "" {
		return v, true
	}
	return peer, false
}

// realIPHeader is the header a trusted proxy overwrites with the client address:
// DUFAKA_REAL_IP_HEADER, "X-Real-IP" (default, matching docs/nginx-shop.conf.example:
// proxy_set_header X-Real-IP $remote_addr) or "X-Forwarded-For" (last entry, for
// proxies that only append $proxy_add_x_forwarded_for). An unknown value falls back to
// X-Real-IP and is logged once.
func realIPHeader() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUFAKA_REAL_IP_HEADER"))) {
	case "", "x-real-ip":
		return "X-Real-IP"
	case "x-forwarded-for":
		return "X-Forwarded-For"
	default:
		badRealIPOnce.Do(func() {
			log.Printf("DUFAKA_REAL_IP_HEADER：只支持 X-Real-IP 或 X-Forwarded-For，已按 X-Real-IP 处理")
		})
		return "X-Real-IP"
	}
}

var badRealIPOnce sync.Once

// TrustsProxy reports whether the TCP peer of r is a trusted reverse proxy,
// i.e. whether ClientIP honours X-Real-IP / X-Forwarded-For and the storefront
// honours X-Forwarded-Proto. Loopback peers are always trusted; any other
// proxy must be listed in DUFAKA_TRUSTED_PROXIES (comma-separated CIDRs or
// single addresses, read once per process).
func TrustsProxy(r *http.Request) bool {
	ip := net.ParseIP(stripZone(hostOnly(r.RemoteAddr)))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range trustedProxies() {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

var (
	trustedOnce sync.Once
	trustedNets []*net.IPNet
)

func trustedProxies() []*net.IPNet {
	trustedOnce.Do(func() { trustedNets = parseTrusted(os.Getenv("DUFAKA_TRUSTED_PROXIES")) })
	return trustedNets
}

// parseTrusted turns "10.0.0.0/8, 192.168.1.5, fd00::/8" into networks. A bare
// address counts as a single host. Entries that do not parse are skipped and
// logged, never silently widened.
func parseTrusted(spec string) []*net.IPNet {
	var nets []*net.IPNet
	var bad []string
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			if _, n, err := net.ParseCIDR(item); err == nil {
				nets = append(nets, n)
				continue
			}
		} else if ip := net.ParseIP(stripZone(item)); ip != nil {
			bits := 128
			if v4 := ip.To4(); v4 != nil {
				ip, bits = v4, 32
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		bad = append(bad, item)
	}
	if len(bad) > 0 {
		log.Printf("DUFAKA_TRUSTED_PROXIES：忽略无法解析的条目 %q", bad)
	}
	return nets
}

func hostOnly(addr string) string {
	addr = strings.TrimSpace(addr)
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(addr, "[]")
}

// stripZone removes an IPv6 zone ("fe80::1%eth0" → "fe80::1").
func stripZone(h string) string {
	if i := strings.IndexByte(h, '%'); i >= 0 {
		return h[:i]
	}
	return h
}

func parseIP(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 {
		return ""
	}
	if ip := net.ParseIP(stripZone(hostOnly(v))); ip != nil {
		return ip.String()
	}
	return ""
}
