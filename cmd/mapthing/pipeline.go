package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jclement/mapthing/internal/build"
	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/elevation"
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
		// Extract sizes (HEAD requests, cached) feed the UI's disk estimates.
		var ids []string
		for _, e := range cat.Entries(catalog.Filter{}) {
			if e.PBFURL != "" {
				ids = append(ids, e.ID)
			}
		}
		if err := cat.FillSizes(ctx, ids); err != nil {
			slog.Warn("catalog extract sizes", "err", err)
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
		m, err := b.Run(ctx, j, build.Options{Regions: regions, Refresh: p.Refresh, SkipRefresh: skip, Restart: restart})
		if err == nil && m != nil {
			j.SetGeneration(m.Generation)
		}
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
		return catalogJSON(cat.Entries(catalog.Filter{})), nil
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

// catalogEntryJSON is a catalog entry in the admin API shape (docs/architecture.md).
type catalogEntryJSON struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Parent           *string   `json:"parent"`
	BBox             []float64 `json:"bbox"`
	ExtractBytes     int64     `json:"extract_bytes"` // 0 while unknown or for grouping nodes
	Downloadable     bool      `json:"downloadable"`  // has its own extract (selectable)
	ElevationSources []string  `json:"elevation_sources"`
}

// catalogJSON adapts the pipeline's catalog to {entries, elevation_sources}.
func catalogJSON(entries []catalog.Entry) map[string]any {
	out := make([]catalogEntryJSON, 0, len(entries))
	for _, e := range entries {
		ce := catalogEntryJSON{ID: e.ID, Name: e.Name, BBox: e.BBox[:], ExtractBytes: e.Size, Downloadable: e.PBFURL != "", ElevationSources: e.ElevationSources}
		if e.Parent != "" {
			p := e.Parent
			ce.Parent = &p
		}
		if ce.ElevationSources == nil {
			ce.ElevationSources = []string{}
		}
		out = append(out, ce)
	}
	sources := make([]elevation.SourceInfo, 0, len(elevation.KnownSources))
	for _, s := range elevation.KnownSources {
		sources = append(sources, s.SourceInfo)
	}
	return map[string]any{"entries": out, "elevation_sources": sources}
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
		r, err := storeRegion(cat, rg)
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

func storeRegion(cat *catalog.Catalog, rg *store.Region) (catalog.Region, error) {
	if rg.Kind != "custom" {
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
	return r, nil
}

// geojsonPolygon reads a GeoJSON Polygon (or the first polygon of a MultiPolygon, or a Feature
// wrapping either) into rings.
func geojsonPolygon(raw json.RawMessage) ([][][2]float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// The admin API stores a drawn polygon as a bare outer ring [[lon,lat],…].
	var ring [][2]float64
	if json.Unmarshal(raw, &ring) == nil && len(ring) > 0 {
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
