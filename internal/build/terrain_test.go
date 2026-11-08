package build

import (
	"math"
	"testing"

	"github.com/jclement/mapthing/internal/elevation"
)

func TestCellNames(t *testing.T) {
	if got := cellName3DEP(-76, 38); got != "n39w076" {
		t.Errorf("3DEP name = %s", got)
	}
	if got := cellNameGLO30(-119, 55); got != "Copernicus_DSM_COG_10_N55_00_W119_00_DEM" {
		t.Errorf("GLO30 name = %s", got)
	}
	if got := cellNameGLO30(5, -3); got != "Copernicus_DSM_COG_10_S03_00_E005_00_DEM" {
		t.Errorf("GLO30 name = %s", got)
	}
}

func TestChooseMaxZoom(t *testing.T) {
	de := area{id: "de", bbox: elevation.BBox{-75.79, 38.45, -75.04, 39.84}}
	tx := area{id: "tx", bbox: elevation.BBox{-106.65, 25.84, -93.51, 36.5}}
	ab := area{id: "ab", bbox: elevation.BBox{-120, 49, -110, 60}}
	mx := area{id: "mx", bbox: elevation.BBox{-102, 20, -101, 21}} // GLO-30 only
	o := terrainOpts{maxTiles: DefaultTerrainMaxTiles}
	for _, c := range []struct {
		a    area
		want int
	}{{de, 13}, {tx, 13}, {ab, 13}, {mx, 12}} {
		if z, _ := chooseMaxZoom([]area{c.a}, o); z != c.want {
			t.Errorf("%s: z = %d want %d", c.a.id, z, c.want)
		}
	}
	// A tight budget lowers the zoom.
	o.maxTiles = 10000
	if z, n := chooseMaxZoom([]area{tx}, o); z >= 13 || n > 10000 {
		t.Errorf("budget: z=%d n=%d", z, n)
	}
	// Forced zoom wins.
	if z, _ := chooseMaxZoom([]area{tx}, terrainOpts{maxZoom: 11, maxTiles: 1}); z != 11 {
		t.Errorf("forced: %d", z)
	}
}

func TestCountTilesPolygon(t *testing.T) {
	box := area{bbox: elevation.BBox{-104, 31, -102, 33}}
	// Triangle covering roughly half the box.
	tri := box
	tri.polygon = [][][2]float64{{{-104, 31}, {-102, 31}, {-104, 33}, {-104, 31}}}
	nb, nt := countTiles([]area{box}, 10), countTiles([]area{tri}, 10)
	if nt >= nb || nt < nb/3 {
		t.Errorf("polygon tiles %d vs bbox %d", nt, nb)
	}
	if !polygonIntersectsBox(tri.polygon, elevation.BBox{-103.9, 31.1, -103.8, 31.2}) {
		t.Errorf("inside box not detected")
	}
	if polygonIntersectsBox(tri.polygon, elevation.BBox{-102.2, 32.8, -102.1, 32.9}) {
		t.Errorf("outside box detected")
	}
	// Polygon entirely inside the box (no box corner inside polygon).
	if !polygonIntersectsBox([][][2]float64{{{0.4, 0.4}, {0.6, 0.4}, {0.5, 0.6}}}, elevation.BBox{0, 0, 1, 1}) {
		t.Errorf("contained polygon not detected")
	}
}

func TestDownsampleAndCut(t *testing.T) {
	nan := float32(math.NaN())
	r := []float32{1, 3, 5, nan, 2, 4, nan, nan, 0, 0, 7, 7, 0, 0, 7, 7}
	d := downsample(r, 4)
	want := []float32{2.5, 5, 0, 7}
	for i := range want {
		if d[i] != want[i] {
			t.Errorf("downsample[%d] = %v want %v", i, d[i], want[i])
		}
	}
	all := []float32{nan, nan, nan, nan}
	if v := downsample(all, 2)[0]; !math.IsNaN(float64(v)) {
		t.Errorf("all-nodata block = %v", v)
	}
	// cutTile on a 2×2-tile raster.
	ts := elevation.TileSize
	big := make([]float32, 4*ts*ts)
	src := make([]uint8, len(big))
	for i := range big {
		big[i] = float32(i % (2 * ts)) // = column index
		src[i] = uint8(1 + (i%(2*ts))/ts)
	}
	te, tsrc, any := cutTile(big, 2*ts, 1, 1, src, true)
	if !any || te[0] != float32(ts) || tsrc[0] != 2 || te[ts-1] != float32(2*ts-1) {
		t.Errorf("cutTile: %v %v %v", te[0], tsrc[0], te[ts-1])
	}
	for i := range big {
		big[i] = nan
	}
	if _, _, any := cutTile(big, 2*ts, 0, 0, src, false); any {
		t.Errorf("empty tile reported data")
	}
}

func TestCellsTouching(t *testing.T) {
	b := &builder{z: 13, areas: []area{{bbox: elevation.BBox{-75.79, 38.45, -75.04, 39.84}}}}
	cells := b.cellsTouching(elevation.BBox{-76, 38, -75, 40})
	if len(cells) != 2 {
		t.Fatalf("cells = %+v", cells)
	}
	for _, c := range cells {
		if c.window[0] < -75.85 || c.window[2] > -75.0 {
			t.Errorf("window not clipped to area: %v", c.window)
		}
	}
}

func TestQuantizeSteps(t *testing.T) {
	b := &builder{z: 13, o: terrainOpts{quantum: 0.25}}
	for z, want := range map[int]float64{13: 0.25, 12: 0.5, 11: 1, 9: 4, 8: 8, 2: 8} {
		if got := b.stepFor(z); got != want {
			t.Errorf("stepFor(%d) = %v want %v", z, got, want)
		}
	}
	in := []float32{100.13, 100.37, float32(math.NaN())}
	out := quantize(in, 0.25)
	if out[0] != 100.25 || out[1] != 100.25 || !math.IsNaN(float64(out[2])) || in[0] != 100.13 {
		t.Errorf("quantize = %v (input %v)", out, in)
	}
}
