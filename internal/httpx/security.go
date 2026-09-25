package httpx

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"dufaka/internal/netx"
)

// staticFiles serves files under root without directory listings and without dotfiles.
// The handler is a plain http.FileServer over assetFS, so Range, HEAD and conditional
// requests keep working and every request opens the file exactly once; the file system
// itself refuses directories and any path segment starting with "." (covers ".git",
// ".env" and the "._NOTICE" AppleDouble files that macOS copies leave behind). The
// handler expects the "/assets/" prefix to be stripped already.
func staticFiles(root string) http.Handler {
	files := http.FileServer(assetFS{http.Dir(root)})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// http.FileServer would answer ".../index.html" with a redirect to the directory;
		// index.html is not a servable asset name, so it is simply not found.
		if path.Base(r.URL.Path) == "index.html" {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// assetFS wraps http.Dir and answers fs.ErrNotExist for anything that is not a plain,
// visibly named file. Refusing directories here means http.FileServer never gets the
// chance to list one or to fall back to index.html: index.html is not a servable asset name.
type assetFS struct{ dir http.Dir }

func (a assetFS) Open(name string) (http.File, error) {
	clean := path.Clean("/" + name)
	if clean == "/" || hasDotSegment(clean) || path.Base(clean) == "index.html" {
		return nil, fs.ErrNotExist
	}
	f, err := a.dir.Open(clean)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		_ = f.Close()
		if err == nil {
			err = fs.ErrNotExist
		}
		return nil, err
	}
	return f, nil
}

func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// clientIP is the storefront's view of the visitor address. The one rule lives in
// netx.ClientIP: proxy headers count only when the TCP peer is a trusted reverse proxy
// (loopback, or DUFAKA_TRUSTED_PROXIES), X-Real-IP wins over the last parsable
// X-Forwarded-For entry, and anything that does not parse as an IP is ignored so free
// text can never reach orders.buy_ip.
func clientIP(r *http.Request) string { return netx.ClientIP(r) }

// securityHeaders sets the browser hardening headers on every response. HSTS is only sent
// when the request arrived over https: directly, or through a trusted proxy that says so
// in X-Forwarded-Proto (the header is ignored from anyone else, so a direct visitor
// cannot pin a plain-http test deployment). No template frames the site, so framing is
// refused outright.
func securityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	if r.TLS != nil || (netx.TrustsProxy(r) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")) {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}
