// Package supervisor runs child processes (valhalla_service, the raster renderer): starts them,
// restarts them with backoff when they die, captures their output, probes their health, and
// restarts them on demand (for example after a generation swap).
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// States a child can be in.
const (
	StateStopped     = "stopped"
	StateStarting    = "starting"
	StateRunning     = "running"
	StateUnhealthy   = "unhealthy"
	StateBackoff     = "backoff"
	StateUnavailable = "unavailable" // binary not installed (local dev outside the container)
	StateWaiting     = "waiting"     // precondition not met (e.g. no generation built yet)
)

// Child describes one supervised process.
type Child struct {
	Name    string
	Command []string // argv; Command[0] is looked up on PATH
	Dir     string
	Env     []string // extra KEY=VALUE entries
	// HealthURL, if set, is probed with GET every 10 s; 2xx = healthy.
	HealthURL string
	// Precheck, if set, must return nil before the child is started (else state "waiting";
	// re-checked every 10 s and on Restart).
	Precheck func() error
	// Standby children (a blue/green replacement warming up) are reported but do not count
	// towards readiness until promoted with SetStandby(name, false).
	Standby bool
}

// Status is a child's state for the admin UI.
type Status struct {
	Name      string     `json:"name"`
	State     string     `json:"state"`
	Message   string     `json:"message,omitempty"`
	PID       int        `json:"pid,omitempty"`
	Restarts  int        `json:"restarts"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Healthy   bool       `json:"healthy"`
	Standby   bool       `json:"standby,omitempty"`
	LogTail   []string   `json:"log_tail,omitempty"`
}

type proc struct {
	spec Child

	mu       sync.Mutex
	state    string
	msg      string
	pid      int
	restarts int
	started  *time.Time
	healthy  bool
	tail     []string
	cmd      *exec.Cmd
	restart  chan struct{}
	standby  bool
	cancel   context.CancelFunc
	done     chan struct{}
}

// Supervisor manages children.
type Supervisor struct {
	// Output, if set, receives every line a child writes (stdout and stderr), and a line from
	// the supervisor itself when a child exits (level "error") or is stopped. level is "" for
	// the child's own lines: the receiver works it out. Set it before Start.
	Output func(child, level, line string)

	mu    sync.Mutex
	procs map[string]*proc
	order []string
	ctx   context.Context
	// lookPath is exec.LookPath, replaceable in tests.
	lookPath func(string) (string, error)
}

// New returns a supervisor. Child output goes to Output (the server's log) and each child's
// last lines are kept for Status.
func New() *Supervisor {
	return &Supervisor{procs: map[string]*proc{}, lookPath: exec.LookPath}
}

func (s *Supervisor) emit(child, level, line string) {
	if s.Output != nil {
		s.Output(child, level, line)
	}
}

// Add registers a child. Call before Start.
// Add registers a child. Before Start it waits for Start; after Start it starts at once.
func (s *Supervisor) Add(c Child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &proc{spec: c, state: StateStopped, restart: make(chan struct{}, 1), standby: c.Standby, done: make(chan struct{})}
	s.procs[c.Name] = p
	s.order = append(s.order, c.Name)
	if s.ctx != nil {
		s.launch(p)
	}
}

func (s *Supervisor) launch(p *proc) {
	ctx, cancel := context.WithCancel(s.ctx)
	p.cancel = cancel
	go func() {
		defer close(p.done)
		s.manage(ctx, p)
	}()
}

// Remove stops a child (SIGTERM, then SIGKILL) and forgets it. Unknown names are ignored.
func (s *Supervisor) Remove(name string) {
	s.mu.Lock()
	p, ok := s.procs[name]
	if ok {
		delete(s.procs, name)
		for i, n := range s.order {
			if n == name {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
	if !ok || p.cancel == nil {
		return
	}
	p.cancel()
	<-p.done
}

// SetStandby marks a child as standby (not counted for readiness) or active.
func (s *Supervisor) SetStandby(name string, standby bool) {
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if ok {
		p.mu.Lock()
		p.standby = standby
		p.mu.Unlock()
	}
}

// markHealthy records a successful external health check (BlueGreen just probed the child),
// so readiness does not wait for the child's own next probe.
func (s *Supervisor) markHealthy(name string) {
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if ok {
		p.mu.Lock()
		p.healthy = true
		if p.state == StateRunning {
			p.msg = ""
		}
		p.mu.Unlock()
	}
}

// Status returns one child's status.
func (s *Supervisor) Status(name string) (Status, bool) {
	for _, st := range s.Statuses() {
		if st.Name == name {
			return st, true
		}
	}
	return Status{}, false
}

// Start launches every child's management loop; they stop when ctx is done.
func (s *Supervisor) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	procs := make([]*proc, 0, len(s.order))
	for _, n := range s.order {
		procs = append(procs, s.procs[n])
	}
	for _, p := range procs {
		s.launch(p)
	}
	s.mu.Unlock()
}

// Restart restarts one child (or starts it if waiting). Unknown names are an error.
func (s *Supervisor) Restart(name string) error {
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no child %q", name)
	}
	select {
	case p.restart <- struct{}{}:
	default:
	}
	return nil
}

// RestartAll restarts every child (generation swap hook).
func (s *Supervisor) RestartAll(context.Context) error {
	s.mu.Lock()
	names := append([]string(nil), s.order...)
	s.mu.Unlock()
	var errs []error
	for _, n := range names {
		errs = append(errs, s.Restart(n))
	}
	return errors.Join(errs...)
}

// Statuses returns every child's status in registration order.
func (s *Supervisor) Statuses() []Status {
	s.mu.Lock()
	procs := make([]*proc, 0, len(s.order))
	for _, n := range s.order {
		procs = append(procs, s.procs[n])
	}
	s.mu.Unlock()
	out := make([]Status, 0, len(procs))
	for _, p := range procs {
		p.mu.Lock()
		out = append(out, Status{
			Name: p.spec.Name, State: p.state, Message: p.msg, PID: p.pid, Restarts: p.restarts,
			StartedAt: p.started, Healthy: p.healthy, Standby: p.standby, LogTail: append([]string(nil), p.tail...),
		})
		p.mu.Unlock()
	}
	return out
}

func (p *proc) set(state, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != state {
		slog.Info("child state", "child", p.spec.Name, "state", state, "msg", msg)
	}
	p.state, p.msg = state, msg
	if state != StateRunning && state != StateUnhealthy {
		p.healthy = false
	}
}

func (p *proc) addLine(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tail = append(p.tail, line)
	if len(p.tail) > 100 {
		p.tail = p.tail[len(p.tail)-100:]
	}
}

const (
	minBackoff  = time.Second
	maxBackoff  = time.Minute
	stableAfter = time.Minute
	pollEvery   = 10 * time.Second
	stopTimeout = 10 * time.Second
)

// manage is one child's loop: precheck → start → wait (probing health) → backoff → repeat.
func (s *Supervisor) manage(ctx context.Context, p *proc) {
	backoff := minBackoff
	for ctx.Err() == nil {
		if len(p.spec.Command) == 0 {
			p.set(StateUnavailable, "no command configured")
			return
		}
		bin, err := s.lookPath(p.spec.Command[0])
		if err != nil {
			p.set(StateUnavailable, fmt.Sprintf("%s not found", p.spec.Command[0]))
			if !s.wait(ctx, p, 0) {
				return
			}
			continue
		}
		if p.spec.Precheck != nil {
			if err := p.spec.Precheck(); err != nil {
				p.set(StateWaiting, err.Error())
				if !s.wait(ctx, p, pollEvery) {
					return
				}
				continue
			}
		}
		start := time.Now()
		restarted, err := s.runOnce(ctx, p, bin)
		if ctx.Err() != nil {
			p.set(StateStopped, "")
			return
		}
		if restarted {
			backoff = minBackoff
			continue
		}
		if time.Since(start) > stableAfter {
			backoff = minBackoff
		}
		msg := "exited"
		if err != nil {
			msg = err.Error()
		}
		p.set(StateBackoff, fmt.Sprintf("%s; restarting in %s", msg, backoff))
		p.mu.Lock()
		p.restarts++
		n := p.restarts
		p.mu.Unlock()
		// In the child's own log, so its last lines and the reason it went sit together.
		s.emit(p.spec.Name, "error", fmt.Sprintf("%s exited after %s (%s); restart %d in %s",
			p.spec.Name, time.Since(start).Round(time.Second), msg, n, backoff))
		if !s.wait(ctx, p, backoff) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// wait sleeps d (0 = until a restart request) or until a restart request; false if ctx ended.
func (s *Supervisor) wait(ctx context.Context, p *proc, d time.Duration) bool {
	var tc <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		tc = t.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-p.restart:
	case <-tc:
	}
	return true
}

// runOnce starts the child and blocks until it exits. restarted is true when it was stopped
// because a restart was requested.
func (s *Supervisor) runOnce(ctx context.Context, p *proc, bin string) (restarted bool, err error) {
	p.set(StateStarting, "")
	cmd := exec.Command(bin, p.spec.Command[1:]...) //nolint:noctx // lifetime is managed here: SIGTERM to the group, then SIGKILL
	cmd.Dir = p.spec.Dir
	cmd.Env = append(os.Environ(), p.spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			p.addLine(line)
			s.emit(p.spec.Name, "", line)
		}
		_, _ = io.Copy(io.Discard, pr)
	}()

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		<-done
		s.emit(p.spec.Name, "error", fmt.Sprintf("%s could not start: %v", p.spec.Name, err))
		return false, err
	}
	now := time.Now().UTC()
	p.mu.Lock()
	p.cmd, p.pid, p.started = cmd, cmd.Process.Pid, &now
	p.mu.Unlock()
	p.set(StateRunning, "")
	s.emit(p.spec.Name, "info", fmt.Sprintf("%s started (pid %d)", p.spec.Name, cmd.Process.Pid))

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Probe every second until the first healthy answer (so a fresh instance is reported
	// ready quickly), then every pollEvery.
	probe := time.NewTicker(time.Second)
	defer probe.Stop()
	everHealthy := false
	if p.spec.HealthURL == "" {
		p.mu.Lock()
		p.healthy = true
		p.mu.Unlock()
	}
	finish := func(werr error) (bool, error) {
		_ = pw.Close()
		<-done
		p.mu.Lock()
		p.cmd, p.pid = nil, 0
		p.mu.Unlock()
		return restarted, werr
	}
	for {
		select {
		case werr := <-exited:
			return finish(werr)
		case <-ctx.Done():
			stop(cmd, exited)
			return finish(nil)
		case <-p.restart:
			restarted = true
			stop(cmd, exited)
			return finish(nil)
		case <-probe.C:
			if p.spec.HealthURL != "" {
				ok := probeURL(ctx, p.spec.HealthURL)
				p.mu.Lock()
				p.healthy = ok
				p.mu.Unlock()
				switch {
				case ok:
					p.set(StateRunning, "")
					if !everHealthy {
						everHealthy = true
						probe.Reset(pollEvery)
					}
				case everHealthy:
					p.set(StateUnhealthy, "health probe failed")
				default:
					p.set(StateRunning, "waiting for the health probe")
				}
			}
		}
	}
}

// stop sends SIGTERM to the child's process group, then SIGKILL after a timeout, and waits
// for the exit.
func stop(cmd *exec.Cmd, exited chan error) {
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case err := <-exited:
		_ = err // the exit status of a process we stopped is not interesting
	case <-time.After(stopTimeout):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		err := <-exited
		_ = err
	}
}

func probeURL(ctx context.Context, u string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
