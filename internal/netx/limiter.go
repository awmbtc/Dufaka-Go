package netx

import "net"

// LimiterKey groups a client address for rate limiting: IPv4 addresses are
// used as-is, IPv6 addresses are reduced to their /64 prefix so one host
// cannot rotate through its subnet to escape a per-IP limit. Non-IP input is
// returned unchanged.
func LimiterKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return v4.String()
	}
	mask := net.CIDRMask(64, 128)
	return p.Mask(mask).String() + "/64"
}
