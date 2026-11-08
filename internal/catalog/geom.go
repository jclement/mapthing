package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// polygon is rings (first outer, rest holes); multiPolygon is a list of polygons.
type polygon [][][2]float64
type multiPolygon []polygon

func parseGeometry(typ string, raw json.RawMessage) (multiPolygon, error) {
	switch typ {
	case "Polygon":
		var p polygon
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return multiPolygon{p}, nil
	case "MultiPolygon":
		var mp multiPolygon
		if err := json.Unmarshal(raw, &mp); err != nil {
			return nil, err
		}
		return mp, nil
	}
	return nil, fmt.Errorf("unsupported geometry %q", typ)
}

func (mp multiPolygon) bbox() BBox {
	b := BBox{180, 90, -180, -90}
	for _, p := range mp {
		if len(p) == 0 {
			continue
		}
		for _, pt := range p[0] {
			b[0], b[1] = math.Min(b[0], pt[0]), math.Min(b[1], pt[1])
			b[2], b[3] = math.Max(b[2], pt[0]), math.Max(b[3], pt[1])
		}
	}
	return b
}

// rings flattens every ring of every part (even-odd semantics).
func (mp multiPolygon) rings() [][][2]float64 {
	var out [][][2]float64
	for _, p := range mp {
		out = append(out, p...)
	}
	return out
}

func (mp multiPolygon) contains(pt [2]float64) bool {
	for _, p := range mp {
		if len(p) == 0 || !ringContains(p[0], pt) {
			continue
		}
		inHole := false
		for _, h := range p[1:] {
			if ringContains(h, pt) {
				inHole = true
				break
			}
		}
		if !inHole {
			return true
		}
	}
	return false
}

// ringContains is the even-odd ray-casting test.
func ringContains(ring [][2]float64, pt [2]float64) bool {
	in := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		a, b := ring[i], ring[j]
		if (a[1] > pt[1]) != (b[1] > pt[1]) &&
			pt[0] < (b[0]-a[0])*(pt[1]-a[1])/(b[1]-a[1])+a[0] {
			in = !in
		}
	}
	return in
}

// --- extract sizes (HEAD, cached) ---

func (c *Catalog) sizesPath() string { return filepath.Join(c.dir(), "sizes.json") }

func (c *Catalog) readSizes() map[string]sizeRec {
	m := map[string]sizeRec{}
	if data, err := os.ReadFile(c.sizesPath()); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

// WarmSizes HEADs every extract matching f (North America by default) whose size is missing or
// stale, so the catalog, estimates and the disk guard see real sizes. Cached sizes are reused for
// MaxAge, so this is cheap after the first run.
func (c *Catalog) WarmSizes(ctx context.Context, f Filter) error {
	var ids []string
	for _, e := range c.Entries(f) {
		if e.PBFURL != "" {
			ids = append(ids, e.ID)
		}
	}
	return c.FillSizes(ctx, ids)
}

// FillSizes HEADs the PBF of each id whose cached size is missing or older than MaxAge, a few
// at a time, and persists the results. Errors for individual entries are ignored (size stays 0).
func (c *Catalog) FillSizes(ctx context.Context, ids []string) error {
	type job struct{ id, url string }
	var todo []job
	c.mu.RLock()
	for _, id := range ids {
		i, ok := c.byID[id]
		if !ok {
			continue
		}
		if s, ok := c.sizes[id]; ok && time.Since(s.Checked) < c.maxAge() {
			continue
		}
		todo = append(todo, job{id, c.entries[i].PBFURL})
	}
	c.mu.RUnlock()
	if len(todo) == 0 {
		return nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[string]sizeRec{}
	sem := make(chan struct{}, 4)
	for _, j := range todo {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, j.url, nil)
			if err != nil {
				return
			}
			resp, err := c.client().Do(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
				mu.Lock()
				results[j.id] = sizeRec{Size: resp.ContentLength, Checked: time.Now()}
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	c.mu.Lock()
	if c.sizes == nil {
		c.sizes = map[string]sizeRec{}
	}
	for k, v := range results {
		c.sizes[k] = v
	}
	data, _ := json.MarshalIndent(c.sizes, "", " ")
	c.mu.Unlock()
	if err := os.MkdirAll(c.dir(), 0o755); err != nil {
		return err
	}
	// Atomic replace: a background warm-up and a request may both be writing.
	tmp, err := os.CreateTemp(c.dir(), "sizes-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), c.sizesPath())
}
