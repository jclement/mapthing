// Command mapthing-build runs one generation build without the server. It is a development and
// smoke-test tool; the server runs the same build as a job.
//
//	mapthing-build -data /data -region north-america/us/delaware [-region ...] [-refresh]
//	mapthing-build -data /data -custom de-md=-75.82,38.62,-75.70,38.70   (clipped from every extract it crosses)
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
	var regions, customs regionFlags
	flag.Var(&regions, "region", "Geofabrik region id (repeatable), e.g. north-america/us/delaware")
	flag.Var(&customs, "custom", "custom box name=w,s,e,n (repeatable), clipped from the extracts covering it")
	flag.Parse()
	if len(regions) == 0 && len(customs) == 0 {
		fmt.Fprintln(os.Stderr, "mapthing-build: at least one -region or -custom is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cat := catalog.New(*data)
	if err := cat.Load(ctx); err != nil {
		fail(err)
	}
	var rs []catalog.Region
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
	m, err := b.Run(ctx, stdoutRep{time.Now()}, build.Options{Regions: rs, Refresh: *refresh, KeepWork: *keep, StopAfter: *stopAfter})
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
