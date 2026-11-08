// Package auth handles the admin session (single credential from the environment) and API-key
// authentication, limits and usage counting for keyed endpoints.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/stats"
	"github.com/jclement/mapthing/internal/store"
)

// CookieName is the admin session cookie.
const CookieName = "mapthing_session"

// SessionTTL is how long an admin session lasts.
const SessionTTL = 7 * 24 * time.Hour

// Principal is who made a keyed request.
type Principal struct {
	Admin    bool
	Internal bool   // the local renderer, authenticated by the internal token
	KeyID    int64  // 0 for the admin session
	Secret   string // the key as presented (for templating it into style URLs); "" for admin
}

type ctxKey struct{}

// FromContext returns the request's principal, if authenticated.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// WithPrincipal returns ctx carrying p (for tests and internal calls).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// Auth is the authentication layer.
type Auth struct {
	user, pass [32]byte // SHA-256 of the configured credential, compared in constant time
	st         *store.Store
	keys       *keys.Service
	stats      *stats.Stats
	publicURL  string // optional, to accept its origin for CSRF checks

	logins *loginLimiter

	internalToken [32]byte // SHA-256 of the renderer's token; zero = disabled
	hasInternal   bool
}

// SetInternalToken enables internal requests: from a loopback address with header
// X-Mapthing-Internal: <token>. They need no API key and count as kind "raster-internal".
func (a *Auth) SetInternalToken(tok string) {
	if tok == "" {
		return
	}
	a.internalToken, a.hasInternal = sha256.Sum256([]byte(tok)), true
}

// InternalHeader carries the internal token.
const InternalHeader = "X-Mapthing-Internal"

// KindInternal is the usage kind for internal (renderer) requests.
const KindInternal = "raster-internal"

func (a *Auth) isInternal(r *http.Request) bool {
	h := r.Header.Get(InternalHeader)
	if !a.hasInternal || h == "" {
		return false
	}
	ip := net.ParseIP(clientIP(r))
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	sum := sha256.Sum256([]byte(h))
	return subtle.ConstantTimeCompare(sum[:], a.internalToken[:]) == 1
}

// New returns an Auth for the given admin credential.
func New(user, pass string, st *store.Store, ks *keys.Service, ss *stats.Stats, publicURL string) *Auth {
	return &Auth{
		user: sha256.Sum256([]byte(user)), pass: sha256.Sum256([]byte(pass)),
		st: st, keys: ks, stats: ss, publicURL: strings.TrimRight(publicURL, "/"),
		logins: newLoginLimiter(5, 5*time.Minute),
	}
}

func (a *Auth) credentialsOK(user, pass string) bool {
	u := sha256.Sum256([]byte(user))
	p := sha256.Sum256([]byte(pass))
	uok := subtle.ConstantTimeCompare(u[:], a.user[:])
	pok := subtle.ConstantTimeCompare(p[:], a.pass[:])
	return uok&pok == 1
}

func tokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// IsAdmin reports whether the request carries a valid admin session.
func (a *Auth) IsAdmin(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" || len(c.Value) > 128 {
		return false
	}
	ok, err := a.st.SessionValid(r.Context(), tokenHash(c.Value))
	if err != nil {
		slog.Warn("session lookup", "err", err)
	}
	return ok
}

// HandleLogin: POST {"username","password"} → sets the session cookie.
func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		WriteError(w, r, http.StatusForbidden, "csrf", "cross-origin request refused")
		return
	}
	ip := clientIP(r)
	if wait := a.logins.blocked(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		WriteError(w, r, http.StatusTooManyRequests, "too_many_attempts", "too many failed logins; try again later")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		WriteError(w, r, http.StatusBadRequest, "bad_request", "expected JSON {username, password}")
		return
	}
	if !a.credentialsOK(body.Username, body.Password) {
		a.logins.fail(ip)
		slog.Warn("admin login failed", "ip", ip)
		WriteError(w, r, http.StatusUnauthorized, "bad_credentials", "wrong username or password")
		return
	}
	a.logins.reset(ip)
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		WriteError(w, r, http.StatusInternalServerError, "internal", "could not create session")
		return
	}
	tok := hex.EncodeToString(b[:])
	exp := time.Now().Add(SessionTTL)
	if err := a.st.CreateSession(r.Context(), tokenHash(tok), exp); err != nil {
		WriteError(w, r, http.StatusInternalServerError, "internal", "could not create session")
		return
	}
	_ = a.st.PurgeSessions(r.Context())
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: tok, Path: "/", Expires: exp, MaxAge: int(SessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": exp.UTC()})
}

// HandleLogout clears the session.
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		_ = a.st.DeleteSession(r.Context(), tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// HandleSession reports whether the caller is signed in (open endpoint, for the UI).
func (a *Auth) HandleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": a.IsAdmin(r)})
}

// RequireAdmin guards admin endpoints: a valid session, and for unsafe methods a same-origin
// request (CSRF protection on top of SameSite=Lax).
func (a *Auth) RequireAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.IsAdmin(r) {
			WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		if !safeMethod(r.Method) && !a.sameOrigin(r) {
			WriteError(w, r, http.StatusForbidden, "csrf", "cross-origin request refused")
			return
		}
		h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{Admin: true})))
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin implements the CSRF check: the browser's Sec-Fetch-Site, else Origin, else
// Referer must name this host (or the configured public URL). Requests with none of these
// headers come from non-browser clients, which cannot be CSRF victims; they are allowed.
func (a *Auth) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		return false
	}
	src := r.Header.Get("Origin")
	if src == "" || src == "null" {
		src = r.Header.Get("Referer")
		if src == "" {
			return r.Header.Get("Origin") == "" // no browser signal at all
		}
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" && strings.EqualFold(u.Host, strings.TrimSpace(strings.Split(fh, ",")[0])) {
		return true
	}
	if a.publicURL != "" {
		if pu, err := url.Parse(a.publicURL); err == nil && strings.EqualFold(pu.Host, u.Host) {
			return true
		}
	}
	return false
}

// KeyFromRequest extracts an API key from, in order: Authorization (Bearer or bare, ORS
// style), X-Goog-Api-Key, ?key=, ?api_key=.
func KeyFromRequest(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return h
	}
	if h := r.Header.Get("X-Goog-Api-Key"); h != "" {
		return strings.TrimSpace(h)
	}
	q := r.URL.Query()
	if k := q.Get("key"); k != "" {
		return k
	}
	return q.Get("api_key")
}

// Wrap guards a keyed endpoint: API key (or admin session), origin, rate limits, and counts
// the request under kind.
func (a *Auth) Wrap(kind string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			cors(w, r)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		cors(w, r)
		if a.isInternal(r) {
			a.stats.Count(0, KindInternal)
			h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{Internal: true})))
			return
		}
		secret := KeyFromRequest(r)
		if secret == "" {
			if a.IsAdmin(r) {
				a.stats.Count(0, kind)
				h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{Admin: true})))
				return
			}
			WriteError(w, r, http.StatusUnauthorized, "missing_key", "an API key is required")
			return
		}
		k, err := a.keys.Authenticate(r.Context(), secret)
		switch {
		case errors.Is(err, keys.ErrInvalid), errors.Is(err, keys.ErrRevoked):
			WriteError(w, r, http.StatusUnauthorized, "invalid_key", err.Error())
			return
		case err != nil:
			WriteError(w, r, http.StatusInternalServerError, "internal", "key lookup failed")
			return
		}
		if err := keys.CheckOrigin(k, r.Header.Get("Origin"), r.Header.Get("Referer")); err != nil {
			WriteError(w, r, http.StatusForbidden, "origin_not_allowed", err.Error())
			return
		}
		if wait, err := a.keys.Allow(r.Context(), k); err != nil {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			WriteError(w, r, http.StatusTooManyRequests, "rate_limited", err.Error())
			return
		}
		a.stats.Count(k.ID, kind)
		h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{KeyID: k.ID, Secret: secret})))
	})
}

// cors allows browsers on other origins to call keyed endpoints (the key's allowed-origins
// list is the access control, not CORS).
func cors(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") == "" {
		return
	}
	hd := w.Header()
	hd.Set("Access-Control-Allow-Origin", "*")
	hd.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Goog-Api-Key, X-Goog-FieldMask, Range, If-None-Match")
	hd.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, ETag, Retry-After")
	hd.Set("Access-Control-Max-Age", "86400")
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginLimiter refuses logins from an IP after max failures within window.
type loginLimiter struct {
	max    int
	window time.Duration
	mu     sync.Mutex
	fails  map[string][]time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *loginLimiter) prune(ip string, now time.Time) []time.Time {
	f := l.fails[ip]
	i := 0
	for i < len(f) && now.Sub(f[i]) > l.window {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = f
	}
	return f
}

func (l *loginLimiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	f := l.prune(ip, now)
	if len(f) < l.max {
		return 0
	}
	return l.window - now.Sub(f[0])
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.prune(ip, time.Now()), time.Now())
	if len(l.fails) > 10000 { // bound memory under a spray of addresses
		for k := range l.fails {
			delete(l.fails, k)
			break
		}
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}
