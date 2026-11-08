package routing

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"testing"
)

func TestRouteTrack(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	rt, err := c.Route(context.Background(), Request{
		Locations: []Location{{Lat: 38.690, Lon: -75.385}, {Lat: 38.4717950, Lon: -75.2588452}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(rt.DistanceM-30147) > 1 || math.Abs(rt.DurationS-1455.133) > 0.01 {
		t.Errorf("distance %.1f duration %.1f", rt.DistanceM, rt.DurationS)
	}
	if len(rt.Legs) != 1 || rt.Legs[0].From != 0 || rt.Legs[0].To != len(rt.Geometry)-1 {
		t.Fatalf("legs %+v (geometry %d)", rt.Legs, len(rt.Geometry))
	}
	if g := rt.SurfaceBreakdown[SurfaceGravel]; g < 700 || g > 900 {
		t.Errorf("gravel metres %.1f, want about 790 (the final track)", g)
	}
	var sum float64
	for _, v := range rt.SurfaceBreakdown {
		sum += v
	}
	if math.Abs(sum-rt.DistanceM) > 1 {
		t.Errorf("breakdown sums to %.1f, distance %.1f", sum, rt.DistanceM)
	}
	last := rt.Surfaces[len(rt.Surfaces)-1]
	if last.Raw != "gravel" || last.To != len(rt.Geometry)-1 {
		t.Errorf("last surface span %+v", last)
	}
	// Default costing keeps tracks usable with a moderate penalty.
	opts := f.last("route")["costing_options"].(map[string]any)["auto"].(map[string]any)
	if opts["use_tracks"] != defaultUseTracks || opts["exclude_unpaved"] != nil {
		t.Errorf("costing options %v", opts)
	}
	if f.last("trace_attributes")["shape_match"] != "edge_walk" {
		t.Error("surface lookup should edge-walk the route shape")
	}
}

func TestRouteOptions(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	_, err := c.Route(context.Background(), Request{
		Locations:    []Location{{Lat: 38.69, Lon: -75.385, RadiusM: 5000}, {Lat: 38.47, Lon: -75.25, RadiusM: -1}},
		Profile:      "truck",
		AvoidUnpaved: true, AvoidHighways: true, AvoidTolls: true, AvoidFerries: true, Shortest: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := f.last("route")
	if b["costing"] != "truck" {
		t.Errorf("costing %v", b["costing"])
	}
	o := b["costing_options"].(map[string]any)["truck"].(map[string]any)
	for k, want := range map[string]any{"use_tracks": 0.0, "exclude_unpaved": true, "use_highways": 0.0, "use_tolls": 0.0, "use_ferry": 0.0, "shortest": true} {
		if o[k] != want {
			t.Errorf("%s = %v, want %v", k, o[k], want)
		}
	}
	locs := b["locations"].([]any)
	if locs[0].(map[string]any)["search_cutoff"] != 5000.0 || locs[1].(map[string]any)["search_cutoff"] != float64(unlimitedRadiusM) {
		t.Errorf("search_cutoff not mapped from radius: %v", locs)
	}
}

func TestRouteMultiLeg(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_multi.json")
	f.on("trace_attributes", 400, `{"error_code":444,"error":"Map Match algorithm failed","status_code":400}`)
	rt, err := c.Route(context.Background(), Request{Profile: "truck", Locations: []Location{
		{Lat: 39.7391, Lon: -75.5398}, {Lat: 39.1582, Lon: -75.5244}, {Lat: 38.690, Lon: -75.385}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Legs) != 2 {
		t.Fatalf("legs %d", len(rt.Legs))
	}
	if rt.Legs[0].To != rt.Legs[1].From || rt.Legs[1].To != len(rt.Geometry)-1 {
		t.Errorf("legs do not tile the geometry: %d..%d, %d..%d of %d", rt.Legs[0].From, rt.Legs[0].To, rt.Legs[1].From, rt.Legs[1].To, len(rt.Geometry))
	}
	var legSum float64
	for _, l := range rt.Legs {
		legSum += l.DistanceM
		for _, m := range l.Maneuvers {
			if m.From < l.From || m.To > l.To {
				t.Errorf("maneuver %q indices %d..%d outside leg %d..%d", m.Instruction, m.From, m.To, l.From, l.To)
			}
		}
	}
	if math.Abs(legSum-rt.DistanceM) > 5 {
		t.Errorf("legs sum %.0f vs total %.0f", legSum, rt.DistanceM)
	}
	// Surface lookup failed: everything is unknown, the route still succeeds.
	if rt.SurfaceBreakdown[SurfaceUnknown] != rt.DistanceM || len(rt.SurfaceBreakdown) != 1 {
		t.Errorf("breakdown %v", rt.SurfaceBreakdown)
	}
}

func TestRouteAlternates(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_alternates.json")
	rs, err := c.Routes(context.Background(), Request{AlternatesMax: 2,
		Locations: []Location{{Lat: 39.7391, Lon: -75.5398}, {Lat: 39.6837, Lon: -75.7497}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("got %d routes, want alternates", len(rs))
	}
	if f.last("route")["alternates"] != 2.0 {
		t.Error("alternates not requested")
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		file   string
		status int
		code   string
		index  int
	}{
		{"valhalla_error_noedge.json", 400, CodeNoRoad, 1},
		{"valhalla_error_unconnected.json", 400, CodeNoRoute, -1},
		{"valhalla_error_nopath.json", 400, CodeNoRoute, -1},
		{`{"error_code":154,"error":"Path distance exceeds the max distance limit","status_code":400}`, 400, CodeLimit, -1},
		{`{"error_code":110,"error":"Insufficiently specified required parameter","status_code":400}`, 400, CodeBadRequest, -1},
		{`{"oops": "not a valhalla error"}`, 502, CodeUnavailable, -1},
	}
	for _, tc := range cases {
		t.Run(tc.code+tc.file[:8], func(t *testing.T) {
			f, c := newFake(t)
			f.on("route", tc.status, tc.file)
			// the second point is at sea, so the default fake locate reports it
			_, err := c.Route(context.Background(), Request{Locations: []Location{{Lat: 39.1, Lon: -75.5}, {Lat: 38.5, Lon: -74.9}}})
			var re *Error
			if !errors.As(err, &re) {
				t.Fatalf("error %v is not *Error", err)
			}
			if re.Code != tc.code || re.Index != tc.index {
				t.Errorf("got %s index %d, want %s index %d (%s)", re.Code, re.Index, tc.code, tc.index, re.Msg)
			}
		})
	}
}

func TestUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1")
	_, err := c.Route(context.Background(), Request{Locations: []Location{{Lat: 1, Lon: 1}, {Lat: 2, Lon: 2}}})
	var re *Error
	if !errors.As(err, &re) || re.Code != CodeUnavailable {
		t.Errorf("got %v", err)
	}
	if c.Ready(context.Background()) {
		t.Error("Ready on a dead port")
	}
}

func TestValidation(t *testing.T) {
	_, c := newFake(t)
	for _, r := range []Request{
		{Locations: []Location{{Lat: 1, Lon: 1}}},
		{Locations: []Location{{Lat: 91, Lon: 1}, {Lat: 1, Lon: 1}}},
		{Locations: []Location{{Lat: 1, Lon: 1}, {Lat: 1, Lon: 1}}, Profile: "bicycle"},
	} {
		_, err := c.Route(context.Background(), r)
		var re *Error
		if !errors.As(err, &re) || re.Code != CodeBadRequest {
			t.Errorf("%+v: got %v", r, err)
		}
	}
}

func TestLocateRecorded(t *testing.T) {
	f, c := newFake(t)
	f.on("locate", 200, "valhalla_locate.json")
	snaps, err := c.Locate(context.Background(), []Location{{Lat: 38.4720, Lon: -75.2590, RadiusM: 10000}, {Lat: 38.5, Lon: -74.9, RadiusM: 500}}, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if snaps[0] == nil || snaps[1] != nil {
		t.Fatalf("snaps %v %v", snaps[0], snaps[1])
	}
	if snaps[0].DistanceM < 50 || snaps[0].DistanceM > 200 {
		t.Errorf("snap distance %.1f", snaps[0].DistanceM)
	}
	// A tighter radius than the snap distance rejects it.
	snaps, _ = c.Locate(context.Background(), []Location{{Lat: 38.4720, Lon: -75.2590, RadiusM: 20}, {Lat: 38.5, Lon: -74.9}}, "auto")
	if snaps[0] != nil {
		t.Error("snap beyond radius accepted")
	}
}

func TestMatrixRecorded(t *testing.T) {
	f, c := newFake(t)
	f.on("sources_to_targets", 200, "valhalla_matrix.json")
	m, err := c.Matrix(context.Background(),
		[]Location{{Lat: 39.7391, Lon: -75.5398}, {Lat: 39.1582, Lon: -75.5244}},
		[]Location{{Lat: 38.690, Lon: -75.385}, {Lat: 39.1582, Lon: -75.5244}}, "driving-car")
	if err != nil {
		t.Fatal(err)
	}
	if d := m.Distances[0][0]; d == nil || math.Abs(*d-139442) > 1 {
		t.Errorf("distance[0][0] %v", d)
	}
	if d := m.Distances[1][1]; d == nil || *d != 0 {
		t.Errorf("distance[1][1] %v (same point)", d)
	}
}

func TestIsochroneRecorded(t *testing.T) {
	f, c := newFake(t)
	f.on("isochrone", 200, "valhalla_isochrone.json")
	cs, err := c.Isochrone(context.Background(), Location{Lat: 38.69, Lon: -75.385}, "auto", []float64{180, 360}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Value != 180 || cs[1].Value != 360 {
		t.Errorf("contours %v", []float64{cs[0].Value, cs[1].Value})
	}
	if got := f.last("isochrone")["contours"].([]any)[0].(map[string]any)["time"]; got != 3.0 {
		t.Errorf("contour sent as %v minutes", got)
	}
}

func TestSurfaceClass(t *testing.T) {
	for in, want := range map[string]string{
		"paved_smooth": SurfacePaved, "paved": SurfacePaved, "paved_rough": SurfacePaved,
		"compacted": SurfaceGravel, "gravel": SurfaceGravel, "dirt": SurfaceDirt, "path": SurfaceDirt,
		"impassable": SurfaceUnknown, "": SurfaceUnknown,
	} {
		if got := SurfaceClass(in); got != want {
			t.Errorf("%q → %s, want %s", in, got, want)
		}
	}
}

func TestURLFromEnv(t *testing.T) {
	t.Setenv("MAPTHING_VALHALLA_URL", "")
	_ = os.Unsetenv("MAPTHING_VALHALLA_URL")
	if URLFromEnv() != DefaultURL {
		t.Error("default URL")
	}
	t.Setenv("MAPTHING_VALHALLA_URL", "http://x:1")
	if URLFromEnv() != "http://x:1" {
		t.Error("env URL")
	}
	_ = http.StatusOK
}

func TestSurfaceFallbackToMapMatch(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_443.json")
	f.handlers["trace_attributes"] = func(body map[string]any) (int, string) {
		if body["shape_match"] == "edge_walk" {
			return 400, "valhalla_error_edgewalk.json"
		}
		return 200, "valhalla_trace_mapsnap.json"
	}
	rt, err := c.Route(context.Background(), Request{Locations: []Location{{Lat: 39.4473, Lon: -75.7163}, {Lat: 39.1582, Lon: -75.5244}}})
	if err != nil {
		t.Fatal(err)
	}
	if rt.SurfaceBreakdown[SurfacePaved] < 0.9*rt.DistanceM || rt.SurfaceBreakdown[SurfaceUnknown] != 0 {
		t.Errorf("breakdown %v of %.0f m", rt.SurfaceBreakdown, rt.DistanceM)
	}
	// No per-vertex spans from a map match: one unknown span covering the route.
	if len(rt.Surfaces) != 1 || rt.Surfaces[0].Class != SurfaceUnknown {
		t.Errorf("spans %+v", rt.Surfaces)
	}
	if f.last("trace_attributes")["shape_match"] != "map_snap" {
		t.Error("fallback did not map-match")
	}
}
