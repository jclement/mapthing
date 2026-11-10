package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// BlueGreen replaces a localhost service without downtime: the new instance starts on the
// other of two ports as a standby child, and only when it is healthy (HealthURL) and passes
// Ready does Switch point clients at it; then the old instance stops. If the new instance
// never becomes ready, it is stopped and the old one keeps serving.
type BlueGreen struct {
	Name  string // child name prefix, e.g. "valhalla"; children are "<name>@<port>"
	Sup   *Supervisor
	Ports [2]int
	// Child returns the child that serves generation gen on port (Name is set by BlueGreen).
	Child func(gen string, port int) (Child, error)
	// Ready is an extra probe of a started instance at baseURL (e.g. a test route). Optional.
	Ready func(ctx context.Context, baseURL string) error
	// Switch points clients at baseURL. Called before the old instance is stopped.
	Switch func(baseURL string)
	// ReadyTimeout bounds the wait for a new instance (default 5 min).
	ReadyTimeout time.Duration
	// PollEvery is the readiness poll interval (default 500 ms).
	PollEvery time.Duration

	// op serialises changes: it is held from Prepare until the Standby is committed or
	// aborted, so a second activation waits for the first to settle.
	op sync.Mutex
	// mu guards the fields below (read by Active at any time).
	mu     sync.Mutex
	active string
	slot   int
	gen    string
	// stopping tracks the previous instance while it shuts down (see Activate).
	stopping sync.WaitGroup
}

// ErrUnavailable means the child's binary is not installed (local development).
var ErrUnavailable = errors.New("service binary not installed")

// Active returns the active child name and generation ("" before the first activation).
func (b *BlueGreen) Active() (name, gen string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.active, b.gen
}

// Activate makes an instance serving gen the active one. It returns once the new instance
// is serving (or failed, leaving the previous one in place). The previous instance is told to
// stop and Activate does not wait for it: a service that is slow to exit would otherwise hold
// up whatever follows the switch (the rest of a generation swap) for its whole stop timeout.
func (b *BlueGreen) Activate(ctx context.Context, gen string) error {
	st, err := b.Prepare(ctx, gen)
	if err != nil {
		return err
	}
	st.Commit()
	return nil
}

// Standby is a new instance that is running and ready but not yet serving: clients still
// reach the previous instance, which is untouched. Exactly one of Commit and Abort must be
// called; until then other activations of the same service wait.
type Standby struct {
	b    *BlueGreen
	once sync.Once
	// "" when the generation asked for was already the active one (nothing to switch).
	name string
	slot int
	gen  string
	base string
}

// Prepare starts an instance for gen beside the active one and waits until it is ready,
// without switching clients to it or stopping anything. It lets a caller make the switch the
// last step of a larger change: prepare, do the part that can fail, then Commit, or Abort and
// be exactly where it started.
func (b *BlueGreen) Prepare(ctx context.Context, gen string) (*Standby, error) {
	b.op.Lock()
	active, activeGen := b.Active()
	if gen == activeGen && active != "" {
		return &Standby{b: b}, nil // holds op until Commit or Abort, like any other
	}
	b.stopping.Wait() // the instance before last must be gone: its port is the one used next
	b.mu.Lock()
	slot := 0
	if b.active != "" {
		slot = 1 - b.slot
	}
	b.mu.Unlock()
	port := b.Ports[slot]
	name := b.Name + "@" + strconv.Itoa(port)
	b.Sup.Remove(name) // a leftover from an earlier failed attempt
	c, err := b.Child(gen, port)
	if err != nil {
		b.op.Unlock()
		return nil, fmt.Errorf("%s: %w", b.Name, err)
	}
	c.Name = name
	c.Standby = active != ""
	b.Sup.Add(c)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	if err := b.waitReady(ctx, c, base); err != nil {
		defer b.op.Unlock()
		if errors.Is(err, ErrUnavailable) && active == "" {
			return nil, err // keep the child so its state shows as unavailable
		}
		b.Sup.Remove(name)
		return nil, fmt.Errorf("%s: new instance for %s not ready, keeping the old one: %w", b.Name, gen, err)
	}
	return &Standby{b: b, name: name, slot: slot, gen: gen, base: base}, nil
}

// Commit points clients at the standby and retires the previous instance.
func (s *Standby) Commit() {
	s.once.Do(func() {
		b := s.b
		defer b.op.Unlock()
		if s.name == "" {
			return
		}
		if b.Switch != nil {
			b.Switch(s.base)
		}
		b.Sup.markHealthy(s.name)
		b.Sup.SetStandby(s.name, false)
		b.mu.Lock()
		old := b.active
		b.active, b.slot, b.gen = s.name, s.slot, s.gen
		b.mu.Unlock()
		if old != "" {
			b.stopping.Add(1)
			go func() {
				defer b.stopping.Done()
				b.Sup.Remove(old)
			}()
		}
		slog.Info("service switched", "service", b.Name, "to", s.name, "generation", s.gen, "previous", old)
	})
}

// Abort stops the standby. Clients were never switched and the previous instance never
// stopped, so the service is exactly as it was before Prepare.
func (s *Standby) Abort() {
	s.once.Do(func() {
		b := s.b
		defer b.op.Unlock()
		if s.name == "" {
			return
		}
		b.Sup.Remove(s.name)
		slog.Info("standby discarded", "service", b.Name, "instance", s.name, "generation", s.gen)
	})
}

func (b *BlueGreen) waitReady(ctx context.Context, c Child, base string) error {
	timeout := b.ReadyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	every := b.PollEvery
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(every)
	defer t.Stop()
	var last error
	for {
		st, _ := b.Sup.Status(c.Name)
		switch st.State {
		case StateUnavailable:
			return fmt.Errorf("%w: %s", ErrUnavailable, st.Message)
		case StateRunning, StateUnhealthy:
			last = nil
			if c.HealthURL != "" && !probeURL(ctx, c.HealthURL) {
				last = errors.New("health probe failing")
			} else if b.Ready != nil {
				last = b.Ready(ctx, base)
			}
			if last == nil {
				return nil
			}
		case StateBackoff:
			last = fmt.Errorf("instance exited: %s", st.Message)
		}
		select {
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			return last
		case <-t.C:
		}
	}
}
