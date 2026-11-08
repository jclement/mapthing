package elevation

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
)

// valleyTerrain: flat 50 m along the equator, a 20 m-deep, ~1.1 km-wide valley
// between lon 0.020 and 0.030 (crossed on a bridge), then a real 40 m climb
// between lon 0.040 and 0.060.
type valleyTerrain struct{}

func (valleyTerrain) Sample(lon, lat float64) (Sample, error) {
	e := 50.0
	switch {
	case lon > 0.020 && lon < 0.030:
		e = 30
	case lon >= 0.040 && lon < 0.060:
		e = 50 + 40*(lon-0.040)/0.020
	case lon >= 0.060:
		e = 90
	}
	return Sample{Lon: lon, Lat: lat, Elevation: &e, Source: "test", Resolution: 10}, nil
}

type fakeFinder struct {
	kind    func(lon float64) string
	matched float64
	err     error
	calls   int
}

func (f *fakeFinder) Structures(_ context.Context, pts [][2]float64) ([]string, float64, error) {
	f.calls++
	if f.err != nil {
		return nil, 0, f.err
	}
	k := make([]string, len(pts))
	for i, p := range pts {
		k[i] = f.kind(p[0])
	}
	return k, f.matched, nil
}

func bridgeOverValley(lon float64) string {
	if lon >= 0.0195 && lon <= 0.0305 {
		return "bridge"
	}
	return ""
}

var valleyPath = []LonLat{{0, 0}, {0.08, 0}}

func TestDeckRemovesBridgeValley(t *testing.T) {
	ctx := context.Background()
	ground, err := profileFrom(ctx, valleyTerrain{}, nil, valleyPath, ProfileOptions{IntervalM: 30})
	if err != nil {
		t.Fatal(err)
	}
	if ground.Bridges != BridgesGround || ground.Warning != "" || len(ground.StructureSpans) != 0 {
		t.Errorf("default must be ground with no spans: %+v", ground.Bridges)
	}
	if math.Abs(ground.GainM-60) > 1 || math.Abs(ground.LossM-20) > 1 {
		t.Errorf("ground gain/loss %v/%v, want ≈60/20 (valley counted)", ground.GainM, ground.LossM)
	}
	f := &fakeFinder{kind: bridgeOverValley, matched: 1}
	deck, err := profileFrom(ctx, valleyTerrain{}, f, valleyPath, ProfileOptions{IntervalM: 30, Bridges: BridgesDeck})
	if err != nil {
		t.Fatal(err)
	}
	if deck.Bridges != BridgesDeck || deck.Warning != "" {
		t.Fatalf("deck: mode %q warning %q", deck.Bridges, deck.Warning)
	}
	if math.Abs(deck.GainM-40) > 0.5 || deck.LossM > 0.5 {
		t.Errorf("deck gain/loss %v/%v, want 40/0", deck.GainM, deck.LossM)
	}
	if math.Abs(deck.GainRawM-40) > 0.5 {
		t.Errorf("deck raw gain %v (raw is unsmoothed but deck-adjusted)", deck.GainRawM)
	}
	if len(deck.StructureSpans) != 1 || deck.StructureSpans[0].Kind != "bridge" {
		t.Fatalf("spans %+v", deck.StructureSpans)
	}
	sp := deck.StructureSpans[0]
	if sp.StartM < 2100 || sp.StartM > 2250 || sp.EndM < 3300 || sp.EndM > 3420 {
		t.Errorf("span %v–%v m, want ≈2170–3390", sp.StartM, sp.EndM)
	}
	for _, s := range deck.Samples {
		if s.Structure == "bridge" && (*s.Elevation < 49.99 || *s.Elevation > 50.01) {
			t.Fatalf("bridge sample at %v m: %v, want 50 (deck level)", s.DistanceM, *s.Elevation)
		}
	}
	if deck.MinM != 50 {
		t.Errorf("deck min %v, want 50 (valley floor gone)", deck.MinM)
	}
}

func TestDeckFallbacks(t *testing.T) {
	ctx := context.Background()
	for name, f := range map[string]StructureFinder{
		"no finder":   nil,
		"error":       &fakeFinder{err: errors.New("valhalla down")},
		"poor match":  &fakeFinder{kind: bridgeOverValley, matched: 0.3},
		"wrong count": wrongCount{},
	} {
		p, err := profileFrom(ctx, valleyTerrain{}, f, valleyPath, ProfileOptions{IntervalM: 30, Bridges: BridgesDeck})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Bridges != BridgesGround || !strings.Contains(p.Warning, "ground") || len(p.StructureSpans) != 0 {
			t.Errorf("%s: mode %q warning %q spans %d", name, p.Bridges, p.Warning, len(p.StructureSpans))
		}
		if math.Abs(p.GainM-60) > 1 {
			t.Errorf("%s: fallback gain %v, want ground's 60", name, p.GainM)
		}
	}
	if _, err := profileFrom(ctx, valleyTerrain{}, nil, valleyPath, ProfileOptions{Bridges: "air"}); err == nil {
		t.Error("unknown mode accepted")
	}
	// Ground mode never calls the finder.
	f := &fakeFinder{kind: bridgeOverValley, matched: 1}
	if _, err := profileFrom(ctx, valleyTerrain{}, f, valleyPath, ProfileOptions{Bridges: BridgesGround}); err != nil || f.calls != 0 {
		t.Errorf("ground mode called the finder %d times (%v)", f.calls, err)
	}
}

type wrongCount struct{}

func (wrongCount) Structures(context.Context, [][2]float64) ([]string, float64, error) {
	return []string{"bridge"}, 1, nil
}

// A structure touching the end of the path is held level with the last ground sample;
// adjacent bridge and tunnel runs become separate spans.
func TestDeckEdgesAndKinds(t *testing.T) {
	f := &fakeFinder{matched: 1, kind: func(lon float64) string {
		switch {
		case lon >= 0.0195 && lon <= 0.0250:
			return "bridge"
		case lon > 0.0250 && lon <= 0.0305:
			return "tunnel"
		case lon >= 0.075:
			return "bridge"
		}
		return ""
	}}
	p, err := profileFrom(context.Background(), valleyTerrain{}, f, valleyPath, ProfileOptions{IntervalM: 30, Bridges: BridgesDeck})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.StructureSpans) != 3 || p.StructureSpans[0].Kind != "bridge" || p.StructureSpans[1].Kind != "tunnel" || p.StructureSpans[2].Kind != "bridge" {
		t.Fatalf("spans %+v", p.StructureSpans)
	}
	last := p.Samples[len(p.Samples)-1]
	if last.Structure != "bridge" || *last.Elevation != 90 {
		t.Errorf("end sample %+v", last)
	}
}

func TestProfileHTTPBridges(t *testing.T) {
	mux, svc := newTestHandler(t)
	f := &fakeFinder{kind: func(float64) string { return "" }, matched: 1}
	svc.SetStructureFinder(f)
	post := func(js string) (*httptest.ResponseRecorder, map[string]any) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/elevation/profile", strings.NewReader(js)))
		var b map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &b)
		return rr, b
	}
	rr, b := post(`{"path":[[-104.0,32.0],[-103.95,32.0]]}`)
	if rr.Code != 200 || b["bridges"] != "ground" || f.calls != 0 {
		t.Errorf("default: %d %v calls %d", rr.Code, b["bridges"], f.calls)
	}
	rr, b = post(`{"path":[[-104.0,32.0],[-103.95,32.0]],"bridges":"deck"}`)
	if rr.Code != 200 || b["bridges"] != "deck" || f.calls != 1 || b["warning"] != nil {
		t.Errorf("deck: %d %v %v calls %d", rr.Code, b["bridges"], b["warning"], f.calls)
	}
	if rr, _ := post(`{"path":[[-104.0,32.0],[-103.95,32.0]],"bridges":"air"}`); rr.Code != 400 {
		t.Errorf("bad mode: %d", rr.Code)
	}
	// The Google-compatible endpoint stays ground (Google semantics).
	before := f.calls
	if rr, _ := get(t, mux, "/maps/api/elevation/json?path=32.0,-104.0|32.0,-103.95&samples=5"); rr.Code != 200 || f.calls != before {
		t.Errorf("google path used the finder (%d)", rr.Code)
	}
}
