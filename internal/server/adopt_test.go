package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/tiles"
)

func TestActivateGenerationEndpoint(t *testing.T) {
	var asked []string
	e := newEnv(t, true, func(d *Deps) {
		d.ActivateGeneration = func(_ context.Context, id string) (Activation, error) {
			asked = append(asked, id)
			switch id {
			case "g2":
				prev := "g1"
				return Activation{ID: id, Previous: &prev, Removed: []string{"g0"}}, nil
			case "g1":
				return Activation{ID: id, AlreadyLive: true}, nil
			case "partial":
				return Activation{}, &ActivateError{Status: http.StatusUnprocessableEntity, Code: "invalid_generation",
					Message: "generation partial is incomplete or invalid: valhalla/tiles.tar is missing", Problems: []string{"valhalla/tiles.tar is missing"}}
			case "building":
				return Activation{}, &ActivateError{Status: http.StatusConflict, Code: "busy", Message: "a build is running"}
			}
			return Activation{}, &ActivateError{Status: http.StatusNotFound, Code: "no_generation", Message: "no such generation"}
		}
	})
	o := map[string]string{"Origin": e.ts.URL}

	// Admin only: no session, and a tile key, are both turned away before anything is asked.
	if r, _ := e.do(nil, "POST", "/admin/api/generations/g2/activate", nil, o); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a session: %d", r.StatusCode)
	}
	c := e.login()
	_, secret := e.createKey(c, map[string]any{"label": "tiles"})
	if r, _ := e.do(nil, "POST", "/admin/api/generations/g2/activate?key="+secret, nil, nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("with an API key: %d", r.StatusCode)
	}
	if len(asked) != 0 {
		t.Fatalf("activation reached without admin auth: %v", asked)
	}

	r, b := e.do(c, "POST", "/admin/api/generations/g2/activate", nil, o)
	var ok struct {
		ID          string   `json:"id"`
		Status      string   `json:"status"`
		AlreadyLive bool     `json:"already_live"`
		Previous    *string  `json:"previous"`
		Removed     []string `json:"removed"`
	}
	_ = json.Unmarshal(b, &ok)
	if r.StatusCode != 200 || ok.ID != "g2" || ok.Status != "live" || ok.AlreadyLive || ok.Previous == nil || *ok.Previous != "g1" || len(ok.Removed) != 1 {
		t.Fatalf("activate: %d %s", r.StatusCode, b)
	}
	r, b = e.do(c, "POST", "/admin/api/generations/g1/activate", nil, o)
	if r.StatusCode != 200 || !strings.Contains(string(b), `"already_live":true`) || !strings.Contains(string(b), `"removed":[]`) || !strings.Contains(string(b), `"previous":null`) {
		t.Fatalf("activate the live one: %d %s", r.StatusCode, b)
	}

	var fail struct {
		Error struct {
			Code     string   `json:"code"`
			Message  string   `json:"message"`
			Problems []string `json:"problems"`
		} `json:"error"`
	}
	r, b = e.do(c, "POST", "/admin/api/generations/partial/activate", nil, o)
	_ = json.Unmarshal(b, &fail)
	if r.StatusCode != 422 || fail.Error.Code != "invalid_generation" || len(fail.Error.Problems) != 1 || !strings.Contains(fail.Error.Message, "tiles.tar is missing") {
		t.Fatalf("invalid: %d %s", r.StatusCode, b)
	}
	if r, b = e.do(c, "POST", "/admin/api/generations/building/activate", nil, o); r.StatusCode != 409 || !strings.Contains(string(b), `"busy"`) {
		t.Fatalf("busy: %d %s", r.StatusCode, b)
	}
	if r, b = e.do(c, "POST", "/admin/api/generations/nope/activate", nil, o); r.StatusCode != 404 || !strings.Contains(string(b), `"no_generation"`) {
		t.Fatalf("unknown: %d %s", r.StatusCode, b)
	}
	if r, _ = e.do(c, "GET", "/admin/api/generations/g2/activate", nil, nil); r.StatusCode != 404 {
		t.Fatalf("GET activate: %d", r.StatusCode)
	}

	// A server wired without the pipeline says so rather than pretending.
	bare := newEnv(t, true)
	if r, b := bare.do(bare.login(), "POST", "/admin/api/generations/g1/activate", nil, map[string]string{"Origin": bare.ts.URL}); r.StatusCode != 503 {
		t.Fatalf("no activation wired: %d %s", r.StatusCode, b)
	}
}

// A generation copied into place is listed as soon as it is there; one still arriving under a
// dot name is not.
func TestGenerationsListsCopiedOnes(t *testing.T) {
	e := newEnv(t, true)
	gens := filepath.Join(e.dir, "generations")
	for _, name := range []string{"g2", ".g3.partial"} {
		dir := filepath.Join(gens, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(dir, "basemap.pmtiles"))
		id := strings.Trim(strings.TrimSuffix(name, ".partial"), ".")
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"generation":"`+id+`","ok":true,"created_at":"2026-11-09T00:00:00Z","regions":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, b := e.do(e.login(), "GET", "/admin/api/generations", nil, nil)
	var gl struct {
		Generations []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"generations"`
	}
	_ = json.Unmarshal(b, &gl)
	got := map[string]string{}
	for _, g := range gl.Generations {
		got[g.ID] = g.Status
	}
	if len(got) != 2 || got["g1"] != "live" || got["g2"] != "kept" {
		t.Fatalf("generations: %s", b)
	}
}

// slimTiles swaps in a tile server configured like a backend's.
func slimTiles(t *testing.T, opt tiles.Options, prepare func(dataDir string)) func(*Deps) {
	return func(d *Deps) {
		if prepare != nil {
			prepare(d.DataDir)
		}
		opt.DataDir, opt.Assets, opt.Stats = d.DataDir, testAssets, d.Stats
		opt.RasterCacheBytes, opt.ImageryCacheBytes, opt.PackCacheBytes = 1<<20, 1<<20, 1<<20
		ts, err := tiles.New(opt)
		if err != nil {
			t.Fatal(err)
		}
		d.Tiles = ts
	}
}

// MAPTHING_RENDERER=off: raster tiles say they are off (without calling a renderer), vector
// tiles and styles still work, and readiness has no renderer to wait for.
func TestRendererOff(t *testing.T) {
	e := newEnv(t, true, slimTiles(t, tiles.Options{RendererOff: true, RendererURL: "http://127.0.0.1:1"}, nil),
		func(d *Deps) {
			d.Info = map[string]any{"renderer": "off", "serve": []string{"tiles", "routing", "elevation"}}
		})
	in := map[string]string{"X-Mapthing-Internal": token}
	for _, p := range []string{"/styles/outdoors/256/4/2/1.png", "/styles/v1/mapthing/outdoors/tiles/512/3/2/1@2x", "/styles/v1/mapthing/outdoors/tiles/3/2/1"} {
		r, b := e.do(nil, "GET", p, nil, in)
		if r.StatusCode != http.StatusNotFound || !strings.Contains(string(b), `"renderer_off"`) || !strings.Contains(string(b), "MAPTHING_RENDERER=off") {
			t.Errorf("%s: %d %s", p, r.StatusCode, b)
		}
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt", nil, in); r.StatusCode != 200 {
		t.Errorf("vector tile with the renderer off: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/styles/outdoors.json", nil, in); r.StatusCode != 200 {
		t.Errorf("style with the renderer off: %d", r.StatusCode)
	}
	r, b := e.do(nil, "GET", "/readyz", nil, nil)
	if r.StatusCode != 200 || strings.Contains(string(b), "renderer") {
		t.Errorf("readyz: %d %s", r.StatusCode, b)
	}
	_, b = e.do(e.login(), "GET", "/admin/api/system", nil, nil)
	if !strings.Contains(string(b), `"renderer":"off"`) || !strings.Contains(string(b), `"serve":["tiles","routing","elevation"]`) {
		t.Errorf("system does not report the slim configuration: %s", b)
	}
}

// A backend that serves no tiles is ready with a live generation that has no basemap, and
// says the tileset is not there rather than failing.
func TestReadyWithoutBasemap(t *testing.T) {
	dropBasemap := func(dataDir string) {
		if err := os.Remove(filepath.Join(dataDir, "generations", "g1", "basemap.pmtiles")); err != nil {
			t.Fatal(err)
		}
	}
	slim := newEnv(t, true, slimTiles(t, tiles.Options{RendererOff: true, BasemapOptional: true}, dropBasemap))
	if r, b := slim.do(nil, "GET", "/readyz", nil, nil); r.StatusCode != 200 || !strings.Contains(string(b), `"generation":true`) {
		t.Fatalf("slim backend readyz: %d %s", r.StatusCode, b)
	}
	r, b := slim.do(nil, "GET", "/tiles/basemap/0/0/0.mvt", nil, map[string]string{"X-Mapthing-Internal": token})
	if r.StatusCode != http.StatusNotFound || !strings.Contains(string(b), `"no_tileset"`) {
		t.Fatalf("basemap tile on a backend without one: %d %s", r.StatusCode, b)
	}
	// Without a generation it is still not ready.
	empty := newEnv(t, false, slimTiles(t, tiles.Options{RendererOff: true, BasemapOptional: true}, nil))
	if r, _ := empty.do(nil, "GET", "/readyz", nil, nil); r.StatusCode != 503 {
		t.Fatalf("slim backend with no generation: %d", r.StatusCode)
	}
	// A full server with the same data is not ready: it is missing what it serves.
	full := newEnv(t, true, slimTiles(t, tiles.Options{}, dropBasemap))
	if r, _ := full.do(nil, "GET", "/readyz", nil, nil); r.StatusCode != 503 {
		t.Fatalf("full server without a basemap: %d", r.StatusCode)
	}
}
