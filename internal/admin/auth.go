package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"dufaka/internal/netx"
)

// dummyHash is a real cost-10 bcrypt hash of a password nobody knows. Logins
// for a username that does not exist are compared against it so the response
// time is the same as for a wrong password and cannot reveal which usernames exist.
const dummyHash = "$2a$10$kUgAZASfQAUJ1sp1RqN40eljdQ/9nOc8qGv81HmkVBtxbVSLJqEHW"

// compareHash is the one bcrypt comparison used by the login path. It is a
// variable so tests can count how often it runs.
var compareHash = bcrypt.CompareHashAndPassword

func bcryptCompare(hash, plain string) bool {
	if hash == "" || plain == "" {
		return false
	}
	return compareHash([]byte(hash), []byte(plain)) == nil
}

// checkLogin spends one bcrypt comparison whether or not the account exists.
func checkLogin(found bool, hash, plain string) bool {
	if !found || hash == "" {
		_ = compareHash([]byte(dummyHash), []byte(plain))
		return false
	}
	return bcryptCompare(hash, plain)
}

const (
	// loginMaxFails is the hard limit per client address (IPv4, or IPv6 /64):
	// the sixth attempt inside loginWindow is refused and blocks the address
	// for loginBlock.
	loginMaxFails = 5
	// loginUnknownMaxFails is the shared budget for all logins that arrive through a
	// trusted proxy without a forwarded client address: generous enough that a stranger
	// cannot lock the owner out with a few guesses, but still a real cap on guessing.
	loginUnknownMaxFails = 50
	loginUnknownKey      = "ip:unknown"
	// loginUserMaxFails is the soft limit per existing username. It never
	// refuses: once an account has this many recorded failures inside the
	// window, every further attempt on it waits loginUserDelay before the
	// password is checked, so a stranger can slow a distributed guess down but
	// can never lock the owner out.
	loginUserMaxFails = 20
	loginUserDelay    = 2 * time.Second
	loginWindow       = 15 * time.Minute
	loginBlock        = 15 * time.Minute
	loginBlocked      = "尝试次数过多，请 15 分钟后再试"
	// loginMaxUser is the longest username the login form even considers;
	// anything longer is refused before it can become a limiter key.
	loginMaxUser = 120
	// loginMaxKeys caps the limiter map. Live entries are never evicted; when
	// the map is full of them, new addresses are refused. (Username keys exist
	// only for real accounts and are recorded even on a full map.)
	loginMaxKeys = 10000
	// loginFullSweepGap spaces out the sweeps a full map triggers, so a flood
	// of new addresses cannot make every request walk the whole map.
	loginFullSweepGap = time.Second
	// loginSweepEvery is how many reservations pass between two sweeps of
	// expired entries.
	loginSweepEvery = 256
)

type loginFails struct {
	count        int
	first        time.Time
	blockedUntil time.Time
}

// loginLimiter tracks login attempts per client address ("ip:" keys, a hard
// gate) and failures per existing username ("user:" keys, a soft delay).
//
// An address attempt is reserved before any password work, so concurrent
// requests cannot slip past the limit while the first ones are still being
// evaluated. Requests that are refused never create an entry, and an entry
// that is still counting or blocked is never evicted: a full map refuses new
// addresses instead of dropping existing counters. The zero value is ready to use.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*loginFails
	now   func() time.Time
	calls int
	// lastFullSweep is when a full map was last swept.
	lastFullSweep time.Time
	// maxKeys overrides loginMaxKeys (tests).
	maxKeys int
}

func (l *loginLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *loginLimiter) capacity() int {
	if l.maxKeys > 0 {
		return l.maxKeys
	}
	return loginMaxKeys
}

func (l *loginLimiter) init() {
	if l.fails == nil {
		l.fails = map[string]*loginFails{}
	}
}

// expired reports whether an entry no longer counts for anything: it is not
// blocked and its window is over (or it holds no attempts at all).
func expired(f *loginFails, now time.Time) bool {
	if now.Before(f.blockedUntil) {
		return false
	}
	return f.count <= 0 || now.Sub(f.first) > loginWindow
}

// sweep drops expired entries. Live entries are never touched.
func (l *loginLimiter) sweep(now time.Time) {
	for k, f := range l.fails {
		if expired(f, now) {
			delete(l.fails, k)
		}
	}
}

// reserve takes one attempt for an address key and reports whether the login
// may be evaluated. It refuses, without creating anything, when the key is
// blocked, when it has used up its loginMaxFails attempts (the key is then
// blocked for loginBlock), or when the key is new and the map is full of
// live entries. A reservation stays counted as a failure until release or clear.
func (l *loginLimiter) reserve(key string) bool { return l.reserveMax(key, loginMaxFails) }

// reserveMax is reserve with a per-key attempt budget.
func (l *loginLimiter) reserveMax(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	now := l.clock()
	l.calls++
	if l.calls%loginSweepEvery == 0 {
		l.sweep(now)
	}
	f := l.fails[key]
	if f != nil && expired(f, now) {
		delete(l.fails, key)
		f = nil
	}
	if f == nil {
		if len(l.fails) >= l.capacity() {
			if now.Sub(l.lastFullSweep) >= loginFullSweepGap {
				l.lastFullSweep = now
				l.sweep(now)
			}
			if len(l.fails) >= l.capacity() {
				return false
			}
		}
		l.fails[key] = &loginFails{count: 1, first: now}
		return true
	}
	if now.Before(f.blockedUntil) {
		return false
	}
	if f.count >= max {
		f.blockedUntil = now.Add(loginBlock)
		f.count = 0
		f.first = now
		return false
	}
	f.count++
	return true
}

// release hands back one reservation when the attempt could not be evaluated
// (a database error), so an outage does not lock anyone out.
func (l *loginLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[key]
	if f == nil {
		return
	}
	if f.count > 0 {
		f.count--
	}
	if expired(f, l.clock()) {
		delete(l.fails, key)
	}
}

// slow reports whether a username key has reached its soft limit, i.e. the
// next attempt on that account must be delayed.
func (l *loginLimiter) slow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[key]
	return f != nil && !expired(f, l.clock()) && f.count >= loginUserMaxFails
}

// fail records one failed password check for a username key. Only called
// for accounts that exist, so these keys are bounded by admin_users.
func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	now := l.clock()
	f := l.fails[key]
	if f == nil || expired(f, now) {
		f = &loginFails{first: now}
		l.fails[key] = f
	}
	f.count++
}

// blocked reports whether any of keys is currently locked out.
func (l *loginLimiter) blocked(keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	for _, k := range keys {
		if f, ok := l.fails[k]; ok && now.Before(f.blockedUntil) {
			return true
		}
	}
	return false
}

// count reports the attempts currently recorded for key (0 when absent).
func (l *loginLimiter) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.fails[key]; f != nil && !expired(f, l.clock()) {
		return f.count
	}
	return 0
}

// clear forgets every key after a successful login.
func (l *loginLimiter) clear(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.fails, k)
	}
}

// size reports how many keys the limiter currently tracks.
func (l *loginLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fails)
}

// loginIPKey is the hard per-address limiter key: IPv4 as is, IPv6 reduced to its /64
// so one host cannot rotate through its subnet. ok is false when a trusted proxy
// forwarded no client address: every login would then share the proxy's own address,
// so those logins share the wider loginUnknownKey budget instead (five wrong guesses
// from anyone must not lock the owner out), and the gap is logged once.
func loginIPKey(r *http.Request) (key string, ok bool) {
	ip, known := netx.ClientAddr(r)
	if !known {
		noClientIPOnce.Do(func() {
			log.Printf("后台登录：反代未传客户端地址，所有登录共用一个较宽的尝试额度（%d 次/%s）；请按 docs/nginx-shop.conf.example 配置 proxy_set_header", loginUnknownMaxFails, loginWindow)
		})
		return loginUnknownKey, false
	}
	return "ip:" + netx.LimiterKey(ip), true
}

var noClientIPOnce sync.Once

// validLoginUser reports whether a username may be looked up at all. Anything
// else is answered with the normal 401 and never becomes a limiter key.
func validLoginUser(user string) bool {
	return user != "" && utf8.ValidString(user) && !strings.ContainsRune(user, 0) && runeLen(user) <= loginMaxUser
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

type loginPage struct {
	View
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentUser(r); ok {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	s.render(w, http.StatusOK, "login", loginPage{View: View{Title: "登录"}})
}

const loginWrong = "账号或密码错误"

// login checks, in this order: the form parses; the username is plausible
// (else the normal 401, no key); the service can log anyone in at all (else
// 503, nothing reserved); the client address may try (hard gate, reserved
// before any password work); the account lookup (a database error hands the
// reservation back); the soft per-account delay; bcrypt.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	user := strings.TrimSpace(r.FormValue("username"))
	pass := r.FormValue("password")
	page := loginPage{View: View{Title: "登录"}}
	if !validLoginUser(user) || pass == "" {
		page.Err = loginWrong
		if user == "" || pass == "" {
			page.Err = "请填写用户名和密码"
		}
		s.render(w, http.StatusUnauthorized, "login", page)
		return
	}
	if s.db() == nil {
		page.Err = "数据库未连接"
		s.render(w, http.StatusServiceUnavailable, "login", page)
		return
	}
	if _, ok := sessionKey(); !ok {
		page.Err = "服务器未配置 DUFAKA_SESSION_KEY"
		s.render(w, http.StatusServiceUnavailable, "login", page)
		return
	}
	ipKey, ipKnown := loginIPKey(r)
	ipMax := loginMaxFails
	if !ipKnown {
		ipMax = loginUnknownMaxFails
	}
	if !s.logins.reserveMax(ipKey, ipMax) {
		page.Err = loginBlocked
		s.render(w, http.StatusTooManyRequests, "login", page)
		return
	}
	var id int64
	var hash, name string
	err := s.db().QueryRow(r.Context(), `SELECT id, password, name FROM admin_users WHERE username=$1`, user).Scan(&id, &hash, &name)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.logins.release(ipKey)
		page.Err = dbErr(err)
		s.render(w, http.StatusInternalServerError, "login", page)
		return
	}
	found := err == nil
	userKey := "user:" + user
	if found && s.logins.slow(userKey) {
		pause := s.loginPause
		if pause == nil {
			pause = sleepCtx
		}
		pause(r.Context(), loginUserDelay)
	}
	if !checkLogin(found, hash, pass) {
		if found {
			s.logins.fail(userKey)
		}
		page.Err = loginWrong
		s.render(w, http.StatusUnauthorized, "login", page)
		return
	}
	if ipKnown {
		s.logins.clear(ipKey, userKey)
	} else {
		// The shared key belongs to everyone behind the proxy; one successful login
		// must not refill the budget for the strangers sharing it.
		s.logins.clear(userKey)
	}
	if strings.TrimSpace(name) == "" {
		name = user
	}
	setSession(w, r, id, name)
	http.Redirect(w, r, "/admin", http.StatusFound)
}

// logout runs behind authed so the _token rendered in the layout is verified;
// a cross-site POST cannot end the owner's session.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ session) {
	clearSession(w)
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}
