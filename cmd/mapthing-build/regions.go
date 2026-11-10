package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jclement/mapthing/internal/catalog"
)

// fileRegion is one region of a -regions-file: the shape the admin API's /admin/api/regions
// returns (and POST accepts). Fields a build does not need (status, sizes, dates) are ignored.
type fileRegion struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	CatalogID *string         `json:"catalog_id"`
	BBox      []float64       `json:"bbox"`
	Polygon   json.RawMessage `json:"polygon"`
	Refresh   *bool           `json:"refresh"`
}

// parseRegionsFile reads {"regions":[…]} (a saved GET /admin/api/regions) or a bare array.
func parseRegionsFile(data []byte) ([]fileRegion, error) {
	var doc struct {
		Regions *[]fileRegion `json:"regions"`
	}
	var list []fileRegion
	if err := json.Unmarshal(data, &list); err != nil {
		if err2 := json.Unmarshal(data, &doc); err2 != nil {
			return nil, err2
		}
		if doc.Regions == nil {
			return nil, fmt.Errorf(`want {"regions":[…]} or an array of regions`)
		}
		list = *doc.Regions
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no regions in the file")
	}
	return list, nil
}

// polygonRing reads a polygon as the admin API gives it ([[lon,lat],…], the outer ring) or as a
// GeoJSON Polygon.
func polygonRing(raw json.RawMessage) ([][2]float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var ring [][2]float64
	if err := json.Unmarshal(raw, &ring); err == nil {
		return ring, nil
	}
	var g struct {
		Type        string         `json:"type"`
		Coordinates [][][2]float64 `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &g); err != nil || g.Type != "Polygon" || len(g.Coordinates) == 0 {
		return nil, fmt.Errorf("polygon must be [[lon,lat],…] or a GeoJSON Polygon")
	}
	return g.Coordinates[0], nil
}

// regionsFromFile resolves a regions file into build regions, the way the server resolves its
// installed regions: a region with a catalog_id (or with no area of its own) is that catalog
// extract; one with a bbox or polygon is a custom area clipped from the extracts covering it,
// and keeps its id. skip holds the ids whose "refresh" is false.
func regionsFromFile(ctx context.Context, cat *catalog.Catalog, path string) (regions []catalog.Region, skip map[string]bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	list, err := parseRegionsFile(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	skip = map[string]bool{}
	seen := map[string]bool{}
	for i, fr := range list {
		r, err := fileRegionToBuild(ctx, cat, fr)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: region %d (%s): %w", path, i+1, firstNonEmpty(fr.ID, fr.Name, "unnamed"), err)
		}
		if seen[r.ID] {
			return nil, nil, fmt.Errorf("%s: region %s is listed twice", path, r.ID)
		}
		seen[r.ID] = true
		if fr.Refresh != nil && !*fr.Refresh {
			skip[r.ID] = true
		}
		regions = append(regions, r)
	}
	return regions, skip, nil
}

func fileRegionToBuild(ctx context.Context, cat *catalog.Catalog, fr fileRegion) (catalog.Region, error) {
	ring, err := polygonRing(fr.Polygon)
	if err != nil {
		return catalog.Region{}, err
	}
	if len(ring) > 0 && len(ring) < 3 {
		return catalog.Region{}, fmt.Errorf("polygon needs at least 3 points, got %d", len(ring))
	}
	hasArea := len(fr.BBox) > 0 || len(ring) > 0
	if fr.CatalogID != nil || !hasArea {
		id := fr.ID
		if fr.CatalogID != nil && *fr.CatalogID != "" {
			id = *fr.CatalogID
		}
		if id == "" {
			return catalog.Region{}, fmt.Errorf("needs an id (a catalog extract) or a name with a bbox or polygon (a custom area)")
		}
		_ = cat.FillSizes(ctx, []string{id}) // real size for the disk guard; a failure leaves the estimate
		r, err := cat.Region(id)
		if err != nil {
			return catalog.Region{}, err
		}
		if e, ok := cat.Get(id); ok && e.Size > 0 {
			r.SizeHint = e.Size
		}
		return r, nil
	}
	name := strings.TrimSpace(fr.Name)
	if name == "" {
		name = strings.TrimPrefix(fr.ID, "custom/")
	}
	if name == "" {
		return catalog.Region{}, fmt.Errorf("a custom area needs a name")
	}
	var box *catalog.BBox
	var poly [][][2]float64
	switch {
	case len(ring) >= 3:
		poly = [][][2]float64{ring}
	case len(fr.BBox) == 4:
		box = &catalog.BBox{fr.BBox[0], fr.BBox[1], fr.BBox[2], fr.BBox[3]}
		if !box.Valid() {
			return catalog.Region{}, fmt.Errorf("bbox must be [w,s,e,n] with w<e and s<n")
		}
	default:
		return catalog.Region{}, fmt.Errorf("bbox must be [w,s,e,n]")
	}
	// Fill extract sizes first so the parents chosen match what the server would choose.
	if _, err := cat.PlanCustom(ctx, box, poly); err != nil {
		return catalog.Region{}, err
	}
	r, err := cat.CustomRegion(name, box, poly)
	if err != nil {
		return catalog.Region{}, err
	}
	if fr.ID != "" {
		r.ID = fr.ID
	}
	return r, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
