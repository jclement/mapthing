// Package scheduler keeps the map data fresh. Every region has its own refresh setting (never,
// daily, weekly, monthly, a cron expression, or "follow the default schedule"), an optional
// build window and an "only when the source changed" switch. Once a minute the scheduler
// works out which regions are due; when any is, it looks at their sources and either records
// that there is nothing new or queues one build that refreshes exactly the regions that need
// it. All times are UTC.
//
// A build always makes the basemap and the routing graph over every region together (routes
// and tiles must cross region borders), so "refreshing a region" means: re-download that
// region's extract, then rebuild. Regions that are not due keep their downloaded extract, and
// the terrain is reused when the set of regions has not changed.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/store"
)

// DefaultCron is weekly, Sunday 03:00 UTC.
const DefaultCron = "0 3 * * 0"

// BuildJob is the job name the scheduler enqueues.
const BuildJob = "build"

// Refresh modes of a region.
const (
	ModeDefault = "default" // follow the default schedule
	ModeNever   = "never"
	ModeDaily   = "daily"   // 03:00
	ModeWeekly  = "weekly"  // Sunday 03:00
	ModeMonthly = "monthly" // the 1st, 03:00
	ModeCron    = "cron"    // RefreshCron
)

// presetCron is the schedule of the named modes.
var presetCron = map[string]string{ModeDaily: "0 3 * * *", ModeWeekly: "0 3 * * 0", ModeMonthly: "0 3 1 * *"}

// ValidMode reports whether m is a refresh mode.
func ValidMode(m string) bool {
	switch m {
	case ModeDefault, ModeNever, ModeDaily, ModeWeekly, ModeMonthly, ModeCron:
		return true
	}
	return false
}

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Parse validates a 5-field cron expression (or @daily/@weekly descriptors). Schedules are
// UTC only: a TZ= or CRON_TZ= prefix, which the cron library would otherwise honour, is
// refused. So is an expression whose fields are each valid but that never occurs (30
// February): the library answers the zero time for those, which must never be taken for a
// time in the past.
func Parse(expr string) (cron.Schedule, error) {
	expr = strings.TrimSpace(expr)
	if up := strings.ToUpper(expr); strings.HasPrefix(up, "TZ=") || strings.HasPrefix(up, "CRON_TZ=") {
		return nil, fmt.Errorf("invalid cron expression %q: schedules are in UTC; a time zone prefix is not supported", expr)
	}
	s, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	if s.Next(time.Now().UTC()).IsZero() {
		return nil, fmt.Errorf("invalid cron expression %q: it never occurs (no such date in the next five years)", expr)
	}
	return s, nil
}

// Window is a daily span of time, UTC, in which builds may start. The zero Window is "any time".
type Window struct {
	Set        bool
	From, Till int // minutes after midnight; Till may be less than From (through midnight)
}

// ParseWindow reads "HH:MM-HH:MM" (UTC). "" is no window. A window through midnight
// ("22:00-06:00") is fine; an empty one ("03:00-03:00") is not.
func ParseWindow(s string) (Window, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Window{}, nil
	}
	bad := fmt.Errorf("build window %q: want HH:MM-HH:MM in UTC, for example 22:00-06:00", s)
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, bad
	}
	parse := func(v string) (int, bool) {
		h, m, ok := strings.Cut(strings.TrimSpace(v), ":")
		if !ok || len(h) == 0 || len(h) > 2 || len(m) != 2 {
			return 0, false
		}
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
			return 0, false
		}
		return hh*60 + mm, true
	}
	from, ok1 := parse(a)
	till, ok2 := parse(b)
	if !ok1 || !ok2 || from%1440 == till%1440 {
		return Window{}, bad
	}
	return Window{Set: true, From: from % 1440, Till: till % 1440}, nil
}

// Contains reports whether t (taken as UTC) is inside the window.
func (w Window) Contains(t time.Time) bool {
	if !w.Set {
		return true
	}
	t = t.UTC()
	m := t.Hour()*60 + t.Minute()
	if w.From < w.Till {
		return m >= w.From && m < w.Till
	}
	return m >= w.From || m < w.Till
}

// NextOpen returns t if it is inside the window, else the next time the window opens.
func (w Window) NextOpen(t time.Time) time.Time {
	if w.Contains(t) {
		return t
	}
	t = t.UTC()
	open := time.Date(t.Year(), t.Month(), t.Day(), w.From/60, w.From%60, 0, 0, time.UTC)
	if !open.After(t) {
		open = open.Add(24 * time.Hour)
	}
	return open
}

func (w Window) String() string {
	if !w.Set {
		return ""
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.From/60, w.From%60, w.Till/60, w.Till%60)
}

// Enqueuer is the part of the job runner the scheduler needs.
type Enqueuer interface {
	Enqueue(ctx context.Context, name string, params any, opt jobs.EnqueueOptions) (*store.JobRow, error)
}

// busyChecker is implemented by the job runner: whether a job of that name is queued or running.
type busyChecker interface {
	Busy(ctx context.Context, name string) bool
}

// SourceCheck is the result of looking at a region's source.
type SourceCheck struct {
	Changed bool   // a newer extract is published than the one downloaded (or none is downloaded)
	Detail  string // for people: "sequence 4321 → 4330", "not downloaded yet"
}

// Checker looks at whether a region's source has something newer than what is downloaded.
type Checker interface {
	CheckSource(ctx context.Context, regionID string) (SourceCheck, error)
}

// Scheduler decides when to refresh.
type Scheduler struct {
	st  *store.Store
	q   Enqueuer
	now func() time.Time

	// Checker looks at region sources. nil → every due region counts as changed.
	Checker Checker

	mu      sync.Mutex
	expr    string
	enabled bool
	lastRun *time.Time
	// retryAfter holds off regions whose source could not be checked.
	retryAfter map[string]time.Time
	// handledMem is when this process decided a region's refresh, for the (unlikely) case
	// that writing it to the database failed: the region is still not decided twice.
	handledMem map[string]time.Time
	tickMu     sync.Mutex // one Tick or CheckAll at a time
	// lastCheckAll is when CheckAll last asked the sources (its cooldown).
	lastCheckAll time.Time
}

// New returns a scheduler. Call Start to load the stored schedule.
func New(st *store.Store, q Enqueuer) *Scheduler {
	return &Scheduler{st: st, q: q, now: time.Now, retryAfter: map[string]time.Time{}, handledMem: map[string]time.Time{}}
}

// checkRetry is how long a region waits after its source could not be checked.
const checkRetry = time.Hour

// Start loads the schedule from the store and starts the once-a-minute loop.
func (s *Scheduler) Start(ctx context.Context) error {
	sc, err := s.st.Schedule(ctx)
	if err != nil {
		return err
	}
	expr := sc.Cron
	if _, err := Parse(expr); err != nil {
		slog.Warn("stored schedule invalid, using default", "cron", sc.Cron, "err", err)
		expr = DefaultCron
	}
	s.mu.Lock()
	s.expr, s.enabled = expr, sc.Enabled
	s.mu.Unlock()
	if h, err := s.st.History(ctx, "schedule", 1); err == nil && len(h) > 0 {
		t := h[0].At
		s.mu.Lock()
		s.lastRun = &t
		s.mu.Unlock()
	}
	// A region's clock starts when the scheduler first sees it: after an upgrade the next
	// refresh is the next schedule time from now, as it was with the single schedule.
	if err := s.adoptRegions(ctx); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Tick(ctx)
			}
		}
	}()
	return nil
}

func (s *Scheduler) adoptRegions(ctx context.Context) error {
	regions, err := s.st.Regions(ctx)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, r := range regions {
		if r.RefreshHandledAt == nil {
			if err := s.st.SetRegionHandled(ctx, r.ID, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// Set validates and stores the default schedule.
func (s *Scheduler) Set(ctx context.Context, expr string, enabled bool) error {
	if _, err := Parse(expr); err != nil {
		return err
	}
	if err := s.st.SetSchedule(ctx, expr, enabled); err != nil {
		return err
	}
	s.mu.Lock()
	s.expr, s.enabled = expr, enabled
	s.mu.Unlock()
	return nil
}

// SetDefaults validates and stores the default build window and only-when-changed switch.
func (s *Scheduler) SetDefaults(ctx context.Context, window string, onlyChanged bool) error {
	w, err := ParseWindow(window)
	if err != nil {
		return err
	}
	return s.st.SetScheduleDefaults(ctx, w.String(), onlyChanged)
}

// RefreshDownloads is kept for the admin API's older shape: a scheduled build always looks
// for newer extracts of the regions that are due.
func (s *Scheduler) RefreshDownloads(context.Context) bool { return true }

// SetRefreshDownloads is accepted and ignored (see RefreshDownloads).
func (s *Scheduler) SetRefreshDownloads(context.Context, bool) error { return nil }

// Policy is a region's refresh setting with the default applied.
type Policy struct {
	Mode        string // the region's own mode
	Cron        string // the schedule that applies; "" = never refreshed
	Window      Window
	OnlyChanged bool
	// Inherited: the region follows the default schedule.
	Inherited bool
}

// PolicyFor resolves a region's setting against the default schedule.
func PolicyFor(r *store.Region, def *store.Schedule) Policy {
	p := Policy{Mode: r.RefreshMode}
	if p.Mode == "" {
		p.Mode = ModeDefault
	}
	defWindow, _ := ParseWindow(def.Window)
	switch p.Mode {
	case ModeNever:
		return p
	case ModeDefault:
		p.Inherited = true
		if def.Enabled {
			p.Cron = def.Cron
		}
		p.Window, p.OnlyChanged = defWindow, def.OnlyChanged
		return p
	case ModeCron:
		p.Cron = r.RefreshCron
	default:
		p.Cron = presetCron[p.Mode]
	}
	p.OnlyChanged = r.RefreshOnlyChanged
	if w, err := ParseWindow(r.RefreshWindow); err == nil && w.Set {
		p.Window = w
	} else {
		p.Window = defWindow
	}
	return p
}

// RegionStatus says where a region stands.
type RegionStatus struct {
	Policy Policy
	// NextDue is the next schedule time after the region was last handled; nil when it is
	// never refreshed. In the past means the region is due now.
	NextDue *time.Time
	// NextBuild is when a refresh can actually start: NextDue moved into the build window.
	NextBuild *time.Time
	Due       bool // NextDue has passed
	Waiting   bool // due, but outside its build window
}

// StatusFor works out a region's next refresh at time now.
func StatusFor(r *store.Region, def *store.Schedule, now time.Time) RegionStatus {
	st := RegionStatus{Policy: PolicyFor(r, def)}
	if st.Policy.Cron == "" {
		return st
	}
	sch, err := Parse(st.Policy.Cron)
	if err != nil {
		return st
	}
	base := now
	if r.RefreshHandledAt != nil {
		base = *r.RefreshHandledAt
	}
	next := sch.Next(base.UTC())
	if next.IsZero() {
		// No occurrence the library can find: unschedulable, never "overdue".
		return st
	}
	st.NextDue = &next
	st.Due = !next.After(now)
	from := next
	if st.Due {
		from = now
	}
	nb := st.Policy.Window.NextOpen(from)
	st.NextBuild = &nb
	st.Waiting = st.Due && !st.Policy.Window.Contains(now)
	return st
}

// Tick decides what is due now and acts on it. It is called once a minute; tests call it
// directly. It returns what it did, for the history line.
func (s *Scheduler) Tick(ctx context.Context) string {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	now := s.now().UTC()
	def, err := s.st.Schedule(ctx)
	if err != nil {
		slog.Warn("schedule: cannot read the schedule", "err", err)
		return ""
	}
	regions, err := s.st.Regions(ctx)
	if err != nil {
		slog.Warn("schedule: cannot read regions", "err", err)
		return ""
	}
	type dueRegion struct {
		r    *store.Region
		pol  Policy
		name string
	}
	var due []dueRegion
	for _, r := range regions {
		if r.RefreshHandledAt == nil {
			// Added since the last tick by something that did not start its clock.
			_ = s.st.SetRegionHandled(ctx, r.ID, now)
			continue
		}
		s.mu.Lock()
		if t, ok := s.handledMem[r.ID]; ok && t.After(*r.RefreshHandledAt) {
			r.RefreshHandledAt = &t
		}
		hold, held := s.retryAfter[r.ID]
		s.mu.Unlock()
		st := StatusFor(r, def, now)
		if !st.Due || st.Waiting {
			continue
		}
		if held && now.Before(hold) {
			continue
		}
		name := r.Name
		if name == "" {
			name = r.ID
		}
		due = append(due, dueRegion{r, st.Policy, name})
	}
	if len(due) == 0 {
		return ""
	}
	// A build is queued or running: whatever is due stays due and is looked at again once it
	// has finished (a build started by hand may well have refreshed it).
	if b, ok := s.q.(busyChecker); ok && b.Busy(ctx, BuildJob) {
		return ""
	}

	// 1. Look at the sources. Results are recorded, but nothing is decided yet.
	var refresh []dueRegion
	var unchanged, failed []string
	var unchangedIDs []string
	for _, d := range due {
		if !d.pol.OnlyChanged || s.Checker == nil {
			refresh = append(refresh, d)
			continue
		}
		res, err := s.Checker.CheckSource(ctx, d.r.ID)
		if err != nil {
			// Not decided: it is still due, and is tried again after a pause.
			_ = s.st.SetRegionCheck(ctx, d.r.ID, now, "error", err.Error())
			s.mu.Lock()
			s.retryAfter[d.r.ID] = now.Add(checkRetry)
			s.mu.Unlock()
			failed = append(failed, d.name)
			continue
		}
		s.mu.Lock()
		delete(s.retryAfter, d.r.ID)
		s.mu.Unlock()
		if res.Changed {
			_ = s.st.SetRegionCheck(ctx, d.r.ID, now, "changed", res.Detail)
			refresh = append(refresh, d)
		} else {
			_ = s.st.SetRegionCheck(ctx, d.r.ID, now, "unchanged", res.Detail)
			unchanged, unchangedIDs = append(unchanged, d.name), append(unchangedIDs, d.r.ID)
		}
	}

	// The checks may have taken minutes, and the tick's deadline may be gone. What follows is
	// quick and must not be cut short half way: its own context, and the time as it is now.
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer ccancel()
	decided := s.now().UTC()
	handle := func(id string) {
		if err := s.st.SetRegionHandled(cctx, id, decided); err != nil {
			slog.Warn("schedule: could not record a refresh decision", "region", id, "err", err)
		}
		s.mu.Lock()
		s.handledMem[id] = decided
		s.mu.Unlock()
	}
	// A region with nothing new is decided whatever happens to the others.
	for _, id := range unchangedIDs {
		handle(id)
	}

	// 2. A build may only start inside each region's window, and the window may have closed
	// while the sources were being asked. Those regions stay due and wait for it to reopen.
	var ids, names []string
	var late []string
	for _, d := range refresh {
		if !d.pol.Window.Contains(decided) {
			late = append(late, d.name)
			continue
		}
		ids, names = append(ids, d.r.ID), append(names, d.name)
	}
	sort.Strings(ids)
	sort.Strings(names)

	h := store.HistoryEntry{Kind: "schedule", At: decided, OK: true}
	details := map[string]any{"refresh": ids, "unchanged": unchanged, "check_failed": failed, "window_closed": late}
	switch {
	case len(ids) == 0 && len(unchanged) == 0 && len(failed) == 0:
		// Only regions whose window closed meanwhile: nothing to record; they are still due.
		return ""
	case len(ids) == 0 && len(unchanged) == 0:
		h.OK = false
		h.Message = "scheduled refresh: could not check " + strings.Join(failed, ", ") + "; trying again in an hour"
	case len(ids) == 0:
		h.Message = "scheduled refresh: nothing new (" + strings.Join(unchanged, ", ") + " unchanged at the source); no build"
		if len(failed) > 0 {
			h.Message += "; could not check " + strings.Join(failed, ", ")
		}
	default:
		// 3. The job first, durably; only then are its regions decided. If the process stops
		// in between, the job is still there and the regions are merely looked at again (and
		// found unchanged against the generation that job produces). If the job cannot be
		// queued, nothing was decided and the regions stay due.
		row, err := s.q.Enqueue(cctx, BuildJob, map[string]any{"refresh": true, "refresh_regions": ids},
			jobs.EnqueueOptions{Trigger: "schedule", Unique: true})
		switch {
		case errors.Is(err, jobs.ErrBusy):
			return "" // lost a race with a build started by hand: still due
		case err != nil:
			h.OK, h.Message = false, "scheduled refresh could not start (it will be tried again): "+err.Error()
		default:
			for _, id := range ids {
				handle(id)
			}
			h.JobID = &row.ID
			h.Message = "scheduled refresh queued for " + strings.Join(names, ", ")
			if len(unchanged) > 0 {
				h.Message += "; unchanged: " + strings.Join(unchanged, ", ")
			}
		}
	}
	if len(late) > 0 {
		h.Message += "; " + strings.Join(late, ", ") + " waits for its build window, which closed during the check"
	}
	s.mu.Lock()
	s.lastRun = &decided
	s.mu.Unlock()
	h.Details = mustJSON(details)
	slog.Info("schedule", "msg", h.Message)
	if err := s.st.AddHistory(cctx, h); err != nil {
		slog.Warn("history", "err", err)
	}
	return h.Message
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// CheckCooldown is how soon after one "check now" another actually asks the sources again.
const CheckCooldown = time.Minute

// CheckAll looks at the source of every region that is refreshed at all and records what it
// found, without building anything ("Check now" in the admin UI). Calls do not pile up on the
// publisher: one runs at a time (and not while the scheduler is deciding), a call that arrives
// within CheckCooldown of the last one changes nothing and reports asked=false (the results of
// the last one are what the caller then reads), and regions that share an extract ask for it
// once (the checker caches by URL).
func (s *Scheduler) CheckAll(ctx context.Context) (asked bool, err error) {
	if s.Checker == nil {
		return false, errors.New("this server cannot check region sources")
	}
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	now := s.now().UTC()
	if !s.lastCheckAll.IsZero() && now.Sub(s.lastCheckAll) < CheckCooldown {
		return false, nil
	}
	regions, err := s.st.Regions(ctx)
	if err != nil {
		return false, err
	}
	s.lastCheckAll = now
	for _, r := range regions {
		if r.RefreshMode == ModeNever {
			continue
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		res, err := s.Checker.CheckSource(ctx, r.ID)
		switch {
		case err != nil:
			_ = s.st.SetRegionCheck(ctx, r.ID, now, "error", err.Error())
		case res.Changed:
			_ = s.st.SetRegionCheck(ctx, r.ID, now, "changed", res.Detail)
		default:
			_ = s.st.SetRegionCheck(ctx, r.ID, now, "unchanged", res.Detail)
		}
	}
	return true, nil
}

// State is the default schedule as reported to the admin UI.
type State struct {
	Cron    string `json:"cron"`
	Enabled bool   `json:"enabled"`
	// NextRun is the next time any region is due (moved into its build window).
	NextRun *time.Time `json:"next_run"`
	LastRun *time.Time `json:"last_run"`
	Default string     `json:"default_cron"`
}

// State returns the default schedule and when the next refresh of any region is due.
func (s *Scheduler) State() State {
	s.mu.Lock()
	st := State{Cron: s.expr, Enabled: s.enabled, LastRun: s.lastRun, Default: DefaultCron}
	s.mu.Unlock()
	now := s.now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	def, err := s.st.Schedule(ctx)
	regions, err2 := s.st.Regions(ctx)
	if err == nil && err2 == nil {
		for _, r := range regions {
			if rs := StatusFor(r, def, now); rs.NextBuild != nil && (st.NextRun == nil || rs.NextBuild.Before(*st.NextRun)) {
				st.NextRun = rs.NextBuild
			}
		}
		if len(regions) > 0 {
			return st
		}
	}
	// No regions yet: when the default schedule would next run.
	if st.Enabled {
		if sch, err := Parse(st.Cron); err == nil {
			if n := sch.Next(now); !n.IsZero() {
				st.NextRun = &n
			}
		}
	}
	return st
}

// NextRuns returns the next n run times of expr after from (for previews).
func NextRuns(expr string, from time.Time, n int) ([]time.Time, error) {
	sch, err := Parse(expr)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, n)
	t := from.UTC()
	for range n {
		t = sch.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}
