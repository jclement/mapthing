// Command mapthing-build runs one generation build without the server: for development and
// smoke tests, and to build on one machine what another serves (docs/operations.md). The server
// runs the same build as a job.
//
//	mapthing-build -data /data -region north-america/us/delaware [-region ...] [-refresh]
//	mapthing-build -data /data -custom de-md=-75.82,38.62,-75.70,38.70   (clipped from every extract it crosses)
//	mapthing-build -data /data -regions-file regions.json -out /builds/next [-reuse-terrain /builds/last]
//
// -regions-file takes the JSON the admin API's GET /admin/api/regions returns. With -out the
// generation is written to that directory, verified, and not made live: copy it to a server's
// <data>/generations/<id>/ and activate it there. -data still holds downloads, caches and
// scratch.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jclement/mapthing/internal/build"
	"github.com/jclement/mapthing/internal/catalog"
)

type regionFlags []string

func (r *regionFlags) String() string     { return strings.Join(*r, ",") }
func (r *regionFlags) Set(v string) error { *r = append(*r, v); return nil }

type stdoutRep struct{ start time.Time }

func (r stdoutRep) Stage(n string) { fmt.Printf("[%6.1fs] ### %s\n", time.Since(r.start).Seconds(), n) }
func (r stdoutRep) Progress(f float64, msg string) {
	fmt.Printf("[%6.1fs] %3.0f%% %s\n", time.Since(r.start).Seconds(), f*100, msg)
}
func (r stdoutRep) Logf(format string, a ...any) {
	fmt.Printf("[%6.1fs] %s\n", time.Since(r.start).Seconds(), fmt.Sprintf(format, a...))
}

func main() {
	data := flag.String("data", envOr("MAPTHING_DATA", "/data"), "data directory")
	refresh := flag.Bool("refresh", false, "re-check downloads for newer extracts")
	keep := flag.Bool("keep-work", false, "keep the work directory")
	stopAfter := flag.String("stop-after", "", "end after this stage (e.g. merge); nothing is swapped in")
	regionsFile := flag.String("regions-file", "", "JSON file of regions, as GET /admin/api/regions returns them (catalog ids and custom areas)")
	outDir := flag.String("out", "", "write the generation to this new or empty directory instead of <data>/generations/<id>; it is not made live")
	reuseTerrain := flag.String("reuse-terrain", "", "a previous generation directory: reuse its terrain (and contours) when built for the same areas and settings")
	var regions, customs regionFlags
	flag.Var(&regions, "region", "Geofabrik region id (repeatable), e.g. north-america/us/delaware")
	flag.Var(&customs, "custom", "custom box name=w,s,e,n (repeatable), clipped from the extracts covering it")
	flag.Parse()
	if len(regions) == 0 && len(customs) == 0 && *regionsFile == "" {
		fmt.Fprintln(os.Stderr, "mapthing-build: at least one -region, -custom or -regions-file is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cat := catalog.New(*data)
	if err := cat.Load(ctx); err != nil {
		fail(err)
	}
	var rs []catalog.Region
	skip := map[string]bool{}
	if *regionsFile != "" {
		var err error
		if rs, skip, err = regionsFromFile(ctx, cat, *regionsFile); err != nil {
			fail(err)
		}
		fmt.Printf("%s: %d region(s)\n", *regionsFile, len(rs))
	}
	for _, id := range regions {
		r, err := cat.Region(id)
		if err != nil {
			fail(err)
		}
		_ = cat.FillSizes(ctx, []string{id})
		if e, ok := cat.Get(id); ok {
			r.SizeHint = e.Size
		}
		rs = append(rs, r)
	}
	for _, c := range customs {
		name, box, ok := strings.Cut(c, "=")
		var bb catalog.BBox
		if !ok {
			fail(fmt.Errorf("-custom %q: want name=w,s,e,n", c))
		}
		if _, err := fmt.Sscanf(box, "%g,%g,%g,%g", &bb[0], &bb[1], &bb[2], &bb[3]); err != nil {
			fail(fmt.Errorf("-custom %q: %w", c, err))
		}
		plan, err := cat.PlanCustom(ctx, &bb, nil)
		if err != nil {
			fail(err)
		}
		r, _, err := cat.CustomRegionPlan(name, &bb, nil)
		if err != nil {
			fail(err)
		}
		fmt.Printf("custom %s: clipped from %s (%d MiB to download)\n", r.ID, plan.ParentIDs(), plan.TotalBytes>>20)
		rs = append(rs, r)
	}
	b := build.New(*data)
	m, err := b.Run(ctx, stdoutRep{time.Now()}, build.Options{Regions: rs, Refresh: *refresh, SkipRefresh: skip,
		KeepWork: *keep, StopAfter: *stopAfter, OutDir: *outDir, ReuseTerrain: *reuseTerrain})
	if m != nil {
		out, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(out))
	}
	if err != nil {
		var se *build.StageError
		if errors.As(err, &se) {
			fmt.Fprintf(os.Stderr, "--- last log lines (%s) ---\n%s\n", se.Stage, strings.Join(se.LogTail, "\n"))
		}
		fail(err)
	}
	if *outDir != "" && m != nil && m.OK {
		fmt.Printf("\ngeneration %s is in %s\nto serve it: copy the directory to <data>/generations/%s on the server, then\n"+
			"  POST /admin/api/generations/%s/activate   (or: mapthing adopt %s)\n", m.Generation, *outDir, m.Generation, m.Generation, m.Generation)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mapthing-build:", err)
	os.Exit(1)
}
