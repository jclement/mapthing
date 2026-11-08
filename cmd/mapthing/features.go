package main

import (
	"context"

	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/server"
	"github.com/jclement/mapthing/internal/store"
	"github.com/jclement/mapthing/internal/supervisor"
	"github.com/jclement/mapthing/internal/tiles"
)

// features wires the feature packages (build pipeline, catalog, routing, elevation) into the
// server. It is the one place that imports them, so each can merge independently.
type features struct {
	cfg     config.Config
	store   *store.Store
	runner  *jobs.Runner
	sup     *supervisor.Supervisor
	watcher *tiles.Watcher
	tiles   *tiles.Server
}

func (f *features) wire(ctx context.Context, d *server.Deps) error {
	_ = ctx
	// Swap hook for the build: after the pipeline moves the current symlink it calls
	// Options.Restart; this applies the swap at once (tiles reopen, children restart).
	restart := func(ctx context.Context) error {
		f.watcher.Check(ctx)
		return nil
	}
	_ = restart

	// Pipeline (merge): register the build job and the catalog, e.g.
	//
	//	b := build.New(build.Config{DataDir: f.cfg.DataDir, Store: f.store, Catalog: cat})
	//	f.runner.Register("build", func(ctx context.Context, j *jobs.Job) error {
	//		var p struct{ Refresh bool `json:"refresh"` }
	//		_ = j.Decode(&p)
	//		_, err := b.Run(ctx, j, build.Options{Refresh: p.Refresh, Restart: restart})
	//		return err
	//	})
	//	f.runner.Details = func(err error) (map[string]any, bool) {
	//		var se *build.StageError
	//		if errors.As(err, &se) {
	//			return map[string]any{"stage": se.Stage, "error": se.Err.Error(), "log_tail": se.LogTail}, true
	//		}
	//		return nil, false
	//	}
	//	d.Catalog = func(ctx context.Context) (any, error) { return cat.List(ctx) }
	//	d.ValidateRegion = cat.Validate
	//
	// Elevation (merge), as given by the elevation worker:
	//
	//	terrain := elevation.NewTerrainForData(f.cfg.DataDir)
	//	d.Mounts = append(d.Mounts, elevation.NewHandler(terrain))   // Mount(mux, wrap)
	//	cat.Coverage = elevation.CoverageIDs
	//	d.ElevationSources = func() any { return terrain.Sources() }
	//
	// Routing (merge): append its Mountable and readiness, e.g.
	//
	//	d.Mounts = append(d.Mounts, routing.NewHandlers(router))
	//	d.ReadyChecks = map[string]func(context.Context) bool{"routing": router.Ready}
	return nil
}
