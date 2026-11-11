package main

import (
	"context"
	"sync/atomic"

	"github.com/jclement/mapthing/internal/build"
	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/routing"
	"github.com/jclement/mapthing/internal/scheduler"
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
	sched   *scheduler.Scheduler
	sup     *supervisor.Supervisor
	watcher *tiles.Watcher
	tiles   *tiles.Server

	valhalla *supervisor.BlueGreen
	// valhallaGen is the generation directory of the instance being started, for its test
	// route (during an adoption that is not yet the live generation).
	valhallaGen atomic.Value
	builder     *build.Builder
}

func (f *features) wire(ctx context.Context, d *server.Deps) error {
	// Swap hook for the build: after the pipeline moves the current symlink it calls
	// Options.Restart; this applies the swap at once (tiles reopen, children restart).
	restart := func(ctx context.Context) error {
		f.watcher.Check(ctx)
		return nil
	}
	// Pipeline: build job, catalog, region validation (cmd/mapthing/pipeline.go).
	f.wirePipeline(ctx, d, restart, func(b catalog.BBox) []string { return elevation.CoverageIDs(b) })

	d.ActivateGeneration = f.activateGeneration // adopt.go
	d.GenerationInventory = f.generationInventory
	d.DeleteGeneration = f.deleteGeneration

	// Routing (wired with the pipeline so the smoke test can route). A server that does not
	// serve routing (MAPTHING_SERVE) starts no Valhalla and mounts no routing API.
	router := routing.New(f.cfg.ValhallaURL)
	if f.cfg.Serve.Routing {
		f.valhalla = f.newValhalla(router) // blue/green across generation swaps (valhalla.go)
		d.Mounts = append(d.Mounts, routing.NewHandlers(router))
		if d.ReadyChecks == nil {
			d.ReadyChecks = map[string]func(context.Context) bool{}
		}
		d.ReadyChecks["routing"] = router.Ready
	}

	// (Original pipeline merge notes, now implemented in pipeline.go:)
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
	//	d.ValidateRegion = func(ctx context.Context, id string) (server.CatalogRegion, error) { … from cat … }
	//
	// Catalog (merge, with the pipeline): cat.Coverage = elevation.CoverageIDs
	//
	// Routing (merge): append its Mountable and readiness, e.g.
	//
	//	d.Mounts = append(d.Mounts, routing.NewHandlers(router))
	//	d.ReadyChecks = map[string]func(context.Context) bool{"routing": router.Ready}
	// Elevation: reads <data>/current/terrain.pmtiles and follows generation swaps.
	if f.cfg.Serve.Elevation {
		terrain := elevation.NewTerrainForData(f.cfg.DataDir)
		if f.cfg.Serve.Routing {
			terrain.SetStructureFinder(router) // bridges=deck profiles map-match through Valhalla
		}
		d.Mounts = append(d.Mounts, elevation.NewHandler(terrain))
		d.ElevationSources = func() any { return terrain.Sources() }
	}
	return nil
}
