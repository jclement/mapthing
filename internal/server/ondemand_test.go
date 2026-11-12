package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/store"
)

// withOnDemand gives the test server an on-demand manager (no DEM host is ever contacted by
// these tests: nothing asks it for a tile).
func withOnDemand(t *testing.T) func(*Deps) {
	return func(d *Deps) {
		st := d.Store
		m, err := ondemand.NewManager(context.Background(), ondemand.ManagerOptions{DataDir: d.DataDir, Settings: st,
			Areas: func(ctx context.Context) ([]ondemand.Area, error) {
				rows, err := st.Regions(ctx)
				var out []ondemand.Area
				for _, r := range rows {
					if r.TerrainMode == store.TerrainOnDemand && len(r.BBox) == 4 {
						out = append(out, ondemand.Area{ID: r.ID, BBox: elevation.BBox{r.BBox[0], r.BBox[1], r.BBox[2], r.BBox[3]}})
					}
				}
				return out, err
			}})
		if err != nil {
			t.Fatal(err)
		}
		d.OnDemand = m
	}
}

func TestOnDemandAdminAPI(t *testing.T) {
	e := newEnv(t, true, withOnDemand(t))
	c := e.login()
	var st ondemand.Status
	get := func() {
		t.Helper()
		r, b := e.do(c, "GET", "/admin/api/ondemand", nil, nil)
		if r.StatusCode != 200 {
			t.Fatalf("status: %d %s", r.StatusCode, b)
		}
		st = ondemand.Status{}
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
	}
	// Off by default, with the default budget.
	get()
	if st.Enabled || st.Config.Outside != "off" || st.Config.CacheMB != ondemand.DefaultCacheMB || st.Cache.MaxBytes != ondemand.DefaultCacheMB*1_000_000 || len(st.Sources) != 4 {
		t.Fatalf("defaults: %+v", st)
	}
	// The overview says nothing about a feature that is off.
	r, b := e.do(c, "GET", "/admin/api/overview", nil, nil)
	var ov map[string]any
	_ = json.Unmarshal(b, &ov)
	if r.StatusCode != 200 || ov["ondemand"] != nil {
		t.Fatalf("overview with on demand off: %d %v", r.StatusCode, ov["ondemand"])
	}
	// Anonymous callers cannot read or change it.
	if r, _ := e.do(nil, "GET", "/admin/api/ondemand", nil, nil); r.StatusCode != 401 {
		t.Fatalf("anonymous: %d", r.StatusCode)
	}
	// Bad values are refused and change nothing.
	for _, body := range []map[string]any{{"outside": "everywhere"}, {"cache_mb": 1}} {
		if r, b := e.do(c, "PUT", "/admin/api/ondemand", body, nil); r.StatusCode != 400 {
			t.Fatalf("%v: %d %s", body, r.StatusCode, b)
		}
	}
	// Turn it on outside built regions with a smaller cache.
	if r, b := e.do(c, "PUT", "/admin/api/ondemand", map[string]any{"outside": "on_demand", "cache_mb": 256}, nil); r.StatusCode != 200 {
		t.Fatalf("put: %d %s", r.StatusCode, b)
	}
	get()
	if !st.Enabled || st.Config.Outside != "on_demand" || st.Cache.MaxBytes != 256_000_000 {
		t.Fatalf("after put: %+v", st)
	}
	_, b = e.do(c, "GET", "/admin/api/overview", nil, nil)
	ov = nil
	_ = json.Unmarshal(b, &ov)
	if ov["ondemand"] == nil {
		t.Fatal("overview does not show on demand once it is on")
	}
	// A partial update keeps the rest.
	if r, _ := e.do(c, "PUT", "/admin/api/ondemand", map[string]any{"outside": "off"}, nil); r.StatusCode != 200 {
		t.Fatalf("partial put: %d", r.StatusCode)
	}
	get()
	if st.Enabled || st.Config.CacheMB != 256 {
		t.Fatalf("after partial put: %+v", st)
	}
	if r, _ := e.do(c, "POST", "/admin/api/ondemand/clear", nil, nil); r.StatusCode != 200 {
		t.Fatalf("clear: %d", r.StatusCode)
	}
}

func TestRegionTerrainMode(t *testing.T) {
	e := newEnv(t, true, withOnDemand(t))
	c := e.login()
	type region struct {
		ID          string `json:"id"`
		TerrainMode string `json:"terrain_mode"`
	}
	add := func(body map[string]any, want int) region {
		t.Helper()
		r, b := e.do(c, "POST", "/admin/api/regions", body, nil)
		if r.StatusCode != want {
			t.Fatalf("add %v: %d %s", body, r.StatusCode, b)
		}
		var rg region
		_ = json.Unmarshal(b, &rg)
		return rg
	}
	// Prebuilt unless asked otherwise.
	if rg := add(map[string]any{"name": "Built", "bbox": []float64{-76, 38, -75, 39}}, 201); rg.TerrainMode != "prebuilt" {
		t.Fatalf("default mode: %+v", rg)
	}
	rg := add(map[string]any{"name": "Lazy", "bbox": []float64{-99, 30, -98, 31}, "terrain_mode": "on_demand"}, 201)
	if rg.TerrainMode != "on_demand" {
		t.Fatalf("on demand at creation: %+v", rg)
	}
	add(map[string]any{"name": "Bad", "bbox": []float64{-99, 30, -98, 31}, "terrain_mode": "sometimes"}, 400)

	status := func() ondemand.Status {
		t.Helper()
		_, b := e.do(c, "GET", "/admin/api/ondemand", nil, nil)
		var st ondemand.Status
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	// The region is where on demand applies, at once.
	if st := status(); !st.Enabled || len(st.Regions) != 1 || st.Regions[0] != rg.ID {
		t.Fatalf("after adding an on-demand region: %+v", st)
	}
	if p := e.srv.d.OnDemand.Svc.Policy(); !p.AllowsBox(elevation.BBox{-98.6, 30.4, -98.5, 30.5}) || p.AllowsBox(elevation.BBox{-75.6, 38.4, -75.5, 38.5}) {
		t.Fatal("on demand must apply inside the on-demand region and not inside the built one")
	}
	// Switching it back turns on demand off there; switching a built one turns it on.
	patch := func(id, mode string, want int) {
		t.Helper()
		if r, b := e.do(c, "PATCH", "/admin/api/regions/"+id, map[string]any{"terrain_mode": mode}, nil); r.StatusCode != want {
			t.Fatalf("patch %s %s: %d %s", id, mode, r.StatusCode, b)
		}
	}
	patch(rg.ID, "prebuilt", 200)
	if st := status(); st.Enabled {
		t.Fatalf("after switching back: %+v", st)
	}
	patch(rg.ID, "nope", 400)
	patch("custom/built", "on_demand", 200)
	if st := status(); len(st.Regions) != 1 || st.Regions[0] != "custom/built" {
		t.Fatalf("after switching the built region: %+v", st)
	}
	// The refresh setting is untouched by a terrain change.
	_, b := e.do(c, "GET", "/admin/api/regions/custom/built", nil, nil)
	var got struct {
		TerrainMode   string `json:"terrain_mode"`
		RefreshPolicy struct {
			Mode string `json:"mode"`
		} `json:"refresh_policy"`
	}
	_ = json.Unmarshal(b, &got)
	if got.TerrainMode != "on_demand" || got.RefreshPolicy.Mode != "default" {
		t.Fatalf("region after patch: %s", b)
	}
	// Removing the region removes it from the policy.
	if r, _ := e.do(c, "DELETE", "/admin/api/regions/custom/built", nil, nil); r.StatusCode != 204 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if st := status(); st.Enabled {
		t.Fatalf("after deleting the region: %+v", st)
	}
}

// coldMount is a keyed endpoint that does cheap work, or with ?cold=1 "reads from the DEM
// hosts": the surcharge every cold on-demand request pays.
type coldMount struct{ d *Deps }

func (m coldMount) Mount(mux *http.ServeMux, wrap func(kind string, h http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/test-elevation", wrap("elevation", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cold") != "" {
			if d, ok := m.d.Auth.Surcharge(r.Context(), keys.KindOnDemand); !ok {
				w.Header().Set("Retry-After", "1")
				writeError(w, r, http.StatusTooManyRequests, "rate_limited", d.Message())
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})))
}

// A request that has to read from the DEM hosts costs its key more than one answered from
// the cache, and the server caps such requests across all keys.
func TestColdOnDemandRequestsCostMore(t *testing.T) {
	e := newEnv(t, true, func(d *Deps) { d.Mounts = append(d.Mounts, coldMount{d}) })
	c := e.login()
	_, b := e.do(c, "GET", "/admin/api/limits", nil, nil)
	var lim struct {
		Weights map[string]int              `json:"weights"`
		Global  map[string]keys.GlobalLimit `json:"global"`
	}
	if err := json.Unmarshal(b, &lim); err != nil {
		t.Fatal(err)
	}
	warm, extra := lim.Weights["elevation"], lim.Weights["ondemand"]
	if extra <= warm || lim.Global["ondemand"].RPS <= 0 {
		t.Fatalf("defaults: a cold request must cost more than a warm one and be capped server-wide: weights %v, global %v", lim.Weights, lim.Global)
	}

	id, secret := e.createKey(c, map[string]any{"label": "app", "rps": 1, "burst": 2*warm + extra + warm})
	call := func(cold bool) int {
		t.Helper()
		p := "/api/v1/test-elevation?key=" + secret
		if cold {
			p += "&cold=1"
		}
		r, _ := e.do(nil, "GET", p, nil, nil)
		return r.StatusCode
	}
	if call(false) != 204 {
		t.Fatal("warm request refused")
	}
	if got := getKey(t, e, c, id).Usage.UnitsToday; got != int64(warm) {
		t.Fatalf("a warm request cost %d units, want %d", got, warm)
	}
	if call(true) != 204 {
		t.Fatal("cold request refused")
	}
	if got := getKey(t, e, c, id).Usage.UnitsToday; got != int64(2*warm+extra) {
		t.Fatalf("warm + cold cost %d units, want %d", got, 2*warm+extra)
	}
	// What is left pays for a warm request but not for a cold one: the cold one is refused
	// before any work, the warm one still goes through.
	if call(true) != 429 {
		t.Fatal("a cold request the key cannot pay for was let through")
	}
	if k := getKey(t, e, c, id); k.Usage.LimitedToday != 1 || k.Usage.LimitedScopes["rate"] != 1 {
		t.Fatalf("the refusal is not on the key's record: %+v", k.Usage)
	}

	// Server-wide: with the cap at one cold request, a second key's cold request waits too.
	lim.Global["ondemand"] = keys.GlobalLimit{RPS: 0.001, Burst: 1}
	if r, b := e.do(c, "PUT", "/admin/api/limits", map[string]any{"weights": lim.Weights, "global": lim.Global}, nil); r.StatusCode != 200 {
		t.Fatalf("put limits: %d %s", r.StatusCode, b)
	}
	_, s1 := e.createKey(c, map[string]any{"label": "one"})
	_, s2 := e.createKey(c, map[string]any{"label": "two"})
	if r, _ := e.do(nil, "GET", "/api/v1/test-elevation?cold=1&key="+s1, nil, nil); r.StatusCode != 204 {
		t.Fatalf("first cold request under the server cap: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/api/v1/test-elevation?cold=1&key="+s2, nil, nil); r.StatusCode != 429 {
		t.Fatalf("second cold request under a server cap of one: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/api/v1/test-elevation?key="+s2, nil, nil); r.StatusCode != 204 {
		t.Fatalf("a warm request is not held by the cold cap: %d", r.StatusCode)
	}
	// The admin session has no key to charge.
	if r, _ := e.do(c, "GET", "/api/v1/test-elevation?cold=1", nil, nil); r.StatusCode != 204 {
		t.Fatalf("admin session: %d", r.StatusCode)
	}
}
