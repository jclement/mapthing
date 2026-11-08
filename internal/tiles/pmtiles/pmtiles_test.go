package pmtiles

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
)

func TestTileIDKnownValues(t *testing.T) {
	// Values from the PMTiles v3 specification's Hilbert ordering.
	cases := []struct {
		z    uint8
		x, y uint32
		id   uint64
	}{
		{0, 0, 0, 0},
		{1, 0, 0, 1}, {1, 0, 1, 2}, {1, 1, 1, 3}, {1, 1, 0, 4},
		{2, 0, 0, 5},
		{3, 0, 0, 21},
		{12, 3423, 1763, 19078479},
	}
	for _, c := range cases {
		if got := ZxyToID(c.z, c.x, c.y); got != c.id {
			t.Errorf("ZxyToID(%d,%d,%d) = %d, want %d", c.z, c.x, c.y, got, c.id)
		}
		z, x, y := IDToZxy(c.id)
		if z != c.z || x != c.x || y != c.y {
			t.Errorf("IDToZxy(%d) = %d/%d/%d, want %d/%d/%d", c.id, z, x, y, c.z, c.x, c.y)
		}
	}
}

func TestTileIDRoundTrip(t *testing.T) {
	for z := uint8(0); z <= 6; z++ {
		n := uint32(1) << z
		seen := map[uint64]bool{}
		for x := range n {
			for y := range n {
				id := ZxyToID(z, x, y)
				if seen[id] {
					t.Fatalf("duplicate id %d", id)
				}
				seen[id] = true
				gz, gx, gy := IDToZxy(id)
				if gz != z || gx != x || gy != y {
					t.Fatalf("round trip %d/%d/%d -> %d -> %d/%d/%d", z, x, y, id, gz, gx, gy)
				}
			}
		}
	}
}

func TestDirectoryRoundTrip(t *testing.T) {
	es := []Entry{{0, 0, 10, 1}, {1, 10, 20, 3}, {5, 0, 10, 1}, {9, 100, 5, 0}}
	for _, c := range []uint8{CompressionNone, CompressionGzip} {
		b, err := MarshalDirectory(es, c)
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnmarshalDirectory(b, c)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(es) {
			t.Fatalf("got %v want %v", got, es)
		}
	}
}

func build(t *testing.T, maxZ uint8, tile func(z uint8, x, y uint32) []byte) *Reader {
	t.Helper()
	w, err := NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for z := uint8(0); z <= maxZ; z++ {
		for x := range uint32(1) << z {
			for y := range uint32(1) << z {
				ids = append(ids, ZxyToID(z, x, y))
			}
		}
	}
	// ids are already increasing per zoom but Hilbert order within a zoom is not x/y order.
	slices.Sort(ids)
	for _, id := range ids {
		z, x, y := IDToZxy(id)
		if err := w.Add(id, tile(z, x, y)); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	h := Header{TileType: TypePNG, TileCompression: CompressionNone, MinZoom: 0, MaxZoom: maxZ,
		MinLonE7: -1800000000, MinLatE7: -850000000, MaxLonE7: 1800000000, MaxLatE7: 850000000}
	if err := w.Finish(&buf, h, []byte(`{"name":"test"}`)); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return r
}


func TestWriteReadWithDedupAndLeaves(t *testing.T) {
	// z0..8 = 87381 tiles: forces leaf directories. Many tiles share content (dedup + runs).
	tile := func(z uint8, x, y uint32) []byte {
		if (x+y)%3 == 0 {
			return fmt.Appendf(nil, "tile %d/%d/%d", z, x, y)
		}
		if z == 7 && x > 100 {
			return nil // missing tiles
		}
		return []byte("ocean")
	}
	r := build(t, 8, tile)
	if r.Header.LeafLength == 0 {
		t.Fatalf("expected leaf directories for %d entries", r.Header.TileEntries)
	}
	if r.Header.TileContents >= r.Header.AddressedTiles {
		t.Fatalf("expected deduplication: contents %d addressed %d", r.Header.TileContents, r.Header.AddressedTiles)
	}
	for _, c := range [][3]uint32{{0, 0, 0}, {3, 1, 6}, {7, 100, 4}, {7, 127, 127}, {7, 101, 3}, {7, 50, 50}} {
		want := tile(uint8(c[0]), c[1], c[2])
		got, ok, err := r.Get(uint8(c[0]), c[1], c[2])
		if err != nil {
			t.Fatal(err)
		}
		if ok != (want != nil) || !bytes.Equal(got, want) {
			t.Errorf("%v: got %q ok=%v want %q", c, got, ok, want)
		}
	}
	if _, ok, _ := r.Get(9, 0, 0); ok {
		t.Error("tile beyond max zoom found")
	}
	m, err := r.Metadata()
	if err != nil || string(m) != `{"name":"test"}` {
		t.Errorf("metadata %q %v", m, err)
	}
}

func TestExtract(t *testing.T) {
	tile := func(z uint8, x, y uint32) []byte { return fmt.Appendf(nil, "%d/%d/%d", z, x, y) }
	src := build(t, 6, tile)
	var buf bytes.Buffer
	b := BBox{-10, -10, 10, 10}
	n, err := Extract(context.Background(), src, b, 2, 5, 0, t.TempDir(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if want := CountTiles(b, 2, 5); n != want {
		t.Fatalf("extracted %d tiles, want %d", n, want)
	}
	r, err := NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if r.Header.MinZoom != 2 || r.Header.MaxZoom != 5 {
		t.Fatalf("zooms %d-%d", r.Header.MinZoom, r.Header.MaxZoom)
	}
	x0, y0, _, _ := TileRange(b, 5)
	got, ok, _ := r.Get(5, x0, y0)
	if !ok || string(got) != fmt.Sprintf("5/%d/%d", x0, y0) {
		t.Fatalf("inside tile: %q %v", got, ok)
	}
	if _, ok, _ := r.Get(5, 0, 0); ok {
		t.Fatal("tile outside bbox present")
	}
	if _, ok, _ := r.Get(1, 0, 0); ok {
		t.Fatal("tile below minzoom present")
	}
	if _, err := Extract(context.Background(), src, b, 0, 6, 10, t.TempDir(), &bytes.Buffer{}); err == nil {
		t.Fatal("expected tile budget error")
	}
}
