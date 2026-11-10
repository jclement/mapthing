package main

// Zero-downtime services across generation swaps.
//
// valhalla_service runs blue/green on two localhost ports (MAPTHING_VALHALLA_URL's port and the
// next one). On a swap the new generation's instance starts on the idle port as a standby;
// once it answers /status and a test route, the routing client switches to it and the old
// instance stops. Routing never sees "no road" because of a swap, and /readyz stays 200.
//
// The renderer needs no restart: it reads tiles from mapthing (already switched), so it only
// drops its cached styles and renderer pools (POST /reload).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/routing"
	"github.com/jclement/mapthing/internal/supervisor"
	"github.com/jclement/mapthing/internal/tiles"
)

// valhallaPorts derives the two blue/green ports from the configured Valhalla URL.
func valhallaPorts(raw string) [2]int {
	p := 8002
	if u, err := url.Parse(raw); err == nil {
		if n, err := strconv.Atoi(u.Port()); err == nil && n > 0 {
			p = n
		}
	}
	return [2]int{p, p + 1}
}

// valhallaInstanceConfig writes <data>/run/valhalla-<port>.json: the generation's valhalla.json
// with its own listen port and IPC endpoints, so two instances can run side by side.
func valhallaInstanceConfig(dataDir, genDir string, port int) (string, error) {
	src := filepath.Join(genDir, "valhalla", "valhalla.json")
	b, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("no routing graph in this generation: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", fmt.Errorf("%s: %w", src, err)
	}
	httpd, _ := cfg["httpd"].(map[string]any)
	if httpd == nil {
		httpd = map[string]any{}
		cfg["httpd"] = httpd
	}
	svc, _ := httpd["service"].(map[string]any)
	if svc == nil {
		svc = map[string]any{}
		httpd["service"] = svc
	}
	run := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		return "", err
	}
	// The build wrote its own absolute paths. The graph is wherever this generation is now: a
	// generation built elsewhere and copied in has a different path than the one it was built at.
	mj, _ := cfg["mjolnir"].(map[string]any)
	if mj == nil {
		mj = map[string]any{}
		cfg["mjolnir"] = mj
	}
	mj["tile_extract"] = filepath.Join(genDir, "valhalla", "tiles.tar")
	svc["listen"] = "tcp://127.0.0.1:" + strconv.Itoa(port)
	svc["loopback"] = "ipc://" + filepath.Join(run, "valhalla-"+strconv.Itoa(port)+"-loopback")
	svc["interrupt"] = "ipc://" + filepath.Join(run, "valhalla-"+strconv.Itoa(port)+"-interrupt")
	out, _ := json.MarshalIndent(cfg, "", "  ")
	dst := filepath.Join(run, "valhalla-"+strconv.Itoa(port)+".json")
	return dst, os.WriteFile(dst, out, 0o644)
}

// commandWith substitutes {config} in the configured command.
func commandWith(cmd []string, config string) []string {
	out := make([]string, len(cmd))
	for i, a := range cmd {
		out[i] = strings.ReplaceAll(a, "{config}", config)
	}
	return out
}

// testRoute asks a fresh instance for a short route inside the generation's first region.
// Any answer from Valhalla's router (a route, or a routing error such as "no path") shows the
// graph is loaded; transport errors and 5xx do not.
func testRoute(ctx context.Context, genDir, base string) error {
	var m struct {
		Regions []struct {
			BBox []float64 `json:"bbox"`
		} `json:"regions"`
	}
	if b, err := os.ReadFile(filepath.Join(genDir, "manifest.json")); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if len(m.Regions) == 0 || len(m.Regions[0].BBox) != 4 {
		return nil // nothing to test against; /status already answered
	}
	bb := m.Regions[0].BBox
	lon, lat := (bb[0]+bb[2])/2, (bb[1]+bb[3])/2
	body, _ := json.Marshal(map[string]any{
		"locations": []map[string]any{
			{"lat": lat, "lon": lon, "radius": 5000},
			{"lat": lat + 0.01, "lon": lon + 0.01, "radius": 5000},
		},
		"costing": "auto",
	})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/route", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("test route: %s %s", resp.Status, bytes.TrimSpace(rb))
	}
	return nil
}

// newValhalla returns the blue/green manager for valhalla_service.
func (f *features) newValhalla(router *routing.Client) *supervisor.BlueGreen {
	return &supervisor.BlueGreen{
		Name:  "valhalla",
		Sup:   f.sup,
		Ports: valhallaPorts(f.cfg.ValhallaURL),
		Child: func(genDir string, port int) (supervisor.Child, error) {
			conf, err := valhallaInstanceConfig(f.cfg.DataDir, genDir, port)
			if err != nil {
				return supervisor.Child{}, err
			}
			f.valhallaGen.Store(genDir)
			return supervisor.Child{
				Command:   commandWith(f.cfg.ValhallaCmd, conf),
				HealthURL: "http://127.0.0.1:" + strconv.Itoa(port) + "/status",
			}, nil
		},
		Ready: func(ctx context.Context, base string) error {
			genDir, _ := f.valhallaGen.Load().(string)
			return testRoute(ctx, genDir, base)
		},
		Switch: router.SetBaseURL,
	}
}

// activateValhalla starts (or switches to) the instance for the live generation, if any.
func (f *features) activateValhalla(ctx context.Context) {
	if f.valhalla == nil {
		return
	}
	_, genDir, ok := tiles.CurrentGeneration(f.cfg.DataDir)
	if !ok {
		return
	}
	if err := f.valhalla.Activate(ctx, genDir); err != nil {
		slog.Warn("routing service not switched", "err", err)
	}
}

// start brings up the services for the live generation (after the supervisor started).
func (f *features) start(ctx context.Context) {
	go f.activateValhalla(ctx)
}

// swap is the generation-swap hook: valhalla blue/green, renderer reload.
func (f *features) swap(ctx context.Context, newID string) {
	_ = newID
	f.activateValhalla(ctx)
	if !f.cfg.RendererOff {
		f.reloadRenderer(ctx)
	}
}

func (f *features) reloadRenderer(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.cfg.RendererURL, "/")+"/reload", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Info("renderer reload skipped", "err", err)
		return
	}
	_ = resp.Body.Close()
}
