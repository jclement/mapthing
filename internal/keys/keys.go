// Package keys generates and checks API keys and enforces their per-key limits.
//
// A key looks like mt_<52 base32 chars> (32 random bytes). Only its SHA-256 (hex) and a short
// display prefix are stored.
package keys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/store"
)

// Prefix starts every key.
const Prefix = "mt_"

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// Generate returns a new secret, its display prefix and its hash.
func Generate() (secret, prefix, hash string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	secret = Prefix + strings.ToLower(enc.EncodeToString(b[:]))
	return secret, secret[:len(Prefix)+6], Hash(secret), nil
}

// Hash returns the hex SHA-256 of a secret.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// LooksLikeKey reports whether s has the key shape (cheap pre-check before hashing).
func LooksLikeKey(s string) bool {
	return strings.HasPrefix(s, Prefix) && len(s) == len(Prefix)+52
}

// Errors returned by Authenticate and Allow.
var (
	ErrInvalid    = errors.New("invalid API key")
	ErrRevoked    = errors.New("API key revoked")
	ErrOrigin     = errors.New("origin not allowed for this API key")
	ErrRateLimit  = errors.New("rate limit exceeded")
	ErrDayLimit   = errors.New("daily quota exceeded")
	ErrMonthLimit = errors.New("monthly quota exceeded")
	ErrBusy       = errors.New("server limit for this kind of request reached")
)

type cached struct {
	key *store.APIKey
	at  time.Time
}

// Service looks keys up (with a short cache) and enforces limits in memory.
type Service struct {
	st  *store.Store
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cached // by hash
	cfg   Config
	// Rate limiting state (limiter.go).
	buckets   map[int64]*bucket     // per key
	ipBuckets map[ipKey]*bucket     // per key and client
	global    map[string]*bucket    // per kind
	quota     map[int64]*quotaState // per key: units used this day and month
	pending   map[store.QuotaKey]int64
	limited   map[store.LimitedKey]int64
	refusals  map[int64][]Refusal
}

// NewService returns a key service with the stored (or default) weights and global limits.
func NewService(st *store.Store) *Service {
	s := &Service{st: st, now: time.Now, cache: map[string]cached{}, buckets: map[int64]*bucket{},
		ipBuckets: map[ipKey]*bucket{}, global: map[string]*bucket{}, quota: map[int64]*quotaState{},
		pending: map[store.QuotaKey]int64{}, limited: map[store.LimitedKey]int64{}, refusals: map[int64][]Refusal{}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.loadConfig(ctx)
	return s
}

const cacheTTL = 30 * time.Second

// Create makes a key and returns the stored row and the secret (shown once).
func (s *Service) Create(ctx context.Context, label string, lim store.Limits) (*store.APIKey, string, error) {
	secret, prefix, hash, err := Generate()
	if err != nil {
		return nil, "", err
	}
	lim = normaliseLimits(lim)
	k, err := s.st.CreateKey(ctx, label, prefix, hash, lim)
	if err != nil {
		return nil, "", err
	}
	return k, secret, nil
}

// Update changes label/limits and drops cached state for the key.
func (s *Service) Update(ctx context.Context, id int64, label string, lim store.Limits) error {
	if err := s.st.UpdateKey(ctx, id, label, normaliseLimits(lim)); err != nil {
		return err
	}
	s.forget(id)
	return nil
}

// Revoke revokes a key; it stops working immediately.
func (s *Service) Revoke(ctx context.Context, id int64) error {
	if err := s.st.RevokeKey(ctx, id); err != nil {
		return err
	}
	s.forget(id)
	return nil
}

func (s *Service) forget(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, c := range s.cache {
		if c.key.ID == id {
			delete(s.cache, h)
		}
	}
	delete(s.buckets, id)
	for k := range s.ipBuckets {
		if k.key == id {
			delete(s.ipBuckets, k)
		}
	}
}

func normaliseLimits(l store.Limits) store.Limits {
	l = l.Normalise()
	out := []string{}
	for _, o := range l.AllowedOrigins {
		o = strings.TrimRight(strings.TrimSpace(strings.ToLower(o)), "/")
		if o != "" {
			out = append(out, o)
		}
	}
	l.AllowedOrigins = out
	return l
}

// Authenticate resolves a secret to an active key.
func (s *Service) Authenticate(ctx context.Context, secret string) (*store.APIKey, error) {
	if !LooksLikeKey(secret) {
		return nil, ErrInvalid
	}
	h := Hash(secret)
	s.mu.Lock()
	c, ok := s.cache[h]
	s.mu.Unlock()
	if !ok || s.now().Sub(c.at) > cacheTTL {
		k, err := s.st.KeyByHash(ctx, h)
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalid
		}
		if err != nil {
			return nil, err
		}
		c = cached{key: k, at: s.now()}
		s.mu.Lock()
		s.cache[h] = c
		s.mu.Unlock()
	}
	if c.key.RevokedAt != nil {
		return nil, ErrRevoked
	}
	return c.key, nil
}

// CheckOrigin enforces the key's allowed origins against the request's Origin, falling back
// to Referer. Entries are origins ("https://app.example.com"), bare hosts ("app.example.com"),
// or wildcards ("*.example.com"). An empty list allows anything, including no origin.
func CheckOrigin(k *store.APIKey, origin, referer string) error {
	if len(k.Limits.AllowedOrigins) == 0 {
		return nil
	}
	src := origin
	if src == "" || src == "null" {
		src = referer
	}
	u, err := url.Parse(src)
	if src == "" || err != nil || u.Host == "" {
		return ErrOrigin
	}
	host := strings.ToLower(u.Hostname())
	full := strings.ToLower(u.Scheme + "://" + u.Host)
	for _, a := range k.Limits.AllowedOrigins {
		switch {
		case a == "*":
			return nil
		case strings.Contains(a, "://"):
			if a == full {
				return nil
			}
		case strings.HasPrefix(a, "*."):
			if strings.HasSuffix(host, a[1:]) {
				return nil
			}
		default:
			if host == a || strings.ToLower(u.Host) == a {
				return nil
			}
		}
	}
	return ErrOrigin
}

// Allow charges one tile-weight request against the key with no client address: the older
// entry point, kept for callers that only want yes or no. New code calls Charge.
func (s *Service) Allow(ctx context.Context, k *store.APIKey) (time.Duration, error) {
	d := s.Charge(ctx, k, "tile", "")
	return d.RetryAfter, d.Err()
}
