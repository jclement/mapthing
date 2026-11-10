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

// ContoursFunc builds contours.pmtiles at outPath from the terrain archive at terrainPath.
// internal/build/contours.go registers one from init(); the terrain stage runs it after a
// successful terrain build.
type ContoursFunc func(ctx context.Context, r Reporter, terrainPath, outPath string) error

// ElevationCheckFunc verifies terrainPath answers at each lon/lat point.
type ElevationCheckFunc func(ctx context.Context, terrainPath string, pts [][2]float64) error

// TerrainFingerprintFunc identifies the terrain archive a build of regions would produce, so a
// previous generation's terrain can be reused when nothing that decides it changed.
type TerrainFingerprintFunc func(regions []catalog.Region) string

type hookSet struct {
	mu       sync.RWMutex
	fp       TerrainFingerprintFunc
	terrainF TerrainFunc
	estimate TerrainEstimateFunc
	check    ElevationCheckFunc
	contours ContoursFunc
}

var hooks hookSet

// RegisterTerrain installs the terrain builder.
func RegisterTerrain(f TerrainFunc) { hooks.mu.Lock(); hooks.terrainF = f; hooks.mu.Unlock() }

// RegisterTerrainFingerprint installs the fingerprint used by Options.ReuseTerrain.
func RegisterTerrainFingerprint(f TerrainFingerprintFunc) {
	hooks.mu.Lock()
	hooks.fp = f
	hooks.mu.Unlock()
}

func (h *hookSet) fingerprintFunc() TerrainFingerprintFunc {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.fp
}

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

// RegisterContours installs the contour builder.
func RegisterContours(f ContoursFunc) { hooks.mu.Lock(); hooks.contours = f; hooks.mu.Unlock() }

func (h *hookSet) contoursFunc() ContoursFunc {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.contours
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
