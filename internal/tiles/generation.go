package tiles

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CurrentGeneration returns the ID (directory name) and absolute directory of the live
// generation, following dataDir/current. ok=false when nothing is live yet.
func CurrentGeneration(dataDir string) (id, dir string, ok bool) {
	link := filepath.Join(dataDir, "current")
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", "", false
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.IsDir() {
		return "", "", false
	}
	return filepath.Base(target), target, true
}

// Watcher polls the current symlink and calls OnSwap when its target changes.
type Watcher struct {
	DataDir string
	OnSwap  func(ctx context.Context, oldID, newID string)

	mu   sync.Mutex
	last string
}

// Check compares the current generation with the last seen one and calls OnSwap on change.
// Safe to call from the build's restart hook for an immediate swap.
func (w *Watcher) Check(ctx context.Context) {
	id, _, _ := CurrentGeneration(w.DataDir)
	w.mu.Lock()
	old := w.last
	changed := id != old
	w.last = id
	w.mu.Unlock()
	if changed && w.OnSwap != nil {
		slog.Info("generation changed", "from", old, "to", id)
		w.OnSwap(ctx, old, id)
	}
}

// Prime records the current generation without firing OnSwap.
func (w *Watcher) Prime() {
	id, _, _ := CurrentGeneration(w.DataDir)
	w.mu.Lock()
	w.last = id
	w.mu.Unlock()
}

// Run polls every interval until ctx is done.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Check(ctx)
		}
	}
}
