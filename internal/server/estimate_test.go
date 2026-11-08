package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// fakeEstimate stands in for the pipeline's cover planner: anything west of -103 (the TX/NM
// line) needs new-mexico too; boxes in the Atlantic are not covered.
func fakeEstimate(_ context.Context, bbox []float64, ring [][2]float64) (CustomEstimate, error) {
	if bbox[0] > -60 {
		return CustomEstimate{}, ErrNotCovered
	}
	est := CustomEstimate{ElevationSources: []string{"usgs-3dep-13"}}
	est.Parents = append(est.Parents, CatalogParent{ID: "north-america/us/texas", Name: "Texas", Bytes: 693 << 20})
	if bbox[0] < -103 {
		est.Parents = append(est.Parents, CatalogParent{ID: "north-america/us/new-mexico", Name: "New Mexico", Bytes: 130 << 20})
	}
	for _, p := range est.Parents {
		est.TotalBytes += p.Bytes
	}
	est.SingleBest = &CatalogParent{ID: "north-america/us", Name: "United States", Bytes: 11687 << 20}
	return est, nil
}

func TestCatalogEstimateAndCustomParents(t *testing.T) {
	e := newEnv(t, false, func(d *Deps) { d.EstimateCustom = fakeEstimate })
	c := e.login()

	resp, body := e.do(c, "POST", "/admin/api/catalog/estimate", map[string]any{"bbox": []float64{-104.5, 31.5, -102.5, 32.5}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("estimate: %d %s", resp.StatusCode, body)
	}
	var est CustomEstimate
	if err := json.Unmarshal(body, &est); err != nil {
		t.Fatal(err)
	}
	if len(est.Parents) != 2 || est.TotalBytes != (693+130)<<20 || est.SingleBest == nil {
		t.Errorf("estimate %+v", est)
	}
	// A polygon works too (bbox derived from the ring).
	resp, _ = e.do(c, "POST", "/admin/api/catalog/estimate", map[string]any{"polygon": [][2]float64{{-104, 31}, {-102, 31}, {-103, 32}}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("polygon estimate: %d", resp.StatusCode)
	}
	resp, body = e.do(c, "POST", "/admin/api/catalog/estimate", map[string]any{"bbox": []float64{-40, 0, -39, 1}}, nil)
	if resp.StatusCode != http.StatusBadRequest || !json.Valid(body) {
		t.Errorf("ocean: %d %s", resp.StatusCode, body)
	}
	resp, _ = e.do(c, "POST", "/admin/api/catalog/estimate", map[string]any{"bbox": []float64{1, 1, 0, 0}}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad bbox: %d", resp.StatusCode)
	}

	// Adding the custom region stores the parents and total, and the regions API shows them.
	resp, body = e.do(c, "POST", "/admin/api/regions", map[string]any{"name": "Permian", "bbox": []float64{-104.5, 31.5, -102.5, 32.5}}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add: %d %s", resp.StatusCode, body)
	}
	var rg regionJSON
	if err := json.Unmarshal(body, &rg); err != nil {
		t.Fatal(err)
	}
	if len(rg.Parents) != 2 || rg.ExtractBytes != (693+130)<<20 || rg.Parent != "north-america/us/texas,north-america/us/new-mexico" {
		t.Errorf("region %+v", rg)
	}
	resp, body = e.do(c, "POST", "/admin/api/regions", map[string]any{"name": "Atlantic", "bbox": []float64{-40, 0, -39, 1}}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("uncovered custom region accepted: %d %s", resp.StatusCode, body)
	}
}
