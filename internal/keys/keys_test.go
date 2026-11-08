package keys

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/store"
)

func TestGenerate(t *testing.T) {
	s, p, h, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeKey(s) || p != s[:9] || h != Hash(s) || len(h) != 64 {
		t.Fatalf("%q %q %q", s, p, h)
	}
	s2, _, _, _ := Generate()
	if s == s2 {
		t.Fatal("keys repeat")
	}
}

func TestCheckOrigin(t *testing.T) {
	k := &store.APIKey{Limits: store.Limits{AllowedOrigins: []string{"https://app.example.com", "*.example.org", "localhost:3000"}}}
	cases := []struct {
		origin, referer string
		ok              bool
	}{
		{"https://app.example.com", "", true},
		{"http://app.example.com", "", false},
		{"https://a.b.example.org", "", true},
		{"https://example.org.evil.com", "", false},
		{"http://localhost:3000", "", true},
		{"", "https://app.example.com/x", true},
		{"null", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		err := CheckOrigin(k, c.origin, c.referer)
		if (err == nil) != c.ok {
			t.Errorf("%q/%q: %v", c.origin, c.referer, err)
		}
	}
	if CheckOrigin(&store.APIKey{}, "", "") != nil {
		t.Error("empty list must allow")
	}
}

func TestAllowRPM(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s := NewService(st)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	k := &store.APIKey{ID: 1, Limits: store.Limits{RPM: 60, RPD: 100}}
	for i := range 60 {
		if _, err := s.Allow(ctx, k); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	wait, err := s.Allow(ctx, k)
	if !errors.Is(err, ErrRateLimit) || wait <= 0 || wait > time.Second {
		t.Fatalf("expected rate limit, got %v %v", wait, err)
	}
	now = now.Add(2 * time.Second) // refills 2 tokens at 1/s
	for range 2 {
		if _, err := s.Allow(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Allow(ctx, k); !errors.Is(err, ErrRateLimit) {
		t.Fatal("expected limit after refill used")
	}
	now = now.Add(time.Hour)
	for range 38 {
		if _, err := s.Allow(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Allow(ctx, k); !errors.Is(err, ErrDayLimit) {
		t.Fatalf("expected day limit, got %v", err)
	}
	now = now.Add(12 * time.Hour) // next UTC day
	if _, err := s.Allow(ctx, k); err != nil {
		t.Fatalf("new day: %v", err)
	}
}
