// Command mt-terrain builds and queries terrain archives outside the server,
// for development and verification.
//
//	mt-terrain build -bbox w,s,e,n [-id name] -out data/t/terrain.pmtiles [-work dir] [-cache dir] [-maxzoom 13]
//	mt-terrain sample -in terrain.pmtiles lon,lat [lon,lat…]
//	mt-terrain contours -in terrain.pmtiles -out contours.pmtiles [-minzoom 10 -maxzoom 12 -grid 256 -gridmax 512 -simplify 1.5 -blur 1]
//
// Set MAPTHING_GDAL_DOCKER=ghcr.io/osgeo/gdal:ubuntu-small-3.11.4 when GDAL
// is not installed locally.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/build"
	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/contours"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/routing"
)

type logReporter struct{ last time.Time }

func (r *logReporter) Stage(name string) { slog.Info("stage", "name", name) }
func (r *logReporter) Progress(f float64, msg string) {
	if time.Since(r.last) > 5*time.Second || f >= 1 {
		r.last = time.Now()
		slog.Info("progress", "pct", fmt.Sprintf("%.1f", f*100), "msg", msg)
	}
}
func (r *logReporter) Logf(format string, a ...any) { slog.Info(fmt.Sprintf(format, a...)) }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mt-terrain build|sample|profile|contours …")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch os.Args[1] {
	case "build":
		fs := flag.NewFlagSet("build", flag.ExitOnError)
		bbox := fs.String("bbox", "", "w,s,e,n")
		id := fs.String("id", "custom", "region id")
		out := fs.String("out", "data/terrain/terrain.pmtiles", "output archive")
		work := fs.String("work", "data/terrain/work", "work dir")
		cache := fs.String("cache", "", "cache dir (default $MAPTHING_DATA/elevation/cache)")
		maxzoom := fs.Int("maxzoom", 0, "force max zoom")
		_ = fs.Parse(os.Args[2:])
		b, err := parseFloats(*bbox, 4)
		if err != nil {
			fail(err)
		}
		if *cache != "" {
			_ = os.Setenv("MAPTHING_ELEVATION_CACHE", *cache)
		}
		if *maxzoom > 0 {
			_ = os.Setenv("MAPTHING_TERRAIN_MAXZOOM", strconv.Itoa(*maxzoom))
		}
		regions := []catalog.Region{{ID: *id, BBox: catalog.BBox{b[0], b[1], b[2], b[3]}}}
		fmt.Fprintf(os.Stderr, "estimate: %.1f MB\n", float64(build.EstimateTerrainBytes(regions))/1e6)
		start := time.Now()
		if err := build.BuildTerrain(ctx, &logReporter{}, regions, *work, *out); err != nil {
			fail(err)
		}
		fmt.Fprintf(os.Stderr, "built in %s\n", time.Since(start).Round(time.Second))
	case "contours":
		fs := flag.NewFlagSet("contours", flag.ExitOnError)
		in := fs.String("in", "", "terrain archive")
		out := fs.String("out", "contours.pmtiles", "output archive")
		var o contours.Options
		fs.IntVar(&o.MinZoom, "minzoom", 0, "min zoom (default 10)")
		fs.IntVar(&o.MaxZoom, "maxzoom", 0, "max zoom (default 12)")
		fs.IntVar(&o.Grid, "grid", 0, "grid pixels per tile below max zoom (default 256)")
		fs.IntVar(&o.GridMax, "gridmax", 0, "grid pixels per tile at max zoom (default 512)")
		fs.Float64Var(&o.Simplify, "simplify", 0, "simplify tolerance, tile units of 4096 (default 1.5)")
		fs.Float64Var(&o.MinRing, "minring", 0, "drop closed lines shorter than this, grid px (default 8)")
		fs.IntVar(&o.Blur, "blur", 0, "smoothing passes (default 1, -1 none)")
		_ = fs.Parse(os.Args[2:])
		o.Progress = (&logReporter{}).Progress
		res, err := contours.Build(ctx, *in, *out, o)
		if err != nil {
			fail(err)
		}
		st, err := os.Stat(*out)
		if err != nil {
			fail(err)
		}
		j, _ := json.Marshal(res)
		fmt.Printf("%s\n%s: %d bytes\n", j, *out, st.Size())
	case "sample":
		fs := flag.NewFlagSet("sample", flag.ExitOnError)
		in := fs.String("in", "data/terrain/terrain.pmtiles", "archive")
		_ = fs.Parse(os.Args[2:])
		a, err := elevation.OpenArchive(*in, 0)
		if err != nil {
			fail(err)
		}
		defer func() { _ = a.Close() }()
		enc := json.NewEncoder(os.Stdout)
		for _, arg := range fs.Args() {
			p, err := parseFloats(arg, 2)
			if err != nil {
				fail(err)
			}
			s, err := a.Sample(p[0], p[1])
			if err != nil {
				fail(err)
			}
			_ = enc.Encode(s)
		}
	case "profile":
		// Prints gain/loss for one path under several sampling and smoothing settings.
		fs := flag.NewFlagSet("profile", flag.ExitOnError)
		in := fs.String("in", "data/terrain/terrain.pmtiles", "archive")
		polyFile := fs.String("polyline", "", "file holding an encoded polyline (precision 5) instead of lon,lat args")
		valhalla := fs.String("valhalla", "", "Valhalla URL: also compare bridges=deck (e.g. http://127.0.0.1:8002)")
		_ = fs.Parse(os.Args[2:])
		a, err := elevation.OpenArchive(*in, 0)
		if err != nil {
			fail(err)
		}
		defer func() { _ = a.Close() }()
		var path []elevation.LonLat
		if *polyFile != "" {
			b, err := os.ReadFile(*polyFile)
			if err != nil {
				fail(err)
			}
			if path, err = elevation.DecodePolyline(strings.TrimSpace(string(b)), 5); err != nil {
				fail(err)
			}
		}
		for _, arg := range fs.Args() {
			p, err := parseFloats(arg, 2)
			if err != nil {
				fail(err)
			}
			path = append(path, elevation.LonLat{p[0], p[1]})
		}
		fmt.Printf("%-26s %8s %8s %8s %8s %8s\n", "sampling / smoothing", "samples", "gain", "loss", "raw gain", "raw loss")
		for _, sm := range []struct {
			name string
			o    elevation.ProfileOptions
		}{
			{"30 m, hyst 2 (old)", elevation.ProfileOptions{IntervalM: 30, SmoothWindowM: -1, HysteresisM: 2}},
			{"30 m, median 100 + h2", elevation.ProfileOptions{IntervalM: 30, SmoothWindowM: 100, HysteresisM: 2}},
			{"30 m, median 150 + h2", elevation.ProfileOptions{IntervalM: 30, SmoothWindowM: 150, HysteresisM: 2}},
			{"30 m, median 200 + h2", elevation.ProfileOptions{IntervalM: 30, SmoothWindowM: 200, HysteresisM: 2}},
			{"30 m, median 300 + h2", elevation.ProfileOptions{IntervalM: 30, SmoothWindowM: 300, HysteresisM: 2}},
			{"10 m, hyst 2 (old)", elevation.ProfileOptions{IntervalM: 10, SmoothWindowM: -1, HysteresisM: 2}},
			{"10 m, default", elevation.ProfileOptions{IntervalM: 10}},
			{"30 m, default", elevation.ProfileOptions{IntervalM: 30}},
			{"100 m, default", elevation.ProfileOptions{IntervalM: 100}},
			{"256 samples, raw", elevation.ProfileOptions{Samples: 256, SmoothWindowM: -1, HysteresisM: -1}},
			{"256 samples, hyst 2 (old)", elevation.ProfileOptions{Samples: 256, SmoothWindowM: -1, HysteresisM: 2}},
			{"256 samples, default", elevation.ProfileOptions{Samples: 256}},
			{"512 samples, raw", elevation.ProfileOptions{Samples: 512, SmoothWindowM: -1, HysteresisM: -1}},
		} {
			p, err := elevation.ProfileArchive(ctx, a, nil, path, sm.o)
			if err != nil {
				fail(err)
			}
			fmt.Printf("%-26s %8d %8.1f %8.1f %8.1f %8.1f\n", sm.name, len(p.Samples), p.GainM, p.LossM, p.GainRawM, p.LossRawM)
		}
		if *valhalla != "" {
			finder := routing.New(*valhalla)
			for _, sm := range []struct {
				name string
				o    elevation.ProfileOptions
			}{
				{"30 m, default, ground", elevation.ProfileOptions{IntervalM: 30}},
				{"30 m, default, deck", elevation.ProfileOptions{IntervalM: 30, Bridges: elevation.BridgesDeck}},
				{"30 m, hyst 2 only, deck", elevation.ProfileOptions{IntervalM: 30, SmoothWindowM: -1, Bridges: elevation.BridgesDeck}},
				{"256 samples, raw, deck", elevation.ProfileOptions{Samples: 256, SmoothWindowM: -1, HysteresisM: -1, Bridges: elevation.BridgesDeck}},
			} {
				p, err := elevation.ProfileArchive(ctx, a, finder, path, sm.o)
				if err != nil {
					fail(err)
				}
				var bridges, tunnels int
				var onStruct float64
				for _, sp := range p.StructureSpans {
					if sp.Kind == "tunnel" {
						tunnels++
					} else {
						bridges++
					}
					onStruct += sp.EndM - sp.StartM
				}
				fmt.Printf("%-26s %8d %8.1f %8.1f %8.1f %8.1f  mode=%s spans=%d bridges/%d tunnels (%.1f km) %s\n",
					sm.name, len(p.Samples), p.GainM, p.LossM, p.GainRawM, p.LossRawM, p.Bridges, bridges, tunnels, onStruct/1000, p.Warning)
			}
		}
	default:
		fail(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func parseFloats(s string, n int) ([]float64, error) {
	parts := strings.Split(s, ",")
	if len(parts) != n {
		return nil, fmt.Errorf("want %d comma-separated numbers, got %q", n, s)
	}
	out := make([]float64, n)
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mt-terrain:", err)
	os.Exit(1)
}
