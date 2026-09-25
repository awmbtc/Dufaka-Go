package httpx

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"dufaka/internal/netx"
)

// Order lookups are the storefront's enumeration surface (order numbers, buyer emails),
// so each client address gets a fixed budget per window. The cashier's status poll has a
// separate, generous budget: a page polling every 5 seconds makes 60 requests in 5
// minutes, well under 120, even with a second tab open.
const (
	lookupLimit    = 30
	lookupOverflow = 300 // shared by all new addresses while the table is full
	pollLimit      = 120
	limitWindow    = 5 * time.Minute
	limiterMaxIPs  = 20000
	tooManyText    = "查询过于频繁，请稍后再试"
)

// ipLimiter counts requests per client address (netx.LimiterKey: IPv4 as is, IPv6 per
// /64) in fixed windows. The map is capped and a live counter is never evicted (that would
// hand its owner a fresh budget). When the map is full of live counters, a new address is
// not refused: with overflow > 0 every such address shares one overflow counter of that
// size (order lookups stay bounded while a flood of /64s cannot lock real buyers out),
// with overflow == 0 it is simply let through untracked (the cashier poll must never stall
// because someone filled the table). Each overflow period is logged once per window.
type ipLimiter struct {
	mu        sync.Mutex
	name      string
	limit     int
	overflow  int
	window    time.Duration
	max       int
	now       func() time.Time
	hits      map[string]*ipWindow
	spill     ipWindow
	lastSweep time.Time
	lastFull  time.Time
}

type ipWindow struct {
	start time.Time
	count int
}

func newIPLimiter(name string, limit, overflow int, window time.Duration) *ipLimiter {
	return &ipLimiter{name: name, limit: limit, overflow: overflow, window: window, max: limiterMaxIPs, now: time.Now, hits: map[string]*ipWindow{}}
}

// allow spends one request of key's budget and reports whether it was available.
func (l *ipLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.hits[key]
	if w != nil && now.Sub(w.start) >= l.window {
		w.start, w.count = now, 0
	}
	if w == nil {
		if len(l.hits) >= l.max {
			l.sweep(now)
		}
		if len(l.hits) >= l.max {
			if l.lastFull.IsZero() || now.Sub(l.lastFull) >= l.window {
				l.lastFull = now
				if l.overflow > 0 {
					log.Printf("%s限流：客户端地址表已满（%d），新地址共用一个溢出额度（%d 次/%s）", l.name, l.max, l.overflow, l.window)
				} else {
					log.Printf("%s限流：客户端地址表已满（%d），新地址暂不计数直接放行", l.name, l.max)
				}
			}
			if l.overflow <= 0 {
				return true
			}
			if now.Sub(l.spill.start) >= l.window {
				l.spill.start, l.spill.count = now, 0
			}
			if l.spill.count >= l.overflow {
				return false
			}
			l.spill.count++
			return true
		}
		w = &ipWindow{start: now}
		l.hits[key] = w
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

// sweep drops expired counters, at most once per second so a full map of live counters
// does not turn every new address into a full scan. Called with mu held.
func (l *ipLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Second {
		return
	}
	l.lastSweep = now
	for k, w := range l.hits {
		if now.Sub(w.start) >= l.window {
			delete(l.hits, k)
		}
	}
}

var unforwardedOnce sync.Once

// limiterKey is the budget key of a request, or "" when the request must not be limited:
// the visitor presents an order this browser bought (the signed dujiaoka_orders cookie —
// the cashier's own poll and the delivery page it redirects to after payment), or the
// peer is a trusted proxy that forwarded no client address, so every buyer would share
// the proxy's own address (logged once; fix the proxy_set_header lines).
func limiterKey(r *http.Request) string {
	if sn := r.PathValue("sn"); sn != "" && ownsOrder(r, sn) {
		return ""
	}
	ip, known := netx.ClientAddr(r)
	if !known {
		unforwardedOnce.Do(func() {
			log.Printf("反代未传 X-Real-IP，查询限流已停用（请按 docs/nginx-shop.conf.example 配置 proxy_set_header）")
		})
		return ""
	}
	return netx.LimiterKey(ip)
}

// limitLookups guards an order-lookup page: over budget the visitor gets a 429 page.
func (a *App) limitLookups(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := limiterKey(r); key != "" && !a.lookups.allow(key) {
			a.failStatus(w, r, http.StatusTooManyRequests, tooManyText)
			return
		}
		next(w, r)
	}
}

// limitPolls guards the cashier status poll: over budget it answers a JSON 429 whose code
// the cashier script ignores, so the page simply keeps polling.
func (a *App) limitPolls(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := limiterKey(r); key != "" && !a.polls.allow(key) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"msg": tooManyText, "code": http.StatusTooManyRequests})
			return
		}
		next(w, r)
	}
}
