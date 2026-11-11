package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/auth"
	"github.com/jclement/mapthing/internal/tiles"
)

func sessionCookie(r *http.Response) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	return nil
}

// Behind a tunnel every request has the proxy's address. With MAPTHING_TRUSTED_PROXY the login
// throttle follows the real client, so one person's wrong password does not lock everyone out;
// without it a forwarded header is not believed, so it cannot be used to dodge the throttle.
func TestLoginThrottleBehindAProxy(t *testing.T) {
	bad := map[string]string{"username": "x", "password": "y"}
	good := map[string]string{"username": adminUser, "password": adminPass}

	trusted := newEnv(t, false, func(d *Deps) { d.Auth.SetTrustProxy(true) })
	attacker := map[string]string{"CF-Connecting-IP": "203.0.113.9"}
	for range 5 {
		trusted.do(nil, "POST", "/admin/api/login", bad, attacker)
	}
	if r, _ := trusted.do(nil, "POST", "/admin/api/login", good, attacker); r.StatusCode != 429 {
		t.Fatalf("the throttled client: %d, want 429", r.StatusCode)
	}
	if r, _ := trusted.do(nil, "POST", "/admin/api/login", good, map[string]string{"CF-Connecting-IP": "198.51.100.7"}); r.StatusCode != 200 {
		t.Fatalf("another client behind the same proxy: %d, want 200", r.StatusCode)
	}
	if r, _ := trusted.do(nil, "POST", "/admin/api/login", good, map[string]string{"X-Forwarded-For": "198.51.100.8, 10.0.0.1"}); r.StatusCode != 200 {
		t.Fatalf("a client named by X-Forwarded-For: %d, want 200", r.StatusCode)
	}

	direct := newEnv(t, false)
	for i := range 5 {
		// A new forwarded address every time: it must not reset the count.
		direct.do(nil, "POST", "/admin/api/login", bad, map[string]string{"X-Forwarded-For": "198.51.100." + string(rune('1'+i))})
	}
	if r, _ := direct.do(nil, "POST", "/admin/api/login", good, map[string]string{"CF-Connecting-IP": "192.0.2.55"}); r.StatusCode != 429 {
		t.Fatalf("untrusted: a forwarded header dodged the login throttle (%d)", r.StatusCode)
	}
}

func TestSessionCookieSecureBehindHTTPS(t *testing.T) {
	good := map[string]string{"username": adminUser, "password": adminPass}
	e := newEnv(t, false)
	r, _ := e.do(nil, "POST", "/admin/api/login", good, nil)
	if c := sessionCookie(r); c == nil || c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("plain http: cookie %+v (must work without Secure on the box itself)", c)
	}
	r, _ = e.do(nil, "POST", "/admin/api/login", good, map[string]string{"X-Forwarded-Proto": "https"})
	if c := sessionCookie(r); c == nil || !c.Secure {
		t.Fatalf("behind an https proxy: cookie %+v, want Secure", c)
	}
	// Logging out clears it with the same attributes, or the browser keeps the Secure one.
	r, _ = e.do(nil, "POST", "/admin/api/logout", nil, map[string]string{"X-Forwarded-Proto": "https"})
	if c := sessionCookie(r); c == nil || !c.Secure || c.MaxAge >= 0 {
		t.Fatalf("logout behind https: cookie %+v", c)
	}

	// A tunnel that sends no X-Forwarded-Proto: the public URL says the site is https.
	pub := newEnv(t, false, func(d *Deps) {
		d.Auth = auth.New(adminUser, adminPass, d.Store, d.Keys, d.Stats, "https://maps.example.com")
	})
	req, _ := http.NewRequest("POST", pub.ts.URL+"/admin/api/login", jsonBody(good))
	req.Host = "maps.example.com"
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if c := sessionCookie(resp); resp.StatusCode != 200 || c == nil || !c.Secure {
		t.Fatalf("public https host: %d cookie %+v, want Secure", resp.StatusCode, c)
	}
	// The same server reached on the box over plain http still gets a usable cookie.
	r, _ = pub.do(nil, "POST", "/admin/api/login", good, nil)
	if c := sessionCookie(r); c == nil || c.Secure {
		t.Fatalf("local http with a public https URL: cookie %+v", c)
	}
}

// Every URL the server writes into a style or TileJSON uses MAPTHING_PUBLIC_URL, whatever
// host the request came in on (the tunnel reaches the container as http://mapthing:8080).
func TestPublicURLInGeneratedURLs(t *testing.T) {
	e := newEnv(t, true, func(d *Deps) {
		// newEnv built the tile server without a public URL; this test needs one.
		*d = withPublicURL(t, *d, "https://maps.example.com")
	})
	c := e.login()
	_, key := e.createKey(c, map[string]any{"label": "k"})
	for _, path := range []string{"/styles/outdoors.json", "/tiles/basemap.json"} {
		req, _ := http.NewRequest("GET", e.ts.URL+path+"?key="+key, nil)
		req.Host = "mapthing:8080"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		if !containsAll(body, "https://maps.example.com/") || containsAll(body, "mapthing:8080") || containsAll(body, "http://127.0.0.1") {
			t.Errorf("%s does not use the public URL throughout:\n%s", path, body)
		}
	}
	// The admin UI's own map (a session, no key) is sent to the host it is being used on, so
	// it works on the box over plain http, where the cookie would not follow it elsewhere.
	_, b := e.do(c, "GET", "/styles/outdoors.json", nil, nil)
	if !containsAll(string(b), e.ts.URL+"/tiles/") || containsAll(string(b), "maps.example.com") {
		t.Errorf("admin session style should use the request's host:\n%s", b)
	}
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// withPublicURL returns d with its tile server and auth rebuilt for a public URL.
func withPublicURL(t *testing.T, d Deps, public string) Deps {
	t.Helper()
	ts, err := tiles.New(tiles.Options{DataDir: d.DataDir, Assets: testAssets, Stats: d.Stats, PublicURL: public,
		RendererURL: "http://127.0.0.1:1", RasterCacheBytes: 1 << 20, ImageryCacheBytes: 1 << 20, PackCacheBytes: 10 << 20})
	if err != nil {
		t.Fatal(err)
	}
	d.Tiles = ts
	d.Auth = auth.New(adminUser, adminPass, d.Store, d.Keys, d.Stats, public)
	return d
}
