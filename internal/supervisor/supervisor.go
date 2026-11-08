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
	"path/filepath"
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
	logPath  string
}

// Supervisor manages children.
type Supervisor struct {
	logDir string
	mu     sync.Mutex
	procs  map[string]*proc
	order  []string
	ctx    context.Context
	// lookPath is exec.LookPath, replaceable in tests.
	lookPath func(string) (string, error)
}

// New returns a supervisor writing child logs under logDir (created if needed; "" = no files).
func New(logDir string) *Supervisor {
	return &Supervisor{logDir: logDir, procs: map[string]*proc{}, lookPath: exec.LookPath}
}

// Add registers a child. Call before Start.
func (s *Supervisor) Add(c Child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &proc{spec: c, state: StateStopped, restart: make(chan struct{}, 1)}
	if s.logDir != "" {
		p.logPath = filepath.Join(s.logDir, c.Name+".log")
	}
	s.procs[c.Name] = p
	s.order = append(s.order, c.Name)
}

// Start launches every child's management loop; they stop when ctx is done.
func (s *Supervisor) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	procs := make([]*proc, 0, len(s.order))
	for _, n := range s.order {
		procs = append(procs, s.procs[n])
	}
	s.mu.Unlock()
	if s.logDir != "" {
		_ = os.MkdirAll(s.logDir, 0o755)
	}
	for _, p := range procs {
		go s.manage(ctx, p)
	}
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
			StartedAt: p.started, Healthy: p.healthy, LogTail: append([]string(nil), p.tail...),
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
		p.mu.Unlock()
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

	var logFile *os.File
	if p.logPath != "" {
		rotate(p.logPath, 10<<20)
		logFile, _ = os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			p.addLine(line)
			if logFile != nil {
				_, _ = fmt.Fprintf(logFile, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		<-done
		if logFile != nil {
			_ = logFile.Close()
		}
		return false, err
	}
	now := time.Now().UTC()
	p.mu.Lock()
	p.cmd, p.pid, p.started = cmd, cmd.Process.Pid, &now
	p.mu.Unlock()
	p.set(StateRunning, "")

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	probe := time.NewTicker(pollEvery)
	defer probe.Stop()
	if p.spec.HealthURL == "" {
		p.mu.Lock()
		p.healthy = true
		p.mu.Unlock()
	}
	finish := func(werr error) (bool, error) {
		_ = pw.Close()
		<-done
		if logFile != nil {
			_ = logFile.Close()
		}
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
				if ok {
					p.set(StateRunning, "")
				} else {
					p.set(StateUnhealthy, "health probe failed")
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

// rotate moves path to path.1 when it exceeds max bytes.
func rotate(path string, max int64) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > max {
		_ = os.Rename(path, path+".1")
	}
}
