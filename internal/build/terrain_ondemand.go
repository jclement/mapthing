package build

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
)

// Regions whose terrain is on demand still get their low zooms built, so a
// zoomed-out map never waits on the DEM hosts. Those tiles are read with the
// on-demand reader (plain range reads of the sources' overviews, no GDAL and
// no cache windows), at the chunk level of the pyramid; lowZooms then builds
// everything below from them, together with the built regions' tiles.

// newLowZoomService returns the reader used for on-demand regions' low zooms.
// Tests replace it.
var newLowZoomService = func() *ondemand.Service {
	// Low zooms may read an overview a little coarser than the tile: a third
	// fewer bytes for a difference no zoomed-out hillshade shows.
	return ondemand.New(ondemand.Options{LevelSlack: 1.3})
}

// onDemandBase renders the chunk-level tiles of the on-demand areas that no
// built chunk produced, writes them, and adds them to the pyramid's base.
func (b *builder) onDemandBase(ctx context.Context, low []area) error {
	zb := b.z - b.shift
	seen := map[[2]int]bool{}
	var todo [][2]int
	b.mu.Lock()
	for _, a := range low {
		x0, y0, x1, y1 := elevation.TileRange(zb, a.bbox[0], a.bbox[1], a.bbox[2], a.bbox[3])
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				k := [2]int{x, y}
				if seen[k] || b.base[k] != nil {
					continue // a built chunk already made this tile
				}
				w, s, e, n := elevation.TileBounds(zb, x, y)
				if !a.intersectsBox(elevation.BBox{w, s, e, n}) {
					continue
				}
				seen[k] = true
				todo = append(todo, k)
			}
		}
	}
	b.mu.Unlock()
	if len(todo) == 0 {
		return nil
	}
	b.report.Logf("terrain: %d on-demand region(s): building %d low-zoom tiles at z%d from source overviews", len(low), len(todo), zb)
	svc := newLowZoomService()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		done  atomic.Int64
		wrote atomic.Int64
	)
	jobs := make(chan [2]int)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobs {
				err := func() error {
					r, err := svc.Render(ctx, zb, k[0], k[1])
					if err != nil {
						return fmt.Errorf("terrain: low-zoom tile %d/%d/%d of an on-demand region: %w", zb, k[0], k[1], err)
					}
					if r.Filled == 0 {
						return nil
					}
					data, err := elevation.EncodeTile(quantize(r.Elev, b.stepFor(zb)), nil)
					if err != nil {
						return err
					}
					if err := b.w.AddTile(uint8(zb), uint32(k[0]), uint32(k[1]), data); err != nil {
						return err
					}
					b.mu.Lock()
					b.base[k] = &elevTile{elev: r.Elev}
					b.mu.Unlock()
					wrote.Add(1)
					return nil
				}()
				if err != nil {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
				}
				if d := done.Add(1); d%50 == 0 {
					b.report.Logf("terrain: on-demand low zooms %d/%d", d, len(todo))
				}
			}
		}()
	}
feed:
	for _, k := range todo {
		select {
		case jobs <- k:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return first
	}
	req, bytes, _ := svc.Upstream().Counters()
	b.report.Logf("terrain: on-demand low zooms: %d tiles written (%d requests, %.1f MB read)", wrote.Load(), req, float64(bytes)/1e6)
	return nil
}
