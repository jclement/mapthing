package pmtiles

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

func TestZxyToID(t *testing.T) {
	cases := []struct {
		z    uint8
		x, y uint32
		id   uint64
	}{
		{0, 0, 0, 0},
		{1, 0, 0, 1},
		{1, 0, 1, 2},
		{1, 1, 1, 3},
		{1, 1, 0, 4},
		{2, 0, 0, 5},
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
	for z := uint8(0); z < 16; z++ {
		n := uint32(1) << z
		for i := 0; i < 50; i++ {
			x, y := rand.Uint32()%n, rand.Uint32()%n
			gz, gx, gy := IDToZxy(ZxyToID(z, x, y))
			if gz != z || gx != x || gy != y {
				t.Fatalf("round trip %d/%d/%d -> %d/%d/%d", z, x, y, gz, gx, gy)
			}
		}
	}
}

func TestWriteRead(t *testing.T) {
	for _, n := range []int{1, 300, 40000} { // 40000 forces leaf directories
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.pmtiles")
			w, err := Create(path)
			if err != nil {
				t.Fatal(err)
			}
			const z = 10
			type key struct{ x, y uint32 }
			want := map[key]string{}
			perm := rand.Perm(n)
			for _, i := range perm {
				k := key{uint32(i % 1024), uint32(i / 1024)}
				data := fmt.Sprintf("tile-%d", i%97) // many duplicates
				want[k] = data
				if err := w.Add(z, k.x, k.y, []byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Finish(Header{TileType: TileTypePNG, TileCompression: CompressionNone, MinZoom: z, MaxZoom: z}, map[string]any{"name": "test"}); err != nil {
				t.Fatal(err)
			}
			r, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			if r.Header.AddressedTiles != uint64(n) {
				t.Errorf("addressed = %d, want %d", r.Header.AddressedTiles, n)
			}
			if n > 97 && r.Header.TileContents != 97 {
				t.Errorf("contents = %d, want 97 (dedupe)", r.Header.TileContents)
			}
			for k, v := range want {
				got, ok, err := r.Tile(z, k.x, k.y)
				if err != nil || !ok || string(got) != v {
					t.Fatalf("tile %v = %q %v %v, want %q", k, got, ok, err, v)
				}
			}
			if _, ok, _ := r.Tile(z, 1023, 1023); ok && n < 1023*1024 {
				t.Errorf("missing tile reported present")
			}
			if _, ok, _ := r.Tile(z+1, 0, 0); ok {
				t.Errorf("tile at other zoom reported present")
			}
			var meta map[string]any
			if err := r.Metadata(&meta); err != nil || meta["name"] != "test" {
				t.Errorf("metadata = %v %v", meta, err)
			}
		})
	}
}
