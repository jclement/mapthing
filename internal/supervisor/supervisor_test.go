package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitState(t *testing.T, s *Supervisor, name, want string) Status {
	t.Helper()
	var last Status
	for range 200 {
		for _, st := range s.Statuses() {
			if st.Name == name {
				last = st
				if st.State == want {
					return st
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: state %q, want %q (%s)", name, last.State, want, last.Message)
	return last
}

func TestUnavailableBinary(t *testing.T) {
	s := New("")
	s.Add(Child{Name: "ghost", Command: []string{"definitely-not-a-real-binary-xyz"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	st := waitState(t, s, "ghost", StateUnavailable)
	if st.Restarts != 0 {
		t.Fatalf("crash-looped: %+v", st)
	}
}

func TestWaitingPrecheck(t *testing.T) {
	s := New("")
	ready := false
	s.Add(Child{Name: "w", Command: []string{"sleep", "60"}, Precheck: func() error {
		if !ready {
			return errors.New("no generation")
		}
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	waitState(t, s, "w", StateWaiting)
	ready = true
	_ = s.Restart("w")
	waitState(t, s, "w", StateRunning)
}

func TestRestartAndBackoff(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.Add(Child{Name: "sleeper", Command: []string{"sh", "-c", "echo hello; exec sleep 60"}})
	s.Add(Child{Name: "crasher", Command: []string{"sh", "-c", "echo boom; exit 3"}})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	first := waitState(t, s, "sleeper", StateRunning)
	if err := s.Restart("sleeper"); err != nil {
		t.Fatal(err)
	}
	var second Status
	for range 200 {
		second = waitState(t, s, "sleeper", StateRunning)
		if second.PID != first.PID && second.PID != 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if second.PID == first.PID {
		t.Fatal("restart did not replace the process")
	}
	cr := waitState(t, s, "crasher", StateBackoff)
	if cr.Restarts < 1 {
		t.Fatalf("crasher restarts %d", cr.Restarts)
	}
	found := false
	for _, l := range cr.LogTail {
		if l == "boom" {
			found = true
		}
	}
	if !found {
		t.Fatalf("log tail %v", cr.LogTail)
	}
	if err := s.Restart("nope"); err == nil {
		t.Fatal("expected error for unknown child")
	}
	cancel()
	waitState(t, s, "sleeper", StateStopped)
}
