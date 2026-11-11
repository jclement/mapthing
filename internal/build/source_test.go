package build

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/catalog"
)

func TestSourceChanged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	regions := []catalog.Region{e.region}

	// Nothing downloaded yet: a refresh would fetch it.
	st, err := e.b.SourceChanged(ctx, e.region)
	if err != nil || !st.Changed || st.Detail != "not downloaded yet" {
		t.Fatalf("before any download: %+v %v", st, err)
	}
	if _, err := e.b.Run(ctx, &testRep{}, Options{Regions: regions}); err != nil {
		t.Fatal(err)
	}
	gets := e.pbfGets.Load()

	// Same replication sequence as the download: unchanged, and the extract is not fetched.
	st, err = e.b.SourceChanged(ctx, e.region)
	if err != nil || st.Changed || !strings.Contains(st.Detail, "sequence 100") || !strings.Contains(st.Detail, "2026-11-07") {
		t.Fatalf("same sequence: %+v %v", st, err)
	}
	// The publisher moves on.
	e.mu.Lock()
	e.sequence = 101
	e.mu.Unlock()
	st, err = e.b.SourceChanged(ctx, e.region)
	if err != nil || !st.Changed || !strings.Contains(st.Detail, "sequence 101, downloaded 100") {
		t.Fatalf("newer sequence: %+v %v", st, err)
	}
	if e.pbfGets.Load() != gets {
		t.Error("checking the source downloaded the extract")
	}

	// No update feed: the file's own validators decide.
	noFeed := e.region
	noFeed.UpdatesURL = ""
	st, err = e.b.SourceChanged(ctx, noFeed)
	if err != nil || st.Changed {
		t.Fatalf("no feed, same file: %+v %v", st, err)
	}
	e.setPBF([]byte("\x00\x00\x00\x0d\x0a\x09OSMHeader delaware v2"))
	st, err = e.b.SourceChanged(ctx, noFeed)
	if err != nil || !st.Changed {
		t.Fatalf("no feed, new file: %+v %v", st, err)
	}

	// A custom region follows the extracts it is clipped from.
	custom := catalog.Region{ID: "custom/box", Name: "Box", BBox: catalog.BBox{-75.6, 38.8, -75.4, 39.0},
		Custom: &catalog.Custom{Parents: []catalog.Region{e.region}}}
	st, err = e.b.SourceChanged(ctx, custom)
	if err != nil || !st.Changed || !strings.HasPrefix(st.Detail, e.region.ID+": ") {
		t.Fatalf("custom, parent changed: %+v %v", st, err)
	}

	// The source cannot be reached: an error, not a guess either way.
	e.srv.Close()
	if st, err := e.b.SourceChanged(ctx, e.region); err == nil {
		t.Fatalf("unreachable source: %+v, want an error", st)
	}
	// And it never touched the download.
	if _, err := os.Stat(e.b.regionFile(e.region.ID)); err != nil {
		t.Error("the downloaded extract is gone")
	}
}
