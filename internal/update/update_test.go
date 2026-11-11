package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memStore) Setting(_ context.Context, k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[k]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}

func (s *memStore) SetSetting(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

// registry is a small stand-in for GHCR: token flow, an index per tag, one image per platform.
type registry struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	revision string // of the image "latest" points at
	created  string
	single   bool // serve a plain manifest instead of an index
	down     bool
	requests int
	tokens   int
}

func newRegistry(t *testing.T) *registry {
	r := &registry{t: t, revision: "1111111111111111111111111111111111111111", created: "2026-11-01T10:00:00Z"}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *registry) host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

func (r *registry) serve(w http.ResponseWriter, q *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	if r.down {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	if q.URL.Path == "/token" {
		if q.URL.Query().Get("scope") != "repository:acme/maps:pull" || q.URL.Query().Get("service") != "registry.test" {
			r.t.Errorf("token request %s", q.URL.RawQuery)
		}
		r.tokens++
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon"})
		return
	}
	if q.Header.Get("Authorization") != "Bearer anon" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="registry.test",scope="repository:acme/maps:pull"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	manifest := map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "config": map[string]string{"digest": "sha256:cfg"}}
	switch q.URL.Path {
	case "/v2/acme/maps/manifests/latest":
		if r.single {
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []map[string]any{
			{"digest": "sha256:other", "platform": map[string]string{"os": "linux", "architecture": "s390x"}},
			{"digest": "sha256:attest", "platform": map[string]string{"os": "unknown", "architecture": "unknown"}},
			{"digest": "sha256:mine", "platform": map[string]string{"os": "linux", "architecture": runtime.GOARCH}},
		}})
	case "/v2/acme/maps/manifests/sha256:mine":
		_ = json.NewEncoder(w).Encode(manifest)
	case "/v2/acme/maps/blobs/sha256:cfg":
		_ = json.NewEncoder(w).Encode(map[string]any{"created": "2020-01-01T00:00:00Z", "config": map[string]any{
			"Labels": map[string]string{"org.opencontainers.image.revision": r.revision, "org.opencontainers.image.created": r.created}}})
	default:
		http.NotFound(w, q)
	}
}

func (r *registry) publish(rev, created string) {
	r.mu.Lock()
	r.revision, r.created = rev, created
	r.mu.Unlock()
}

func (r *registry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

const (
	revA = "1111111111111111111111111111111111111111"
	revB = "2222222222222222222222222222222222222222"
)

func newChecker(r *registry, st *memStore, now *time.Time) *Checker {
	return &Checker{Image: r.host() + "/acme/maps:latest", Revision: revA, Created: "2026-11-01T10:00:00Z",
		Store: st, Scheme: "http", Now: func() time.Time { return *now }}
}

func TestNewerImageIsReportedAtMostDaily(t *testing.T) {
	r := newRegistry(t)
	st := &memStore{m: map[string]string{}}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	c := newChecker(r, st, &now)
	ctx := context.Background()

	if s := c.Status(); !s.Enabled || s.CheckedAt != nil || s.Available {
		t.Fatalf("before any check: %+v", s)
	}
	c.Check(ctx)
	s := c.Status()
	if s.Error != "" || s.CheckedAt == nil || s.LatestRevision != revA || s.Available {
		t.Fatalf("running the published image: %+v", s)
	}
	first := r.count() // challenge, token, index, manifest, config
	if first != 5 {
		t.Errorf("%d registry requests for one check, want 5", first)
	}

	// A new image is published. Nothing is asked again for a day, however often the page loads.
	r.publish(revB, "2026-11-11T13:00:00Z")
	for range 50 {
		now = now.Add(20 * time.Minute)
		c.Check(ctx)
		c.Status()
	}
	if r.count() != first || c.Status().Available {
		t.Fatalf("asked again within the day (%d requests)", r.count()-first)
	}
	now = now.Add(8 * time.Hour) // 24 h 40 min after the first
	c.Check(ctx)
	s = c.Status()
	if !s.Available || s.LatestRevision != revB || s.LatestCreated == nil || s.LatestCreated.Hour() != 13 {
		t.Fatalf("after a day: %+v", s)
	}

	// A restart keeps the answer and the clock: no request, still available.
	n := r.count()
	c2 := newChecker(r, st, &now)
	c2.Load(ctx)
	c2.Check(ctx)
	if s := c2.Status(); !s.Available || r.count() != n {
		t.Fatalf("after a restart: %+v, %d new requests", s, r.count()-n)
	}
	// The operator updates: the new image runs revision B. No update any more, without asking.
	c3 := newChecker(r, st, &now)
	c3.Revision, c3.Created = revB, "2026-11-11T13:00:00Z"
	c3.Load(ctx)
	if s := c3.Status(); s.Available || r.count() != n {
		t.Fatalf("after updating: %+v", s)
	}
	// Another tag is configured: what was learned about the old one is dropped.
	c4 := newChecker(r, st, &now)
	c4.Image = r.host() + "/acme/maps:stable"
	c4.Load(ctx)
	if s := c4.Status(); s.LatestRevision != "" || s.CheckedAt != nil {
		t.Fatalf("another tag inherited the answer: %+v", s)
	}
}

func TestOfflineFailsQuietlyAndKeepsWhatItKnew(t *testing.T) {
	r := newRegistry(t)
	st := &memStore{m: map[string]string{}}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	c := newChecker(r, st, &now)
	ctx := context.Background()
	r.publish(revB, "2026-11-11T13:00:00Z")
	c.Check(ctx)
	if !c.Status().Available {
		t.Fatal("setup")
	}
	// The registry is down the next day: the earlier answer stands, the error is recorded,
	// and it is not hammered: one attempt, then nothing for a day.
	r.mu.Lock()
	r.down = true
	r.mu.Unlock()
	now = now.Add(25 * time.Hour)
	n := r.count()
	for range 10 {
		c.Check(ctx)
		now = now.Add(time.Hour)
	}
	s := c.Status()
	if !s.Available || s.Error == "" || !strings.Contains(s.Error, "502") || r.count() != n+1 {
		t.Fatalf("registry down: %+v, %d requests", s, r.count()-n)
	}
	// No network at all.
	r.srv.Close()
	now = now.Add(25 * time.Hour)
	c.Check(ctx)
	if s := c.Status(); !s.Available || s.Error == "" || s.LatestRevision != revB {
		t.Fatalf("offline: %+v", s)
	}
	// Never reached anything: enabled, nothing known, nothing claimed.
	c0 := newChecker(r, &memStore{m: map[string]string{}}, &now)
	c0.Check(ctx)
	if s := c0.Status(); s.Available || s.CheckedAt != nil || s.Error == "" {
		t.Fatalf("first check offline: %+v", s)
	}
}

func TestWhenThereIsNothingToCheck(t *testing.T) {
	r := newRegistry(t)
	now := time.Now()
	ctx := context.Background()
	for name, edit := range map[string]func(*Checker){
		"local build":      func(c *Checker) { c.Revision = "unknown" },
		"no revision":      func(c *Checker) { c.Revision = "" },
		"turned off":       func(c *Checker) { c.Off = true },
		"pinned by digest": func(c *Checker) { c.Image = "ghcr.io/acme/maps@sha256:abc" },
		"not an image":     func(c *Checker) { c.Image = "maps" },
	} {
		c := newChecker(r, &memStore{m: map[string]string{}}, &now)
		edit(c)
		c.Check(ctx)
		if s := c.Status(); s.Enabled || s.Reason == "" || s.Available {
			t.Errorf("%s: %+v", name, s)
		}
	}
	if r.count() != 0 {
		t.Errorf("%d requests were made", r.count())
	}
	// Run returns at once when disabled.
	c := newChecker(r, &memStore{m: map[string]string{}}, &now)
	c.Off = true
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return for a disabled checker")
	}
}

func TestOlderOrOddPublishedImages(t *testing.T) {
	r := newRegistry(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	// The tag points at something older than what runs (a rollback upstream, or a server
	// running a newer build than its tag): not an update.
	r.publish(revB, "2026-10-01T00:00:00Z")
	c := newChecker(r, &memStore{m: map[string]string{}}, &now)
	c.Check(ctx)
	if s := c.Status(); s.Available || s.LatestRevision != revB {
		t.Fatalf("older image offered as an update: %+v", s)
	}
	// Build times unknown on one side: a different revision is reported.
	c = newChecker(r, &memStore{m: map[string]string{}}, &now)
	c.Created = "unknown"
	c.Check(ctx)
	if !c.Status().Available {
		t.Fatal("different revision, unknown build time: not reported")
	}
	// A single-platform manifest instead of an index.
	r.mu.Lock()
	r.single = true
	r.mu.Unlock()
	r.publish(revB, "2026-11-11T13:00:00Z")
	c = newChecker(r, &memStore{m: map[string]string{}}, &now)
	c.Check(ctx)
	if s := c.Status(); !s.Available || s.Error != "" {
		t.Fatalf("plain manifest: %+v", s)
	}
	// An image that does not say what it is: an error, not an update.
	r.publish("", "")
	c = newChecker(r, &memStore{m: map[string]string{}}, &now)
	c.Check(ctx)
	if s := c.Status(); s.Available || s.Error == "" {
		t.Fatalf("unlabelled image: %+v", s)
	}
}

func TestParseImage(t *testing.T) {
	for in, want := range map[string][3]string{
		"ghcr.io/jclement/mapthing:latest":      {"ghcr.io", "jclement/mapthing", "latest"},
		"ghcr.io/jclement/mapthing":             {"ghcr.io", "jclement/mapthing", "latest"},
		"ghcr.io/jclement/mapthing:sha-0a1b2c3": {"ghcr.io", "jclement/mapthing", "sha-0a1b2c3"},
		"registry.example.com:5000/a/b/c:v1":    {"registry.example.com:5000", "a/b/c", "v1"},
	} {
		h, r, tag, err := parseImage(in)
		if err != nil || [3]string{h, r, tag} != want {
			t.Errorf("%s → %s %s %s %v", in, h, r, tag, err)
		}
	}
	for _, bad := range []string{"", "mapthing", "jclement/mapthing:latest", "ghcr.io/x@sha256:abc", "ghcr.io/UPPER/x", "http://ghcr.io/a/b"} {
		if _, _, _, err := parseImage(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
