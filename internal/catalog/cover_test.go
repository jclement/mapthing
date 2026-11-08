package catalog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fixtureCatalog loads testdata/geofabrik-fixture.json: real Geofabrik boundaries (simplified to
// 0.01°) and real extract sizes for the states and provinces around the cross-border test areas,
// plus their us / canada / north-america parents.
func fixtureCatalog(t *testing.T) *Catalog {
	t.Helper()
	data, err := os.ReadFile("testdata/geofabrik-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "catalog"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog", "geofabrik-index-v1.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(dir)
	c.IndexURL = "http://127.0.0.1:1/unused"
	if err := c.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Features []struct {
			Properties struct {
				URLs map[string]string `json:"urls"`
				Size int64             `json:"size"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	c.sizes = map[string]sizeRec{}
	for _, f := range doc.Features {
		c.sizes[IDFromURL(f.Properties.URLs["pbf"])] = sizeRec{Size: f.Properties.Size, Checked: time.Now()}
	}
	return c
}

func TestPlanCrossBorder(t *testing.T) {
	c := fixtureCatalog(t)
	tests := []struct {
		name    string
		bbox    BBox
		parents []string
		single  string // cheapest single extract covering it
	}{
		{"Permian TX/NM", BBox{-104.5, 31.5, -102.5, 32.5},
			[]string{"north-america/us/new-mexico", "north-america/us/texas"}, "north-america/us"},
		{"Haynesville TX/LA", BBox{-94.5, 31.8, -93.3, 32.8},
			[]string{"north-america/us/louisiana", "north-america/us/texas"}, "north-america/us"},
		{"NE BC / AB (Peace River)", BBox{-120.6, 55.5, -119.4, 56.2},
			[]string{"north-america/canada/alberta", "north-america/canada/british-columbia/north-admreg"}, "north-america/canada"},
		{"Delaware/Maryland line", BBox{-75.9, 38.6, -75.6, 38.8},
			[]string{"north-america/us/delaware", "north-america/us/maryland"}, "north-america/us"},
		{"inside Texas", BBox{-100, 31, -99, 32},
			[]string{"north-america/us/texas"}, "north-america/us/texas"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, plan, err := c.CustomRegionPlan("area", &tt.bbox, nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			var sum int64
			for _, p := range plan.Parents {
				got = append(got, p.ID)
				sum += p.Bytes
				if p.Estimated {
					t.Errorf("%s size estimated; fixture has it", p.ID)
				}
			}
			if !reflect.DeepEqual(got, tt.parents) {
				t.Fatalf("parents %v, want %v", got, tt.parents)
			}
			if plan.TotalBytes != sum || r.SizeHint != sum || len(r.Custom.Parents) != len(got) {
				t.Errorf("total %d, sum %d, size hint %d, region parents %d", plan.TotalBytes, sum, r.SizeHint, len(r.Custom.Parents))
			}
			if plan.SingleBest == nil || plan.SingleBest.ID != tt.single {
				t.Errorf("single best %+v, want %s", plan.SingleBest, tt.single)
			}
			if len(got) > 1 && plan.TotalBytes*5 > plan.SingleBest.Bytes {
				t.Errorf("union %d MB is not much cheaper than %s %d MB", plan.TotalBytes>>20, plan.SingleBest.ID, plan.SingleBest.Bytes>>20)
			}
			t.Logf("%s: %s = %d MiB (single best %s %d MiB)", tt.name, plan.ParentIDs(), plan.TotalBytes>>20, plan.SingleBest.ID, plan.SingleBest.Bytes>>20)
		})
	}
}

func TestPlanPolygonAcrossRedRiver(t *testing.T) {
	c := fixtureCatalog(t)
	// An unclosed triangle across the Texas/Oklahoma line (Red River, ~34°N near -98°).
	poly := [][][2]float64{{{-98.6, 33.6}, {-97.6, 33.6}, {-98.1, 34.4}}}
	r, plan, err := c.CustomRegionPlan("red river", nil, poly)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.ParentIDs(); got != "north-america/us/oklahoma, north-america/us/texas" {
		t.Fatalf("parents %s", got)
	}
	if ring := r.Polygon[0]; ring[0] != ring[len(ring)-1] {
		t.Error("ring not closed")
	}
}

func TestPlanUnknownSizesAndErrors(t *testing.T) {
	c := fixtureCatalog(t)
	// Forget Maryland's size: it is estimated from its area and flagged.
	delete(c.sizes, "north-america/us/maryland")
	_, plan, err := c.CustomRegionPlan("x", &BBox{-75.9, 38.6, -75.6, 38.8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Estimated || !strings.Contains(plan.ParentIDs(), "maryland") {
		t.Errorf("plan %+v", plan)
	}
	if _, _, err := c.CustomRegionPlan("x", &BBox{-40, 0, -39, 1}, nil); !errors.Is(err, ErrNotCovered) {
		t.Errorf("open ocean: %v", err)
	}
	if _, _, err := c.CustomRegionPlan("x", nil, [][][2]float64{{{1, 1}, {2, 2}}}); err == nil {
		t.Error("two-point polygon accepted")
	}
}
