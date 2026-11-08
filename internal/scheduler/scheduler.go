// Package scheduler triggers refresh builds on a cron schedule (UTC).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Parse validates a 5-field cron expression (or @daily/@weekly descriptors).
func Parse(expr string) (cron.Schedule, error) {
	s, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return s, nil
}

// Enqueuer is the part of the job runner the scheduler needs.
type Enqueuer interface {
	Enqueue(ctx context.Context, name string, params any, opt jobs.EnqueueOptions) (*store.JobRow, error)
}

// Scheduler owns the cron loop.
type Scheduler struct {
	st  *store.Store
	q   Enqueuer
	now func() time.Time

	mu      sync.Mutex
	c       *cron.Cron
	entry   cron.EntryID
	expr    string
	enabled bool
	lastRun *time.Time
}

// New returns a scheduler. Call Start to load the stored schedule.
func New(st *store.Store, q Enqueuer) *Scheduler {
	return &Scheduler{st: st, q: q, now: time.Now, c: cron.New(cron.WithLocation(time.UTC), cron.WithParser(parser))}
}

// Start loads the schedule from the store and starts the cron loop.
func (s *Scheduler) Start(ctx context.Context) error {
	sc, err := s.st.Schedule(ctx)
	if err != nil {
		return err
	}
	if err := s.apply(sc.Cron, sc.Enabled); err != nil {
		slog.Warn("stored schedule invalid, using default", "cron", sc.Cron, "err", err)
		if err := s.apply(DefaultCron, sc.Enabled); err != nil {
			return err
		}
	}
	if h, err := s.st.History(ctx, "schedule", 1); err == nil && len(h) > 0 {
		t := h[0].At
		s.lastRun = &t
	}
	s.c.Start()
	go func() {
		<-ctx.Done()
		<-s.c.Stop().Done()
	}()
	return nil
}

func (s *Scheduler) apply(expr string, enabled bool) error {
	if _, err := Parse(expr); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry != 0 {
		s.c.Remove(s.entry)
		s.entry = 0
	}
	s.expr, s.enabled = expr, enabled
	if !enabled {
		return nil
	}
	id, err := s.c.AddFunc(expr, s.fire)
	if err != nil {
		return err
	}
	s.entry = id
	return nil
}

// Set validates and stores a new schedule, and applies it.
func (s *Scheduler) Set(ctx context.Context, expr string, enabled bool) error {
	if _, err := Parse(expr); err != nil {
		return err
	}
	if err := s.st.SetSchedule(ctx, expr, enabled); err != nil {
		return err
	}
	return s.apply(expr, enabled)
}

const refreshSetting = "schedule.refresh_downloads"

// RefreshDownloads reports whether scheduled builds re-download extracts (default true).
func (s *Scheduler) RefreshDownloads(ctx context.Context) bool {
	v, err := s.st.Setting(ctx, refreshSetting)
	return err != nil || v != "false"
}

// SetRefreshDownloads stores whether scheduled builds re-download extracts.
func (s *Scheduler) SetRefreshDownloads(ctx context.Context, on bool) error {
	return s.st.SetSetting(ctx, refreshSetting, strconv.FormatBool(on))
}

// fire is the cron callback.
func (s *Scheduler) fire() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := s.now().UTC()
	s.mu.Lock()
	s.lastRun = &now
	s.mu.Unlock()
	h := store.HistoryEntry{Kind: "schedule", At: now}
	row, err := s.q.Enqueue(ctx, BuildJob, map[string]any{"refresh": s.RefreshDownloads(ctx)}, jobs.EnqueueOptions{Trigger: "schedule", Unique: true})
	switch {
	case errors.Is(err, jobs.ErrBusy):
		h.OK, h.Message = true, "scheduled refresh skipped: a build is already queued or running"
	case err != nil:
		h.OK, h.Message = false, "scheduled refresh could not start: "+err.Error()
	default:
		h.OK, h.Message, h.JobID = true, "scheduled refresh queued", &row.ID
	}
	slog.Info("schedule fired", "msg", h.Message)
	if err := s.st.AddHistory(ctx, h); err != nil {
		slog.Warn("history", "err", err)
	}
}

// State is the schedule as reported to the admin UI.
type State struct {
	Cron    string     `json:"cron"`
	Enabled bool       `json:"enabled"`
	NextRun *time.Time `json:"next_run"`
	LastRun *time.Time `json:"last_run"`
	Default string     `json:"default_cron"`
}

// State returns the current schedule and its next run.
func (s *Scheduler) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Cron: s.expr, Enabled: s.enabled, LastRun: s.lastRun, Default: DefaultCron}
	if s.enabled {
		if sch, err := Parse(s.expr); err == nil {
			n := sch.Next(s.now().UTC())
			st.NextRun = &n
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
		out = append(out, t)
	}
	return out, nil
}
