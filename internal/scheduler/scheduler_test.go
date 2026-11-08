package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/store"
)

type fakeQ struct {
	calls []string
	err   error
}

func (f *fakeQ) Enqueue(_ context.Context, name string, _ any, opt jobs.EnqueueOptions) (*store.JobRow, error) {
	f.calls = append(f.calls, name+":"+opt.Trigger)
	if f.err != nil {
		return nil, f.err
	}
	return &store.JobRow{ID: 7}, nil
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

func TestSchedulerStateAndFire(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	q := &fakeQ{}
	s := New(st, q)
	s.now = func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := s.Start(cctx); err != nil {
		t.Fatal(err)
	}
	stt := s.State()
	if stt.Cron != DefaultCron || !stt.Enabled || stt.NextRun == nil || !stt.NextRun.Equal(time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("state %+v", stt)
	}
	if err := s.Set(ctx, "bad", true); err == nil {
		t.Fatal("expected error")
	}
	if err := s.Set(ctx, "0 5 * * *", false); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Enabled || st.NextRun != nil || st.Cron != "0 5 * * *" {
		t.Fatalf("disabled state %+v", st)
	}
	sc, _ := st.Schedule(ctx)
	if sc.Cron != "0 5 * * *" || sc.Enabled {
		t.Fatalf("stored %+v", sc)
	}
	s.fire()
	q.err = jobs.ErrBusy
	s.fire()
	if len(q.calls) != 2 || q.calls[0] != "build:schedule" {
		t.Fatalf("calls %v", q.calls)
	}
	h, _ := st.History(ctx, "schedule", 10)
	if len(h) != 2 || h[0].Message != "scheduled refresh skipped: a build is already queued or running" || h[1].Message != "scheduled refresh queued" {
		t.Fatalf("history %+v", h)
	}
}
