//go:build integration

package routing

// Run against a live Valhalla serving the Delaware graph:
//
//	docker/valhalla/dev-build.sh && docker/valhalla/dev-serve.sh   (in its own terminal)
//	MAPTHING_VALHALLA_URL=http://127.0.0.1:8002 go test -tags integration -run Integration -v ./internal/routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func liveClient(t *testing.T) *Client {
	t.Helper()
	c := New(URLFromEnv())
	if !c.Ready(context.Background()) {
		t.Skipf("no Valhalla at %s", c.BaseURL)
	}
	return c
}

// Delaware: Georgetown to three farm tracks (OSM ways 17232106 compacted, 17229878 dirt,
// 17230913 private gravel), plus Wilmington to Dover on paved roads.
var delawareRoutes = []struct {
	name             string
	from, to         Location
	wantUnpavedAtEnd bool
}{
	{"wilmington-dover", Location{Lat: 39.7391, Lon: -75.5398}, Location{Lat: 39.1582, Lon: -75.5244}, false},
	{"georgetown-compacted-track", Location{Lat: 38.6867, Lon: -75.3770}, Location{Lat: 38.7561805, Lon: -75.6217861}, true},
	{"georgetown-dirt-track", Location{Lat: 38.6867, Lon: -75.3770}, Location{Lat: 38.5997761, Lon: -75.2479164}, true},
	{"georgetown-private-gravel-track", Location{Lat: 38.6867, Lon: -75.3770}, Location{Lat: 38.4717950, Lon: -75.2588452}, true},
}

func TestIntegrationDelawareRoutes(t *testing.T) {
	c := liveClient(t)
	for _, tc := range delawareRoutes {
		for _, profile := range []string{"auto", "truck"} {
			for _, avoid := range []bool{false, true} {
				rt, err := c.Route(context.Background(), Request{Locations: []Location{tc.from, tc.to}, Profile: profile, AvoidUnpaved: avoid})
				if err != nil {
					t.Errorf("%s %s avoid=%v: %v", tc.name, profile, avoid, err)
					continue
				}
				last := rt.Surfaces[len(rt.Surfaces)-1]
				t.Logf("%-32s %-5s avoid_unpaved=%-5v %6.1f km %5.0f s  surfaces %v  ends on %s",
					tc.name, profile, avoid, rt.DistanceM/1000, rt.DurationS, rt.SurfaceBreakdown, last.Raw)
				if tc.wantUnpavedAtEnd && last.Class == SurfacePaved {
					t.Errorf("%s: expected the route to end on the unpaved track", tc.name)
				}
				if rt.SurfaceBreakdown[SurfaceUnknown] > 0.05*rt.DistanceM {
					t.Errorf("%s: %.0f m of unknown surface", tc.name, rt.SurfaceBreakdown[SurfaceUnknown])
				}
			}
		}
	}
}

func TestIntegrationErrors(t *testing.T) {
	c := liveClient(t)
	_, err := c.Route(context.Background(), Request{Locations: []Location{{Lat: 39.1, Lon: -75.5}, {Lat: 38.5, Lon: -74.9, RadiusM: 500}}})
	re := asError(err)
	if re.Code != CodeNoRoad || re.Index != 1 {
		t.Errorf("sea point: %v (index %d)", err, re.Index)
	}
}

func TestIntegrationCompatEndpoints(t *testing.T) {
	c := liveClient(t)
	mux := http.NewServeMux()
	NewHandlers(c).Mount(mux, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(path, body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	for _, tc := range []struct{ path, body, wantKey string }{
		{"/ors/v2/directions/driving-hgv/geojson", `{"coordinates":[[-75.377,38.6867],[-75.2588452,38.471795]],"radiuses":[10000,10000],"extra_info":["surface"]}`, "features"},
		{"/ors/v2/matrix/driving-hgv", `{"locations":[[-75.5398,39.7391],[-75.5244,39.1582],[-75.377,38.6867]],"metrics":["distance","duration"]}`, "durations"},
		{"/ors/v2/snap/driving-hgv", `{"locations":[[-75.259,38.472]],"radius":10000}`, "locations"},
		{"/ors/v2/isochrones/driving-car", `{"locations":[[-75.377,38.6867]],"range":[600,1200]}`, "features"},
		{"/directions/v2:computeRoutes", `{"origin":{"location":{"latLng":{"latitude":39.7391,"longitude":-75.5398}}},"destination":{"location":{"latLng":{"latitude":39.1582,"longitude":-75.5244}}},"computeAlternativeRoutes":true}`, "routes"},
	} {
		status, m := post(tc.path, tc.body)
		if status != 200 || m[tc.wantKey] == nil {
			t.Errorf("%s: %d %v", tc.path, status, m)
		}
	}
	status, m := post("/ors/v2/directions/driving-hgv/geojson", `{"coordinates":[[-75.5,39.1],[-74.9,38.5]],"radiuses":[350,350]}`)
	if code := m["error"].(map[string]any)["code"]; status != 404 || code != 2010.0 {
		t.Errorf("ORS 2010: %d %v", status, m)
	}
}
