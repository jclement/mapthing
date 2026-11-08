package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/tiles/pmtiles"
)

// Check is one verification result recorded in the manifest.
type Check struct {
	Name   string `json:"name"`
	Region string `json:"region,omitempty"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func (s *state) verify(ctx context.Context) error {
	var failed []string
	record := func(c Check) {
		s.m.Checks = append(s.m.Checks, c)
		status := "ok"
		if !c.OK {
			status = "FAILED"
			failed = append(failed, c.Name+" "+c.Region)
		}
		s.logf("check %s %s: %s %s", c.Name, c.Region, status, c.Detail)
	}

	for _, c := range s.verifyTiles() {
		record(c)
	}
	s.rep.Progress(0.2, "tiles checked")
	for _, c := range s.verifyRoutes(ctx) {
		record(c)
	}
	s.rep.Progress(0.7, "routes checked")
	if s.terrain {
		if f := hooks.elevationCheck(); f != nil {
			path := filepath.Join(s.genDir, "terrain.pmtiles")
			for _, r := range s.opt.Regions {
				c := r.BBox.Center()
				err := f(ctx, path, [][2]float64{c})
				ch := Check{Name: "elevation", Region: r.ID, OK: err == nil, Detail: fmt.Sprintf("at %.4f,%.4f", c[0], c[1])}
				if err != nil {
					ch.Detail += ": " + err.Error()
				}
				record(ch)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d check(s) failed: %v", len(failed), failed)
	}
	return nil
}

// verifyTiles opens basemap.pmtiles and reads a tile at each region centre.
func (s *state) verifyTiles() []Check {
	path := filepath.Join(s.genDir, "basemap.pmtiles")
	r, err := pmtiles.Open(path)
	if err != nil {
		return []Check{{Name: "basemap", OK: false, Detail: err.Error()}}
	}
	defer func() { _ = r.Close() }()
	h := r.Header
	out := []Check{{Name: "basemap", OK: h.TileType == pmtiles.TypeMVT && h.MaxZoom >= 14,
		Detail: fmt.Sprintf("type=%d zoom %d-%d bounds %v", h.TileType, h.MinZoom, h.MaxZoom, h.Bounds())}}
	z := min(h.MaxZoom, 12)
	for _, reg := range s.opt.Regions {
		c := reg.BBox.Center()
		x, y, _, _ := pmtiles.TileRange(pmtiles.BBox{c[0], c[1], c[0], c[1]}, z)
		data, ok, err := r.Get(z, x, y)
		ch := Check{Name: "tile", Region: reg.ID, OK: err == nil && ok && len(data) > 0,
			Detail: fmt.Sprintf("z%d/%d/%d %d bytes", z, x, y, len(data))}
		if err != nil {
			ch.Detail += ": " + err.Error()
		}
		out = append(out, ch)
	}
	return out
}

// samplePairs returns candidate route endpoints inside a region, nearest the centre first.
func samplePairs(b catalog.BBox) [][2][2]float64 {
	c := b.Center()
	w, h := b[2]-b[0], b[3]-b[1]
	var out [][2][2]float64
	for _, f := range []float64{0.15, 0.08, 0.25} {
		out = append(out,
			[2][2]float64{{c[0] - w*f, c[1] - h*f}, {c[0] + w*f, c[1] + h*f}},
			[2][2]float64{{c[0] - w*f, c[1] + h*f}, {c[0] + w*f, c[1] - h*f}},
		)
	}
	return out
}

// verifyRoutes starts a throwaway valhalla_service on a free localhost port and routes between
// two points inside each region.
func (s *state) verifyRoutes(ctx context.Context) []Check {
	fail := func(err error) []Check { return []Check{{Name: "valhalla", OK: false, Detail: err.Error()}} }
	data, err := os.ReadFile(ValhallaConfig(s.genDir))
	if err != nil {
		return fail(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fail(err)
	}
	port, err := freePort(ctx)
	if err != nil {
		return fail(err)
	}
	setPath(cfg, []string{"httpd", "service", "listen"}, fmt.Sprintf("tcp://127.0.0.1:%d", port))
	tmpCfg := filepath.Join(s.workDir, "valhalla-verify.json")
	data, _ = json.Marshal(cfg)
	if err := os.WriteFile(tmpCfg, data, 0o644); err != nil {
		return fail(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.b.Tools.ValhallaService, tmpCfg, "1")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	logw := &lineWriter{fn: func(l string) { s.logf("valhalla_service: %s", l) }}
	cmd.Stdout, cmd.Stderr = logw, logw
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 60 * time.Second}
	ready := false
	for i := 0; i < 120 && !ready; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/status", nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			ready = resp.StatusCode == http.StatusOK
		}
		if !ready {
			select {
			case <-ctx.Done():
				return fail(ctx.Err())
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	if !ready {
		return fail(errors.New("valhalla_service did not become ready in 60s"))
	}
	var out []Check
	for _, reg := range s.opt.Regions {
		var last error
		ch := Check{Name: "route", Region: reg.ID}
		for _, p := range samplePairs(reg.BBox) {
			dist, err := routeOnce(ctx, client, base, p)
			if err == nil && dist > 0 {
				ch.OK = true
				ch.Detail = fmt.Sprintf("%.4f,%.4f → %.4f,%.4f: %.1f km", p[0][0], p[0][1], p[1][0], p[1][1], dist)
				break
			}
			last = err
		}
		if !ch.OK && last != nil {
			ch.Detail = last.Error()
		}
		out = append(out, ch)
	}
	return out
}

func routeOnce(ctx context.Context, c *http.Client, base string, p [2][2]float64) (float64, error) {
	body, _ := json.Marshal(map[string]any{
		"locations": []map[string]float64{{"lon": p[0][0], "lat": p[0][1]}, {"lon": p[1][0], "lat": p[1][1]}},
		"costing":   "auto",
		"units":     "kilometers",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/route", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("route %s: %s", resp.Status, truncate(string(raw), 200))
	}
	var r struct {
		Trip struct {
			Summary struct {
				Length float64 `json:"length"`
			} `json:"summary"`
		} `json:"trip"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, err
	}
	return r.Trip.Summary.Length, nil
}

func freePort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// lineWriter splits written bytes into lines.
type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.fn(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}
