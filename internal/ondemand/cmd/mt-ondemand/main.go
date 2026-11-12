// Command mt-ondemand measures on-demand terrain against the live DEM hosts,
// for development: how long a cold tile or profile takes and how many bytes
// it pulls.
//
//	mt-ondemand measure [-n 20] [-zooms 10,12,14] [-areas texas,calgary,…]
//	mt-ondemand profile [-runs 3]
//	mt-ondemand viewport [-zooms 10,12,14] [-areas …]   a screenful of tiles, as a browser asks
//	mt-ondemand tile z/x/y [-out tile.png]
//	mt-ondemand contours lon,lat DIR            what a cold contour tile costs at each zoom
//	mt-ondemand seam lon,lat DIR out            contours across a built/on-demand edge, to look at
//	mt-ondemand share DIR                       what the shared source-block cache saves (DIR: scratch)
//	mt-ondemand shade z/x/y NXxNY out.png      a block of tiles as one hillshade, to look for seams
//
// It keeps nothing on disk and reads only the windows it needs.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
)

type area struct {
	name string
	note string
	box  elevation.BBox
}

var areas = []area{
	{"texas", "3DEP 1/3″ (Hill Country, Austin)", elevation.BBox{-99.6, 29.6, -97.2, 31.4}},
	{"calgary", "Alberta, HRDEM where present (Calgary and foothills)", elevation.BBox{-114.9, 50.6, -113.6, 51.4}},
	{"alberta-north", "Alberta, mostly MRDEM (north of Fort McMurray)", elevation.BBox{-113.5, 57.6, -110.5, 59.0}},
	{"alps", "Copernicus GLO-30 (Swiss Alps)", elevation.BBox{7.0, 46.0, 9.0, 47.0}},
	{"monterrey", "GLO-30 inside the 3DEP coverage box (Mexico)", elevation.BBox{-101.0, 25.2, -99.5, 26.4}},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mt-ondemand measure|profile|tile …")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch os.Args[1] {
	case "measure":
		fs := flag.NewFlagSet("measure", flag.ExitOnError)
		n := fs.Int("n", 20, "tiles per area and zoom")
		zooms := fs.String("zooms", "10,12,14", "zooms")
		only := fs.String("areas", "", "comma-separated area names (default all)")
		_ = fs.Parse(os.Args[2:])
		measure(ctx, *n, *zooms, *only)
	case "profile":
		fs := flag.NewFlagSet("profile", flag.ExitOnError)
		runs := fs.Int("runs", 3, "paths per area")
		_ = fs.Parse(os.Args[2:])
		profiles(ctx, *runs)
	case "viewport":
		fs := flag.NewFlagSet("viewport", flag.ExitOnError)
		zooms := fs.String("zooms", "10,12,14", "zooms")
		only := fs.String("areas", "", "comma-separated area names (default all)")
		runs := fs.Int("runs", 5, "viewports per area and zoom")
		_ = fs.Parse(os.Args[2:])
		viewports(ctx, *zooms, *only, *runs)
	case "shade":
		var z, x, y, nx, ny int
		if len(os.Args) < 5 {
			fatal(fmt.Errorf("usage: mt-ondemand shade z/x/y NXxNY out.png"))
		}
		if _, err := fmt.Sscanf(os.Args[2], "%d/%d/%d", &z, &x, &y); err != nil {
			fatal(err)
		}
		if _, err := fmt.Sscanf(os.Args[3], "%dx%d", &nx, &ny); err != nil {
			fatal(err)
		}
		if err := shade(ctx, z, x, y, nx, ny, os.Args[4]); err != nil {
			fatal(err)
		}
	case "contours":
		if len(os.Args) < 4 {
			fatal(fmt.Errorf("usage: mt-ondemand contours lon,lat SCRATCHDIR"))
		}
		var lon, lat float64
		if _, err := fmt.Sscanf(os.Args[2], "%f,%f", &lon, &lat); err != nil {
			fatal(err)
		}
		if err := contourCost(ctx, lon, lat, os.Args[3]); err != nil {
			fatal(err)
		}
	case "seam":
		if len(os.Args) < 5 {
			fatal(fmt.Errorf("usage: mt-ondemand seam lon,lat SCRATCHDIR out-prefix"))
		}
		var lon, lat float64
		if _, err := fmt.Sscanf(os.Args[2], "%f,%f", &lon, &lat); err != nil {
			fatal(err)
		}
		if err := seam(ctx, lon, lat, os.Args[3], os.Args[4]); err != nil {
			fatal(err)
		}
	case "share":
		if len(os.Args) < 3 {
			fatal(fmt.Errorf("usage: mt-ondemand share <empty scratch directory>"))
		}
		share(ctx, os.Args[2])
	case "tile":
		fs := flag.NewFlagSet("tile", flag.ExitOnError)
		out := fs.String("out", "", "write the PNG here")
		_ = fs.Parse(os.Args[2:])
		var z, x, y int
		if _, err := fmt.Sscanf(fs.Arg(0), "%d/%d/%d", &z, &x, &y); err != nil {
			fatal(err)
		}
		up := ondemand.NewUpstream(ondemand.UpstreamOptions{})
		s := ondemand.New(ondemand.Options{Upstream: up})
		t0 := time.Now()
		r, err := s.Render(ctx, z, x, y)
		if err != nil {
			fatal(err)
		}
		png, err := r.Encode(z)
		if err != nil {
			fatal(err)
		}
		req, by, _ := up.Counters()
		lo, hi := minMax(r.Elev)
		fmt.Printf("%d/%d/%d: %s, %d requests, %d bytes fetched, %d bytes png, filled %d/65536, sources %v, elevation %.1f..%.1f m\n",
			z, x, y, time.Since(t0).Round(time.Millisecond), req, by, len(png), r.Filled, sourceMix(r), lo, hi)
		for _, f := range r.Files {
			fmt.Println("  candidate", f.URL)
		}
		// A few pixels with their positions, to compare with gdallocationinfo.
		for _, p := range [][2]int{{128, 128}, {40, 200}, {220, 30}} {
			lon, lat := elevation.WorldPixelToLonLat(float64(x*256+p[0])+0.5, float64(y*256+p[1])+0.5, z)
			fmt.Printf("  pixel %d,%d at %.6f %.6f = %.2f m\n", p[0], p[1], lon, lat, r.Elev[p[1]*256+p[0]])
		}
		if *out != "" {
			if err := os.WriteFile(*out, png, 0o644); err != nil {
				fatal(err)
			}
		}
	default:
		fatal(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mt-ondemand:", err)
	os.Exit(1)
}

func minMax(e []float32) (lo, hi float32) {
	lo, hi = float32(math.Inf(1)), float32(math.Inf(-1))
	for _, v := range e {
		if v == v {
			lo, hi = min(lo, v), max(hi, v)
		}
	}
	return
}

func sourceMix(r *ondemand.Rendered) map[string]int {
	m := map[string]int{}
	for _, s := range r.Src {
		if s == 0 {
			m["none"]++
		} else {
			m[elevation.KnownSources[s-1].ID]++
		}
	}
	return m
}

type sample struct {
	d        time.Duration
	bytes    int64
	requests int64
	png      int
}

func summarise(label string, ss []sample, mix map[string]int) {
	if len(ss) == 0 {
		return
	}
	ds := make([]float64, len(ss))
	var by, rq, png float64
	bs := make([]float64, len(ss))
	for i, s := range ss {
		ds[i] = float64(s.d.Milliseconds())
		bs[i] = float64(s.bytes)
		by += float64(s.bytes)
		rq += float64(s.requests)
		png += float64(s.png)
	}
	sort.Float64s(ds)
	sort.Float64s(bs)
	n := float64(len(ss))
	var total int
	for _, v := range mix {
		total += v
	}
	var parts []string
	for k, v := range mix {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", k, 100*float64(v)/float64(total)))
	}
	sort.Strings(parts)
	fmt.Printf("| %s | %d | %.0f | %.0f | %.0f | %.0f | %.0f | %.1f | %.0f | %s |\n", label, len(ss),
		pct(ds, 0.5), pct(ds, 0.95), ds[len(ds)-1], by/n/1024, pct(bs, 0.95)/1024, rq/n, png/n/1024, strings.Join(parts, ", "))
}

func pct(sorted []float64, p float64) float64 {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(len(sorted)-1, i))]
}

func measure(ctx context.Context, n int, zooms, only string) {
	up := ondemand.NewUpstream(ondemand.UpstreamOptions{})
	// One throwaway request per host so the first sample does not carry the
	// TLS handshake: a running server keeps connections open.
	fmt.Println("| area / zoom / state | n | p50 ms | p95 ms | max ms | mean KB fetched | p95 KB fetched | mean requests | mean PNG KB | sources |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|")
	for _, a := range areas {
		if only != "" && !strings.Contains(","+only+",", ","+a.name+",") {
			continue
		}
		for _, zs := range strings.Split(zooms, ",") {
			var z int
			if _, err := fmt.Sscanf(zs, "%d", &z); err != nil {
				fatal(err)
			}
			rng := rand.New(rand.NewPCG(uint64(z), 42)) //nolint:gosec // a repeatable sample of tiles, nothing secret
			x0, y0, x1, y1 := elevation.TileRange(z, a.box[0], a.box[1], a.box[2], a.box[3])
			seen := map[[2]int]bool{}
			var tiles [][2]int
			for len(tiles) < n && len(seen) < (x1-x0+1)*(y1-y0+1) {
				t := [2]int{x0 + rng.IntN(x1-x0+1), y0 + rng.IntN(y1-y0+1)}
				if !seen[t] {
					seen[t] = true
					tiles = append(tiles, t)
				}
			}
			// Cold: nothing known, not even which files exist or their headers.
			var cold, warm []sample
			mixC, mixW := map[string]int{}, map[string]int{}
			run := func(s *ondemand.Service, t [2]int, mix map[string]int) (sample, bool) {
				r0, b0, _ := up.Counters()
				t0 := time.Now()
				r, err := s.Render(ctx, z, t[0], t[1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s z%d %d/%d: %v\n", a.name, z, t[0], t[1], err)
					return sample{}, false
				}
				png, err := r.Encode(z)
				if err != nil {
					fatal(err)
				}
				d := time.Since(t0)
				r1, b1, _ := up.Counters()
				for k, v := range sourceMix(r) {
					mix[k] += v
				}
				return sample{d, b1 - b0, r1 - r0, len(png)}, true
			}
			// Warm the connections (not counted).
			_, _ = ondemand.New(ondemand.Options{Upstream: up}).Render(ctx, z, tiles[0][0], tiles[0][1])
			for _, t := range tiles {
				if s, ok := run(ondemand.New(ondemand.Options{Upstream: up}), t, mixC); ok {
					cold = append(cold, s)
				}
				// The same tile with the files' versions and headers known (as on a
				// server that has served the area before) but no elevation data.
				primed := ondemand.New(ondemand.Options{Upstream: up})
				if err := primed.Prime(ctx, z, t[0], t[1]); err != nil {
					fmt.Fprintln(os.Stderr, a.name, z, err)
					continue
				}
				if s, ok := run(primed, t, mixW); ok {
					warm = append(warm, s)
				}
			}
			summarise(fmt.Sprintf("%s z%d cold", a.name, z), cold, mixC)
			summarise(fmt.Sprintf("%s z%d headers known", a.name, z), warm, mixW)
		}
	}
	req, by, fail := up.Counters()
	fmt.Printf("\ntotal: %d requests, %.1f MB, %d failed attempts\n", req, float64(by)/1e6, fail)
}

// viewports times a screenful of tiles asked for the way a browser asks: a
// 4×3 block of neighbours, six at a time, nothing known beforehand.
func viewports(ctx context.Context, zooms, only string, runs int) {
	up := ondemand.NewUpstream(ondemand.UpstreamOptions{})
	fmt.Println("| viewport of 4×3 tiles, cold | n | p50 s | p95 s | max s | mean MB fetched | mean requests | first tile p50 s |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	for _, a := range areas {
		if only != "" && !strings.Contains(","+only+",", ","+a.name+",") {
			continue
		}
		for _, zs := range strings.Split(zooms, ",") {
			var z int
			if _, err := fmt.Sscanf(zs, "%d", &z); err != nil {
				fatal(err)
			}
			rng := rand.New(rand.NewPCG(uint64(z), 7)) //nolint:gosec // a repeatable sample of tiles, nothing secret
			x0, y0, x1, y1 := elevation.TileRange(z, a.box[0], a.box[1], a.box[2], a.box[3])
			_, _ = ondemand.New(ondemand.Options{Upstream: up}).Render(ctx, z, x0, y0) // warm the connections
			var total, first, mb, rq []float64
			for run := 0; run < runs; run++ {
				vx, vy := x0+rng.IntN(max(1, x1-x0-3)), y0+rng.IntN(max(1, y1-y0-2))
				s := ondemand.New(ondemand.Options{Upstream: up})
				r0, b0, _ := up.Counters()
				t0 := time.Now()
				var wg sync.WaitGroup
				var mu sync.Mutex
				var firstDone time.Duration
				sem := make(chan struct{}, 6)
				failed := false
				for dy := 0; dy < 3; dy++ {
					for dx := 0; dx < 4; dx++ {
						wg.Add(1)
						sem <- struct{}{}
						go func() {
							defer wg.Done()
							defer func() { <-sem }()
							r, err := s.Render(ctx, z, vx+dx, vy+dy)
							if err == nil {
								_, err = r.Encode(z)
							}
							mu.Lock()
							defer mu.Unlock()
							if err != nil {
								fmt.Fprintln(os.Stderr, a.name, z, err)
								failed = true
							}
							if firstDone == 0 {
								firstDone = time.Since(t0)
							}
						}()
					}
				}
				wg.Wait()
				if failed {
					continue
				}
				r1, b1, _ := up.Counters()
				total = append(total, time.Since(t0).Seconds())
				first = append(first, firstDone.Seconds())
				mb = append(mb, float64(b1-b0)/1e6)
				rq = append(rq, float64(r1-r0))
			}
			if len(total) == 0 {
				continue
			}
			sort.Float64s(total)
			sort.Float64s(first)
			fmt.Printf("| %s z%d | %d | %.2f | %.2f | %.2f | %.1f | %.0f | %.2f |\n", a.name, z, len(total),
				pct(total, 0.5), pct(total, 0.95), total[len(total)-1], mean(mb), mean(rq), pct(first, 0.5))
		}
	}
}

func mean(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(max(1, len(v)))
}

// destination returns the point d metres from p on bearing brg (degrees).
func destination(p elevation.LonLat, brg, d float64) elevation.LonLat {
	const r = 6371008.8
	rad := math.Pi / 180
	lat1, lon1 := p[1]*rad, p[0]*rad
	lat2 := math.Asin(math.Sin(lat1)*math.Cos(d/r) + math.Cos(lat1)*math.Sin(d/r)*math.Cos(brg*rad))
	lon2 := lon1 + math.Atan2(math.Sin(brg*rad)*math.Sin(d/r)*math.Cos(lat1), math.Cos(d/r)-math.Sin(lat1)*math.Sin(lat2))
	return elevation.LonLat{lon2 / rad, lat2 / rad}
}

func profiles(ctx context.Context, runs int) {
	type path struct {
		name  string
		start elevation.LonLat
		brg   float64
	}
	paths := []path{
		{"texas (3DEP)", elevation.LonLat{-99.2, 30.2}, 75},
		{"texas (3DEP)", elevation.LonLat{-98.4, 31.2}, 160},
		{"texas (3DEP)", elevation.LonLat{-97.4, 29.8}, 300},
		{"calgary west (HRDEM/MRDEM)", elevation.LonLat{-114.05, 51.05}, 265},
		{"calgary north (HRDEM/MRDEM)", elevation.LonLat{-114.0, 51.1}, 5},
		{"calgary south-east (HRDEM/MRDEM)", elevation.LonLat{-113.9, 50.9}, 120},
		{"alps (GLO-30)", elevation.LonLat{7.2, 46.2}, 70},
		{"alps (GLO-30)", elevation.LonLat{8.8, 46.9}, 250},
		{"alps (GLO-30)", elevation.LonLat{7.5, 46.9}, 110},
	}
	fmt.Println("| 100 km profile, 30 m spacing | samples | z13 tiles | seconds | MB fetched | requests | with data |")
	fmt.Println("|---|---|---|---|---|---|---|")
	up := ondemand.NewUpstream(ondemand.UpstreamOptions{})
	done := map[string]int{}
	for _, p := range paths {
		if done[p.name[:4]] >= runs {
			continue
		}
		done[p.name[:4]]++
		// 100 km at 30 m.
		var pts []elevation.LonLat
		for d := 0.0; d <= 100000; d += 30 {
			pts = append(pts, destination(p.start, p.brg, d))
		}
		// Warm the connection, then start cold.
		s := ondemand.New(ondemand.Options{Upstream: up})
		_, _ = s.Render(ctx, 13, 0, 0)
		s = ondemand.New(ondemand.Options{Upstream: up})
		r0, b0, _ := up.Counters()
		t0 := time.Now()
		type tk struct{ x, y int }
		tiles := map[tk]*ondemand.Rendered{}
		for _, q := range pts {
			px, py := elevation.LonLatToWorldPixel(q[0], q[1], ondemand.SampleZoom)
			for _, d := range [4][2]float64{{-0.5, -0.5}, {0.5, -0.5}, {-0.5, 0.5}, {0.5, 0.5}} {
				tiles[tk{int(math.Floor((px + d[0]) / 256)), int(math.Floor((py + d[1]) / 256))}] = nil
			}
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 12)
		var ferr error
		for k := range tiles {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				r, err := s.Render(ctx, ondemand.SampleZoom, k.x, k.y)
				if err == nil {
					_, err = r.Encode(ondemand.SampleZoom) // a server would also store the tile
				}
				mu.Lock()
				defer mu.Unlock()
				if err != nil && ferr == nil {
					ferr = err
				}
				tiles[k] = r
			}()
		}
		wg.Wait()
		if ferr != nil {
			fmt.Fprintln(os.Stderr, p.name, ferr)
			continue
		}
		with := 0
		for _, q := range pts {
			px, py := elevation.LonLatToWorldPixel(q[0], q[1], ondemand.SampleZoom)
			t := tiles[tk{int(math.Floor(px / 256)), int(math.Floor(py / 256))}]
			if v := t.Elev[(int(py)%256)*256+int(px)%256]; v == v {
				with++
			}
		}
		d := time.Since(t0)
		r1, b1, _ := up.Counters()
		fmt.Printf("| %s | %d | %d | %.2f | %.1f | %d | %.0f%% |\n", p.name, len(pts), len(tiles), d.Seconds(), float64(b1-b0)/1e6, r1-r0, 100*float64(with)/float64(len(pts)))
	}
}
