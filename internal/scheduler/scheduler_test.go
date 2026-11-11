package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/store"
)

type fakeQ struct {
	calls  []string
	params []map[string]any
	err    error
	busy   bool
}

func (f *fakeQ) Enqueue(_ context.Context, name string, params any, opt jobs.EnqueueOptions) (*store.JobRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, name+":"+opt.Trigger)
	b, _ := json.Marshal(params)
	var p map[string]any
	_ = json.Unmarshal(b, &p)
	f.params = append(f.params, p)
	return &store.JobRow{ID: 7}, nil
}

func (f *fakeQ) Busy(context.Context, string) bool { return f.busy }

// refreshed returns the regions the n-th queued build refreshes.
func (f *fakeQ) refreshed(n int) string {
	var ids []string
	for _, v := range f.params[n]["refresh_regions"].([]any) {
		ids = append(ids, v.(string))
	}
	return strings.Join(ids, ",")
}

type fakeChecker struct {
	mu      sync.Mutex
	changed map[string]bool
	fail    map[string]bool
	asked   []string
	// during runs inside each check (a slow publisher: tests move the clock in it).
	during func(id string)
}

func (c *fakeChecker) CheckSource(_ context.Context, id string) (SourceCheck, error) {
	c.mu.Lock()
	c.asked = append(c.asked, id)
	during := c.during
	c.mu.Unlock()
	if during != nil {
		during(id)
	}
	if c.fail[id] {
		return SourceCheck{}, errors.New("geofabrik unreachable")
	}
	if c.changed[id] {
		return SourceCheck{Changed: true, Detail: "newer data published"}, nil
	}
	return SourceCheck{Detail: "the latest"}, nil
}

type env struct {
	t   *testing.T
	st  *store.Store
	q   *fakeQ
	chk *fakeChecker
	s   *Scheduler
	now time.Time
}

// newEnv starts a scheduler at Wednesday 2026-11-04 12:00 UTC with the default weekly schedule.
func newEnv(t *testing.T, regions ...string) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{t: t, st: st, q: &fakeQ{}, chk: &fakeChecker{changed: map[string]bool{}, fail: map[string]bool{}},
		now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	e.s = New(st, e.q)
	e.s.Checker = e.chk
	e.s.now = func() time.Time { return e.now }
	for _, id := range regions {
		if err := st.UpsertRegion(ctx, &store.Region{ID: id, Name: strings.ToUpper(id[:1]) + id[1:], Refresh: true}); err != nil {
			t.Fatal(err)
		}
		// UpsertRegion stamps the wall clock; put the region on the test's clock.
		_ = st.SetRegionHandled(ctx, id, e.now)
	}
	cctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	if err := e.s.Start(cctx); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) policy(id, mode, cron, window string, onlyChanged bool) {
	e.t.Helper()
	if err := e.st.SetRegionRefreshPolicy(context.Background(), id, mode, cron, window, onlyChanged); err != nil {
		e.t.Fatal(err)
	}
	_ = e.st.SetRegionHandled(context.Background(), id, e.now)
}

func (e *env) at(y int, m time.Month, d, h, min int) string {
	e.now = time.Date(y, m, d, h, min, 0, 0, time.UTC)
	return e.s.Tick(context.Background())
}

func (e *env) region(id string) *store.Region {
	e.t.Helper()
	r, err := e.st.Region(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) lastHistory() store.HistoryEntry {
	e.t.Helper()
	h, _ := e.st.History(context.Background(), "schedule", 1)
	if len(h) == 0 {
		return store.HistoryEntry{}
	}
	return h[0]
}

func TestNextRun(t *testing.T) {
	from := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) // a Wednesday
	runs, err := NextRuns(DefaultCron, from, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)}
	for i := range want {
		if !runs[i].Equal(want[i]) {
			t.Errorf("run %d = %v, want %v", i, runs[i], want[i])
		}
	}
	if _, err := Parse("61 * * * *"); err == nil {
		t.Error("expected invalid cron")
	}
	if _, err := Parse("@daily"); err != nil {
		t.Errorf("@daily: %v", err)
	}
}

// The default schedule drives every region that has no setting of its own, as the single
// schedule did before regions had settings.
func TestDefaultScheduleRefreshesWhatChanged(t *testing.T) {
	e := newEnv(t, "texas", "alberta")
	if st := e.s.State(); !st.Enabled || st.Cron != DefaultCron || st.NextRun == nil || !st.NextRun.Equal(time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("state %+v", st)
	}
	// Not due yet: nothing is checked, nothing built.
	if msg := e.at(2026, 11, 7, 23, 0); msg != "" || len(e.chk.asked) != 0 || len(e.q.calls) != 0 {
		t.Fatalf("before the schedule time: %q, asked %v, queued %v", msg, e.chk.asked, e.q.calls)
	}
	// Sunday 03:00: both are due; only Texas has something new.
	e.chk.changed["texas"] = true
	msg := e.at(2026, 11, 8, 3, 0)
	if len(e.q.calls) != 1 || e.q.calls[0] != "build:schedule" || e.q.refreshed(0) != "texas" || e.q.params[0]["refresh"] != true {
		t.Fatalf("queued %v %v (%q)", e.q.calls, e.q.params, msg)
	}
	if !strings.Contains(msg, "queued for Texas") || !strings.Contains(msg, "unchanged: Alberta") {
		t.Errorf("history message %q", msg)
	}
	if r := e.region("alberta"); r.CheckResult != "unchanged" || r.CheckedAt == nil || r.RefreshHandledAt == nil || !r.RefreshHandledAt.Equal(e.now) {
		t.Errorf("alberta after the check: %+v", r)
	}
	if r := e.region("texas"); r.CheckResult != "changed" {
		t.Errorf("texas after the check: %+v", r)
	}
	// A minute later nothing is due any more: one build, not one a minute.
	if msg := e.at(2026, 11, 8, 3, 1); msg != "" || len(e.q.calls) != 1 {
		t.Fatalf("second tick: %q, queued %v", msg, e.q.calls)
	}
	if st := e.s.State(); !st.NextRun.Equal(time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)) || st.LastRun == nil {
		t.Errorf("next run after the refresh: %+v", st)
	}
}

func TestNothingNewMeansNoBuild(t *testing.T) {
	e := newEnv(t, "texas", "alberta")
	msg := e.at(2026, 11, 8, 3, 0)
	if len(e.q.calls) != 0 {
		t.Fatalf("a build was queued with nothing new: %v", e.q.params)
	}
	if !strings.Contains(msg, "nothing new") || !strings.Contains(msg, "no build") {
		t.Errorf("message %q", msg)
	}
	h := e.lastHistory()
	if !h.OK || h.Message != msg || h.JobID != nil {
		t.Errorf("history %+v", h)
	}
	// It is recorded once, and the regions wait for their next schedule time.
	if msg := e.at(2026, 11, 8, 3, 1); msg != "" {
		t.Errorf("recorded again: %q", msg)
	}
	e.chk.changed["alberta"] = true
	if e.at(2026, 11, 14, 12, 0); len(e.q.calls) != 0 {
		t.Error("built before the next schedule time")
	}
	if e.at(2026, 11, 15, 3, 0); len(e.q.calls) != 1 || e.q.refreshed(0) != "alberta" {
		t.Errorf("next week: %v", e.q.params)
	}
}

func TestPerRegionSettings(t *testing.T) {
	e := newEnv(t, "daily", "monthly", "never", "custom", "always", "followers")
	e.policy("daily", ModeDaily, "", "", true)
	e.policy("monthly", ModeMonthly, "", "", true)
	e.policy("never", ModeNever, "", "", true)
	e.policy("custom", ModeCron, "30 14 * * 3", "", true) // Wednesdays 14:30
	e.policy("always", ModeDaily, "", "", false)          // whether or not the source changed
	for _, id := range []string{"daily", "monthly", "never", "custom", "followers"} {
		e.chk.changed[id] = true
	}

	// Wednesday 14:30: only the custom cron is due.
	if e.at(2026, 11, 4, 14, 30); len(e.q.calls) != 1 || e.q.refreshed(0) != "custom" {
		t.Fatalf("custom cron: %v", e.q.params)
	}
	// Thursday 03:00: the two daily regions; "always" is not even checked.
	e.chk.asked = nil
	if e.at(2026, 11, 5, 3, 0); len(e.q.calls) != 2 || e.q.refreshed(1) != "always,daily" {
		t.Fatalf("daily: %v", e.q.params)
	}
	if strings.Join(e.chk.asked, ",") != "daily" {
		t.Errorf("checked %v; a region set to refresh regardless needs no check", e.chk.asked)
	}
	// Sunday 03:00: daily again, plus the one following the weekly default.
	if e.at(2026, 11, 8, 3, 0); e.q.refreshed(2) != "always,daily,followers" {
		t.Fatalf("sunday: %v", e.q.params[2])
	}
	// 1 December 03:00: monthly joins. "never" never does.
	e.at(2026, 11, 30, 3, 0)
	if e.at(2026, 12, 1, 3, 0); !strings.Contains(e.q.refreshed(len(e.q.params)-1), "monthly") {
		t.Fatalf("1 December: %v", e.q.params[len(e.q.params)-1])
	}
	for i := range e.q.params {
		if strings.Contains(e.q.refreshed(i), "never") {
			t.Errorf("build %d refreshes a region set to never", i)
		}
	}
	if r := e.region("never"); r.CheckedAt != nil {
		t.Errorf("a never region was checked: %+v", r)
	}
}

func TestDefaultScheduleOffStopsOnlyItsFollowers(t *testing.T) {
	e := newEnv(t, "followers", "daily")
	e.policy("daily", ModeDaily, "", "", false)
	if err := e.s.Set(context.Background(), DefaultCron, false); err != nil {
		t.Fatal(err)
	}
	if e.at(2026, 11, 8, 3, 0); len(e.q.calls) != 1 || e.q.refreshed(0) != "daily" {
		t.Fatalf("with the default off: %v", e.q.params)
	}
	def, _ := e.st.Schedule(context.Background())
	if st := StatusFor(e.region("followers"), def, e.now); st.NextDue != nil || st.Policy.Cron != "" {
		t.Errorf("a follower of a disabled default has a next refresh: %+v", st)
	}
}

func TestBuildWindow(t *testing.T) {
	e := newEnv(t, "texas", "alberta")
	ctx := context.Background()
	// Default: only between 22:00 and 06:00. Alberta has its own window, in the afternoon.
	if err := e.s.SetDefaults(ctx, "22:00-06:00", false); err != nil {
		t.Fatal(err)
	}
	e.policy("texas", ModeCron, "0 12 * * *", "", false)              // due at noon, default window
	e.policy("alberta", ModeCron, "0 12 * * *", "13:00-15:00", false) // due at noon, own window
	def, _ := e.st.Schedule(ctx)

	if e.at(2026, 11, 5, 12, 0); len(e.q.calls) != 0 {
		t.Fatalf("built outside every window: %v", e.q.params)
	}
	st := StatusFor(e.region("texas"), def, e.now)
	if !st.Due || !st.Waiting || !st.NextBuild.Equal(time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("texas waiting for its window: %+v next build %v", st, st.NextBuild)
	}
	if e.at(2026, 11, 5, 13, 0); len(e.q.calls) != 1 || e.q.refreshed(0) != "alberta" {
		t.Fatalf("13:00: %v", e.q.params)
	}
	if e.at(2026, 11, 5, 21, 59); len(e.q.calls) != 1 {
		t.Fatal("texas built a minute before its window")
	}
	if e.at(2026, 11, 5, 22, 0); len(e.q.calls) != 2 || e.q.refreshed(1) != "texas" {
		t.Fatalf("22:00: %v", e.q.params)
	}
	// Through midnight: still open at 05:59, closed at 06:00.
	w, _ := ParseWindow("22:00-06:00")
	for at, want := range map[string]bool{"21:59": false, "22:00": true, "23:59": true, "00:00": true, "05:59": true, "06:00": false, "12:00": false} {
		tm, _ := time.Parse("15:04", at)
		if got := w.Contains(tm); got != want {
			t.Errorf("22:00-06:00 contains %s = %v, want %v", at, got, want)
		}
	}
	for _, bad := range []string{"22-06", "25:00-06:00", "22:00", "03:00-03:00", "aa:bb-cc:dd", "22:60-23:00"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("window %q accepted", bad)
		}
	}
	if w, err := ParseWindow(" 9:00-17:30 "); err != nil || w.String() != "09:00-17:30" {
		t.Errorf("9:00-17:30 → %q, %v", w.String(), err)
	}
}

func TestBusyBuildLeavesRegionsDue(t *testing.T) {
	e := newEnv(t, "texas")
	e.chk.changed["texas"] = true
	e.q.busy = true
	if msg := e.at(2026, 11, 8, 3, 0); msg != "" || len(e.q.calls) != 0 || len(e.chk.asked) != 0 {
		t.Fatalf("while a build runs: %q %v %v", msg, e.q.calls, e.chk.asked)
	}
	// The build finished an hour later: the region is still due and is handled then.
	e.q.busy = false
	if e.at(2026, 11, 8, 4, 0); len(e.q.calls) != 1 || e.q.refreshed(0) != "texas" {
		t.Fatalf("after the build: %v", e.q.params)
	}
}

func TestCheckFailureIsRetriedNotBuilt(t *testing.T) {
	e := newEnv(t, "texas", "alberta")
	e.chk.fail["texas"] = true
	msg := e.at(2026, 11, 8, 3, 0)
	if len(e.q.calls) != 0 || !strings.Contains(msg, "nothing new") || !strings.Contains(msg, "could not check Texas") {
		t.Fatalf("%q %v", msg, e.q.calls)
	}
	if r := e.region("texas"); r.CheckResult != "error" || !strings.Contains(r.CheckDetail, "unreachable") {
		t.Errorf("texas: %+v", r)
	}
	// Not asked again every minute.
	e.chk.asked = nil
	if e.at(2026, 11, 8, 3, 30); len(e.chk.asked) != 0 {
		t.Errorf("retried after 30 minutes: %v", e.chk.asked)
	}
	// An hour on the source answers, with news: the build happens then, not next week.
	e.chk.fail["texas"] = false
	e.chk.changed["texas"] = true
	if e.at(2026, 11, 8, 4, 1); len(e.q.calls) != 1 || e.q.refreshed(0) != "texas" {
		t.Fatalf("after the retry: %v", e.q.params)
	}
	// Only a failure, nothing else due: recorded as not OK.
	e2 := newEnv(t, "texas")
	e2.chk.fail["texas"] = true
	e2.at(2026, 11, 8, 3, 0)
	if h := e2.lastHistory(); h.OK || !strings.Contains(h.Message, "could not check") {
		t.Errorf("history %+v", h)
	}
}

// A database from before regions had refresh settings: the old per-region switch and the one
// schedule carry over, and nothing fires just because the server was upgraded.
func TestMigrationFromTheSingleSchedule(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	// What migration 0003 does to rows written by the old schema: the columns it added hold
	// their defaults, except that refresh = 0 became mode never.
	for id, refresh := range map[string]int{"on": 1, "off": 0} {
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO regions (id, name, refresh, added_at, updated_at) VALUES (?, ?, ?, 1, 1)`, id, id, refresh); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE regions SET refresh_mode = 'never' WHERE refresh = 0`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSchedule(ctx, "0 4 * * 1", true); err != nil { // the operator's own schedule
		t.Fatal(err)
	}
	q := &fakeQ{}
	s := New(st, q)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := s.Start(cctx); err != nil {
		t.Fatal(err)
	}
	on, _ := st.Region(ctx, "on")
	off, _ := st.Region(ctx, "off")
	if on.RefreshMode != ModeDefault || !on.Refresh || !on.RefreshOnlyChanged || on.RefreshHandledAt == nil || !on.RefreshHandledAt.Equal(now) {
		t.Errorf("region that was on: %+v", on)
	}
	if off.RefreshMode != ModeNever || off.Refresh {
		t.Errorf("region that was off: %+v", off)
	}
	def, _ := st.Schedule(ctx)
	if def.Cron != "0 4 * * 1" || !def.Enabled || !def.OnlyChanged || def.Window != "" {
		t.Errorf("schedule after the migration: %+v", def)
	}
	// Nothing is due on start; the next refresh is the operator's own next schedule time.
	if msg := s.Tick(ctx); msg != "" || len(q.calls) != 0 {
		t.Fatalf("a refresh fired at upgrade: %q", msg)
	}
	if stt := s.State(); !stt.NextRun.Equal(time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("next run %v", stt.NextRun)
	}
	// The old switch still works both ways.
	if err := st.SetRegionRefresh(ctx, "off", true); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Region(ctx, "off"); r.RefreshMode != ModeDefault {
		t.Errorf("switched back on: mode %q", r.RefreshMode)
	}
	if err := st.SetRegionRefresh(ctx, "on", false); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Region(ctx, "on"); r.RefreshMode != ModeNever || r.Refresh {
		t.Errorf("switched off: %+v", r)
	}
}

func TestCheckAllRecordsWithoutBuilding(t *testing.T) {
	e := newEnv(t, "texas", "alberta", "pinned")
	e.policy("pinned", ModeNever, "", "", true)
	e.chk.changed["texas"] = true
	e.chk.fail["alberta"] = true
	if asked, err := e.s.CheckAll(context.Background()); err != nil || !asked {
		t.Fatal(asked, err)
	}
	if len(e.q.calls) != 0 {
		t.Fatal("a check started a build")
	}
	if e.region("texas").CheckResult != "changed" || e.region("alberta").CheckResult != "error" || e.region("pinned").CheckedAt != nil {
		t.Errorf("results: %q %q %v", e.region("texas").CheckResult, e.region("alberta").CheckResult, e.region("pinned").CheckedAt)
	}
	// Checking does not use up the region's turn.
	if e.at(2026, 11, 8, 3, 0); len(e.q.calls) != 1 || e.q.refreshed(0) != "texas" {
		t.Errorf("after a manual check the schedule still runs: %v", e.q.params)
	}
}

// A region added after start has its clock started at the first tick, not a refresh.
func TestNewRegionWaitsForItsFirstScheduleTime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.st.DB.ExecContext(ctx, `INSERT INTO regions (id, name, added_at, updated_at) VALUES ('late', 'Late', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	e.chk.changed["late"] = true
	if msg := e.at(2026, 11, 9, 12, 0); msg != "" || len(e.q.calls) != 0 {
		t.Fatalf("a new region was refreshed at once: %q", msg)
	}
	if e.at(2026, 11, 15, 3, 0); len(e.q.calls) != 1 {
		t.Fatal("not refreshed at its first schedule time")
	}
}

// Review 2, finding 2: an expression whose fields are valid but that never occurs is refused,
// and one already in the database is unschedulable, never "overdue".
func TestImpossibleCronNeverFires(t *testing.T) {
	for _, bad := range []string{"0 0 30 2 *", "0 0 31 4 *", "TZ=America/Edmonton 0 3 * * *", "CRON_TZ=UTC 0 3 * * *", "tz=UTC 0 3 * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if _, err := Parse("0 0 29 2 *"); err != nil { // leap days do occur
		t.Errorf("29 February refused: %v", err)
	}
	if runs, err := NextRuns("0 0 30 2 *", time.Now(), 3); err == nil || len(runs) != 0 {
		t.Errorf("preview of an impossible expression: %v %v", runs, err)
	}

	e := newEnv(t, "texas", "alberta")
	ctx := context.Background()
	if err := e.s.Set(ctx, "0 0 30 2 *", true); err == nil {
		t.Error("the default schedule accepted an impossible expression")
	}
	// Written by an older version, or by hand: straight into the database.
	if _, err := e.st.DB.ExecContext(ctx, `UPDATE regions SET refresh_mode = 'cron', refresh_cron = '0 0 30 2 *', refresh_only_changed = 0 WHERE id = 'texas'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.ExecContext(ctx, `UPDATE regions SET refresh_mode = 'cron', refresh_cron = '0 0 30 2 *', refresh_only_changed = 1 WHERE id = 'alberta'`); err != nil {
		t.Fatal(err)
	}
	def, _ := e.st.Schedule(ctx)
	if st := StatusFor(e.region("texas"), def, e.now); st.Due || st.NextDue != nil || st.NextBuild != nil {
		t.Fatalf("an impossible schedule is due: %+v", st)
	}
	for day := 5; day < 25; day++ {
		for _, h := range []int{0, 3, 12} {
			if msg := e.at(2026, 11, day, h, 0); msg != "" {
				t.Fatalf("tick on the %dth at %d:00: %q", day, h, msg)
			}
		}
	}
	if len(e.q.calls) != 0 || len(e.chk.asked) != 0 {
		t.Fatalf("an impossible schedule queued %d build(s) and made %d check(s)", len(e.q.calls), len(e.chk.asked))
	}
}

// Review 2, finding 4: a region's refresh is only decided once its build is durably queued.
func TestHandledOnlyAfterTheJobIsQueued(t *testing.T) {
	e := newEnv(t, "texas", "alberta")
	e.chk.changed["texas"] = true
	before := *e.region("texas").RefreshHandledAt

	// The job cannot be created (the database is unhappy).
	e.q.err = errors.New("database is locked")
	msg := e.at(2026, 11, 8, 3, 0)
	if !strings.Contains(msg, "could not start") {
		t.Fatalf("message %q", msg)
	}
	if got := *e.region("texas").RefreshHandledAt; !got.Equal(before) {
		t.Fatalf("texas was marked handled (%v) although no job exists", got)
	}
	if h := e.lastHistory(); h.OK || h.JobID != nil {
		t.Errorf("history %+v", h)
	}
	// Alberta had nothing new: that decision stands on its own.
	if got := *e.region("alberta").RefreshHandledAt; !got.Equal(e.now) {
		t.Errorf("alberta (unchanged) was not decided: %v", got)
	}
	// Next minute the database is fine: Texas is still due and gets its build, Alberta is not asked again.
	e.q.err = nil
	e.chk.asked = nil
	if e.at(2026, 11, 8, 3, 1); len(e.q.calls) != 1 || e.q.refreshed(0) != "texas" {
		t.Fatalf("after the failure: %v", e.q.params)
	}
	if strings.Join(e.chk.asked, ",") != "texas" {
		t.Errorf("asked %v", e.chk.asked)
	}
	if got := *e.region("texas").RefreshHandledAt; !got.Equal(e.now) {
		t.Errorf("texas not decided after its job was queued: %v", got)
	}
	// And only once.
	if e.at(2026, 11, 8, 3, 2); len(e.q.calls) != 1 {
		t.Fatal("queued twice")
	}

	// A restart between the two: the decisions are in the database, not just in memory.
	q2 := &fakeQ{}
	s2 := New(e.st, q2)
	s2.Checker = e.chk
	s2.now = func() time.Time { return e.now }
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s2.Start(cctx); err != nil {
		t.Fatal(err)
	}
	if msg := s2.Tick(cctx); msg != "" || len(q2.calls) != 0 {
		t.Fatalf("after a restart the handled regions fired again: %q", msg)
	}
}

// Review 2, finding 5: a build only starts inside the window, also when asking the sources
// took the clock past its end.
func TestWindowIsCheckedAgainBeforeQueueing(t *testing.T) {
	e := newEnv(t, "texas", "alberta")
	ctx := context.Background()
	if err := e.s.SetDefaults(ctx, "22:00-06:00", true); err != nil {
		t.Fatal(err)
	}
	e.policy("texas", ModeCron, "59 5 * * *", "", true)              // due at 05:59, default window ends 06:00
	e.policy("alberta", ModeCron, "59 5 * * *", "05:00-07:00", true) // its own, longer window
	e.chk.changed["texas"], e.chk.changed["alberta"] = true, true
	// The publisher is slow: each answer takes a minute.
	e.chk.during = func(string) { e.now = e.now.Add(time.Minute) }

	msg := e.at(2026, 11, 5, 5, 59)
	// It is 06:01 by the time the sources have answered: Texas's window has closed.
	if len(e.q.calls) != 1 || e.q.refreshed(0) != "alberta" {
		t.Fatalf("queued %v (%q); only alberta is still inside its window", e.q.params, msg)
	}
	if !strings.Contains(msg, "Texas waits for its build window") {
		t.Errorf("message %q", msg)
	}
	def, _ := e.st.Schedule(ctx)
	if st := StatusFor(e.region("texas"), def, e.now); !st.Due || !st.Waiting {
		t.Fatalf("texas should still be due and waiting: %+v", st)
	}
	// All day it waits; at 22:00 it is built.
	e.chk.during = nil
	if e.at(2026, 11, 5, 12, 0); len(e.q.calls) != 1 {
		t.Fatal("texas was built outside its window")
	}
	if e.at(2026, 11, 5, 22, 0); len(e.q.calls) != 2 || e.q.refreshed(1) != "texas" {
		t.Fatalf("at 22:00: %v", e.q.params)
	}

	// Every due region out of its window by then: nothing queued, nothing recorded, all still due.
	e2 := newEnv(t, "texas")
	_ = e2.s.SetDefaults(ctx, "22:00-06:00", true)
	e2.policy("texas", ModeCron, "59 5 * * *", "", true)
	e2.chk.changed["texas"] = true
	e2.chk.during = func(string) { e2.now = e2.now.Add(2 * time.Minute) }
	if msg := e2.at(2026, 11, 5, 5, 59); msg != "" || len(e2.q.calls) != 0 {
		t.Fatalf("%q %v", msg, e2.q.calls)
	}
}

// Review 2, finding 6: "check now" does not pile up on the publisher.
func TestCheckAllCoalescesAndCoolsDown(t *testing.T) {
	e := newEnv(t, "texas", "alberta")
	ctx := context.Background()
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	e.chk.during = func(string) { started <- struct{}{}; <-release }

	// Five impatient clicks at once.
	var wg sync.WaitGroup
	results := make(chan bool, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			asked, err := e.s.CheckAll(ctx)
			if err != nil {
				t.Error(err)
			}
			results <- asked
		}()
	}
	<-started // one of them is talking to the publisher; the rest wait behind it
	close(release)
	wg.Wait()
	close(results)
	asked := 0
	for a := range results {
		if a {
			asked++
		}
	}
	e.chk.mu.Lock()
	n := len(e.chk.asked)
	e.chk.mu.Unlock()
	if asked != 1 || n != 2 {
		t.Fatalf("5 concurrent checks: %d of them asked the sources, %d source requests; want 1 and 2 (one per region)", asked, n)
	}
	// Again inside the cooldown: nothing is asked, the recorded results stand.
	e.chk.during = nil
	e.now = e.now.Add(CheckCooldown - time.Second)
	if asked, err := e.s.CheckAll(ctx); err != nil || asked {
		t.Fatalf("inside the cooldown: asked=%v err=%v", asked, err)
	}
	if e.region("texas").CheckedAt == nil {
		t.Error("the first check's result is gone")
	}
	// After it: asked again.
	e.now = e.now.Add(2 * time.Second)
	if asked, err := e.s.CheckAll(ctx); err != nil || !asked {
		t.Fatalf("after the cooldown: asked=%v err=%v", asked, err)
	}
	e.chk.mu.Lock()
	defer e.chk.mu.Unlock()
	if len(e.chk.asked) != 4 {
		t.Errorf("source requests in total: %d, want 4", len(e.chk.asked))
	}
}
