package routing

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func loadPoints(t *testing.T) [][2]float64 {
	t.Helper()
	b, err := os.ReadFile("testdata/valhalla_trace_structures_points.json")
	if err != nil {
		t.Fatal(err)
	}
	var pts [][2]float64
	if err := json.Unmarshal(b, &pts); err != nil {
		t.Fatal(err)
	}
	return pts
}

func TestStructuresRecorded(t *testing.T) {
	f, c := newFake(t)
	f.handlers["trace_attributes"] = func(body map[string]any) (int, string) {
		if body["shape_match"] != "map_snap" || body["encoded_polyline"] == nil {
			t.Errorf("unexpected request %v", body)
		}
		return 200, "valhalla_trace_structures.json"
	}
	pts := loadPoints(t)
	kinds, matched, err := c.Structures(context.Background(), pts)
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != len(pts) || matched < 0.99 {
		t.Fatalf("%d kinds for %d points, matched %.2f", len(kinds), len(pts), matched)
	}
	// The C&D Canal bridge: one contiguous run of bridge points in the middle.
	first, last, n := -1, -1, 0
	for i, k := range kinds {
		if k == StructureBridge {
			if first < 0 {
				first = i
			}
			last = i
			n++
		} else if k != "" {
			t.Errorf("point %d: kind %q", i, k)
		}
	}
	if n == 0 || first == 0 || last == len(kinds)-1 {
		t.Fatalf("bridge points %d (first %d, last %d of %d)", n, first, last, len(kinds))
	}
	// The bridged points lie over the canal (≈39.54–39.56 N on SR-1).
	if lat := pts[(first+last)/2][1]; lat < 39.535 || lat > 39.565 {
		t.Errorf("bridge midpoint at lat %v", lat)
	}
}

func TestStructureKinds(t *testing.T) {
	var tr vStructureTrace
	raw := `{"edges":[{"bridge":false},{"bridge":true},{"tunnel":true,"bridge":false}],
	 "matched_points":[{"type":"matched","edge_index":0},{"type":"interpolated","edge_index":1},
	  {"type":"unmatched"},{"type":"matched","edge_index":2},{"type":"matched","edge_index":7}]}`
	if err := json.Unmarshal([]byte(raw), &tr); err != nil {
		t.Fatal(err)
	}
	kinds, matched, err := structureKinds(tr, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", StructureBridge, "", StructureTunnel, ""}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kind %d = %q want %q", i, kinds[i], want[i])
		}
	}
	if matched != 0.6 { // the unmatched point and the out-of-range edge index do not count
		t.Errorf("matched = %v", matched)
	}
	if _, _, err := structureKinds(tr, 4); err == nil {
		t.Error("point count mismatch accepted")
	}
}

func TestStructuresUnavailable(t *testing.T) {
	f, c := newFake(t)
	f.handlers["trace_attributes"] = func(map[string]any) (int, string) {
		return 400, `{"error_code":443,"error":"Exact route match algorithm failed to find path","status_code":400,"status":"Bad Request"}`
	}
	if _, _, err := c.Structures(context.Background(), loadPoints(t)); err == nil {
		t.Error("Valhalla error not reported")
	}
}
