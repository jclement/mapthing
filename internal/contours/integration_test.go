package contours

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/pmtiles"
)

// TestRealTerrain builds contours from a real terrain archive and checks that contour
// vertices sit at their elevation: sampled from the terrain at max zoom, the median
// |terrain - ele| at the contour max zoom must stay within a quarter of the minor interval.
// Lower zooms are contoured on coarser grids (a z10 grid pixel is ~120 m), so in steep
// terrain their vertical error is larger for the same sub-pixel position error: logged only.
//
//	MAPTHING_TEST_TERRAIN=data/de/terrain.pmtiles go test ./internal/contours -run Real -v
func TestRealTerrain(t *testing.T) {
	path := os.Getenv("MAPTHING_TEST_TERRAIN")
	if path == "" {
		t.Skip("set MAPTHING_TEST_TERRAIN to a terrain.pmtiles")
	}
	out := filepath.Join(t.TempDir(), "contours.pmtiles")
	res, err := Build(context.Background(), path, out, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(out)
	t.Logf("tiles %v, archive %d bytes, %.1fs", res.TilesByZoom, st.Size(), res.DurationSec)

	a, err := elevation.OpenArchive(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	r, err := pmtiles.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	for z := res.MinZoom; z <= res.MaxZoom; z++ {
		minor, _ := Interval(z)
		var errs []float64
		hb := r.Header.Bounds()
		x0, y0, x1, y1 := elevation.TileRange(z, hb[0], hb[1], hb[2], hb[3])
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				data, ok, err := r.Get(uint8(z), uint32(x), uint32(y))
				if err != nil || !ok {
					continue
				}
				raw, err := pmtiles.Decompress(data, pmtiles.CompressionGzip)
				if err != nil {
					t.Fatal(err)
				}
				ls, err := Decode(raw)
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range ls[0].Features {
					ele := float64(f.Props["ele"].(int64))
					for _, ln := range f.Lines {
						for k, p := range ln {
							if k%3 != 0 || p[0] < 0 || p[1] < 0 || p[0] > Extent || p[1] > Extent {
								continue
							}
							px := (float64(x) + float64(p[0])/Extent) * elevation.TileSize
							py := (float64(y) + float64(p[1])/Extent) * elevation.TileSize
							lon, lat := elevation.WorldPixelToLonLat(px, py, z)
							s, err := a.Sample(lon, lat)
							if err != nil || s.Elevation == nil {
								continue
							}
							errs = append(errs, math.Abs(*s.Elevation-ele))
						}
					}
				}
			}
		}
		if len(errs) == 0 {
			t.Fatalf("z%d: no vertices", z)
		}
		sort.Float64s(errs)
		med, p90 := errs[len(errs)/2], errs[len(errs)*9/10]
		t.Logf("z%d (minor %d m): %d vertices, |terrain-ele| median %.2f m, p90 %.2f m", z, minor, len(errs), med, p90)
		if z == res.MaxZoom && med > float64(minor)/4 {
			t.Errorf("z%d: median error %.2f m exceeds a quarter of the %d m interval", z, med, minor)
		}
	}
}
