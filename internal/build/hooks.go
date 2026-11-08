package build

import (
	"context"
	"sync"

	"github.com/jclement/mapthing/internal/catalog"
)

// TerrainFunc builds terrain.pmtiles at outPath for the regions, using workDir for scratch.
// The elevation package's internal/build/terrain*.go registers one from init().
type TerrainFunc func(ctx context.Context, r Reporter, regions []catalog.Region, workDir, outPath string) error

// TerrainEstimateFunc estimates the disk bytes a terrain build needs (DEM cache + output).
type TerrainEstimateFunc func(regions []catalog.Region) int64

// ElevationCheckFunc verifies terrainPath answers at each lon/lat point.
type ElevationCheckFunc func(ctx context.Context, terrainPath string, pts [][2]float64) error

type hookSet struct {
	mu       sync.RWMutex
	terrainF TerrainFunc
	estimate TerrainEstimateFunc
	check    ElevationCheckFunc
}

var hooks hookSet

// RegisterTerrain installs the terrain builder.
func RegisterTerrain(f TerrainFunc) { hooks.mu.Lock(); hooks.terrainF = f; hooks.mu.Unlock() }

// RegisterTerrainEstimate installs the terrain disk estimate used by the guard.
func RegisterTerrainEstimate(f TerrainEstimateFunc) {
	hooks.mu.Lock()
	hooks.estimate = f
	hooks.mu.Unlock()
}

// RegisterElevationCheck installs the verify-stage elevation check.
func RegisterElevationCheck(f ElevationCheckFunc) {
	hooks.mu.Lock()
	hooks.check = f
	hooks.mu.Unlock()
}

func (h *hookSet) terrain() TerrainFunc {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.terrainF
}

func (h *hookSet) terrainEstimate() TerrainEstimateFunc {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.estimate
}

func (h *hookSet) elevationCheck() ElevationCheckFunc {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.check
}
