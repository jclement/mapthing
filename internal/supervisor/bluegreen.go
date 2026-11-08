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

	mu     sync.Mutex
	active string
	slot   int
	gen    string
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
// is serving (or failed, leaving the previous one in place).
func (b *BlueGreen) Activate(ctx context.Context, gen string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen == b.gen && b.active != "" {
		return nil
	}
	slot := 0
	if b.active != "" {
		slot = 1 - b.slot
	}
	port := b.Ports[slot]
	name := b.Name + "@" + strconv.Itoa(port)
	b.Sup.Remove(name) // a leftover from an earlier failed attempt
	c, err := b.Child(gen, port)
	if err != nil {
		return fmt.Errorf("%s: %w", b.Name, err)
	}
	c.Name = name
	c.Standby = b.active != ""
	b.Sup.Add(c)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	if err := b.waitReady(ctx, c, base); err != nil {
		if errors.Is(err, ErrUnavailable) && b.active == "" {
			return err // keep the child so its state shows as unavailable
		}
		b.Sup.Remove(name)
		return fmt.Errorf("%s: new instance for %s not ready, keeping the old one: %w", b.Name, gen, err)
	}
	if b.Switch != nil {
		b.Switch(base)
	}
	b.Sup.markHealthy(name)
	b.Sup.SetStandby(name, false)
	old := b.active
	b.active, b.slot, b.gen = name, slot, gen
	if old != "" {
		b.Sup.Remove(old)
	}
	slog.Info("service switched", "service", b.Name, "to", name, "generation", gen, "previous", old)
	return nil
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
