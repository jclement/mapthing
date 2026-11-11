package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/diskcache"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
)

// share measures what the shared source-block cache saves: the same tiles
// fetched each on its own (no cache at all), then through one service with the
// disk cache a server has, then a neighbouring screen, then after a restart.
func share(ctx context.Context, dir string) {
	type xy struct{ x, y int }
	up := ondemand.NewUpstream(ondemand.UpstreamOptions{})
	n := 0
	fresh := func() *ondemand.Service { // a server with an empty cache
		n++
		return withStore(up, filepath.Join(dir, fmt.Sprint(n)))
	}
	run := func(tiles []xy, z int, svc func() *ondemand.Service) (float64, float64) {
		_, b0, _ := up.Counters()
		t0 := time.Now()
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		for _, t := range tiles {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if _, _, err := svc().Tile(ctx, z, t.x, t.y); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}()
		}
		wg.Wait()
		_, b1, _ := up.Counters()
		return time.Since(t0).Seconds(), float64(b1-b0) / 1e6
	}
	screen := func(x, y int) []xy {
		var out []xy
		for dy := 0; dy < 3; dy++ {
			for dx := 0; dx < 4; dx++ {
				out = append(out, xy{x + dx, y + dy})
			}
		}
		return out
	}
	fmt.Println("| 12-tile screen, z12 | each tile alone, no cache | shared cache, first screen | adjacent screen after it | first screen again after a restart |")
	fmt.Println("|---|---|---|---|---|")
	for _, a := range []struct {
		name     string
		lon, lat float64
	}{{"texas (3DEP)", -98.3, 30.6}, {"calgary (HRDEM/MRDEM)", -114.3, 51.1}, {"alps (GLO-30)", 8.1, 46.6}} {
		px, py := elevation.LonLatToWorldPixel(a.lon, a.lat, 12)
		x, y := int(px)/256, int(py)/256
		_, _ = ondemand.New(ondemand.Options{Upstream: up}).Render(ctx, 12, x+40, y) // open connections
		t0, m0 := run(screen(x, y), 12, func() *ondemand.Service { return ondemand.New(ondemand.Options{Upstream: up}) })
		s := fresh()
		one := func() *ondemand.Service { return s }
		t1, m1 := run(screen(x, y), 12, one)
		t2, m2 := run(screen(x+4, y), 12, one)
		s = withStore(up, filepath.Join(dir, fmt.Sprint(n))) // same directory: a restart
		t3, m3 := run(screen(x, y), 12, one)
		fmt.Printf("| %s | %.2f s, %.1f MB | %.2f s, %.1f MB | %.2f s, %.1f MB | %.3f s, %.1f MB |\n", a.name, t0, m0, t1, m1, t2, m2, t3, m3)
	}
	fmt.Println()
	fmt.Println("| 100 km profile, 30 m, z13 tiles | each tile alone, no cache | shared cache, cold | a parallel profile 3 km away, after it | the same profile again |")
	fmt.Println("|---|---|---|---|---|")
	for _, a := range []struct {
		name  string
		start elevation.LonLat
		brg   float64
	}{{"texas (3DEP)", elevation.LonLat{-99.2, 30.2}, 75}, {"calgary (HRDEM/MRDEM)", elevation.LonLat{-114.05, 51.05}, 265}, {"alps (GLO-30)", elevation.LonLat{7.2, 46.2}, 70}} {
		tilesOf := func(start elevation.LonLat) []xy {
			seen := map[xy]bool{}
			var out []xy
			for d := 0.0; d <= 100000; d += 30 {
				q := destination(start, a.brg, d)
				px, py := elevation.LonLatToWorldPixel(q[0], q[1], 13)
				for _, o := range [4][2]float64{{-0.5, -0.5}, {0.5, -0.5}, {-0.5, 0.5}, {0.5, 0.5}} {
					k := xy{int(math.Floor((px + o[0]) / 256)), int(math.Floor((py + o[1]) / 256))}
					if !seen[k] {
						seen[k] = true
						out = append(out, k)
					}
				}
			}
			return out
		}
		p1, p2 := tilesOf(a.start), tilesOf(destination(a.start, a.brg+90, 3000))
		t0, m0 := run(p1, 13, func() *ondemand.Service { return ondemand.New(ondemand.Options{Upstream: up}) })
		s := fresh()
		one := func() *ondemand.Service { return s }
		t1, m1 := run(p1, 13, one)
		t2, m2 := run(p2, 13, one)
		t3, m3 := run(p1, 13, one)
		fmt.Printf("| %s (%d tiles) | %.2f s, %.1f MB | %.2f s, %.1f MB | %.2f s, %.1f MB | %.3f s, %.1f MB |\n", a.name, len(p1), t0, m0, t1, m1, t2, m2, t3, m3)
	}
}

func withStore(up *ondemand.Upstream, dir string) *ondemand.Service {
	c, err := diskcache.New(filepath.Join(dir, "cache"), 5_000_000_000)
	if err != nil {
		fatal(err)
	}
	return ondemand.New(ondemand.Options{Upstream: up, Store: ondemand.NewDiskStore(c), StatePath: filepath.Join(dir, "versions.json")})
}
