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
	"math"
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

// RequestInfo is what the keyed-endpoint wrapper learns about a request, for the outer
// request middleware (logging and traffic timing), which creates it with WithRequestInfo.
type RequestInfo struct {
	Kind  string // usage kind the endpoint is counted under ("" = not a keyed endpoint)
	KeyID int64  // API key that authenticated it (0 = none, or the admin session)
	// Limited is which limit refused the request with a 429 ("" = none): keys.ScopeRate …
	Limited string
}

type infoKey struct{}

// WithRequestInfo returns ctx carrying a RequestInfo for Wrap to fill in.
func WithRequestInfo(ctx context.Context) (context.Context, *RequestInfo) {
	ri := &RequestInfo{}
	return context.WithValue(ctx, infoKey{}, ri), ri
}

func requestInfo(ctx context.Context) *RequestInfo {
	if ri, ok := ctx.Value(infoKey{}).(*RequestInfo); ok {
		return ri
	}
	return &RequestInfo{} // nobody is listening
}

// Auth is the authentication layer.
type Auth struct {
	user, pass [32]byte // SHA-256 of the configured credential, compared in constant time
	st         *store.Store
	keys       *keys.Service
	stats      *stats.Stats
	publicURL  string // optional, to accept its origin for CSRF checks
	// trustProxy: the server is only reachable through a reverse proxy or tunnel, so the
	// client address is the one the proxy reports (MAPTHING_TRUSTED_PROXY).
	trustProxy bool

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

// SetTrustProxy says whether the client address comes from the proxy's headers
// (CF-Connecting-IP, else the first X-Forwarded-For entry) instead of the connection. Turn it
// on only when every request arrives through a proxy that sets them: a client that can reach
// the server directly could otherwise claim any address.
func (a *Auth) SetTrustProxy(on bool) { a.trustProxy = on }

// ClientIP is the address a request came from, for login throttling, per-client limits and
// logs: the connection's peer, or behind a trusted proxy the address the proxy reports.
func (a *Auth) ClientIP(r *http.Request) string {
	if a.trustProxy {
		if ip := parseIP(r.Header.Get("CF-Connecting-IP")); ip != "" {
			return ip
		}
		first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if ip := parseIP(first); ip != "" {
			return ip
		}
	}
	return peerIP(r)
}

// parseIP returns s as a canonical IP address, or "" when it is not one (a header may hold
// anything; an address is also sometimes given with a port or in brackets).
func parseIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if ip := net.ParseIP(strings.Trim(s, "[]")); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return ""
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
	// The connection's own peer, never a forwarded address: the renderer talks to this
	// process directly, and a header naming 127.0.0.1 proves nothing.
	ip := net.ParseIP(peerIP(r))
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
	ip := a.ClientIP(r)
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
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.isHTTPS(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": body.Username, "expires_at": exp.UTC()})
}

// HandleLogout clears the session.
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		_ = a.st.DeleteSession(r.Context(), tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.isHTTPS(r)})
	w.WriteHeader(http.StatusNoContent)
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
		ri := requestInfo(r.Context())
		ri.Kind = kind
		if a.isInternal(r) {
			ri.Kind = KindInternal
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
		d := a.keys.Charge(r.Context(), k, kind, a.ClientIP(r))
		setRateHeaders(w, d)
		ri.KeyID = k.ID
		if !d.OK {
			ri.Limited = d.Scope
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
			code := "rate_limited"
			if d.Scope == keys.ScopeDaily || d.Scope == keys.ScopeMonthly {
				code = "quota_exceeded"
			}
			WriteError(w, r, http.StatusTooManyRequests, code, d.Message())
			return
		}
		a.stats.Count(k.ID, kind)
		h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{KeyID: k.ID, Secret: secret})))
	})
}

// setRateHeaders tells the client where it stands against the limit closest to running out:
// X-RateLimit-Limit (its size, in cost units), X-RateLimit-Remaining, X-RateLimit-Reset
// (seconds until it is whole again), and on a refusal X-RateLimit-Scope (which limit it was).
// A key with no limits gets none of them.
func setRateHeaders(w http.ResponseWriter, d keys.Decision) {
	h := w.Header()
	if d.Limit > 0 {
		h.Set("X-RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
		h.Set("X-RateLimit-Remaining", strconv.FormatInt(d.Remaining, 10))
		h.Set("X-RateLimit-Reset", strconv.Itoa(int(math.Ceil(d.Reset.Seconds()))))
	}
	if d.Scope != "" {
		h.Set("X-RateLimit-Scope", d.Scope)
	}
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
	hd.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, ETag, Retry-After, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, X-RateLimit-Scope")
	hd.Set("Access-Control-Max-Age", "86400")
}

// isHTTPS reports whether the browser reached this request over HTTPS, so the session cookie
// can be marked Secure: a TLS connection, a proxy saying so (X-Forwarded-Proto, or
// Cloudflare's CF-Visitor), or the request being for the host of an https MAPTHING_PUBLIC_URL.
// Plain HTTP on the box itself (http://127.0.0.1:8080) keeps working: its cookie is not Secure.
func (a *Auth) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	if strings.EqualFold(strings.TrimSpace(proto), "https") {
		return true
	}
	if strings.Contains(r.Header.Get("CF-Visitor"), `"scheme":"https"`) {
		return true
	}
	if pu, err := url.Parse(a.publicURL); err == nil && pu.Scheme == "https" && pu.Host != "" {
		host := r.Host
		if fh, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); strings.TrimSpace(fh) != "" {
			host = strings.TrimSpace(fh)
		}
		return strings.EqualFold(host, pu.Host)
	}
	return false
}

// peerIP is the address of the connection itself.
func peerIP(r *http.Request) string {
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
