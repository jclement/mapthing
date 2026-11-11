package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/keys"
)

type keyResp struct {
	ID     string `json:"id"`
	Limits struct {
		RPM          *int     `json:"rpm"`
		RPD          *int     `json:"rpd"`
		RPS          *float64 `json:"rps"`
		Burst        *int     `json:"burst"`
		DailyQuota   *int64   `json:"daily_quota"`
		MonthlyQuota *int64   `json:"monthly_quota"`
		IPRPS        *float64 `json:"ip_rps"`
		IPBurst      *int     `json:"ip_burst"`
	} `json:"limits"`
	Usage struct {
		Today         int64            `json:"today"`
		UnitsToday    int64            `json:"units_today"`
		UnitsMonth    int64            `json:"units_month"`
		RateRemaining *int64           `json:"rate_remaining"`
		LimitedToday  int64            `json:"limited_today"`
		LimitedDaily  []int64          `json:"limited_daily"`
		LimitedScopes map[string]int64 `json:"limited_scopes"`
		Recent        []keys.Refusal   `json:"recent_limited"`
	} `json:"usage"`
}

func getKey(t *testing.T, e *env, c *http.Client, id int64) keyResp {
	t.Helper()
	r, b := e.do(c, "GET", "/admin/api/keys/"+strconv.FormatInt(id, 10), nil, nil)
	if r.StatusCode != 200 {
		t.Fatalf("get key: %d %s", r.StatusCode, b)
	}
	var k keyResp
	if err := json.Unmarshal(b, &k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRateLimitResponses(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	id, secret := e.createKey(c, map[string]any{"label": "app", "rps": 1, "burst": 3})
	tile := func(hdr map[string]string) *http.Response {
		r, _ := e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+secret, nil, hdr)
		return r
	}
	// Allowed requests say where the client stands.
	r := tile(nil)
	if r.StatusCode != 200 || r.Header.Get("X-RateLimit-Limit") != "3" || r.Header.Get("X-RateLimit-Remaining") != "2" ||
		r.Header.Get("X-RateLimit-Reset") == "" || r.Header.Get("X-RateLimit-Scope") != "" || r.Header.Get("Retry-After") != "" {
		t.Fatalf("first request: %d %v", r.StatusCode, r.Header)
	}
	tile(nil)
	tile(nil)
	// The fourth is refused, with everything a client needs to back off.
	resp, body := e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+secret, nil, map[string]string{"Origin": "https://app.example.com"})
	if resp.StatusCode != 429 {
		t.Fatalf("over the rate: %d %s", resp.StatusCode, body)
	}
	if ra, _ := strconv.Atoi(resp.Header.Get("Retry-After")); ra < 1 || ra > 2 {
		t.Errorf("Retry-After %q, want 1 (whole seconds, never 0)", resp.Header.Get("Retry-After"))
	}
	if resp.Header.Get("X-RateLimit-Limit") != "3" || resp.Header.Get("X-RateLimit-Remaining") != "0" || resp.Header.Get("X-RateLimit-Scope") != "rate" {
		t.Errorf("429 headers: %v", resp.Header)
	}
	var er struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(body, &er)
	if er.Error.Code != "rate_limited" || !strings.Contains(er.Error.Message, "rate limit") {
		t.Errorf("429 body: %s", body)
	}
	// A browser on another origin can read them.
	if x := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(x, "Retry-After") || !strings.Contains(x, "X-RateLimit-Remaining") {
		t.Errorf("exposed headers: %q", x)
	}

	// The Keys page sees the refusal: counted today, by which limit, with the last ones listed.
	k := getKey(t, e, c, id)
	if k.Usage.LimitedToday != 1 || k.Usage.LimitedScopes["rate"] != 1 || k.Usage.LimitedDaily[13] != 1 ||
		len(k.Usage.Recent) != 1 || k.Usage.Recent[0].Kind != "tile" || k.Usage.Recent[0].Scope != "rate" {
		t.Errorf("usage after a refusal: %+v", k.Usage)
	}
	if k.Usage.UnitsToday != 3 || k.Usage.UnitsMonth != 3 || k.Usage.RateRemaining == nil || *k.Usage.RateRemaining != 0 {
		t.Errorf("units: %+v", k.Usage)
	}
	// The overview's traffic counts it as refused, not failed.
	_, b := e.do(c, "GET", "/admin/api/overview", nil, nil)
	if !strings.Contains(string(b), `"rejected":1`) {
		t.Errorf("overview: %s", b)
	}

	// A daily quota: a different code, and a wait until midnight UTC.
	_, quota := e.createKey(c, map[string]any{"label": "trial", "daily_quota": 2})
	for range 2 {
		e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+quota, nil, nil)
	}
	resp, body = e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+quota, nil, nil)
	_ = json.Unmarshal(body, &er)
	if ra, _ := strconv.Atoi(resp.Header.Get("Retry-After")); resp.StatusCode != 429 || er.Error.Code != "quota_exceeded" ||
		resp.Header.Get("X-RateLimit-Scope") != "daily" || ra < 1 || ra > 86400 {
		t.Errorf("over the daily quota: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	// A key without limits gets no rate headers at all.
	_, free := e.createKey(c, map[string]any{"label": "free"})
	if r, _ := e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+free, nil, nil); r.StatusCode != 200 || r.Header.Get("X-RateLimit-Limit") != "" {
		t.Errorf("unlimited key: %d %v", r.StatusCode, r.Header)
	}
	// The admin UI's own requests (a session, no key) are never limited.
	for range 20 {
		if r, _ := e.do(c, "GET", "/tiles/basemap/0/0/0.mvt", nil, nil); r.StatusCode != 200 {
			t.Fatalf("admin session limited: %d", r.StatusCode)
		}
	}
}

// One abuser of a key that sits in a browser cannot use it all up. Clients are told apart by
// the address the proxy reports.
func TestPerClientLimitBehindAProxy(t *testing.T) {
	e := newEnv(t, true, func(d *Deps) { d.Auth.SetTrustProxy(true) })
	c := e.login()
	_, secret := e.createKey(c, map[string]any{"label": "web", "rps": 100, "burst": 100, "ip_rps": 1, "ip_burst": 2})
	get := func(ip string) *http.Response {
		r, _ := e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+secret, nil, map[string]string{"CF-Connecting-IP": ip})
		return r
	}
	get("203.0.113.9")
	get("203.0.113.9")
	if r := get("203.0.113.9"); r.StatusCode != 429 || r.Header.Get("X-RateLimit-Scope") != "ip" {
		t.Fatalf("the abuser's third request: %d %v", r.StatusCode, r.Header)
	}
	for _, ip := range []string{"198.51.100.7", "198.51.100.8", "2001:db8::1"} {
		if r := get(ip); r.StatusCode != 200 {
			t.Errorf("another client (%s): %d", ip, r.StatusCode)
		}
	}
}

func TestKeyLimitsAPI(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	o := map[string]string{"Origin": e.ts.URL}
	id, _ := e.createKey(c, map[string]any{"label": "full", "rps": 20, "burst": 50, "daily_quota": 100000,
		"monthly_quota": 2000000, "ip_rps": 5, "ip_burst": 15, "origins": []string{"https://app.example.com"}})
	k := getKey(t, e, c, id)
	l := k.Limits
	if l.RPS == nil || *l.RPS != 20 || *l.Burst != 50 || *l.DailyQuota != 100000 || *l.MonthlyQuota != 2000000 || *l.IPRPS != 5 || *l.IPBurst != 15 {
		t.Fatalf("limits as created: %+v", l)
	}
	// The older fields are derived for clients that read them.
	if l.RPM == nil || *l.RPM != 1200 || l.RPD == nil || *l.RPD != 100000 {
		t.Errorf("derived rpm/rpd: %v %v", l.RPM, l.RPD)
	}
	// The older fields still work on write.
	old, _ := e.createKey(c, map[string]any{"label": "old client", "rpm": 300, "rpd": 5000})
	l = getKey(t, e, c, old).Limits
	if l.RPS == nil || *l.RPS != 5 || *l.Burst != 300 || *l.DailyQuota != 5000 || l.MonthlyQuota != nil || l.IPRPS != nil {
		t.Errorf("rpm 300, rpd 5000: %+v", l)
	}
	// A rate without a burst gets a default one.
	auto, _ := e.createKey(c, map[string]any{"label": "auto", "rps": 50})
	if l := getKey(t, e, c, auto).Limits; *l.Burst != 100 {
		t.Errorf("default burst: %d", *l.Burst)
	}
	// PATCH changes what it names; null removes a limit; the rest is untouched.
	path := "/admin/api/keys/" + strconv.FormatInt(id, 10)
	r, b := e.do(c, "PATCH", path, map[string]any{"rps": 2, "monthly_quota": nil}, o)
	if r.StatusCode != 200 {
		t.Fatalf("patch: %d %s", r.StatusCode, b)
	}
	l = getKey(t, e, c, id).Limits
	if *l.RPS != 2 || *l.Burst != 10 || l.MonthlyQuota != nil || *l.DailyQuota != 100000 || *l.IPRPS != 5 {
		t.Errorf("after patch: %+v", l)
	}
	e.do(c, "PATCH", path, map[string]any{"rps": nil, "ip_rps": 0}, o)
	l = getKey(t, e, c, id).Limits
	if l.RPS != nil || l.Burst != nil || l.RPM != nil || l.IPRPS != nil || l.IPBurst != nil {
		t.Errorf("limits removed: %+v", l)
	}
	for name, bad := range map[string]map[string]any{
		"negative":       {"rps": -1},
		"text":           {"daily_quota": "lots"},
		"fractional":     {"burst": 2.5},
		"absurd":         {"monthly_quota": 1e15},
		"negative ip":    {"ip_rps": -0.5},
		"quota fraction": {"daily_quota": 10.5},
	} {
		if r, b := e.do(c, "PATCH", path, bad, o); r.StatusCode != 400 {
			t.Errorf("%s: %d %s, want 400", name, r.StatusCode, b)
		}
	}
}

func TestLimitsConfigAPI(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	o := map[string]string{"Origin": e.ts.URL}
	type cfgResp struct {
		Weights  map[string]int              `json:"weights"`
		Global   map[string]keys.GlobalLimit `json:"global"`
		Kinds    []string                    `json:"kinds"`
		Defaults keys.Config                 `json:"defaults"`
	}
	get := func() cfgResp {
		t.Helper()
		r, b := e.do(c, "GET", "/admin/api/limits", nil, nil)
		if r.StatusCode != 200 {
			t.Fatalf("limits: %d %s", r.StatusCode, b)
		}
		var out cfgResp
		_ = json.Unmarshal(b, &out)
		return out
	}
	def := get()
	if def.Weights["tile"] != 1 || def.Weights["raster"] != 10 || def.Global["raster"].RPS == 0 || len(def.Kinds) < 8 || def.Defaults.Weights["route"] != 10 {
		t.Fatalf("defaults: %+v", def)
	}
	if r, _ := e.do(nil, "GET", "/admin/api/limits", nil, nil); r.StatusCode != 401 {
		t.Errorf("limits without a session: %d", r.StatusCode)
	}
	// Make a vector tile cost 5 and cap tiles server-wide; the change applies at once.
	_, secret := e.createKey(c, map[string]any{"label": "k", "rps": 1, "burst": 10})
	r, b := e.do(c, "PUT", "/admin/api/limits", map[string]any{"weights": map[string]int{"tile": 5},
		"global": map[string]any{"tile": map[string]any{"rps": 1000, "burst": 2000}}}, o)
	if r.StatusCode != 200 {
		t.Fatalf("put limits: %d %s", r.StatusCode, b)
	}
	now := get()
	if now.Weights["tile"] != 5 || now.Weights["raster"] != 10 || now.Global["tile"].Burst != 2000 || len(now.Global) != 1 {
		t.Errorf("after put: %+v", now)
	}
	for i, want := range []int{200, 200, 429} {
		if r, _ := e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+secret, nil, nil); r.StatusCode != want {
			t.Errorf("tile %d at weight 5 on a burst of 10: %d, want %d", i+1, r.StatusCode, want)
		}
	}
	for name, bad := range map[string]map[string]any{
		"zero weight":  {"weights": map[string]int{"tile": 0}},
		"unknown kind": {"weights": map[string]int{"warp": 2}},
		"negative":     {"global": map[string]any{"route": map[string]any{"rps": -5}}},
	} {
		if r, _ := e.do(c, "PUT", "/admin/api/limits", bad, o); r.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", name, r.StatusCode)
		}
	}
	// A server-wide limit answers 429 too, and says it is the server's.
	_, free := e.createKey(c, map[string]any{"label": "free"})
	e.do(c, "PUT", "/admin/api/limits", map[string]any{"global": map[string]any{"tile": map[string]any{"rps": 1, "burst": 2}}}, o)
	e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+free, nil, nil)
	e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+free, nil, nil)
	r, b = e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+free, nil, nil)
	if r.StatusCode != 429 || r.Header.Get("X-RateLimit-Scope") != "global" || r.Header.Get("Retry-After") == "" || !strings.Contains(string(b), "server is at its limit") {
		t.Errorf("server limit: %d %v %s", r.StatusCode, r.Header, b)
	}
}
