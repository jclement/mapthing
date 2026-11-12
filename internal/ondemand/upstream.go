package ondemand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jclement/mapthing/internal/elevation"
)

// UserAgent identifies mapthing to the DEM hosts. It names the software and
// its repository, never a person.
const UserAgent = "mapthing (+https://github.com/jclement/mapthing)"

// ErrUpstream marks a failure to get data from a DEM host: the host is down,
// too slow, or answered something other than the bytes asked for. Such a
// failure is never stored as data.
var ErrUpstream = elevation.ErrSourceUnavailable

// UpstreamOptions bound how hard mapthing leans on the DEM hosts.
type UpstreamOptions struct {
	// MaxConcurrent caps requests in flight to all hosts together (default 16).
	MaxConcurrent int
	// MaxPerHost caps requests in flight to one host (default 8).
	MaxPerHost int
	// Timeout is the limit for one attempt (default 20 s).
	Timeout time.Duration
	// Attempts is how many times a request is tried (default 3).
	Attempts int
	// Backoff is the wait before the second attempt; it doubles after that
	// (default 300 ms).
	Backoff time.Duration
	// Client overrides the HTTP client (tests).
	Client *http.Client
}

// Upstream is the only door to the DEM hosts: every request goes through its
// concurrency caps, timeouts and retries, and is counted.
type Upstream struct {
	opt    UpstreamOptions
	client *http.Client
	global chan struct{}

	mu    sync.Mutex
	hosts map[string]*hostState
	// allow, when not empty, is the set of hosts requests and redirects may go to.
	allow map[string]bool

	requests atomic.Int64
	bytes    atomic.Int64
	failures atomic.Int64
}

type hostState struct {
	sem chan struct{}
	HostHealth
}

// HostHealth is what the Overview shows for one DEM host.
type HostHealth struct {
	Host      string    `json:"host"`
	Requests  int64     `json:"requests"`
	Failures  int64     `json:"failures"`
	Bytes     int64     `json:"bytes"`
	LastOK    time.Time `json:"last_ok,omitzero"`
	LastError time.Time `json:"last_error,omitzero"`
	Error     string    `json:"error,omitempty"`
	// Healthy is false while the most recent request to the host failed.
	Healthy bool `json:"healthy"`
}

// NewUpstream returns an Upstream with defaults filled in.
func NewUpstream(opt UpstreamOptions) *Upstream {
	if opt.MaxConcurrent <= 0 {
		opt.MaxConcurrent = 16
	}
	if opt.MaxPerHost <= 0 {
		opt.MaxPerHost = 8
	}
	if opt.MaxPerHost > opt.MaxConcurrent {
		opt.MaxPerHost = opt.MaxConcurrent
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 20 * time.Second
	}
	if opt.Attempts <= 0 {
		opt.Attempts = 3
	}
	if opt.Backoff <= 0 {
		opt.Backoff = 300 * time.Millisecond
	}
	u := &Upstream{opt: opt, client: opt.Client, global: make(chan struct{}, opt.MaxConcurrent), hosts: map[string]*hostState{}}
	if u.client == nil {
		u.client = &http.Client{CheckRedirect: u.checkRedirect, Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   opt.MaxPerHost,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: opt.Timeout,
			ForceAttemptHTTP2:     true,
		}}
	}
	return u
}

// Restrict limits requests, and the redirects they follow, to the given
// hosts (added to any given before). The DEM sources live on a handful of
// known hosts; nothing a host answers can send this server anywhere else.
func (u *Upstream) Restrict(hosts ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.allow == nil {
		u.allow = map[string]bool{}
	}
	for _, h := range hosts {
		if h != "" {
			u.allow[h] = true
		}
	}
}

func (u *Upstream) allowed(host string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.allow) == 0 || u.allow[host]
}

// checkRedirect follows a redirect only to an allowed host, over the same
// scheme, and only a few times.
func (u *Upstream) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != via[0].URL.Scheme || !u.allowed(req.URL.Host) {
		return fmt.Errorf("redirect to %s refused: not a host the elevation sources are read from", req.URL.Host)
	}
	return nil
}

func (u *Upstream) host(name string) *hostState {
	u.mu.Lock()
	defer u.mu.Unlock()
	h, ok := u.hosts[name]
	if !ok {
		h = &hostState{sem: make(chan struct{}, u.opt.MaxPerHost)}
		h.Host, h.Healthy = name, true
		u.hosts[name] = h
	}
	return h
}

// Counters returns requests sent, body bytes received and failed requests
// since start.
func (u *Upstream) Counters() (requests, bytes, failures int64) {
	return u.requests.Load(), u.bytes.Load(), u.failures.Load()
}

// Health lists the hosts contacted so far.
func (u *Upstream) Health() []HostHealth {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]HostHealth, 0, len(u.hosts))
	for _, h := range u.hosts {
		out = append(out, h.HostHealth)
	}
	return out
}

// notFound is returned (wrapped) when the host says the object does not exist.
var errNotFound = errors.New("not found")

// do runs one logical request with caps and retries. handle reads the
// response and returns the body bytes consumed; it is only called for a
// status in want.
func (u *Upstream) do(ctx context.Context, method, rawURL string, hdr map[string]string, want []int, handle func(*http.Response) (int64, error)) error {
	pu, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if !u.allowed(pu.Host) {
		return fmt.Errorf("%w: %s is not a host the elevation sources are read from", ErrUpstream, pu.Host)
	}
	hs := u.host(pu.Host)
	var last error
	for attempt := 0; attempt < u.opt.Attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: %w", ErrUpstream, ctx.Err())
			case <-time.After(u.opt.Backoff << (attempt - 1)):
			}
		}
		n, retry, err := u.once(ctx, hs, method, rawURL, hdr, want, handle)
		u.requests.Add(1)
		u.bytes.Add(n)
		u.mu.Lock()
		hs.Requests++
		hs.Bytes += n
		if err == nil || errors.Is(err, errNotFound) {
			hs.LastOK, hs.Healthy, hs.Error = time.Now(), true, ""
		} else if ctx.Err() == nil { // a caller that went away says nothing about the host
			hs.Failures++
			hs.LastError, hs.Healthy, hs.Error = time.Now(), false, err.Error()
		}
		u.mu.Unlock()
		if err == nil || errors.Is(err, errNotFound) {
			return err
		}
		u.failures.Add(1)
		last = err
		if !retry || ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("%w: %s %s: %w", ErrUpstream, method, pu.Host, last)
}

func (u *Upstream) once(ctx context.Context, hs *hostState, method, rawURL string, hdr map[string]string, want []int, handle func(*http.Response) (int64, error)) (n int64, retry bool, err error) {
	for _, sem := range []chan struct{}{u.global, hs.sem} {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, u.opt.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return 0, true, err
	}
	defer func() { _ = resp.Body.Close() }()
	for _, s := range want {
		if resp.StatusCode == s {
			n, err = handle(resp)
			return n, err != nil, err
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusForbidden:
		// S3 answers 403 for a missing key in a bucket that cannot be listed.
		return 0, false, errNotFound
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return 0, true, fmt.Errorf("answered %s", resp.Status)
	}
	return 0, false, fmt.Errorf("answered %s", resp.Status)
}

// Range reads n bytes at off. Fewer bytes come back only at the end of the
// object.
func (u *Upstream) Range(ctx context.Context, rawURL string, off, n int64) ([]byte, error) {
	var out []byte
	hdr := map[string]string{"Range": "bytes=" + strconv.FormatInt(off, 10) + "-" + strconv.FormatInt(off+n-1, 10)}
	err := u.do(ctx, http.MethodGet, rawURL, hdr, []int{http.StatusPartialContent}, func(resp *http.Response) (int64, error) {
		b, err := io.ReadAll(io.LimitReader(resp.Body, n+1))
		if err != nil {
			return int64(len(b)), err
		}
		if int64(len(b)) > n {
			return int64(len(b)), errors.New("more bytes than asked for")
		}
		out = b
		return int64(len(b)), nil
	})
	return out, err
}

// Version identifies one release of a remote file.
type Version struct {
	// Exists is false when the host says there is no such object.
	Exists bool `json:"exists"`
	// Tag is the ETag, or the Last-Modified time when there is no ETag.
	Tag  string `json:"tag,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// Head asks for a file's current version.
func (u *Upstream) Head(ctx context.Context, rawURL string) (Version, error) {
	var v Version
	err := u.do(ctx, http.MethodHead, rawURL, nil, []int{http.StatusOK}, func(resp *http.Response) (int64, error) {
		v.Exists = true
		v.Tag = strings.Trim(strings.TrimPrefix(resp.Header.Get("ETag"), "W/"), `"`)
		if v.Tag == "" {
			v.Tag = resp.Header.Get("Last-Modified")
		}
		v.Size = resp.ContentLength
		return 0, nil
	})
	if errors.Is(err, errNotFound) {
		return Version{}, nil
	}
	return v, err
}

// Get fetches a small document (the HRDEM index), up to limit bytes.
func (u *Upstream) Get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	var out []byte
	err := u.do(ctx, http.MethodGet, rawURL, nil, []int{http.StatusOK}, func(resp *http.Response) (int64, error) {
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
		out = b
		return int64(len(b)), err
	})
	return out, err
}
