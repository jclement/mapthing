package build

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/ondemand/ondemandtest"
	"github.com/jclement/mapthing/internal/pmtiles"
)

type quietReporter struct{ t *testing.T }

func (quietReporter) Stage(string)              {}
func (quietReporter) Progress(float64, string)  {}
func (q quietReporter) Logf(f string, a ...any) { q.t.Logf(f, a...) }

// A region set to on demand gets its low zooms built (from the sources'
// overviews, without GDAL) and nothing deeper.
func TestOnDemandRegionGetsOnlyLowZooms(t *testing.T) {
	hosts := ondemandtest.New()
	defer hosts.Close()
	ground := ondemandtest.Plane(1000)
	for lon := -100; lon <= -97; lon++ {
		for lat := 29; lat <= 31; lat++ {
			hosts.Put3DEP(lon, lat, ground, "v1")
		}
	}
	old := newLowZoomService
	defer func() { newLowZoomService = old }()
	newLowZoomService = func() *ondemand.Service {
		return ondemand.New(ondemand.Options{URLs: hosts.URLs(), LevelSlack: 1.3,
			Upstream: ondemand.NewUpstream(ondemand.UpstreamOptions{Attempts: 1, Timeout: 5 * time.Second})})
	}
	out := filepath.Join(t.TempDir(), "terrain.pmtiles")
	lazy := area{id: "custom/lazy", bbox: elevation.BBox{-98.9, 30.1, -97.1, 30.9}, lowOnly: true}
	o := terrainOpts{maxTiles: DefaultTerrainMaxTiles, quantum: TerrainQuantumM, cacheDir: t.TempDir()}
	res, err := buildTerrain(context.Background(), quietReporter{t}, []area{lazy}, t.TempDir(), out, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxZoom != DefaultTerrainMaxZoom || len(res.OnDemand) != 1 || res.OnDemand[0] != "custom/lazy" {
		t.Fatalf("result: %+v", res)
	}
	r, err := pmtiles.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	base := DefaultTerrainMaxZoom - chunkShift // the chunk level: the deepest zoom built for the region
	at := func(z int) (int, int) {
		px, py := elevation.LonLatToWorldPixel(-98.0, 30.5, z)
		return int(px) / 256, int(py) / 256
	}
	for z := 0; z <= DefaultTerrainMaxZoom; z++ {
		x, y := at(z)
		data, found, err := r.Get(uint8(z), uint32(x), uint32(y))
		if err != nil {
			t.Fatal(err)
		}
		if want := z <= base; found != want {
			t.Fatalf("z%d tile present = %v, want %v", z, found, want)
		}
		if z != base {
			continue
		}
		tile, err := elevation.DecodeTile(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range []int{0, 255, 128*256 + 128, 65535} {
			lon, lat := elevation.WorldPixelToLonLat(float64(x*256+i%256)+0.5, float64(y*256+i/256)+0.5, z)
			if !lazy.bbox.Contains(lon, lat) {
				continue
			}
			if got, want := float64(tile.Elev[i]), ground(lon, lat); !ondemandtest.Near(got, want, 2.5) {
				t.Errorf("z%d pixel %d = %v, want %v", z, i, got, want)
			}
		}
	}
	var meta elevation.Metadata
	if err := r.MetadataJSON(&meta); err != nil || len(meta.OnDemand) != 1 || meta.MaxZoom != DefaultTerrainMaxZoom {
		t.Fatalf("metadata: %+v %v", meta, err)
	}
	// Nothing deeper is estimated, and changing the mode changes what a build would reuse.
	built := lazy
	built.lowOnly = false
	if terrainFingerprint([]area{lazy}, o) == terrainFingerprint([]area{built}, o) {
		t.Error("the terrain mode is not part of the fingerprint: a build would reuse the wrong archive")
	}
	rg := catalog.Region{ID: "custom/lazy", BBox: catalog.BBox(lazy.bbox)}
	full := EstimateTerrainBytes([]catalog.Region{rg})
	rg.TerrainOnDemand = true
	if low := EstimateTerrainBytes([]catalog.Region{rg}); low <= 0 || low*20 > full {
		t.Errorf("estimate for an on-demand region is %d bytes, built %d", low, full)
	}
}

func TestVerifySkipsOnDemandRegions(t *testing.T) {
	areas := areasFromRegions([]catalog.Region{{ID: "a"}, {ID: "b", TerrainOnDemand: true}})
	if len(fullAreas(areas)) != 1 || len(lowAreas(areas)) != 1 || lowAreas(areas)[0].id != "b" {
		t.Fatalf("areas: %+v", areas)
	}
}
