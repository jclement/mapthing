package keys

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/store"
)

type limEnv struct {
	t   *testing.T
	st  *store.Store
	s   *Service
	now time.Time
}

func newLimEnv(t *testing.T) *limEnv {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &limEnv{t: t, st: st, now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	e.s = NewService(st)
	e.s.now = func() time.Time { return e.now }
	return e
}

// restart is a new process on the same database: what was flushed is all it knows.
func (e *limEnv) restart(flush bool) {
	e.t.Helper()
	if flush {
		if err := e.s.Flush(context.Background()); err != nil {
			e.t.Fatal(err)
		}
	}
	e.s = NewService(e.st)
	e.s.now = func() time.Time { return e.now }
}

func (e *limEnv) charge(k *store.APIKey, kind, ip string) Decision {
	return e.s.Charge(context.Background(), k, kind, ip)
}

// ok charges n requests and fails the test unless all are allowed.
func (e *limEnv) ok(k *store.APIKey, kind, ip string, n int) {
	e.t.Helper()
	for i := range n {
		if d := e.charge(k, kind, ip); !d.OK {
			e.t.Fatalf("%s request %d of %d refused (%s, retry in %v)", kind, i+1, n, d.Scope, d.RetryAfter)
		}
	}
}

func key(id int64, l store.Limits) *store.APIKey { return &store.APIKey{ID: id, Limits: l} }

func TestRateWithBurstAndWeights(t *testing.T) {
	e := newLimEnv(t)
	// 2 units a second, burst 20.
	k := key(1, store.Limits{RPS: 2, Burst: 20})
	// A raster render costs 10: two fit in the burst, a third does not.
	e.ok(k, "raster", "", 2)
	d := e.charge(k, "raster", "")
	if d.OK || d.Scope != ScopeRate || d.Cost != 10 || !errors.Is(d.Err(), ErrRateLimit) {
		t.Fatalf("third raster: %+v", d)
	}
	// 10 units at 2 a second: 5 seconds.
	if d.RetryAfter != 5*time.Second {
		t.Errorf("retry after %v, want 5s", d.RetryAfter)
	}
	// A refused request takes nothing: a tile (cost 1) is refused too, and says 1 second
	// (never less: Retry-After is whole seconds).
	if d := e.charge(k, "tile", ""); d.OK || d.RetryAfter != time.Second {
		t.Fatalf("tile with an empty bucket: %+v", d)
	}
	e.now = e.now.Add(time.Second) // +2 units
	e.ok(k, "tile", "", 2)
	if d := e.charge(k, "tile", ""); d.OK {
		t.Fatal("more than the refill was allowed")
	}
	// The bucket never holds more than the burst, however long the key was idle.
	e.now = e.now.Add(time.Hour)
	e.ok(k, "tile", "", 20)
	if d := e.charge(k, "tile", ""); d.OK {
		t.Fatal("more than the burst after an idle hour")
	}
	// Headers describe the bucket: limit 20, nothing left, full again in 10 s.
	if d.Limit != 20 || d.Remaining != 0 {
		t.Errorf("headers: limit %d remaining %d", d.Limit, d.Remaining)
	}
	e.now = e.now.Add(5 * time.Second)
	if d := e.charge(k, "tile", ""); !d.OK || d.Limit != 20 || d.Remaining != 9 || d.Reset != 6*time.Second {
		t.Errorf("after 5 s: %+v", d)
	}
	// A request that costs more than the burst is not refused for ever.
	small := key(2, store.Limits{RPS: 1, Burst: 3})
	if d := e.charge(small, "pack", ""); !d.OK || d.Cost != 50 {
		t.Fatalf("a pack on a small bucket: %+v", d)
	}
	if d := e.charge(small, "tile", ""); d.OK {
		t.Fatal("the bucket should be empty after it")
	}
	// A key without limits is never refused and gets no headers.
	free := key(3, store.Limits{})
	e.ok(free, "tile", "", 5000)
	if d := e.charge(free, "tile", ""); !d.OK || d.Limit != 0 {
		t.Errorf("unlimited key: %+v", d)
	}
}

func TestPerClientLimitWithinAKey(t *testing.T) {
	e := newLimEnv(t)
	// The key allows plenty; one address may only take 5 units at a time, 1 a second.
	k := key(1, store.Limits{RPS: 100, Burst: 1000, IPRPS: 1, IPBurst: 5})
	e.ok(k, "tile", "203.0.113.9", 5)
	d := e.charge(k, "tile", "203.0.113.9")
	if d.OK || d.Scope != ScopeIP {
		t.Fatalf("the abuser's sixth request: %+v", d)
	}
	// Everyone else on the key is unaffected.
	e.ok(k, "tile", "198.51.100.7", 5)
	e.ok(k, "tile", "2001:db8:1:2::1", 5)
	// One IPv6 subscriber (a /64) is one client.
	if d := e.charge(k, "tile", "2001:db8:1:2:aaaa:bbbb:cccc:dddd"); d.OK || d.Scope != ScopeIP {
		t.Fatalf("another address in the same /64: %+v", d)
	}
	e.ok(k, "tile", "2001:db8:1:3::1", 1) // the next /64 is someone else
	// The refused request did not take from the key's own bucket: 16 used so far.
	if u := e.s.UsageOf(context.Background(), k); u.RateRemaining != 1000-16 || u.UnitsToday != 16 {
		t.Errorf("after the refusals: %+v", u)
	}
	// No address known (not behind a proxy that reports one): only the key's limit applies.
	e.ok(k, "tile", "", 50)
	// A second later the abuser has one more.
	e.now = e.now.Add(time.Second)
	e.ok(k, "tile", "203.0.113.9", 1)
	if d := e.charge(k, "tile", "203.0.113.9"); d.OK {
		t.Fatal("refilled more than 1 a second")
	}
}

func TestQuotasDailyAndMonthly(t *testing.T) {
	e := newLimEnv(t)
	k := key(1, store.Limits{DailyQuota: 30, MonthlyQuota: 50})
	e.ok(k, "route", "", 3) // 30 units
	d := e.charge(k, "tile", "")
	if d.OK || d.Scope != ScopeDaily || !errors.Is(d.Err(), ErrDayLimit) {
		t.Fatalf("over the daily quota: %+v", d)
	}
	if d.RetryAfter != 12*time.Hour || d.Limit != 30 || d.Remaining != 0 || d.Reset != 12*time.Hour {
		t.Errorf("daily refusal: %+v (it is noon UTC)", d)
	}
	// A request that would take the quota past its end is refused whole, not let through.
	k2 := key(2, store.Limits{DailyQuota: 15})
	e.ok(k2, "route", "", 1)
	if d := e.charge(k2, "route", ""); d.OK {
		t.Fatal("10 more units on 10 of 15 used")
	}
	e.ok(k2, "tile", "", 5) // but what still fits does
	// Next day the daily quota is back; the month keeps counting.
	e.now = e.now.Add(24 * time.Hour)
	e.ok(k, "route", "", 2) // month: 50
	d = e.charge(k, "tile", "")
	if d.OK || d.Scope != ScopeMonthly || !errors.Is(d.Err(), ErrMonthLimit) {
		t.Fatalf("over the monthly quota: %+v", d)
	}
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Sub(e.now); d.RetryAfter != want {
		t.Errorf("monthly retry after %v, want %v", d.RetryAfter, want)
	}
	// 1 December: both are back.
	e.now = time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC)
	e.ok(k, "route", "", 3)
	if u := e.s.UsageOf(context.Background(), k); u.UnitsToday != 30 || u.UnitsMonth != 30 {
		t.Errorf("usage on 1 December: %+v", u)
	}
}

// Quotas survive a restart; bursts need not.
func TestQuotasSurviveARestart(t *testing.T) {
	e := newLimEnv(t)
	k := key(1, store.Limits{RPS: 1, Burst: 5, DailyQuota: 100, MonthlyQuota: 1000})
	e.ok(k, "tile", "", 5)
	e.now = e.now.Add(time.Minute)
	e.ok(k, "route", "", 0)
	e.ok(k, "tile", "", 5) // 10 units so far
	if d := e.charge(k, "tile", ""); d.OK {
		t.Fatal("test setup: the bucket should be empty")
	}
	e.restart(true)
	// The bucket starts full again (burst state is not kept)…
	e.ok(k, "tile", "", 5)
	// …and the quota remembers: 15 of 100 used.
	u := e.s.UsageOf(context.Background(), k)
	if u.UnitsToday != 15 || u.UnitsMonth != 15 {
		t.Fatalf("after a restart: %+v, want 15 units today and this month", u)
	}
	e.now = e.now.Add(time.Hour)
	for range 8 {
		e.ok(k, "route", "", 1) // 80 more: 95
		e.now = e.now.Add(time.Minute)
	}
	if d := e.charge(k, "route", ""); d.OK || d.Scope != ScopeDaily {
		t.Fatalf("the daily quota forgot what was used before the restart: %+v", d)
	}
	// Refusals are counted and survive too.
	e.restart(true)
	rows, err := e.st.LimitedSince(context.Background(), "2026-11-11")
	if err != nil {
		t.Fatal(err)
	}
	byScope := map[string]int64{}
	for _, r := range rows {
		byScope[r.Scope] += r.Count
	}
	if byScope[ScopeRate] != 1 || byScope[ScopeDaily] != 1 {
		t.Errorf("stored refusals: %v", byScope)
	}
	// A crash (no final flush) loses at most what was not written yet: the quota is never
	// more generous than the truth by more than one flush interval of use.
	e.ok(k, "tile", "", 3) // 98, flushed
	_ = e.s.Flush(context.Background())
	e.ok(k, "tile", "", 2) // 100, not flushed
	e.restart(false)
	if u := e.s.UsageOf(context.Background(), k); u.UnitsToday != 98 {
		t.Errorf("after a crash: %d units, want the 98 that were written", u.UnitsToday)
	}
	// Seeding once per period: counting does not double after several flushes.
	e.ok(k, "tile", "", 1)
	_ = e.s.Flush(context.Background())
	e.ok(k, "tile", "", 1)
	_ = e.s.Flush(context.Background())
	if u := e.s.UsageOf(context.Background(), k); u.UnitsToday != 100 {
		t.Errorf("after two flushes: %d units, want 100", u.UnitsToday)
	}
	if d := e.charge(k, "tile", ""); d.OK {
		t.Error("101st unit allowed")
	}
}

func TestGlobalSafetyLimits(t *testing.T) {
	e := newLimEnv(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Global = map[string]GlobalLimit{"route": {RPS: 1, Burst: 3}}
	if err := e.s.SetConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	a, b := key(1, store.Limits{}), key(2, store.Limits{RPS: 100, Burst: 1000})
	// The limit is across keys, in requests.
	e.ok(a, "route", "", 2)
	e.ok(b, "route", "", 1)
	d := e.charge(a, "route", "")
	if d.OK || d.Scope != ScopeGlobal || !errors.Is(d.Err(), ErrBusy) {
		t.Fatalf("fourth route: %+v", d)
	}
	// Other kinds are not held back by it.
	e.ok(a, "tile", "", 100)
	e.ok(b, "elevation", "", 10)
	// A request the server limit refuses costs the key nothing…
	if u := e.s.UsageOf(ctx, b); u.RateRemaining != 1000-10-50 {
		t.Errorf("key b after a server refusal: %+v", u)
	}
	if d := e.charge(b, "route", ""); d.OK {
		t.Fatal("still over the server limit")
	}
	if u := e.s.UsageOf(ctx, b); u.RateRemaining != 1000-10-50 {
		t.Errorf("a refused request was charged: %+v", u)
	}
	// …and one the key's own limit refuses does not use up the server's.
	poor := key(3, store.Limits{RPS: 0.01, Burst: 10})
	e.now = e.now.Add(3 * time.Second) // server bucket: 3 again
	e.ok(poor, "route", "", 1)
	for range 5 {
		if d := e.charge(poor, "route", ""); d.OK || d.Scope != ScopeRate {
			t.Fatalf("poor key: %+v", d)
		}
	}
	e.ok(a, "route", "", 2) // the two the poor key did not take

	// The configuration is stored: a restart keeps it.
	e.restart(true)
	if g := e.s.Config().Global; len(g) != 1 || g["route"].Burst != 3 {
		t.Errorf("config after a restart: %+v", g)
	}
	// No server limits at all is a valid choice, and is not replaced by the defaults.
	if err := e.s.SetConfig(ctx, Config{Weights: map[string]int{"tile": 2}}); err != nil {
		t.Fatal(err)
	}
	e.restart(true)
	c := e.s.Config()
	if len(c.Global) != 0 || c.Weights["tile"] != 2 || c.Weights["raster"] != DefaultConfig().Weights["raster"] {
		t.Errorf("config: %+v", c)
	}
	e.ok(a, "route", "", 500)
}

func TestConfigValidationAndDefaults(t *testing.T) {
	e := newLimEnv(t)
	ctx := context.Background()
	def := e.s.Config()
	// A fresh server: every kind has a weight, tiles cheapest, and the expensive kinds a server limit.
	for _, k := range Kinds {
		if def.Weights[k] < 1 {
			t.Errorf("no weight for %s", k)
		}
	}
	if def.Weights["tile"] != 1 || def.Weights["raster"] <= def.Weights["tile"] || def.Weights["route"] <= def.Weights["tile"] {
		t.Errorf("weights %v: a render or a route must cost more than a vector tile", def.Weights)
	}
	for _, k := range []string{"raster", "route", "pack"} {
		if def.Global[k].RPS <= 0 {
			t.Errorf("no server limit for %s by default", k)
		}
	}
	if _, ok := def.Global["tile"]; ok {
		t.Error("vector tiles have a server limit by default")
	}
	for name, bad := range map[string]Config{
		"unknown kind":       {Weights: map[string]int{"teleport": 3}},
		"zero weight":        {Weights: map[string]int{"tile": 0}},
		"negative limit":     {Global: map[string]GlobalLimit{"route": {RPS: -1}}},
		"unknown limit kind": {Global: map[string]GlobalLimit{"nope": {RPS: 1}}},
	} {
		if err := e.s.SetConfig(ctx, bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if got := e.s.Config(); got.Weights["tile"] != 1 {
		t.Error("a refused configuration was applied")
	}
	// Kinds that are counted under a longer name still find their weight; unknown kinds cost 1.
	if c := def.Cost("elevation-profile"); c != def.Weights["elevation"] {
		t.Errorf("elevation-profile costs %d", c)
	}
	if c := def.Cost("compat"); c != 1 {
		t.Errorf("an unlisted kind costs %d, want 1", c)
	}
}

// Keys stored by a version that only knew requests per minute and per day keep their meaning.
func TestOlderLimitsAreUnderstood(t *testing.T) {
	l := store.Limits{RPM: 300, RPD: 10000}.Normalise()
	if l.RPS != 5 || l.Burst != 300 || l.DailyQuota != 10000 || l.RPM != 0 || l.RPD != 0 {
		t.Errorf("rpm 300, rpd 10000 → %+v", l)
	}
	// A rate without a burst gets two seconds' worth, at least 10.
	if l := (store.Limits{RPS: 50}).Normalise(); l.Burst != 100 {
		t.Errorf("default burst for 50/s: %d", l.Burst)
	}
	if l := (store.Limits{RPS: 0.5, IPRPS: 2}).Normalise(); l.Burst != 10 || l.IPBurst != 10 {
		t.Errorf("small rates: %+v", l)
	}
	if l := (store.Limits{Burst: 50, IPBurst: 5}).Normalise(); l.Burst != 0 || l.IPBurst != 0 {
		t.Errorf("a burst without a rate means nothing: %+v", l)
	}
}

func TestRecentRefusalsAndPerClientBuckets(t *testing.T) {
	e := newLimEnv(t)
	ctx := context.Background()
	k := key(1, store.Limits{RPS: 1, Burst: 1, IPRPS: 1, IPBurst: 1})
	e.ok(k, "tile", "203.0.113.9", 1)
	for range 30 {
		e.charge(k, "route", "203.0.113.9")
	}
	u := e.s.UsageOf(ctx, k)
	if len(u.Limited) != keepRefusals {
		t.Fatalf("kept %d recent refusals, want the last %d", len(u.Limited), keepRefusals)
	}
	last := u.Limited[len(u.Limited)-1]
	if last.Kind != "route" || last.Scope != ScopeRate || last.IP != "203.0.113.9" || !last.At.Equal(e.now) {
		t.Errorf("last refusal: %+v", last)
	}
	// Per-client buckets do not grow without bound under a spray of addresses.
	wide := key(2, store.Limits{IPRPS: 1, IPBurst: 1})
	for i := range maxIPBuckets + 5000 {
		ip := time.Duration(i).String() // any distinct string will do
		e.s.Charge(ctx, wide, "tile", ip)
	}
	e.s.mu.Lock()
	n := len(e.s.ipBuckets)
	e.s.mu.Unlock()
	if n > maxIPBuckets {
		t.Errorf("%d per-client buckets, the cap is %d", n, maxIPBuckets)
	}
	// Changing or revoking a key forgets its buckets.
	e.s.forget(1)
	e.ok(k, "tile", "203.0.113.9", 1)
}

// Review 3, finding 1: a quota whose stored use cannot be read grants nothing.
func TestFailedQuotaReadGrantsNothing(t *testing.T) {
	e := newLimEnv(t)
	ctx := context.Background()
	k := key(1, store.Limits{DailyQuota: 30})
	e.ok(k, "route", "", 3) // the whole day's quota
	e.restart(true)

	// The database does not answer (or the first request after the restart gave up waiting).
	reads := 0
	real := e.s.readQuota
	e.s.readQuota = func(context.Context, int64, string) (int64, error) {
		reads++
		return 0, errors.New("database is locked")
	}
	for range 5 {
		d := e.charge(k, "tile", "")
		if d.OK || d.Scope != ScopeUnavailable || !errors.Is(d.Err(), ErrUnavailable) || d.RetryAfter <= 0 {
			t.Fatalf("with the quota unreadable: %+v (the key was out of quota before the restart)", d)
		}
	}
	if reads != 5 {
		t.Errorf("%d reads for 5 requests: a failed read must be tried again", reads)
	}
	// It is not a refusal by a limit, and nothing was charged.
	if n := len(e.s.PendingLimited()); n != 0 {
		t.Errorf("counted as %d limit refusals", n)
	}
	// A key without a quota does not depend on that read.
	e.ok(key(2, store.Limits{RPS: 10}), "tile", "", 3)
	// The database is back: the earlier use is found, the key is out of quota. Nothing was
	// remembered from the failures.
	e.s.readQuota = real
	if d := e.charge(k, "tile", ""); d.OK || d.Scope != ScopeDaily {
		t.Fatalf("after the database came back: %+v", d)
	}

	// A client that hangs up while its request waits does not decide what the quota is.
	e.restart(true)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if d := e.s.Charge(gone, k, "tile", ""); d.OK || d.Scope != ScopeDaily {
		t.Fatalf("first request after a restart, cancelled: %+v", d)
	}
	if d := e.charge(k, "tile", ""); d.OK || d.Scope != ScopeDaily {
		t.Fatalf("the request after it: %+v", d)
	}

	// Many first requests at once, some of whose reads fail: none of them wins a fresh quota.
	e.restart(true)
	var mu sync.Mutex
	n := 0
	real = e.s.readQuota
	e.s.readQuota = func(c context.Context, id int64, p string) (int64, error) {
		mu.Lock()
		n++
		fail := n%2 == 1
		mu.Unlock()
		if fail {
			return 0, errors.New("busy")
		}
		return real(c, id, p)
	}
	var wg sync.WaitGroup
	allowed := make(chan bool, 40)
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed <- e.charge(k, "tile", "").OK
		}()
	}
	wg.Wait()
	close(allowed)
	for ok := range allowed {
		if ok {
			t.Fatal("a request was allowed on an exhausted quota")
		}
	}
	// A key given a quota after it was already in use counts what this run flushed, once.
	late := key(7, store.Limits{})
	e.s.readQuota = real
	e.ok(late, "route", "", 2) // 20 units, no quota, never read
	_ = e.s.Flush(ctx)
	e.ok(late, "route", "", 1) // 10 more, not flushed
	late.Limits = store.Limits{DailyQuota: 45}
	e.ok(late, "route", "", 1) // 40
	if d := e.charge(late, "route", ""); d.OK {
		t.Fatal("50 units on a quota of 45: the units flushed before the quota existed were lost or doubled")
	}
	e.ok(late, "tile", "", 5)
	if u := e.s.UsageOf(ctx, late); u.UnitsToday != 45 {
		t.Errorf("units today %d, want 45", u.UnitsToday)
	}
}

// Review 3, finding 3: requests on both sides of midnight UTC do not disturb each other.
func TestRolloverKeepsBothPeriods(t *testing.T) {
	e := newLimEnv(t)
	ctx := context.Background()
	k := key(1, store.Limits{DailyQuota: 100, MonthlyQuota: 150})
	before := time.Date(2026, 11, 30, 23, 59, 59, 900e6, time.UTC)
	after := time.Date(2026, 12, 1, 0, 0, 0, 100e6, time.UTC)
	e.now = before
	e.ok(k, "route", "", 6) // 60 on 30 November
	// B is charged just after midnight; A, which read the clock just before it, lands after B.
	e.now = after
	e.ok(k, "route", "", 5) // B: 50 on 1 December
	e.now = before
	e.ok(k, "route", "", 1) // A: belongs to November (70 of the day, 70 of the month)
	e.now = after
	// The new day still knows B's 50: five more routes fit, a sixth does not.
	e.ok(k, "route", "", 5)
	d := e.charge(k, "route", "")
	if d.OK || d.Scope != ScopeDaily {
		t.Fatalf("after a straggler from the old day, the new day forgot what it had used: %+v", d)
	}
	if u := e.s.UsageOf(ctx, k); u.UnitsToday != 100 || u.UnitsMonth != 100 {
		t.Errorf("1 December: %+v, want 100 and 100", u)
	}
	// And what is written is right for all four periods.
	if err := e.s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for period, want := range map[string]int64{"2026-11-30": 70, "2026-11": 70, "2026-12-01": 100, "2026-12": 100} {
		if got, _ := e.st.QuotaUsed(ctx, 1, period); got != want {
			t.Errorf("stored %s: %d, want %d", period, got, want)
		}
	}
	// The periods that are over are dropped once written; the current ones stay.
	e.s.mu.Lock()
	n := len(e.s.periods)
	e.s.mu.Unlock()
	if n != 2 {
		t.Errorf("%d periods kept after the flush, want the current day and month", n)
	}
	// The same at a month's end when only the monthly quota is near: November's straggler
	// does not eat December's month.
	e.restart(true)
	if u := e.s.UsageOf(ctx, k); u.UnitsMonth != 100 {
		t.Errorf("after a restart: %+v", u)
	}
}

// Review 3, finding 6: a refusal reports the figures of the limit that refused.
func TestRefusalFiguresAreOfTheRefusingLimit(t *testing.T) {
	e := newLimEnv(t)
	ctx := context.Background()
	// Only a per-client limit.
	k := key(1, store.Limits{IPRPS: 2, IPBurst: 4})
	if d := e.charge(k, "tile", "203.0.113.9"); !d.OK || d.Limit != 4 || d.Remaining != 3 || d.Reset != time.Second {
		t.Fatalf("allowed, per-client only: %+v", d)
	}
	e.ok(k, "tile", "203.0.113.9", 3)
	d := e.charge(k, "tile", "203.0.113.9")
	if d.OK || d.Scope != ScopeIP || d.Limit != 4 || d.Remaining != 0 || d.Reset != 2*time.Second {
		t.Fatalf("per-client refusal: %+v, want limit 4, remaining 0, whole again in 2 s", d)
	}
	// Another client of the same key sees its own bucket, not the abuser's.
	if d := e.charge(k, "tile", "198.51.100.7"); !d.OK || d.Remaining != 3 {
		t.Fatalf("another client: %+v", d)
	}
	// A big key bucket and a small client one: the refusal describes the client's.
	both := key(2, store.Limits{RPS: 100, Burst: 1000, IPRPS: 1, IPBurst: 2, DailyQuota: 1000000})
	e.ok(both, "tile", "203.0.113.9", 2)
	if d := e.charge(both, "tile", "203.0.113.9"); d.Scope != ScopeIP || d.Limit != 2 || d.Remaining != 0 {
		t.Fatalf("per-client refusal on a key with other limits: %+v", d)
	}
	// Only the server's limit: a key with no limits still learns what it ran into (in requests).
	cfg := DefaultConfig()
	cfg.Global = map[string]GlobalLimit{"route": {RPS: 0.5, Burst: 3}}
	if err := e.s.SetConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	free := key(3, store.Limits{})
	e.ok(free, "route", "", 3)
	d = e.charge(free, "route", "")
	if d.OK || d.Scope != ScopeGlobal || d.Limit != 3 || d.Remaining != 0 || d.Reset != 6*time.Second || d.RetryAfter != 2*time.Second {
		t.Fatalf("server-limit refusal: %+v, want limit 3, remaining 0, reset 6 s, retry 2 s", d)
	}
	// Allowed requests of a key without limits still carry no figures.
	e.now = e.now.Add(time.Minute)
	if d := e.charge(free, "route", ""); !d.OK || d.Limit != 0 {
		t.Fatalf("unlimited key, allowed: %+v", d)
	}
	// Quota refusals describe the quota, also when a rate is closer to empty in share.
	q := key(4, store.Limits{RPS: 1000, Burst: 2000, DailyQuota: 3})
	e.ok(q, "tile", "", 3)
	if d := e.charge(q, "tile", ""); d.Scope != ScopeDaily || d.Limit != 3 || d.Remaining != 0 {
		t.Fatalf("quota refusal: %+v", d)
	}
}

// Review 3, extras: nothing is kept for revoked keys, and a full per-client table loses its
// least recently used clients first.
func TestLimiterStateIsReclaimed(t *testing.T) {
	e := newLimEnv(t)
	ctx := context.Background()
	stored, secret, err := e.s.Create(ctx, "soon gone", store.Limits{RPS: 1, Burst: 2, IPRPS: 1, IPBurst: 1, DailyQuota: 100})
	if err != nil {
		t.Fatal(err)
	}
	k, err := e.s.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	e.ok(k, "tile", "203.0.113.9", 1)
	e.charge(k, "tile", "203.0.113.9") // refused: a refusal to remember
	if err := e.s.Revoke(ctx, stored.ID); err != nil {
		t.Fatal(err)
	}
	// Looking at the revoked key (the Keys page can list them) must not bring state back.
	if u := e.s.UsageOf(ctx, k); u.UnitsToday != 1 || len(u.Limited) != 0 {
		t.Errorf("usage of the revoked key: %+v", u)
	}
	if err := e.s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	e.s.UsageOf(ctx, k)
	e.s.mu.Lock()
	left := len(e.s.periods) + len(e.s.buckets) + len(e.s.ipBuckets) + len(e.s.refusals) + len(e.s.gone)
	e.s.mu.Unlock()
	if left != 0 {
		t.Errorf("%d pieces of limiter state left for a revoked key", left)
	}
	// Its last units were written all the same.
	if got, _ := e.st.QuotaUsed(ctx, stored.ID, e.now.Format("2006-01-02")); got != 1 {
		t.Errorf("stored units of the revoked key: %d", got)
	}

	// Per-client buckets: fill the table with clients seen at different times within the
	// idle window, then add one more. The oldest tenth goes; the recent ones stay.
	wide := key(50, store.Limits{IPRPS: 1, IPBurst: 1})
	start := e.now
	for i := range maxIPBuckets {
		e.now = start.Add(time.Duration(i) * 10 * time.Millisecond) // 200 s in all
		e.s.Charge(ctx, wide, "tile", "c"+time.Duration(i).String())
	}
	e.now = e.now.Add(time.Second)
	e.s.Charge(ctx, wide, "tile", "newcomer")
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	if n := len(e.s.ipBuckets); n > maxIPBuckets*9/10 || n < maxIPBuckets*8/10 {
		t.Fatalf("%d buckets after making room, want about nine tenths of %d", n, maxIPBuckets)
	}
	has := func(i int) bool {
		_, ok := e.s.ipBuckets[ipKey{50, "c" + time.Duration(i).String()}]
		return ok
	}
	if has(0) || has(maxIPBuckets/20) {
		t.Error("the oldest clients were kept")
	}
	if !has(maxIPBuckets-1) || !has(maxIPBuckets/2) {
		t.Error("recently seen clients were dropped")
	}
	if _, ok := e.s.ipBuckets[ipKey{50, "newcomer"}]; !ok {
		t.Error("the new client has no bucket")
	}
}
