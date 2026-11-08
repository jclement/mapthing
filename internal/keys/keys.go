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
	"math"
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
	ErrInvalid   = errors.New("invalid API key")
	ErrRevoked   = errors.New("API key revoked")
	ErrOrigin    = errors.New("origin not allowed for this API key")
	ErrRateLimit = errors.New("rate limit exceeded")
	ErrDayLimit  = errors.New("daily request limit exceeded")
)

type cached struct {
	key *store.APIKey
	at  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

type daily struct {
	day   string
	count int64
}

// Service looks keys up (with a short cache) and enforces limits in memory.
type Service struct {
	st  *store.Store
	now func() time.Time

	mu      sync.Mutex
	cache   map[string]cached // by hash
	buckets map[int64]*bucket
	days    map[int64]*daily
}

// NewService returns a key service.
func NewService(st *store.Store) *Service {
	return &Service{st: st, now: time.Now, cache: map[string]cached{}, buckets: map[int64]*bucket{}, days: map[int64]*daily{}}
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
}

func normaliseLimits(l store.Limits) store.Limits {
	if l.RPM < 0 {
		l.RPM = 0
	}
	if l.RPD < 0 {
		l.RPD = 0
	}
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

// Allow charges one request against the key's per-minute bucket and daily counter. On
// refusal it returns the error and how long to wait.
func (s *Service) Allow(ctx context.Context, k *store.APIKey) (time.Duration, error) {
	now := s.now()
	day := now.UTC().Format("2006-01-02")
	var seed bool
	s.mu.Lock()
	d := s.days[k.ID]
	if d == nil || d.day != day {
		seed = k.Limits.RPD > 0
		d = &daily{day: day}
		s.days[k.ID] = d
	}
	s.mu.Unlock()
	if seed {
		// First request of the day since start: include what is already in the database.
		if n, err := s.st.KeyDayTotal(ctx, k.ID, day); err == nil {
			s.mu.Lock()
			d.count += n
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if k.Limits.RPD > 0 && d.count >= int64(k.Limits.RPD) {
		midnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		return midnight.Sub(now), ErrDayLimit
	}
	if k.Limits.RPM > 0 {
		capacity := float64(k.Limits.RPM)
		rate := capacity / 60 // tokens per second
		b := s.buckets[k.ID]
		if b == nil {
			b = &bucket{tokens: capacity, last: now}
			s.buckets[k.ID] = b
		}
		b.tokens = math.Min(capacity, b.tokens+now.Sub(b.last).Seconds()*rate)
		b.last = now
		if b.tokens < 1 {
			wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
			return wait, ErrRateLimit
		}
		b.tokens--
	}
	d.count++
	return 0, nil
}
