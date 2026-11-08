package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

const orsTrackBody = `{"coordinates":[[-75.385,38.690],[-75.2588452,38.4717950]],"radiuses":[10000,10000],"extra_info":["surface"]}`

func TestORSDirectionsGeoJSON(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	rec := do(t, c, "POST", "/ors/v2/directions/driving-hgv/geojson", orsTrackBody, nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	golden(t, "ors_directions.geojson", rec.Body.Bytes())
	if f.last("route")["costing"] != "truck" {
		t.Error("driving-hgv should map to truck")
	}
	var fc struct {
		Features []struct {
			Properties struct {
				Summary   struct{ Distance, Duration float64 }
				WayPoints []int `json:"way_points"`
				Extras    struct {
					Surface struct {
						Values [][3]int `json:"values"`
					} `json:"surface"`
				} `json:"extras"`
			} `json:"properties"`
			Geometry struct{ Coordinates [][2]float64 }
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatal(err)
	}
	p := fc.Features[0].Properties
	n := len(fc.Features[0].Geometry.Coordinates)
	if p.Summary.Distance != 30147 || p.WayPoints[1] != n-1 {
		t.Errorf("summary %+v waypoints %v (n=%d)", p.Summary, p.WayPoints, n)
	}
	vals := p.Extras.Surface.Values
	if last := vals[len(vals)-1]; last[2] != 10 || last[1] != n-1 {
		t.Errorf("last surface run %v, want gravel (10) to the end", last)
	}
}

func TestORSDirectionsJSON(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	body := strings.Replace(orsTrackBody, `"extra_info":["surface"]`, `"units":"km","instructions":false,"options":{"avoid_features":["tollways","unpavedroads"]}`, 1)
	rec := do(t, c, "POST", "/ors/v2/directions/driving-car", body, nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	golden(t, "ors_directions.json", rec.Body.Bytes())
	o := f.last("route")["costing_options"].(map[string]any)["auto"].(map[string]any)
	if o["exclude_unpaved"] != true || o["use_tolls"] != 0.0 {
		t.Errorf("avoid_features not mapped: %v", o)
	}
	var resp struct {
		Routes []struct {
			Geometry string
			Summary  struct{ Distance float64 }
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Routes[0].Summary.Distance != 30.1 {
		t.Errorf("km distance %v", resp.Routes[0].Summary.Distance)
	}
	if pts, err := DecodePolyline(resp.Routes[0].Geometry, 5); err != nil || len(pts) < 10 {
		t.Errorf("geometry not a precision-5 polyline: %v", err)
	}
}

func TestORSDirectionsGET(t *testing.T) {
	f, c := newFake(t)
	f.on("route", 200, "valhalla_route_track.json")
	rec := do(t, c, "GET", "/ors/v2/directions/driving-car?start=-75.385,38.69&end=-75.2588452,38.471795", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"FeatureCollection"`) {
		t.Errorf("%d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
}

func TestORSErrors(t *testing.T) {
	cases := []struct {
		name, path, body, fixture string
		status, code              int
		msg                       string
	}{
		{"point not found", "/ors/v2/directions/driving-hgv/geojson",
			`{"coordinates":[[-75.5,39.1],[-74.9,38.5]],"radiuses":[350,500]}`, "valhalla_error_noedge.json",
			404, 2010, "radius of 500.0 meters of specified coordinate 1: -74.9000000 38.5000000"},
		{"route not found", "/ors/v2/directions/driving-hgv/geojson",
			`{"coordinates":[[-75.5,39.1],[-75.4,38.5]]}`, "valhalla_error_nopath.json", 404, 2009, "Route could not be found"},
		{"radius limit", "/ors/v2/directions/driving-hgv/geojson",
			`{"coordinates":[[-75.5,39.1],[-75.4,38.5]],"radiuses":[10001,-1]}`, "", 400, 2004, "server limit"},
		{"radius count", "/ors/v2/directions/driving-hgv",
			`{"coordinates":[[-75.5,39.1],[-75.4,38.5]],"radiuses":[1]}`, "", 400, 2003, "radiuses"},
		{"profile", "/ors/v2/directions/cycling-regular/geojson",
			`{"coordinates":[[-75.5,39.1],[-75.4,38.5]]}`, "", 400, 2003, "profile"},
		{"json", "/ors/v2/directions/driving-car/geojson", `{`, "", 400, 2000, "parse"},
		{"one point", "/ors/v2/directions/driving-car/geojson", `{"coordinates":[[-75.5,39.1]]}`, "", 400, 2001, "two points"},
		{"matrix point", "/ors/v2/matrix/driving-car", `{"locations":[[-75.5,39.1],[-74.9,38.5]]}`, "", 404, 6010, "coordinate 1"},
		{"matrix metric", "/ors/v2/matrix/driving-car", `{"locations":[[-75.5,39.1],[-75.4,38.5]],"metrics":["weight"]}`, "", 400, 6003, "metrics"},
		{"iso range", "/ors/v2/isochrones/driving-car", `{"locations":[[-75.5,39.1]],"range":[7201]}`, "", 400, 3004, "out of bounds"},
		{"iso ranges", "/ors/v2/isochrones/driving-car", `{"locations":[[-75.5,39.1]],"range":[3600],"interval":600}`, "", 400, 3004, "ranges"},
		{"snap radius", "/ors/v2/snap/driving-car", `{"locations":[[-75.5,39.1]]}`, "", 400, 8001, "radius"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t)
			if tc.fixture != "" {
				f.on("route", 400, tc.fixture)
			}
			rec := do(t, c, "POST", tc.path, tc.body, nil)
			var e struct {
				Error struct {
					Code    int
					Message string
				}
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			if rec.Code != tc.status || e.Error.Code != tc.code || !strings.Contains(e.Error.Message, tc.msg) {
				t.Errorf("got %d %d %q; want %d %d containing %q", rec.Code, e.Error.Code, e.Error.Message, tc.status, tc.code, tc.msg)
			}
		})
	}
}

func TestORSMatrix(t *testing.T) {
	f, c := newFake(t)
	f.on("sources_to_targets", 200, "valhalla_matrix.json")
	body := `{"locations":[[-75.5398,39.7391],[-75.5244,39.1582],[-75.385,38.690]],"sources":[0,1],"destinations":["2","1"],"metrics":["distance","duration"],"units":"km"}`
	rec := do(t, c, "POST", "/ors/v2/matrix/driving-hgv", body, nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	golden(t, "ors_matrix.json", rec.Body.Bytes())
	b := f.last("sources_to_targets")
	if len(b["sources"].([]any)) != 2 || len(b["targets"].([]any)) != 2 || b["costing"] != "truck" {
		t.Errorf("matrix request %v", b)
	}
}

func TestORSSnap(t *testing.T) {
	_, c := newFake(t)
	rec := do(t, c, "POST", "/ors/v2/snap/driving-car", `{"locations":[[-75.5,39.1],[-74.9,38.5]],"radius":350}`, nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	golden(t, "ors_snap.json", rec.Body.Bytes())
}

func TestORSIsochrones(t *testing.T) {
	f, c := newFake(t)
	f.on("isochrone", 200, "valhalla_isochrone.json")
	rec := do(t, c, "POST", "/ors/v2/isochrones/driving-car", `{"locations":[[-75.385,38.69]],"range":[360],"interval":180,"smoothing":25}`, nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var fc struct {
		Features []struct {
			Properties struct {
				GroupIndex int       `json:"group_index"`
				Value      float64   `json:"value"`
				Center     []float64 `json:"center"`
			} `json:"properties"`
			Geometry struct{ Type string }
		} `json:"features"`
		BBox []float64 `json:"bbox"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatal(err)
	}
	if len(fc.Features) != 2 || fc.Features[0].Properties.Value != 180 || fc.Features[1].Properties.Value != 360 || fc.Features[0].Geometry.Type != "Polygon" {
		t.Errorf("features %+v", fc.Features)
	}
	if len(fc.BBox) != 4 || fc.BBox[0] >= fc.BBox[2] {
		t.Errorf("bbox %v", fc.BBox)
	}
	contours := f.last("isochrone")["contours"].([]any)
	if contours[0].(map[string]any)["time"] != 3.0 || contours[1].(map[string]any)["time"] != 6.0 {
		t.Errorf("contours %v", contours)
	}
	// distance ranges in km go to Valhalla as km
	_ = do(t, c, "POST", "/ors/v2/isochrones/driving-car", `{"locations":[[-75.385,38.69]],"range":[5],"range_type":"distance","units":"km"}`, nil)
	if d := f.last("isochrone")["contours"].([]any)[0].(map[string]any)["distance"]; d != 5.0 {
		t.Errorf("distance contour %v", d)
	}
}
