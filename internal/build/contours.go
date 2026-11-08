package build

// Contours: terrain.pmtiles → contours.pmtiles (MVT layer "contours"), run by the terrain
// stage right after the terrain archive is written. See internal/contours.

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/jclement/mapthing/internal/contours"
)

func init() { RegisterContours(BuildContours) }

// envContoursMaxZoom overrides the contour archive's max zoom (default 12); 0 disables contours.
const envContoursMaxZoom = "MAPTHING_CONTOURS_MAXZOOM"

// BuildContours is the ContoursFunc registered with the pipeline.
func BuildContours(ctx context.Context, r Reporter, terrainPath, outPath string) error {
	o := contours.Options{Progress: r.Progress}
	if v := os.Getenv(envContoursMaxZoom); v != "" {
		z, err := strconv.Atoi(v)
		if err != nil || z < 0 || z > 15 {
			return fmt.Errorf("%s=%q: want 0..15", envContoursMaxZoom, v)
		}
		if z == 0 {
			r.Logf("contours disabled (%s=0)", envContoursMaxZoom)
			return nil
		}
		o.MaxZoom = z
		o.MinZoom = min(contours.DefaultMinZoom, z)
	}
	if w, err := strconv.Atoi(os.Getenv(envWorkers)); err == nil && w > 0 {
		o.Workers = w
	}
	r.Stage("contours")
	res, err := contours.Build(ctx, terrainPath, outPath, o)
	if err != nil {
		return err
	}
	r.Logf("contours: %d tiles z%d-%d, %d KB of tiles, %.1fs (by zoom: %v)",
		res.Tiles, res.MinZoom, res.MaxZoom, res.Bytes/1024, res.DurationSec, res.TilesByZoom)
	return nil
}
