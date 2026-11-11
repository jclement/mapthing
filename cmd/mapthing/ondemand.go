package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/jclement/mapthing/internal/auth"
	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/store"
)

// On-demand terrain (internal/ondemand) is wired here: its manager is made before the tile
// server (which serves its tiles) and handed to the elevation service and the admin API by
// features.wire.

// onDemandAreas lists the regions set to on demand, with their outlines when the catalog
// knows them (a Geofabrik extract's boundary) or they were drawn (a custom region).
type onDemandAreas struct {
	st      *store.Store
	dataDir string

	mu  sync.Mutex
	cat *catalog.Catalog // set once the pipeline is wired
}

func (a *onDemandAreas) setCatalog(c *catalog.Catalog) {
	a.mu.Lock()
	a.cat = c
	a.mu.Unlock()
}

func (a *onDemandAreas) list(ctx context.Context) ([]ondemand.Area, error) {
	rows, err := a.st.Regions(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	cat := a.cat
	a.mu.Unlock()
	// A region switched back to built terrain stays on demand until a build has made that
	// terrain: the live archive says which regions it holds only the low zooms of.
	lowOnly := map[string]bool{}
	var live struct {
		OnDemand []string `json:"on_demand"`
	}
	if b, err := os.ReadFile(filepath.Join(a.dataDir, "current", "terrain.pmtiles.json")); err == nil && json.Unmarshal(b, &live) == nil {
		for _, id := range live.OnDemand {
			lowOnly[id] = true
		}
	}
	var out []ondemand.Area
	for _, rg := range rows {
		if rg.TerrainMode != store.TerrainOnDemand && !lowOnly[rg.ID] {
			continue
		}
		area := ondemand.Area{ID: rg.ID}
		if len(rg.BBox) == 4 {
			area.BBox = elevation.BBox{rg.BBox[0], rg.BBox[1], rg.BBox[2], rg.BBox[3]}
		}
		if rg.Kind == "custom" {
			if rings, err := geojsonPolygon(rg.Polygon); err == nil {
				area.Rings = rings
			}
		} else if cat != nil {
			// Only what is already loaded: this runs on admin requests and must not wait
			// on Geofabrik. Until the catalog is in, the region's box stands in.
			if r, err := cat.Region(rg.ID); err == nil {
				area.Rings = r.Polygon
				if area.BBox == (elevation.BBox{}) {
					area.BBox = elevation.BBox(r.BBox)
				}
			}
		}
		if area.BBox == (elevation.BBox{}) {
			continue
		}
		out = append(out, area)
	}
	return out, nil
}

// newOnDemand makes the manager, or nil for a server that serves neither tiles nor elevation.
func newOnDemand(ctx context.Context, cfg config.Config, st *store.Store) (*ondemand.Manager, *onDemandAreas, error) {
	if !cfg.Serve.Tiles && !cfg.Serve.Elevation {
		return nil, nil, nil
	}
	areas := &onDemandAreas{st: st, dataDir: cfg.DataDir}
	m, err := ondemand.NewManager(ctx, ondemand.ManagerOptions{DataDir: cfg.DataDir, Settings: st, Areas: areas.list})
	return m, areas, err
}

// coldGate returns, for a request, the context its on-demand work runs under: the first time
// that work has to read from the DEM hosts, the request's key is charged one "ondemand"
// request on top of what it already paid, through the key's limits and the server's limit for
// that kind. A cached answer never gets that far.
func coldGate(au *auth.Auth) func(r *http.Request) context.Context {
	return func(r *http.Request) context.Context {
		ctx := r.Context()
		return ondemand.WithGate(ctx, func() error {
			d, ok := au.Surcharge(ctx, keys.KindOnDemand)
			if ok {
				return nil
			}
			return &ondemand.ErrLimited{RetryAfter: d.RetryAfter, Message: d.Message()}
		})
	}
}
