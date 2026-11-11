package build

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// countingTransport counts requests by path suffix.
type countingTransport struct {
	next  http.RoundTripper
	state atomic.Int32
	head  atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/state.txt") {
		c.state.Add(1)
	}
	if r.Method == http.MethodHead {
		c.head.Add(1)
	}
	return c.next.RoundTrip(r)
}

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
	// A clock the test moves: the publisher's answers are cached for a minute.
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	e.b.Now = func() time.Time { return clock }
	later := func() { clock = clock.Add(sourceCacheTTL + time.Second) }

	// Same replication sequence as the download: unchanged, and the extract is not fetched.
	st, err = e.b.SourceChanged(ctx, e.region)
	if err != nil || st.Changed || !strings.Contains(st.Detail, "sequence 100") || !strings.Contains(st.Detail, "2026-11-07") {
		t.Fatalf("same sequence: %+v %v", st, err)
	}
	// The publisher moves on.
	e.mu.Lock()
	e.sequence = 101
	e.mu.Unlock()
	later()
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
	later()
	st, err = e.b.SourceChanged(ctx, noFeed)
	if err != nil || !st.Changed {
		t.Fatalf("no feed, new file: %+v %v", st, err)
	}

	// A custom region that has never been clipped.
	custom := catalog.Region{ID: "custom/box", Name: "Box", BBox: catalog.BBox{-75.6, 38.8, -75.4, 39.0},
		Custom: &catalog.Custom{Parents: []catalog.Region{e.region}}}
	st, err = e.b.SourceChanged(ctx, custom)
	if err != nil || !st.Changed || st.Detail != "not clipped yet" {
		t.Fatalf("custom, no clip: %+v %v", st, err)
	}

	// The source cannot be reached: an error, not a guess either way.
	e.srv.Close()
	later()
	if st, err := e.b.SourceChanged(ctx, e.region); err == nil {
		t.Fatalf("unreachable source: %+v, want an error", st)
	}
	// And it never touched the download.
	if _, err := os.Stat(e.b.regionFile(e.region.ID)); err != nil {
		t.Error("the downloaded extract is gone")
	}
}

// Review 2, finding 1: a build downloads newer data and then fails. The download cache is
// current, the live map is not; that must still count as something to build, at every later
// check, with no need for the publisher to release anything further (or even to be reachable).
func TestSourceChangedAfterAFailedBuild(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	regions := []catalog.Region{e.region}
	if _, err := e.b.Run(ctx, &testRep{}, Options{Regions: regions}); err != nil {
		t.Fatal(err)
	}
	live := e.b.CurrentGeneration()

	// Sequence 101 is published and a refresh build fetches it, then fails in the basemap stage.
	e.mu.Lock()
	e.sequence = 101
	e.mu.Unlock()
	e.setPBF([]byte("\x00\x00\x00\x0d\x0a\x09OSMHeader delaware 101"))
	t.Setenv("MAPTHING_FAKE_FAIL", "java")
	if _, err := e.b.Run(ctx, &testRep{}, Options{Regions: regions, Refresh: true}); err == nil {
		t.Fatal("the build was meant to fail")
	}
	t.Setenv("MAPTHING_FAKE_FAIL", "")
	if e.b.CurrentGeneration() != live {
		t.Fatal("a failed build changed what is live")
	}
	if meta, err := readMeta(e.b.RegionDir(e.region.ID)); err != nil || meta.Sequence != 101 {
		t.Fatalf("test setup: the download cache should hold sequence 101: %+v %v", meta, err)
	}

	// The publisher is still at 101: the cache is "current". It is still something to build.
	for i := range 3 {
		st, err := e.b.SourceChanged(ctx, e.region)
		if err != nil || !st.Changed || !strings.Contains(st.Detail, "did not finish") {
			t.Fatalf("check %d after the failed build: %+v %v (the live map still has the old data)", i, st, err)
		}
	}
	// Even with the publisher unreachable: nothing needs asking.
	e.srv.Close()
	if st, err := e.b.SourceChanged(ctx, e.region); err != nil || !st.Changed {
		t.Fatalf("publisher down: %+v %v", st, err)
	}

	// The retry needs no download (the data is on disk) and puts it into service; after that
	// there is nothing new. (The fake publisher is gone, so a download would fail the build.)
	rep := &testRep{}
	if _, err := e.b.Run(ctx, rep, Options{Regions: regions}); err != nil {
		t.Fatalf("retry build: %v\n%s", err, strings.Join(rep.logs, "\n"))
	}
	if e.b.CurrentGeneration() == live {
		t.Fatal("the retry did not go live")
	}
	live2, _ := e.b.liveRegions()
	if meta, _ := readMeta(e.b.RegionDir(e.region.ID)); live2[e.region.ID].MD5 != meta.MD5 {
		t.Fatalf("the live generation was not built from the download: %q vs %q", live2[e.region.ID].MD5, meta.MD5)
	}
}

// Review 2, finding 3: a custom region that is not due keeps its clip when an extract it
// shares with a due region is refreshed, and is reported as having something new when its
// own turn comes.
func TestSkippedCustomRegionKeepsItsClip(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	md := e.region
	md.ID, md.Name = "north-america/us/maryland", "Maryland"
	a := catalog.Region{ID: "custom/a", Name: "A", BBox: catalog.BBox{-75.9, 38.6, -75.6, 38.8},
		Custom: &catalog.Custom{Parents: []catalog.Region{e.region, md}}}
	b := catalog.Region{ID: "custom/b", Name: "B", BBox: catalog.BBox{-75.7, 38.9, -75.4, 39.2},
		Custom: &catalog.Custom{Parents: []catalog.Region{e.region}}}
	regions := []catalog.Region{a, b}
	if _, err := e.b.Run(ctx, &testRep{}, Options{Regions: regions}); err != nil {
		t.Fatal(err)
	}
	before, err := readMeta(e.b.RegionDir(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	clipStat, _ := os.Stat(e.b.regionFile(b.ID))

	// Delaware is republished. Only A is due: B is skipped, Maryland too.
	e.mu.Lock()
	e.sequence = 101
	e.mu.Unlock()
	e.setPBF([]byte("\x00\x00\x00\x0d\x0a\x09OSMHeader delaware 101"))
	rep := &testRep{}
	m, err := e.b.Run(ctx, rep, Options{Regions: regions, Refresh: true,
		SkipRefresh: map[string]bool{b.ID: true, md.ID: true}})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rep.logs, "\n"))
	}
	if !rep.has("custom/a: clipped from") {
		t.Errorf("the due region was not re-clipped\n%s", strings.Join(rep.logs, "\n"))
	}
	if rep.has("custom/b: clipped from") || !rep.has("custom/b: keeping its clip") {
		t.Errorf("the region that was not due was re-clipped from the refreshed parent\n%s", strings.Join(rep.logs, "\n"))
	}
	after, _ := readMeta(e.b.RegionDir(b.ID))
	if after.ParentMD5 != before.ParentMD5 || after.Sequence != before.Sequence || !after.DownloadedAt.Equal(before.DownloadedAt) {
		t.Errorf("B's clip metadata changed: %+v → %+v", before, after)
	}
	if st, _ := os.Stat(e.b.regionFile(b.ID)); !st.ModTime().Equal(clipStat.ModTime()) {
		t.Error("B's clip file was rewritten")
	}
	// The manifest records what each region was really built from.
	for _, mr := range m.Regions {
		if mr.ID == b.ID && mr.Sequence != 100 {
			t.Errorf("manifest says B is sequence %d; it was built from its old clip (100)", mr.Sequence)
		}
		if mr.ID == a.ID && mr.Sequence != 101 {
			t.Errorf("manifest says A is sequence %d, want 101", mr.Sequence)
		}
	}
	// A build with no refresh at all keeps it too.
	rep = &testRep{}
	if _, err := e.b.Run(ctx, rep, Options{Regions: regions}); err != nil {
		t.Fatal(err)
	}
	if rep.has("custom/b: clipped from") {
		t.Error("a build without refresh re-clipped B")
	}
	// When B's own turn comes there is something new for it, and its refresh re-clips it.
	st, err := e.b.SourceChanged(ctx, b)
	if err != nil || !st.Changed || !strings.Contains(st.Detail, "downloaded again since it was clipped") {
		t.Fatalf("B at its own turn: %+v %v", st, err)
	}
	rep = &testRep{}
	if _, err := e.b.Run(ctx, rep, Options{Regions: regions, Refresh: true, SkipRefresh: map[string]bool{a.ID: true, md.ID: true}}); err != nil {
		t.Fatal(err)
	}
	if !rep.has("custom/b: clipped from") {
		t.Errorf("B was not re-clipped at its own refresh\n%s", strings.Join(rep.logs, "\n"))
	}
	if st, err := e.b.SourceChanged(ctx, b); err != nil || st.Changed {
		t.Errorf("B after its refresh: %+v %v", st, err)
	}
	// A changed outline is always re-clipped, due or not.
	b2 := b
	b2.BBox = catalog.BBox{-75.7, 38.9, -75.3, 39.3}
	rep = &testRep{}
	if _, err := e.b.Run(ctx, rep, Options{Regions: []catalog.Region{a, b2}}); err != nil {
		t.Fatal(err)
	}
	if !rep.has("custom/b: clipped from") {
		t.Error("a redrawn region kept its old clip")
	}
}

// Review 2, finding 6: regions that share an extract ask the publisher for it once.
func TestSourceChecksShareOneRequestPerExtract(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ct := &countingTransport{next: e.srv.Client().Transport}
	e.b.HTTP = &http.Client{Transport: ct}
	var regions []catalog.Region
	for _, id := range []string{"custom/a", "custom/b", "custom/c", "custom/d"} {
		regions = append(regions, catalog.Region{ID: id, Name: id, BBox: catalog.BBox{-75.7, 38.9, -75.4, 39.2},
			Custom: &catalog.Custom{Parents: []catalog.Region{e.region}}})
	}
	regions = append(regions, e.region)
	if _, err := e.b.Run(ctx, &testRep{}, Options{Regions: regions}); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	e.b.Now = func() time.Time { return clock }
	ct.state.Store(0)
	for range 3 { // a scheduled check and two impatient clicks
		for _, r := range regions {
			if st, err := e.b.SourceChanged(ctx, r); err != nil || st.Changed {
				t.Fatalf("%s: %+v %v", r.ID, st, err)
			}
		}
	}
	if n := ct.state.Load(); n != 1 {
		t.Errorf("15 checks of 5 regions on one extract made %d requests for its state, want 1", n)
	}
	// A minute later the answer is asked for again, once.
	clock = clock.Add(sourceCacheTTL + time.Second)
	for _, r := range regions {
		_, _ = e.b.SourceChanged(ctx, r)
	}
	if n := ct.state.Load(); n != 2 {
		t.Errorf("after the cache expired: %d requests, want 2", n)
	}
	// A failed answer is not hammered either.
	e.srv.Close()
	clock = clock.Add(sourceCacheTTL + time.Second)
	ct.state.Store(0)
	for range 4 {
		if _, err := e.b.SourceChanged(ctx, e.region); err == nil {
			t.Fatal("unreachable publisher reported no error")
		}
	}
	if n := ct.state.Load(); n != 1 {
		t.Errorf("an unreachable publisher was asked %d times in a minute, want 1", n)
	}
}
