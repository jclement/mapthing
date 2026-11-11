package cog_test

import (
	"context"
	"math"
	"testing"

	"github.com/jclement/mapthing/internal/cog"
	"github.com/jclement/mapthing/internal/cog/cogtest"
)

type memReader struct {
	b     []byte
	reads int
	bytes int64
}

func (m *memReader) ReadRange(_ context.Context, off, n int64) ([]byte, error) {
	m.reads++
	if off >= int64(len(m.b)) {
		return nil, nil
	}
	end := min(off+n, int64(len(m.b)))
	m.bytes += end - off
	return m.b[off:end], nil
}

func surface(w, h int) []float32 {
	d := make([]float32, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d[y*w+x] = float32(100 + 0.37*float64(x) + 1.21*float64(y) + 5*math.Sin(float64(x)/7))
		}
	}
	return d
}

func TestReadEveryLayout(t *testing.T) {
	const w, h, tile = 300, 200, 64
	data := surface(w, h)
	// A hole, and a whole empty block.
	data[10*w+10] = float32(math.NaN())
	for y := 64; y < 128; y++ {
		for x := 128; x < 192; x++ {
			data[y*w+x] = float32(math.NaN())
		}
	}
	for _, tc := range []struct {
		name      string
		big       bool
		comp, pre int
		sparse    bool
	}{
		{"classic-none", false, 1, 1, false},
		{"classic-lzw-float", false, 5, 3, false},
		{"classic-deflate-float", false, 8, 3, false},
		{"big-lzw", true, 5, 1, true},
		{"big-deflate", true, 8, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := cogtest.Write(cogtest.Options{Width: w, Height: h, Tile: tile, Data: data, OriginX: -100, OriginY: 50,
				PixelW: 0.01, PixelH: 0.02, BigTIFF: tc.big, Compression: tc.comp, Predictor: tc.pre, NoData: -32767,
				EPSG: 4269, Geographic: true, Overviews: 2, Sparse: tc.sparse})
			src := &memReader{b: raw}
			f, err := cog.Open(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Levels) != 3 || f.Levels[0].Width != w || f.Levels[1].Width != 150 || f.Levels[2].Width != 75 {
				t.Fatalf("levels: %+v", f.Levels)
			}
			if f.OriginX != -100 || f.OriginY != 50 || f.Levels[0].PixelW != 0.01 || f.Levels[0].PixelH != 0.02 || f.EPSG != 4269 || !f.HasNoData {
				t.Fatalf("georef: %+v", f)
			}
			if got := f.Levels[1].PixelW; math.Abs(got-0.02) > 1e-12 {
				t.Fatalf("overview pixel = %v", got)
			}
			lv := f.Levels[0]
			for row := 0; row < lv.Down; row++ {
				for col := 0; col < lv.Across; col++ {
					ref, err := f.Locate(context.Background(), 0, col, row)
					if err != nil {
						t.Fatal(err)
					}
					empty := col == 2 && row == 1
					if tc.sparse && empty != (ref.Size == 0) {
						t.Fatalf("block %d,%d: size %d", col, row, ref.Size)
					}
					var b []byte
					if ref.Size > 0 {
						b = raw[ref.Offset : ref.Offset+ref.Size]
					}
					got, err := f.Decode(0, b)
					if err != nil {
						t.Fatalf("block %d,%d: %v", col, row, err)
					}
					for y := 0; y < tile; y++ {
						for x := 0; x < tile; x++ {
							sx, sy := col*tile+x, row*tile+y
							g := got[y*tile+x]
							if sx >= w || sy >= h {
								if g == g {
									t.Fatalf("padding at %d,%d = %v", sx, sy, g)
								}
								continue
							}
							want := data[sy*w+sx]
							if (want != want) != (g != g) || (want == want && want != g) {
								t.Fatalf("pixel %d,%d = %v, want %v", sx, sy, g, want)
							}
						}
					}
				}
			}
			if src.reads != 1 {
				t.Errorf("metadata took %d reads, want 1", src.reads)
			}
		})
	}
}

// A big tile index is read a page at a time, not whole.
func TestIndexIsReadInPages(t *testing.T) {
	const w, h, tile = 4096, 4096, 16
	data := make([]float32, w*h)
	raw := cogtest.Write(cogtest.Options{Width: w, Height: h, Tile: tile, Data: data, PixelW: 1, PixelH: 1, BigTIFF: true, NoData: -1, EPSG: 3979})
	src := &memReader{b: raw}
	f, err := cog.Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Locate(context.Background(), 0, 200, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Locate(context.Background(), 0, 201, 200); err != nil {
		t.Fatal(err)
	}
	// 65536 blocks × 16 bytes of index is 1 MB; two neighbours need the header page
	// and at most one page of offsets and one of sizes.
	if src.reads > 3 || src.bytes > 3*cog.PageSize {
		t.Fatalf("read %d ranges, %d bytes", src.reads, src.bytes)
	}
}

func TestRejectsWhatItCannotRead(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":   {},
		"not tif": []byte("GIF89a..........................."),
		"cut":     cogtest.Write(cogtest.Options{Width: 8, Height: 8, Tile: 8, Data: make([]float32, 64), PixelW: 1, PixelH: 1})[:40],
	} {
		if _, err := cog.Open(context.Background(), &memReader{b: b}); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}
