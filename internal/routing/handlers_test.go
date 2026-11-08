package routing

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const trackBody = `{"locations":[{"lat":38.690,"lon":-75.385},{"lat":38.4717950,"lon":-75.2588452}],"profile":"auto"}`

func TestNativeRoute(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	rec := do(t, c, "POST", "/api/v1/route", trackBody, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	golden(t, "native_route.json", rec.Body.Bytes())
	var feat struct {
		Type       string `json:"type"`
		Geometry   struct{ Coordinates [][2]float64 }
		Properties struct {
			DistanceM        float64            `json:"distance_m"`
			SurfaceBreakdown map[string]float64 `json:"surface_breakdown"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feat); err != nil {
		t.Fatal(err)
	}
	if feat.Type != "Feature" || len(feat.Geometry.Coordinates) < 10 || feat.Properties.SurfaceBreakdown["gravel"] == 0 {
		t.Errorf("unexpected feature: %+v", feat.Properties)
	}
}

func TestNativeErrors(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 400, "valhalla_error_noedge.json")
	rec := do(t, c, "POST", "/api/v1/route", `{"locations":[{"lat":39.1,"lon":-75.5},{"lat":38.5,"lon":-74.9}]}`, nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"no_road"`) || !strings.Contains(rec.Body.String(), `"location_index":1`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	rec = do(t, c, "POST", "/api/v1/route", `{"locations":`, nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad_request") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestPassthrough(t *testing.T) {
	f, c := newFake(t)
	f.on("status", 200, `{"version":"3.9.1"}`)
	f.on("route", 200, "valhalla_route_track.json")
	rec := do(t, c, "GET", "/api/v1/valhalla/status?key=mt_secret", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "3.9.1") {
		t.Errorf("status: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, c, "POST", "/api/v1/valhalla/route", `{"locations":[]}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"trip"`) {
		t.Errorf("route: %d", rec.Code)
	}
	if rec := do(t, c, "POST", "/api/v1/valhalla/tile", `{}`, nil); rec.Code != 400 {
		t.Errorf("non-allowlisted action: %d", rec.Code)
	}
	if rec := do(t, c, "DELETE", "/api/v1/valhalla/route", ``, nil); rec.Code != http.StatusMethodNotAllowed { // mux: GET/POST only
		t.Errorf("DELETE: %d", rec.Code)
	}
}

// ---- Google ----

const googleBody = `{
  "origin": {"location": {"latLng": {"latitude": 38.690, "longitude": -75.385}}},
  "destination": {"location": {"latLng": {"latitude": 38.4717950, "longitude": -75.2588452}}},
  "travelMode": "DRIVE",
  "polylineQuality": "HIGH_QUALITY",
  "routeModifiers": {"avoidTolls": true}
}`

func TestGoogleComputeRoutes(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	rec := do(t, c, "POST", "/directions/v2:computeRoutes", googleBody, map[string]string{"X-Goog-FieldMask": "*"})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	golden(t, "google_compute_routes.json", rec.Body.Bytes())
	o := f.last("route")["costing_options"].(map[string]any)["auto"].(map[string]any)
	if o["use_tolls"] != 0.0 {
		t.Error("avoidTolls not mapped")
	}
}

func TestGoogleFieldMask(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	rec := do(t, c, "POST", "/directions/v2:computeRoutes", googleBody,
		map[string]string{"X-Goog-FieldMask": "routes.duration, routes.distanceMeters,routes.polyline.encodedPolyline,routes.legs.startLocation"})
	golden(t, "google_compute_routes_masked.json", rec.Body.Bytes())
	var resp struct {
		Routes []map[string]json.RawMessage `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	r := resp.Routes[0]
	if len(r) != 4 || r["viewport"] != nil || r["duration"] == nil {
		t.Errorf("mask not applied: keys %v", keys(r))
	}
}

func TestGoogleGeoJSONOverview(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	body := strings.Replace(googleBody, `"HIGH_QUALITY"`, `"OVERVIEW", "polylineEncoding": "GEO_JSON_LINESTRING"`, 1)
	rec := do(t, c, "POST", "/directions/v2:computeRoutes?profile=truck", body, map[string]string{"X-Goog-FieldMask": "routes.polyline"})
	var resp struct {
		Routes []struct {
			Polyline struct {
				GeoJSONLinestring struct {
					Type        string       `json:"type"`
					Coordinates [][2]float64 `json:"coordinates"`
				} `json:"geoJsonLinestring"`
			} `json:"polyline"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	ls := resp.Routes[0].Polyline.GeoJSONLinestring
	if ls.Type != "LineString" || len(ls.Coordinates) < 2 || len(ls.Coordinates) > 400 {
		t.Errorf("overview linestring: %s %d points", ls.Type, len(ls.Coordinates))
	}
	if f.last("route")["costing"] != "truck" {
		t.Error("?profile=truck ignored")
	}
}

func TestGoogleErrors(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 400, "valhalla_error_nopath.json")
	rec := do(t, c, "POST", "/directions/v2:computeRoutes", googleBody, nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Errorf("no route: %d %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{
		"address": `{"origin":{"address":"1 Main St"},"destination":{"location":{"latLng":{"latitude":1,"longitude":1}}}}`,
		"mode":    strings.Replace(googleBody, `"DRIVE"`, `"WALK"`, 1),
		"missing": `{"origin":{"location":{"latLng":{"latitude":1,"longitude":1}}}}`,
		"json":    `{`,
	} {
		rec := do(t, c, "POST", "/directions/v2:computeRoutes", body, nil)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "INVALID_ARGUMENT") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestFieldMaskPaths(t *testing.T) {
	v := map[string]any{"routes": []any{
		map[string]any{"a": 1, "b": map[string]any{"c": 2, "d": 3}, "legs": []any{map[string]any{"x": 1, "y": 2}}},
	}}
	cases := map[string]string{
		"":                       `{"routes":[{"a":1,"b":{"c":2,"d":3},"legs":[{"x":1,"y":2}]}]}`,
		"routes.b.c":             `{"routes":[{"b":{"c":2}}]}`,
		"routes.b,routes.b.c":    `{"routes":[{"b":{"c":2,"d":3}}]}`,
		"routes.legs.y,routes.a": `{"routes":[{"a":1,"legs":[{"y":2}]}]}`,
		"routes.*":               `{"routes":[{"a":1,"b":{"c":2,"d":3},"legs":[{"x":1,"y":2}]}]}`,
		"nothing.here":           `{}`,
	}
	for mask, want := range cases {
		b, _ := json.Marshal(applyFieldMask(v, mask))
		if string(b) != want {
			t.Errorf("mask %q: got %s want %s", mask, b, want)
		}
	}
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
