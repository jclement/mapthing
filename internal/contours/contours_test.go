package contours

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/pmtiles"
)

func TestIsolinesCone(t *testing.T) {
	const n = 41
	v := make([]float32, n*n)
	for j := 0; j < n; j++ {
		for i := 0; i < n; i++ {
			v[j*n+i] = float32(100 - math.Hypot(float64(i-20), float64(j-20))) // peak 100 at the centre
		}
	}
	levels := []float64{85.01, 90.01, 95.01}
	got := Isolines(v, n, n, levels)
	for k, ls := range got {
		if len(ls) != 1 {
			t.Fatalf("level %v: %d lines, want one ring", levels[k], len(ls))
		}
		ln := ls[0]
		if ln[0] != ln[len(ln)-1] {
			t.Fatalf("level %v: ring not closed", levels[k])
		}
		r := 100 - levels[k]
		for _, p := range ln {
			if d := math.Hypot(p.X-20, p.Y-20); math.Abs(d-r) > 0.1 {
				t.Fatalf("level %v: point %v at radius %.3f, want %.2f", levels[k], p, d, r)
			}
		}
		if l := Length(ln); math.Abs(l-2*math.Pi*r)/(2*math.Pi*r) > 0.03 {
			t.Fatalf("level %v: length %.2f, want ~%.2f", levels[k], l, 2*math.Pi*r)
		}
	}
}

func TestIsolinesPlaneAndNoData(t *testing.T) {
	const w, h = 10, 6
	v := make([]float32, w*h)
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			v[j*w+i] = float32(i * 10) // rises to the east
		}
	}
	ls := Isolines(v, w, h, []float64{45})[0]
	if len(ls) != 1 || len(ls[0]) != h {
		t.Fatalf("plane: %v", ls)
	}
	for _, p := range ls[0] {
		if math.Abs(p.X-4.5) > 1e-9 {
			t.Fatalf("plane point %v, want x=4.5", p)
		}
	}
	// a hole of no data across the line splits it in two open lines
	v[2*w+4], v[2*w+5] = float32(math.NaN()), float32(math.NaN())
	ls = Isolines(v, w, h, []float64{45})[0]
	if len(ls) != 2 {
		t.Fatalf("with hole: %d lines, want 2", len(ls))
	}
}

func TestIsolinesSaddle(t *testing.T) {
	// 2×2: high corners top-left and bottom-right
	v := []float32{1, 0, 0, 1}
	if ls := Isolines(v, 2, 2, []float64{0.4})[0]; len(ls) != 2 {
		t.Fatalf("saddle (centre above): %d segments", len(ls))
	}
	if ls := Isolines(v, 2, 2, []float64{0.6})[0]; len(ls) != 2 {
		t.Fatalf("saddle (centre below): %d segments", len(ls))
	}
}

func TestSimplify(t *testing.T) {
	pts := []Point{{0, 0}, {1, 0.01}, {2, -0.01}, {3, 0}, {3, 5}}
	got := Simplify(pts, 0.1)
	if len(got) != 3 || got[1] != (Point{3, 0}) {
		t.Fatalf("simplify: %v", got)
	}
}

func TestMVTRoundTrip(t *testing.T) {
	feats := []Feature{
		{Ele: 750, Major: true, Lines: [][][2]int32{{{0, 0}, {10, 0}, {10, 0}, {10, 20}}, {{-5, 4100}, {300, 200}}}},
		{Ele: -20, Major: false, Lines: [][][2]int32{{{7, 7}}}}, // no drawable line: dropped
		{Ele: 760, Major: false, Lines: [][][2]int32{{{1, 2}, {3, 4}}}},
	}
	ls, err := Decode(EncodeLayer("contours", feats))
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0].Name != "contours" || ls[0].Extent != Extent || ls[0].Version != 2 {
		t.Fatalf("layer: %+v", ls)
	}
	fs := ls[0].Features
	if len(fs) != 2 {
		t.Fatalf("features: %d", len(fs))
	}
	if fs[0].Type != 2 || fs[0].Props["ele"] != int64(750) || fs[0].Props["major"] != true {
		t.Fatalf("feature 0: %+v", fs[0])
	}
	want := [][][2]int32{{{0, 0}, {10, 0}, {10, 20}}, {{-5, 4100}, {300, 200}}}
	if len(fs[0].Lines) != 2 || len(fs[0].Lines[0]) != 3 || fs[0].Lines[1][0] != want[1][0] || fs[0].Lines[0][2] != want[0][2] {
		t.Fatalf("lines: %v", fs[0].Lines)
	}
	if fs[1].Props["ele"] != int64(760) || fs[1].Props["major"] != false {
		t.Fatalf("feature 1: %+v", fs[1].Props)
	}
	if EncodeLayer("contours", feats[1:2]) != nil {
		t.Fatal("tile with no drawable line should be nil")
	}
}

// writeTerrain writes a z10–12 Terrarium archive over one z10 tile whose elevation at world
// pixel (px, py) of zoom 12 is f(px, py).
func writeTerrain(t *testing.T, path string, f func(px, py float64) float64) (bx, by int) {
	t.Helper()
	bx, by = 200, 400 // z10 tile
	w, err := pmtiles.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := elevation.TileSize
	tile := func(z, x, y int) {
		elev := make([]float32, ts*ts)
		src := make([]uint8, ts*ts)
		s := math.Exp2(float64(12 - z))
		for j := 0; j < ts; j++ {
			for i := 0; i < ts; i++ {
				px := (float64(x*ts+i) + 0.5) * s
				py := (float64(y*ts+j) + 0.5) * s
				elev[j*ts+i] = float32(f(px, py))
				src[j*ts+i] = 1
			}
		}
		if z != 12 {
			src = nil
		}
		data, err := elevation.EncodeTile(elev, src)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.AddTile(uint8(z), uint32(x), uint32(y), data); err != nil {
			t.Fatal(err)
		}
	}
	tile(10, bx, by)
	for y := by * 4; y < by*4+4; y++ {
		for x := bx * 4; x < bx*4+4; x++ {
			tile(12, x, y)
		}
	}
	wb, sb, _, _ := elevation.TileBounds(10, bx, by)
	_, _, eb, nb := elevation.TileBounds(10, bx, by)
	meta, _ := json.Marshal(elevation.Metadata{Name: "terrain", Encoding: "terrarium", TileSize: ts, MinZoom: 10, MaxZoom: 12, Attribution: "test DEM"})
	h := pmtiles.Header{TileType: pmtiles.TypePNG, TileCompression: pmtiles.CompressionNone, InternalCompression: pmtiles.CompressionGzip,
		MinZoom: 10, MaxZoom: 12, MinLonE7: int32(wb * 1e7), MinLatE7: int32(sb * 1e7), MaxLonE7: int32(eb * 1e7), MaxLatE7: int32(nb * 1e7)}
	if err := w.Commit(h, meta); err != nil {
		t.Fatal(err)
	}
	return bx, by
}

func readTile(t *testing.T, r *pmtiles.Reader, z, x, y int) []DecodedFeature {
	t.Helper()
	data, ok, err := r.Get(uint8(z), uint32(x), uint32(y))
	if err != nil || !ok {
		t.Fatalf("tile %d/%d/%d: ok=%v err=%v", z, x, y, ok, err)
	}
	raw, err := pmtiles.Decompress(data, pmtiles.CompressionGzip)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := Decode(raw)
	if err != nil || len(ls) != 1 {
		t.Fatalf("decode: %v %d", err, len(ls))
	}
	return ls[0].Features
}

// TestBuildPlane: on a plane rising east, every contour in an interior z12 tile is one
// straight north–south line crossing the whole tile (so lines join across tile edges), at
// the x where the plane has that elevation, with the right major flags per zoom.
func TestBuildPlane(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "terrain.pmtiles"), filepath.Join(dir, "contours.pmtiles")
	const slope = 0.25 // metres per z12 pixel: 10 m every 40 px
	bx, by := writeTerrain(t, in, func(px, py float64) float64 { return 300 + slope*(px-float64(200*1024)) })
	res, err := Build(context.Background(), in, out, Options{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.TilesByZoom[10] != 1 || res.TilesByZoom[11] != 4 || res.TilesByZoom[12] != 16 {
		t.Fatalf("tiles by zoom: %v", res.TilesByZoom)
	}
	r, err := pmtiles.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if r.Header.TileType != pmtiles.TypeMVT || r.Header.MinZoom != 10 || r.Header.MaxZoom != 12 {
		t.Fatalf("header: %+v", r.Header)
	}
	var meta Metadata
	if err := r.MetadataJSON(&meta); err != nil || meta.VectorLayers[0].ID != "contours" || meta.Attribution != "test DEM" {
		t.Fatalf("metadata: %+v %v", meta, err)
	}

	// interior z12 tile (1, 1) of the block
	x, y := bx*4+1, by*4+1
	feats := readTile(t, r, 12, x, y)
	x0 := float64(x * 256) // tile's west edge in z12 world pixels
	e0 := 300 + slope*(x0-float64(200*1024))
	if len(feats) < 6 { // 256 px × 0.25 m = 64 m per tile: 6 or 7 levels at 10 m
		t.Fatalf("levels in tile: %d", len(feats))
	}
	for _, f := range feats {
		ele := float64(f.Props["ele"].(int64))
		if f.Props["major"] != (int64(ele)%50 == 0) {
			t.Fatalf("ele %v major %v", ele, f.Props["major"])
		}
		if len(f.Lines) != 1 {
			t.Fatalf("ele %v: %d lines, want 1", ele, len(f.Lines))
		}
		ln := f.Lines[0]
		wantX := (ele - e0) / slope * Extent / 256 // tile units
		top, bottom := ln[0][1], ln[len(ln)-1][1]
		if top > bottom {
			top, bottom = bottom, top
		}
		if top > 0 || bottom < Extent {
			t.Fatalf("ele %v: line spans y %d..%d, want across the tile", ele, top, bottom)
		}
		for _, p := range ln {
			if math.Abs(float64(p[0])-wantX) > 2 {
				t.Fatalf("ele %v: x %d, want %.1f", ele, p[0], wantX)
			}
		}
		if len(ln) > 3 {
			t.Fatalf("ele %v: straight line kept %d points after simplification", ele, len(ln))
		}
	}
	// z10/z11: 20 m minor, 100 m major
	for _, f := range readTile(t, r, 10, bx, by) {
		ele := f.Props["ele"].(int64)
		if ele%20 != 0 || f.Props["major"] != (ele%100 == 0) {
			t.Fatalf("z10 ele %d major %v", ele, f.Props["major"])
		}
	}
}

// TestBuildPeakRing: a small round hill gives closed rings centred on it; a one-pixel bump
// in flat ground (shorter than MinRing) gives nothing.
func TestBuildPeakRing(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "terrain.pmtiles"), filepath.Join(dir, "contours.pmtiles")
	cx, cy := float64(200*1024+512), float64(400*1024+512) // centre of the block
	bx, by := writeTerrain(t, in, func(px, py float64) float64 {
		d := math.Hypot(px-cx, py-cy)
		e := 500 + math.Max(0, 60-d*0.5)                                    // cone 60 m high, 120 px radius
		if math.Abs(px-(cx-300.5)) < 0.5 && math.Abs(py-(cy-300.5)) < 0.5 { // exactly one pixel
			e += 15 // single-pixel spike
		}
		return e
	})
	if _, err := Build(context.Background(), in, out, Options{Workers: 1, Blur: -1}); err != nil {
		t.Fatal(err)
	}
	r, err := pmtiles.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	feats := readTile(t, r, 10, bx, by) // the whole hill is inside the z10 tile
	// 500 (the cone's foot), 520, 540; the spike averages to ~500.9 m at z10 and so also
	// crosses 500, but its ring is shorter than MinRing and must be dropped
	if len(feats) != 3 {
		t.Fatalf("z10 levels: %d", len(feats))
	}
	for _, f := range feats {
		if len(f.Lines) != 1 {
			t.Fatalf("ele %v: %d lines (spike not dropped?)", f.Props["ele"], len(f.Lines))
		}
		ln := f.Lines[0]
		if ln[0] != ln[len(ln)-1] {
			t.Fatalf("ele %v: ring not closed", f.Props["ele"])
		}
	}
}
