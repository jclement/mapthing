// Package jobs runs long background work (builds) one at a time, with persisted progress and
// logs and a server-sent-events fan-out for the admin UI.
//
// A job is a plain function registered by name:
//
//	runner.Register("build", func(ctx context.Context, j *jobs.Job) error { ... })
//
// *Job satisfies the pipeline's build.Reporter (Stage, Progress, Logf), so a builder can
// report straight into it.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/store"
)

// Func is the body of a job. Return nil on success. ctx is cancelled when the job is
// cancelled or the server shuts down.
type Func func(ctx context.Context, j *Job) error

// Status values.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// ErrBusy is returned by Enqueue with Unique when a job of that name is already queued or running.
var ErrBusy = errors.New("a job with that name is already queued or running")

// Job is the handle a running job reports through.
type Job struct {
	ID      int64
	Name    string
	Trigger string
	params  json.RawMessage

	ctx    context.Context
	r      *Runner
	mu     sync.Mutex
	row    store.JobRow
	lastDB time.Time
}

// Context returns the job's context.
func (j *Job) Context() context.Context { return j.ctx }

// Params returns the raw JSON params the job was enqueued with.
func (j *Job) Params() json.RawMessage { return j.params }

// Decode unmarshals the job params into v.
func (j *Job) Decode(v any) error {
	if len(j.params) == 0 {
		return nil
	}
	return json.Unmarshal(j.params, v)
}

// Stage starts a named stage: the previous running stage is marked done.
func (j *Job) Stage(name string) {
	now := time.Now().UTC()
	j.mu.Lock()
	j.row.Stage = name
	j.row.Pct = 0
	j.row.Message = ""
	idx := -1
	for i := range j.row.Stages {
		s := &j.row.Stages[i]
		if s.Status == "running" {
			s.Status, s.Progress, s.FinishedAt = "done", 1, &now
		}
		if s.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		j.row.Stages = append(j.row.Stages, store.StageRow{Name: name, Status: "pending"})
		idx = len(j.row.Stages) - 1
	}
	s := &j.row.Stages[idx]
	s.Status, s.Progress, s.Detail, s.StartedAt, s.FinishedAt = "running", 0, "", &now, nil
	j.mu.Unlock()
	j.flush(true)
	j.Logf("== stage: %s", name)
}

// SetGeneration records the generation a build produced.
func (j *Job) SetGeneration(id string) {
	j.mu.Lock()
	j.row.Generation = id
	j.mu.Unlock()
	j.flush(true)
}

// Progress reports progress within the current stage; frac is 0..1.
func (j *Job) Progress(frac float64, msg string) {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	msg = j.r.redact(msg)
	j.mu.Lock()
	j.row.Pct = frac
	j.row.Message = msg
	for i := range j.row.Stages {
		if s := &j.row.Stages[i]; s.Status == "running" {
			s.Progress, s.Detail = frac, msg
		}
	}
	j.mu.Unlock()
	j.flush(false)
}

// Logf appends a log line (persisted, and streamed to the UI).
func (j *Job) Logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	now := time.Now()
	for _, l := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
		l = j.r.redact(l) // before the store, the event stream and the log
		if err := j.r.st.AppendJobLog(context.WithoutCancel(j.ctx), j.ID, now, l); err != nil {
			slog.Warn("job log", "job", j.ID, "err", err)
		}
		j.r.hub.publish(Event{Type: "log", Data: LogView(store.LogLine{JobID: j.ID, At: now.UTC(), Line: l})})
		if j.r.Log != nil {
			j.r.Log(j.ID, j.Name, now, l)
		}
	}
}

// Snapshot returns the job's current row.
func (j *Job) Snapshot() store.JobRow {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.row
}

// flush publishes the job state, writing to the DB at most once a second unless forced.
func (j *Job) flush(force bool) {
	j.mu.Lock()
	row := j.row
	write := force || time.Since(j.lastDB) >= time.Second
	if write {
		j.lastDB = time.Now()
	}
	j.mu.Unlock()
	if write {
		if err := j.r.st.UpdateJob(context.WithoutCancel(j.ctx), &row); err != nil {
			slog.Warn("job update", "job", j.ID, "err", err)
		}
	}
	j.r.hub.publish(Event{Type: "job", Data: View(row)})
}

// ErrorDetails extracts history details from a job error (for example the pipeline's
// *build.StageError: its stage and log tail). Return ok=false for a plain error.
type ErrorDetails func(err error) (details map[string]any, ok bool)

// Runner executes queued jobs one at a time.
type Runner struct {
	st  *store.Store
	hub *Hub

	// Details, if set, turns job errors into structured history details.
	Details ErrorDetails
	// Log, if set, receives every job log line as well (the server's log: internal/logs).
	Log func(jobID int64, name string, at time.Time, line string)
	// Redact, if set, is applied to everything a job says before it is stored or published:
	// log lines, progress messages, the error, and the failure details kept in history. A
	// tool's output can carry a download URL with credentials or a request header.
	Redact func(string) string

	mu      sync.Mutex
	funcs   map[string]Func
	plans   map[string][]string
	queue   []int64
	current *Job
	cancels map[int64]context.CancelFunc
	wake    chan struct{}
}

// NewRunner returns a runner backed by st. Call Start to begin processing.
func NewRunner(st *store.Store) *Runner {
	return &Runner{st: st, hub: NewHub(), funcs: map[string]Func{}, plans: map[string][]string{"build": BuildStages}, cancels: map[int64]context.CancelFunc{}, wake: make(chan struct{}, 1)}
}

// Hub returns the event hub (SSE fan-out).
func (r *Runner) Hub() *Hub { return r.hub }

// Register adds a job function under name.
func (r *Runner) Register(name string, f Func) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs[name] = f
}

// BuildStages are the pipeline's stages in order; a build job starts with them all pending.
var BuildStages = []string{"guard", "download", "merge", "basemap", "valhalla", "terrain", "verify", "swap", "retention"}

// SetStages declares the stages a job of this name goes through, so the UI can show the
// whole plan from the start. Stages reported that are not in the plan are appended.
func (r *Runner) SetStages(name string, stages []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans[name] = stages
}

// Registered reports whether a job name has a function.
func (r *Runner) Registered(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.funcs[name]
	return ok
}

// EnqueueOptions control Enqueue.
type EnqueueOptions struct {
	Trigger string // manual (default) | schedule | startup
	Unique  bool   // refuse with ErrBusy if a job of this name is queued or running
}

// Enqueue queues a job and returns its row.
func (r *Runner) Enqueue(ctx context.Context, name string, params any, opt EnqueueOptions) (*store.JobRow, error) {
	r.mu.Lock()
	if _, ok := r.funcs[name]; !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("no job named %q", name)
	}
	if opt.Unique && r.busyLocked(ctx, name) {
		r.mu.Unlock()
		return nil, ErrBusy
	}
	r.mu.Unlock()
	if opt.Trigger == "" {
		opt.Trigger = "manual"
	}
	p := []byte("{}")
	if params != nil {
		var err error
		if p, err = json.Marshal(params); err != nil {
			return nil, err
		}
	}
	row, err := r.st.CreateJob(ctx, name, string(p), opt.Trigger)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	for _, n := range r.plans[name] {
		row.Stages = append(row.Stages, store.StageRow{Name: n, Status: "pending"})
	}
	r.mu.Unlock()
	if len(row.Stages) > 0 {
		if err := r.st.UpdateJob(ctx, row); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	r.queue = append(r.queue, row.ID)
	r.mu.Unlock()
	r.hub.publish(Event{Type: "job", Data: View(*row)})
	r.poke()
	return row, nil
}

func (r *Runner) busyLocked(ctx context.Context, name string) bool {
	if r.current != nil && r.current.Name == name {
		return true
	}
	for _, id := range r.queue {
		if j, err := r.st.Job(ctx, id); err == nil && j.Name == name {
			return true
		}
	}
	return false
}

// Busy reports whether a job of this name is queued or running.
func (r *Runner) Busy(ctx context.Context, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busyLocked(ctx, name)
}

// Current returns the running job's snapshot, if any.
func (r *Runner) Current() (store.JobRow, bool) {
	r.mu.Lock()
	j := r.current
	r.mu.Unlock()
	if j == nil {
		return store.JobRow{}, false
	}
	return j.Snapshot(), true
}

// Cancel cancels a queued or running job.
func (r *Runner) Cancel(ctx context.Context, id int64) error {
	r.mu.Lock()
	if c, ok := r.cancels[id]; ok {
		r.mu.Unlock()
		c()
		return nil
	}
	for i, q := range r.queue {
		if q == id {
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			r.mu.Unlock()
			row, err := r.st.Job(ctx, id)
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			row.Status, row.FinishedAt = StatusCancelled, &now
			if err := r.st.UpdateJob(ctx, row); err != nil {
				return err
			}
			r.hub.publish(Event{Type: "job", Data: View(*row)})
			return nil
		}
	}
	r.mu.Unlock()
	return store.ErrNotFound
}

func (r *Runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) redact(s string) string {
	if r.Redact == nil || s == "" {
		return s
	}
	return r.Redact(s)
}

// redactValue applies redact to every string inside a decoded JSON-like value.
func redactValue(v any, redact func(string) string) any {
	switch x := v.(type) {
	case string:
		return redact(x)
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = redact(s)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactValue(e, redact)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = redactValue(e, redact)
		}
		return out
	}
	return v
}

// Start recovers jobs interrupted by a restart (running → failed, queued → re-queued) and
// processes the queue until ctx is done.
func (r *Runner) Start(ctx context.Context) error {
	running, err := r.st.JobsByStatus(ctx, StatusRunning)
	if err != nil {
		return err
	}
	for _, j := range running {
		now := time.Now().UTC()
		j.Status, j.Error, j.FinishedAt = StatusFailed, "interrupted by server restart", &now
		_ = r.st.UpdateJob(ctx, j)
		jid := j.ID
		_ = r.st.AddHistory(ctx, store.HistoryEntry{Kind: "job", OK: false, Message: j.Name + ": interrupted by server restart", JobID: &jid})
	}
	queued, err := r.st.JobsByStatus(ctx, StatusQueued)
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, j := range queued {
		r.queue = append(r.queue, j.ID)
	}
	r.mu.Unlock()
	go r.loop(ctx)
	r.poke()
	return nil
}

func (r *Runner) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		for ctx.Err() == nil {
			r.mu.Lock()
			if len(r.queue) == 0 {
				r.mu.Unlock()
				break
			}
			id := r.queue[0]
			r.queue = r.queue[1:]
			r.mu.Unlock()
			r.run(ctx, id)
		}
	}
}

func (r *Runner) run(parent context.Context, id int64) {
	row, err := r.st.Job(parent, id)
	if err != nil {
		slog.Error("job load", "job", id, "err", err)
		return
	}
	r.mu.Lock()
	f := r.funcs[row.Name]
	row.Stages = row.Stages[:0]
	for _, n := range r.plans[row.Name] {
		row.Stages = append(row.Stages, store.StageRow{Name: n, Status: "pending"})
	}
	ctx, cancel := context.WithCancel(parent)
	j := &Job{ID: row.ID, Name: row.Name, Trigger: row.Trigger, params: json.RawMessage(row.Params), ctx: ctx, r: r, row: *row}
	r.current = j
	r.cancels[id] = cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		r.current = nil
		delete(r.cancels, id)
		r.mu.Unlock()
	}()

	now := time.Now().UTC()
	j.mu.Lock()
	j.row.Status, j.row.StartedAt = StatusRunning, &now
	j.mu.Unlock()
	j.flush(true)
	slog.Info("job start", "job", id, "name", row.Name, "trigger", row.Trigger)

	err = safeRun(ctx, f, j)

	// The error is kept in the job row and history, sent over the event stream and shown
	// on several pages: mask it once, here.
	var errText string
	if err != nil {
		errText = r.redact(err.Error())
	}
	fin := time.Now().UTC()
	j.mu.Lock()
	j.row.FinishedAt = &fin
	for i := range j.row.Stages {
		s := &j.row.Stages[i]
		switch {
		case s.Status == "running" && err == nil:
			s.Status, s.Progress, s.FinishedAt = "done", 1, &fin
		case s.Status == "running":
			s.Status, s.FinishedAt = "failed", &fin
		case s.Status == "pending" && err == nil:
			s.Status = "skipped"
		}
	}
	switch {
	case err == nil:
		j.row.Status, j.row.Pct = StatusSucceeded, 1
	case ctx.Err() != nil && parent.Err() == nil:
		j.row.Status, j.row.Error = StatusCancelled, "cancelled"
	default:
		j.row.Status, j.row.Error = StatusFailed, errText
	}
	final := j.row
	j.mu.Unlock()
	j.flush(true)
	if err != nil {
		j.Logf("job %s: %s", final.Status, errText)
	}

	h := store.HistoryEntry{Kind: "job", OK: err == nil, JobID: &final.ID}
	h.Message = fmt.Sprintf("%s (%s) %s in %s", final.Name, final.Trigger, final.Status, fin.Sub(now).Round(time.Second))
	if err != nil {
		h.Message += ": " + errText
		if r.Details != nil {
			if d, ok := r.Details(err); ok {
				h.Details, _ = json.Marshal(redactValue(d, r.redact))
			}
		}
	}
	if herr := r.st.AddHistory(context.WithoutCancel(parent), h); herr != nil {
		slog.Warn("history", "err", herr)
	}
	_ = r.st.PruneJobLogs(context.WithoutCancel(parent), 50)
	slog.Info("job end", "job", id, "status", final.Status, "err", errText)
}

func safeRun(ctx context.Context, f Func, j *Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return f(ctx, j)
}
