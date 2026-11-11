package ondemand

import (
	"context"
	"sync"

	"github.com/jclement/mapthing/internal/elevation"
)

// Fallback adapts the service to elevation.Fallback: the elevation API asks
// it about points the built terrain does not cover.
type Fallback struct{ S *Service }

var _ elevation.Fallback = Fallback{}

// Zoom implements elevation.Fallback.
func (Fallback) Zoom() int { return SampleZoom }

// Covers implements elevation.Fallback.
func (f Fallback) Covers(lon, lat float64) bool {
	p := f.S.Policy()
	if p.Outside {
		return true
	}
	for _, a := range p.Areas {
		if a.BBox.Contains(lon, lat) && (len(a.Rings) == 0 || elevation.PointInRings(a.Rings, lon, lat)) {
			return true
		}
	}
	return false
}

// Tile implements elevation.Fallback.
func (f Fallback) Tile(ctx context.Context, x, y int) (*elevation.Tile, error) {
	return f.S.Decoded(ctx, x, y)
}

// prepareWorkers is how many tiles of one request are fetched at a time; the
// Upstream caps what actually goes out.
const prepareWorkers = 12

// Prepare implements elevation.Fallback: the tiles are fetched side by side,
// and the first failure fails the request.
func (f Fallback) Prepare(ctx context.Context, tiles [][2]int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	jobs := make(chan [2]int)
	for i := 0; i < min(prepareWorkers, len(tiles)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if _, err := f.S.Decoded(ctx, t[0], t[1]); err != nil {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
				}
			}
		}()
	}
feed:
	for _, t := range tiles {
		select {
		case jobs <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return first
}
