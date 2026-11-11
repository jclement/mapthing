// Package update tells the operator whether a newer image has been published.
//
// Once a day at most it asks the registry what the configured tag points at now (the image's
// revision label, read from its config) and compares that with the revision this image was
// built from. It only reports: updating is `docker compose pull && docker compose up -d`, by
// the operator or a timer of theirs. Offline, or with a registry that does not answer, the
// check fails quietly and is tried again a day later.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Interval is the least time between two questions to the registry, successful or not.
const Interval = 24 * time.Hour

// DefaultImage is the published image.
const DefaultImage = "ghcr.io/jclement/mapthing:latest"

const settingKey = "update.check"

// Settings is the part of the store the checker keeps its last answer in.
type Settings interface {
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Status is what the overview shows.
type Status struct {
	// Enabled is false when no check is made; Reason says why.
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
	Image   string `json:"image"`
	// Running image: its revision (a commit) and build time, "" / nil for a local build.
	CurrentRevision string     `json:"current_revision"`
	CurrentCreated  *time.Time `json:"current_created"`
	// What the tag pointed at when last asked.
	CheckedAt      *time.Time `json:"checked_at"`
	LatestRevision string     `json:"latest_revision"`
	LatestCreated  *time.Time `json:"latest_created"`
	// Available: the tag now points at a different, newer image than the one running.
	Available bool `json:"update_available"`
	// Error is why the last check found nothing out ("" after a good one).
	Error string `json:"error,omitempty"`
}

// record is what is stored between restarts.
type record struct {
	Image     string     `json:"image"`
	TriedAt   time.Time  `json:"tried_at"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	Revision  string     `json:"revision,omitempty"`
	Created   *time.Time `json:"created,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Checker asks the registry, at most once per Interval.
type Checker struct {
	Image    string // registry/repository:tag
	Revision string // of the running image; "" or "unknown" for a local build
	Created  string // RFC 3339 build time of the running image, when known
	Off      bool   // MAPTHING_UPDATE_CHECK=off
	Store    Settings
	Client   *http.Client
	Scheme   string // "https"; tests use "http"
	Now      func() time.Time

	mu  sync.Mutex
	rec record
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) revision() string {
	if c.Revision == "unknown" || c.Revision == "dev" {
		return ""
	}
	return c.Revision
}

// disabled says why no check is made, or "".
func (c *Checker) disabled() string {
	switch {
	case c.Off:
		return "turned off (MAPTHING_UPDATE_CHECK=off)"
	case c.revision() == "":
		return "this is a local build, not a published image"
	}
	if _, _, _, err := parseImage(c.Image); err != nil {
		return err.Error()
	}
	return ""
}

// Load reads the last answer from the store.
func (c *Checker) Load(ctx context.Context) {
	v, err := c.Store.Setting(ctx, settingKey)
	if err != nil || v == "" {
		return
	}
	var r record
	if json.Unmarshal([]byte(v), &r) != nil || r.Image != c.Image {
		return // another image or tag was configured since: what was learned no longer applies
	}
	c.mu.Lock()
	c.rec = r
	c.mu.Unlock()
}

// Status reports what is known, without asking anyone.
func (c *Checker) Status() Status {
	c.mu.Lock()
	r := c.rec
	c.mu.Unlock()
	st := Status{Image: c.Image, CurrentRevision: c.revision(), CurrentCreated: parseTime(c.Created)}
	if why := c.disabled(); why != "" {
		st.Reason = why
		return st
	}
	st.Enabled = true
	st.CheckedAt, st.LatestRevision, st.LatestCreated, st.Error = r.CheckedAt, r.Revision, r.Created, r.Error
	if r.Revision != "" && r.Revision != st.CurrentRevision {
		// A different image. It is an update unless it is known to be the older of the two
		// (a tag moved back, or this server runs something newer than the tag).
		st.Available = st.CurrentCreated == nil || r.Created == nil || r.Created.After(*st.CurrentCreated)
	}
	return st
}

// Due reports whether enough time has passed to ask again.
func (c *Checker) Due() bool {
	if c.disabled() != "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rec.TriedAt.IsZero() || c.now().Sub(c.rec.TriedAt) >= Interval
}

// Check asks the registry now if a check is due, and stores the answer. A failure is
// recorded, logged at debug level and otherwise kept quiet.
func (c *Checker) Check(ctx context.Context) {
	if !c.Due() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rev, created, err := c.fetch(ctx)
	now := c.now().UTC()
	c.mu.Lock()
	c.rec.Image, c.rec.TriedAt = c.Image, now
	if err != nil {
		c.rec.Error = err.Error() // what was learned before stays
		slog.Debug("update check failed", "image", c.Image, "err", err)
	} else {
		c.rec.CheckedAt, c.rec.Revision, c.rec.Created, c.rec.Error = &now, rev, created, ""
	}
	b, _ := json.Marshal(c.rec)
	c.mu.Unlock()
	if err := c.Store.SetSetting(ctx, settingKey, string(b)); err != nil {
		slog.Debug("update check: store", "err", err)
	}
}

// Run checks shortly after start (when due) and then looks every hour whether a day has passed.
func (c *Checker) Run(ctx context.Context) {
	c.Load(ctx)
	if c.disabled() != "" {
		return
	}
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-tick.C:
		}
		c.Check(ctx)
	}
}

var imageRE = regexp.MustCompile(`^([a-z0-9.-]+(?::\d+)?)/([a-z0-9._/-]+?)(?::([A-Za-z0-9_][A-Za-z0-9._-]{0,127}))?$`)

// parseImage splits registry/repository[:tag]. A digest reference is refused: it can never
// have an update.
func parseImage(image string) (host, repo, tag string, err error) {
	if strings.Contains(image, "@") {
		return "", "", "", errors.New("the image is pinned by digest, so there is nothing to check")
	}
	m := imageRE.FindStringSubmatch(image)
	if m == nil || !strings.ContainsAny(m[1], ".:") {
		return "", "", "", fmt.Errorf("MAPTHING_UPDATE_IMAGE %q is not registry/repository:tag", image)
	}
	tag = m[3]
	if tag == "" {
		tag = "latest"
	}
	return m[1], m[2], tag, nil
}

func parseTime(s string) *time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
		t = t.UTC()
		return &t
	}
	return nil
}

const acceptManifests = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// fetch returns the revision label and build time of the image the tag points at, for this
// machine's architecture.
func (c *Checker) fetch(ctx context.Context) (revision string, created *time.Time, err error) {
	host, repo, tag, err := parseImage(c.Image)
	if err != nil {
		return "", nil, err
	}
	scheme := c.Scheme
	if scheme == "" {
		scheme = "https"
	}
	base := scheme + "://" + host + "/v2/" + repo
	s := &session{c: c, ctx: ctx}

	var m struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS, Architecture string
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := s.get(base+"/manifests/"+tag, acceptManifests, &m); err != nil {
		return "", nil, err
	}
	if m.Config.Digest == "" { // an index: one manifest per platform
		digest := ""
		for _, p := range m.Manifests {
			if p.Platform.OS == "linux" && p.Platform.Architecture == runtime.GOARCH {
				digest = p.Digest
				break
			}
		}
		if digest == "" {
			return "", nil, fmt.Errorf("%s has no linux/%s image", c.Image, runtime.GOARCH)
		}
		m.Config.Digest = ""
		if err := s.get(base+"/manifests/"+digest, acceptManifests, &m); err != nil {
			return "", nil, err
		}
		if m.Config.Digest == "" {
			return "", nil, errors.New("the registry's manifest names no image config")
		}
	}
	var cfg struct {
		Created string `json:"created"`
		Config  struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := s.get(base+"/blobs/"+m.Config.Digest, "application/json", &cfg); err != nil {
		return "", nil, err
	}
	revision = cfg.Config.Labels["org.opencontainers.image.revision"]
	if revision == "" || revision == "unknown" {
		return "", nil, errors.New("the published image does not say which revision it is")
	}
	created = parseTime(cfg.Config.Labels["org.opencontainers.image.created"])
	if created == nil {
		created = parseTime(cfg.Created)
	}
	return revision, created, nil
}

// session makes registry requests, getting an anonymous pull token the first time one is
// asked for (the Docker registry token flow: 401 with a WWW-Authenticate challenge).
type session struct {
	c     *Checker
	ctx   context.Context
	token string
}

func (s *session) client() *http.Client {
	if s.c.Client != nil {
		return s.c.Client
	}
	return http.DefaultClient
}

func (s *session) do(u, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "mapthing-update-check")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	return s.client().Do(req)
}

func (s *session) get(u, accept string, into any) error {
	resp, err := s.do(u, accept)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized && s.token == "" {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		if err := s.authorise(challenge); err != nil {
			return err
		}
		if resp, err = s.do(u, accept); err != nil {
			return err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the registry answered %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(into)
}

var challengeRE = regexp.MustCompile(`(realm|service|scope)="([^"]*)"`)

func (s *session) authorise(challenge string) error {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return errors.New("the registry wants a login this check does not have")
	}
	p := map[string]string{}
	for _, m := range challengeRE.FindAllStringSubmatch(challenge, -1) {
		p[m[1]] = m[2]
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Host == "" || (realm.Scheme != "https" && realm.Scheme != s.c.Scheme) {
		return errors.New("the registry's token address is not usable")
	}
	q := realm.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	if p["scope"] != "" {
		q.Set("scope", p["scope"])
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mapthing-update-check")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the registry's token service answered %d", resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return err
	}
	if s.token = t.Token; s.token == "" {
		s.token = t.AccessToken
	}
	if s.token == "" {
		return errors.New("the registry's token service gave no token")
	}
	return nil
}
