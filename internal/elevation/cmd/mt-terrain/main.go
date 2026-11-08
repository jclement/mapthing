// Command mt-terrain builds and queries terrain archives outside the server,
// for development and verification.
//
//	mt-terrain build -bbox w,s,e,n [-id name] -out data/t/terrain.pmtiles [-work dir] [-cache dir] [-maxzoom 13]
//	mt-terrain sample -in terrain.pmtiles lon,lat [lon,lat…]
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
	"github.com/jclement/mapthing/internal/elevation"
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
		fmt.Fprintln(os.Stderr, "usage: mt-terrain build|sample …")
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
		fs.Parse(os.Args[2:])
		b, err := parseFloats(*bbox, 4)
		if err != nil {
			fail(err)
		}
		if *cache != "" {
			os.Setenv("MAPTHING_ELEVATION_CACHE", *cache)
		}
		if *maxzoom > 0 {
			os.Setenv("MAPTHING_TERRAIN_MAXZOOM", strconv.Itoa(*maxzoom))
		}
		regions := []catalog.Region{{ID: *id, BBox: catalog.BBox{b[0], b[1], b[2], b[3]}}}
		fmt.Fprintf(os.Stderr, "estimate: %.1f MB\n", float64(build.EstimateTerrainBytes(regions))/1e6)
		start := time.Now()
		if err := build.BuildTerrain(ctx, &logReporter{}, regions, *work, *out); err != nil {
			fail(err)
		}
		fmt.Fprintf(os.Stderr, "built in %s\n", time.Since(start).Round(time.Second))
	case "sample":
		fs := flag.NewFlagSet("sample", flag.ExitOnError)
		in := fs.String("in", "data/terrain/terrain.pmtiles", "archive")
		fs.Parse(os.Args[2:])
		a, err := elevation.OpenArchive(*in, 0)
		if err != nil {
			fail(err)
		}
		defer a.Close()
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
			enc.Encode(s)
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
