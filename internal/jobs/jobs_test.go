package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func waitStatus(t *testing.T, st *store.Store, id int64, want string) *store.JobRow {
	t.Helper()
	for range 200 {
		j, err := st.Job(context.Background(), id)
		if err == nil && j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := st.Job(context.Background(), id)
	t.Fatalf("job %d status %q want %q", id, j.Status, want)
	return nil
}

func TestRunOneAtATimeAndFailureDetails(t *testing.T) {
	st := open(t)
	r := NewRunner(st)
	r.Details = func(err error) (map[string]any, bool) { return map[string]any{"stage": "planetiler"}, true }
	running := make(chan int, 4)
	release := make(chan struct{})
	r.Register("a", func(ctx context.Context, j *Job) error {
		running <- int(j.ID)
		<-release
		return nil
	})
	r.Register("bad", func(ctx context.Context, j *Job) error { return errors.New("exploded") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	j1, _ := r.Enqueue(ctx, "a", nil, EnqueueOptions{})
	j2, _ := r.Enqueue(ctx, "bad", nil, EnqueueOptions{})
	<-running
	if j, _ := st.Job(ctx, j2.ID); j.Status != StatusQueued {
		t.Fatalf("second job ran concurrently: %s", j.Status)
	}
	if _, err := r.Enqueue(ctx, "a", nil, EnqueueOptions{Unique: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("unique: %v", err)
	}
	close(release)
	waitStatus(t, st, j1.ID, StatusSucceeded)
	f := waitStatus(t, st, j2.ID, StatusFailed)
	if f.Error != "exploded" {
		t.Fatalf("error %q", f.Error)
	}
	h, _ := st.History(ctx, "job", 10)
	if len(h) != 2 || h[0].OK || string(h[0].Details) != `{"stage":"planetiler"}` {
		t.Fatalf("history %+v", h)
	}
	if _, err := r.Enqueue(ctx, "nope", nil, EnqueueOptions{}); err == nil {
		t.Fatal("unregistered job accepted")
	}
}

func TestCancelAndRecovery(t *testing.T) {
	st := open(t)
	// a job left running by a previous process, and one left queued
	stale, _ := st.CreateJob(context.Background(), "a", "{}", "manual")
	stale.Status = StatusRunning
	_ = st.UpdateJob(context.Background(), stale)
	queued, _ := st.CreateJob(context.Background(), "a", "{}", "manual")

	r := NewRunner(st)
	r.Register("a", func(ctx context.Context, j *Job) error {
		j.Stage("wait")
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if j := waitStatus(t, st, stale.ID, StatusFailed); j.Error != "interrupted by server restart" {
		t.Fatalf("stale: %q", j.Error)
	}
	waitStatus(t, st, queued.ID, StatusRunning)
	if err := r.Cancel(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, queued.ID, StatusCancelled)
}
