package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jclement/mapthing/internal/build"
	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/scheduler"
	"github.com/jclement/mapthing/internal/server"
	"github.com/jclement/mapthing/internal/store"
)

// wirePipeline registers the build job and the region catalog.
func (f *features) wirePipeline(ctx context.Context, d *server.Deps, restart func(context.Context) error, coverage func(catalog.BBox) []string) *catalog.Catalog {
	cat := catalog.New(f.cfg.DataDir)
	cat.Coverage = coverage // set before the background load starts
	go func() {
		if err := cat.Ensure(ctx); err != nil {
			slog.Warn("region catalog not loaded yet", "err", err)
			return
		}
		// Extract sizes for the catalog UI, custom-region estimates and the disk guard.
		if err := cat.WarmSizes(ctx, catalog.Filter{}); err != nil {
			slog.Warn("catalog extract sizes not saved", "err", err)
		}
	}()
	b := build.New(f.cfg.DataDir)

	f.runner.Register(scheduler.BuildJob, func(ctx context.Context, j *jobs.Job) error {
		var p struct {
			Refresh bool `json:"refresh"`
		}
		_ = j.Decode(&p)
		regions, skip, err := f.buildRegions(ctx, cat)
		if err != nil {
			return &build.StageError{Stage: build.StageGuard, Err: err}
		}
		_, err = b.Run(ctx, j, build.Options{Regions: regions, Refresh: p.Refresh, SkipRefresh: skip, Restart: restart})
		return err
	})
	f.runner.Details = func(err error) (map[string]any, bool) {
		var se *build.StageError
		if errors.As(err, &se) {
			return map[string]any{"stage": se.Stage, "error": se.Err.Error(), "log_tail": se.LogTail}, true
		}
		return nil, false
	}
	d.Catalog = func(ctx context.Context) (any, error) {
		if err := cat.Ensure(ctx); err != nil {
			return nil, err
		}
		return cat.Entries(catalog.Filter{}), nil
	}
	d.ValidateRegion = func(ctx context.Context, id string) (server.CatalogRegion, error) {
		if err := cat.Ensure(ctx); err != nil {
			return server.CatalogRegion{}, err
		}
		_ = cat.FillSizes(ctx, []string{id})
		e, ok := cat.Get(id)
		if !ok {
			return server.CatalogRegion{}, fmt.Errorf("unknown region %q (ids look like north-america/us/texas)", id)
		}
		return server.CatalogRegion{Name: e.Name, Parent: e.Parent, BBox: e.BBox[:],
			ExtractBytes: e.Size, ElevationSources: e.ElevationSources}, nil
	}
	return cat
}

// buildRegions turns the installed regions into build regions.
func (f *features) buildRegions(ctx context.Context, cat *catalog.Catalog) ([]catalog.Region, map[string]bool, error) {
	if err := cat.Ensure(ctx); err != nil {
		return nil, nil, fmt.Errorf("region catalog: %w", err)
	}
	rows, err := f.store.Regions(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []catalog.Region
	skip := map[string]bool{}
	for _, rg := range rows {
		r, err := storeRegion(ctx, cat, rg)
		if err != nil {
			return nil, nil, fmt.Errorf("region %s: %w", rg.ID, err)
		}
		if !rg.Refresh {
			skip[r.ID] = true
		}
		out = append(out, r)
	}
	return out, skip, nil
}

func storeRegion(ctx context.Context, cat *catalog.Catalog, rg *store.Region) (catalog.Region, error) {
	if rg.Kind != "custom" {
		_ = cat.FillSizes(ctx, []string{rg.ID}) // real size for the disk guard (cached a week)
		return cat.Region(rg.ID)
	}
	var bbox *catalog.BBox
	if len(rg.BBox) == 4 {
		b := catalog.BBox{rg.BBox[0], rg.BBox[1], rg.BBox[2], rg.BBox[3]}
		bbox = &b
	}
	poly, err := geojsonPolygon(rg.Polygon)
	if err != nil {
		return catalog.Region{}, err
	}
	r, err := cat.CustomRegion(rg.Name, bbox, poly)
	if err != nil {
		return catalog.Region{}, err
	}
	r.ID = rg.ID
	if p := &r.Custom.Parent; p.ID != "" {
		_ = cat.FillSizes(ctx, []string{p.ID})
		if e, ok := cat.Get(p.ID); ok && e.Size > 0 {
			p.SizeHint = e.Size
		}
	}
	return r, nil
}

// geojsonPolygon reads a GeoJSON Polygon (or the first polygon of a MultiPolygon, or a Feature
// wrapping either) into rings.
func geojsonPolygon(raw json.RawMessage) ([][][2]float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// The admin API stores a drawn polygon as a bare outer ring [[lon,lat],…] (polygonRing).
	var ring [][2]float64
	if err := json.Unmarshal(raw, &ring); err == nil {
		if len(ring) < 3 {
			return nil, fmt.Errorf("polygon ring needs at least 3 points, got %d", len(ring))
		}
		return [][][2]float64{ring}, nil
	}
	var g struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
		Geometry    json.RawMessage `json:"geometry"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, err
	}
	switch g.Type {
	case "Feature":
		return geojsonPolygon(g.Geometry)
	case "Polygon":
		var p [][][2]float64
		return p, json.Unmarshal(g.Coordinates, &p)
	case "MultiPolygon":
		var mp [][][][2]float64
		if err := json.Unmarshal(g.Coordinates, &mp); err != nil || len(mp) == 0 {
			return nil, fmt.Errorf("empty multipolygon")
		}
		return mp[0], nil
	}
	return nil, fmt.Errorf("unsupported polygon type %q", g.Type)
}
