package supervisor

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	s := New()
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
	s := New()
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
	s := New()
	var outMu sync.Mutex
	var out []string
	s.Output = func(child, level, line string) {
		outMu.Lock()
		defer outMu.Unlock()
		out = append(out, child+"|"+level+"|"+line)
	}
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
	// Everything a child prints, and why it went, reaches the log under the child's name.
	// (The exit line is written just after the state changes: give it a moment.)
	wants := []string{"sleeper||hello", "crasher||boom", "crasher|error|crasher exited after", "exit status 3", "sleeper|info|sleeper started (pid "}
	var all string
	for range 200 {
		outMu.Lock()
		all = strings.Join(out, "\n")
		outMu.Unlock()
		missing := false
		for _, want := range wants {
			missing = missing || !strings.Contains(all, want)
		}
		if !missing {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range wants {
		if !strings.Contains(all, want) {
			t.Errorf("output lacks %q:\n%s", want, all)
		}
	}
	if err := s.Restart("nope"); err == nil {
		t.Fatal("expected error for unknown child")
	}
	cancel()
	waitState(t, s, "sleeper", StateStopped)
}

// A child that dies but leaves a helper holding its output open (the renderer's start script
// did, when its X server failed to start) is still seen to have exited, restarted, and the
// helper is not left running.
func TestChildThatLeavesAHelperBehindIsRestarted(t *testing.T) {
	old := waitDelay
	waitDelay = 200 * time.Millisecond
	defer func() { waitDelay = old }()
	s := New()
	marker := t.TempDir() + "/helper.pid"
	// The helper inherits stdout and outlives the script by a long way.
	s.Add(Child{Name: "leaky", Command: []string{"sh", "-c", "sleep 300 & echo $! > " + marker + "; echo failing; exit 1"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	st := waitState(t, s, "leaky", StateBackoff)
	if st.Restarts < 1 {
		t.Fatalf("restarts %d: the exit was not noticed", st.Restarts)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	alive := true
	for range 80 {
		if err := syscall.Kill(pid, 0); err != nil {
			alive = false
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if alive {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("the helper (pid %d) was left running", pid)
	}
}
