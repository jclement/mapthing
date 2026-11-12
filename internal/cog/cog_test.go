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

// exercise opens a file and touches every code path a server would: it must
// return errors for anything malformed, never panic.
func exercise(b []byte) {
	ctx := context.Background()
	src := &memReader{b: b}
	f, err := cog.Open(ctx, src)
	if err != nil {
		return
	}
	for li, lv := range f.Levels {
		for _, rc := range [][2]int{{0, 0}, {lv.Across - 1, lv.Down - 1}, {lv.Across, lv.Down}, {-1, 0}} {
			ref, err := f.Locate(ctx, li, rc[0], rc[1])
			if err != nil || ref.Size == 0 || ref.Size > 1<<20 {
				continue
			}
			raw, _ := src.ReadRange(ctx, ref.Offset, ref.Size)
			_, _ = f.Decode(li, raw)
		}
	}
	_, _ = f.Locate(ctx, len(f.Levels), 0, 0)
	_, _ = f.Decode(-1, nil)
	_ = f.MemBytes()
}

func le16(b []byte, off int, v uint16) { b[off], b[off+1] = byte(v), byte(v>>8) }
func le64(b []byte, off int, v uint64) {
	for i := 0; i < 8; i++ {
		b[off+i] = byte(v >> (8 * i))
	}
}

// Files that lie about themselves are refused, not obeyed: each of these
// used to be able to panic the reader (and with it the server).
func TestMalformedFilesAreRefusedNotObeyed(t *testing.T) {
	data := surface(64, 64)
	classic := cogtest.Write(cogtest.Options{Width: 64, Height: 64, Tile: 32, Data: data, PixelW: 1, PixelH: 1, Compression: 5, Predictor: 3, EPSG: 4326, Geographic: true, Overviews: 1})
	big := cogtest.Write(cogtest.Options{Width: 64, Height: 64, Tile: 32, Data: data, PixelW: 1, PixelH: 1, BigTIFF: true, Compression: 8, EPSG: 3979})
	if _, err := cog.Open(context.Background(), &memReader{b: classic}); err != nil {
		t.Fatal(err)
	}
	// Classic TIFF entries are 12 bytes from offset 10: tag(2) type(2) count(4) value(4).
	// BigTIFF entries are 20 bytes from offset 24: tag(2) type(2) count(8) value(8).
	type mut func(b []byte)
	cases := map[string]struct {
		base []byte
		m    mut
	}{
		"classic: ImageWidth typed DOUBLE":  {classic, func(b []byte) { le16(b, 10+2, 12) }},
		"classic: ImageWidth typed LONG8":   {classic, func(b []byte) { le16(b, 10+2, 16) }},
		"classic: ImageWidth with 2 values": {classic, func(b []byte) { b[10+4] = 2 }},
		"classic: unknown type":             {classic, func(b []byte) { le16(b, 10+2, 99) }},
		"big: count that overflows":         {big, func(b []byte) { le64(b, 24+8*20+4, 1<<61+1) }},
		"big: offsets beyond the file":      {big, func(b []byte) { le64(b, 24+8*20+12, 1<<62) }},
		"big: huge image":                   {big, func(b []byte) { le64(b, 24+12, 1<<31-1); le64(b, 24+20+12, 1<<31-1) }},
		"big: first directory out of range": {big, func(b []byte) { le64(b, 8, 1<<63+5) }},
		"big: directory with 2^60 entries":  {big, func(b []byte) { le64(b, 16, 1<<60) }},
	}
	for name, c := range cases {
		b := append([]byte(nil), c.base...)
		c.m(b)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", name, r)
				}
			}()
			if _, err := cog.Open(context.Background(), &memReader{b: b}); err == nil {
				t.Errorf("%s: opened", name)
			}
			exercise(b)
		}()
	}
	// Truncation anywhere, and any single corrupted byte in the header area, must not panic.
	for _, base := range [][]byte{classic, big} {
		for n := 0; n < len(base); n += 7 {
			exercise(base[:n])
		}
		for i := 0; i < 600 && i < len(base); i++ {
			for _, v := range []byte{0, 0xff, 0x80, base[i] + 1} {
				b := append([]byte(nil), base...)
				b[i] = v
				exercise(b)
			}
		}
	}
}

// FuzzOpen feeds arbitrary bytes to the reader; the seeds are real layouts.
func FuzzOpen(f *testing.F) {
	data := surface(48, 40)
	for _, o := range []cogtest.Options{
		{Compression: 1}, {Compression: 5, Predictor: 3, Overviews: 1}, {Compression: 8, Predictor: 3},
		{Compression: 5, BigTIFF: true, Sparse: true, Overviews: 2}, {Compression: 8, BigTIFF: true},
	} {
		o.Width, o.Height, o.Tile, o.Data, o.PixelW, o.PixelH, o.EPSG = 48, 40, 16, data, 0.5, 0.5, 4326
		f.Add(cogtest.Write(o))
	}
	f.Fuzz(func(_ *testing.T, b []byte) { exercise(b) })
}
