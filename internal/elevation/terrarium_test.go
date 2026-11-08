package elevation

import (
	"bytes"
	"image/png"
	"math"
	"testing"
)

func TestTerrariumRoundTrip(t *testing.T) {
	for _, e := range []float64{-32768, -432.17, -0.004, 0, 0.001953125, 1.5, 123.456, 1609.34, 4663.2, 8848.86, 32767.99} {
		r, g, b := EncodeTerrarium(e)
		got := DecodeTerrarium(r, g, b)
		if math.Abs(got-e) > 1.0/512+1e-9 {
			t.Errorf("round trip %v -> %v (diff %v)", e, got, got-e)
		}
	}
	// Known values from the Terrarium definition.
	if r, g, b := EncodeTerrarium(0); r != 128 || g != 0 || b != 0 {
		t.Errorf("Encode(0) = %d,%d,%d want 128,0,0", r, g, b)
	}
	if e := DecodeTerrarium(128, 100, 128); e != 100.5 {
		t.Errorf("Decode(128,100,128) = %v want 100.5", e)
	}
	// Clamping.
	if r, g, b := EncodeTerrarium(-40000); r|g|b != 0 {
		t.Errorf("clamp low = %d,%d,%d", r, g, b)
	}
}

func TestTileEncodeDecode(t *testing.T) {
	elev := make([]float32, TileSize*TileSize)
	src := make([]uint8, TileSize*TileSize)
	for i := range elev {
		x, y := i%TileSize, i/TileSize
		elev[i] = float32(100 + float64(x)*0.37 - float64(y)*1.13)
		src[i] = 1
		if x < 10 {
			src[i] = 2
		}
		if y > 250 {
			elev[i] = float32(math.NaN())
			src[i] = 0
		}
	}
	data, err := EncodeTile(elev, src)
	if err != nil {
		t.Fatal(err)
	}
	// Still a valid PNG for any decoder (the source chunk is ancillary).
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("standard decoder rejects tile: %v", err)
	}
	tile, err := DecodeTile(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range elev {
		if src[i] != tile.Src[i] {
			t.Fatalf("src[%d] = %d want %d", i, tile.Src[i], src[i])
		}
		if math.IsNaN(float64(elev[i])) {
			if !math.IsNaN(float64(tile.Elev[i])) {
				t.Fatalf("pixel %d should be NaN", i)
			}
			continue
		}
		if d := math.Abs(float64(tile.Elev[i] - elev[i])); d > 1.0/256 {
			t.Fatalf("pixel %d: %v vs %v", i, tile.Elev[i], elev[i])
		}
	}
	// Without a source chunk, NaN becomes 0 m and Src is nil.
	data, _ = EncodeTile(elev, nil)
	tile, _ = DecodeTile(data)
	if tile.Src != nil || tile.Elev[TileSize*TileSize-1] != 0 {
		t.Errorf("no-chunk tile: src=%v last=%v", tile.Src != nil, tile.Elev[TileSize*TileSize-1])
	}
}

func TestTileMath(t *testing.T) {
	px, py := LonLatToWorldPixel(0, 0, 0)
	if px != 128 || math.Abs(py-128) > 1e-9 {
		t.Errorf("origin = %v,%v", px, py)
	}
	px1, py1 := LonLatToWorldPixel(-118.8, 55.17, 13)
	lon, lat := WorldPixelToLonLat(px1, py1, 13)
	if math.Abs(lon+118.8) > 1e-9 || math.Abs(lat-55.17) > 1e-9 {
		t.Errorf("inverse = %v,%v", lon, lat)
	}
	minx, miny, maxx, maxy := TileMercBounds(1, 0, 0)
	if minx != -originShiftM || maxy != originShiftM || maxx != 0 || math.Abs(miny) > 1e-6 {
		t.Errorf("merc bounds = %v %v %v %v", minx, miny, maxx, maxy)
	}
	if r := GroundResolution(13, 30); math.Abs(r-16.55) > 0.05 {
		t.Errorf("ground res z13@30 = %v", r)
	}
	if d := HaversineM(LonLat{0, 0}, LonLat{0, 1}); math.Abs(d-111195) > 5 {
		t.Errorf("1° lat = %v m", d)
	}
}
