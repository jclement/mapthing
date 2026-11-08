package build

// STUB: stands in for the pipeline worker's declarations until feature/pipeline
// merges. Delete this file at merge; the real ones have the same names and
// signatures (agreed via the lead).

import (
	"context"

	"github.com/jclement/mapthing/internal/catalog"
)

// Reporter receives build progress.
type Reporter interface {
	Stage(name string)
	Progress(frac float64, msg string)
	Logf(format string, args ...any)
}

// TerrainFunc builds terrain.pmtiles for the regions at outPath.
type TerrainFunc func(ctx context.Context, r Reporter, regions []catalog.Region, workDir, outPath string) error

var (
	terrainFunc     TerrainFunc
	terrainEstimate func([]catalog.Region) int64
	elevationCheck  func(ctx context.Context, terrainPath string, pts [][2]float64) error
)

// RegisterTerrain installs the terrain builder.
func RegisterTerrain(f TerrainFunc) { terrainFunc = f }

// RegisterTerrainEstimate installs the disk-size estimator (bytes).
func RegisterTerrainEstimate(f func([]catalog.Region) int64) { terrainEstimate = f }

// RegisterElevationCheck installs the post-build verification.
func RegisterElevationCheck(f func(ctx context.Context, terrainPath string, pts [][2]float64) error) {
	elevationCheck = f
}
