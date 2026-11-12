// Command mapthing is a self-hosted map service: tiles, routing and elevation.
//
//	mapthing [serve]     run the server (default)
//	mapthing adopt DIR   make a generation built elsewhere live (docs/operations.md)
//	mapthing healthcheck exit 0 if the local server answers /healthz (Docker HEALTHCHECK)
//	mapthing version     print the version
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jclement/mapthing/assets"
	"github.com/jclement/mapthing/internal/auth"
	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/logs"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/scheduler"
	"github.com/jclement/mapthing/internal/server"
	"github.com/jclement/mapthing/internal/stats"
	"github.com/jclement/mapthing/internal/store"
	"github.com/jclement/mapthing/internal/supervisor"
	"github.com/jclement/mapthing/internal/tiles"
	"github.com/jclement/mapthing/internal/update"
	"github.com/jclement/mapthing/web"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "mapthing:", err)
			os.Exit(1)
		}
	case "adopt":
		if err := adopt(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "mapthing adopt:", err)
			os.Exit(1)
		}
	case "healthcheck":
		os.Exit(healthcheck())
	case "version", "--version", "-v":
		fmt.Println(server.Version)
	default:
		fmt.Fprintf(os.Stderr, "usage: mapthing [serve|adopt <dir-or-id>|healthcheck|version]\n")
		os.Exit(2)
	}
}

func healthcheck() int {
	listen := os.Getenv("MAPTHING_LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		port = "8080"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.Status)
		return 1
	}
	return 0
}

func serve() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	// One log for everything (internal/logs): this process's slog output, the children's output
	// and build output. It redacts, keeps a bounded history under <data>/logs for the admin Logs
	// page, and still prints every line to stderr, so `docker logs` shows it all.
	sink := logs.New(logs.Options{Dir: filepath.Join(cfg.DataDir, "logs"), MaxBytes: cfg.LogMaxMB * 1_000_000, Console: os.Stderr, MinLevel: minLogLevel()})
	defer sink.Close()
	sink.AddSecret(cfg.AdminPassword)
	slog.SetDefault(slog.New(sink.Handler(logLevel())))
	if cfg.InternalToken == "" {
		var b [24]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		cfg.InternalToken = hex.EncodeToString(b[:])
	}
	sink.AddSecret(cfg.InternalToken)
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "logs"), filepath.Join(cfg.DataDir, "generations")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "mapthing.db"))
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer func() { _ = st.Close() }()

	ks := keys.NewService(st)
	ss := stats.New(st)
	au := auth.New(cfg.AdminUser, cfg.AdminPassword, st, ks, ss, cfg.PublicURL)
	au.SetInternalToken(cfg.InternalToken)
	au.SetTrustProxy(cfg.TrustedProxy)
	runner := jobs.NewRunner(st)
	runner.Redact = sink.Redact
	runner.Log = func(id int64, _ string, at time.Time, line string) {
		sink.Add(logs.Entry{Time: at, Source: logs.SourceBuild, Job: strconv.FormatInt(id, 10), Msg: line})
	}
	sched := scheduler.New(st, runner)

	_, port, _ := net.SplitHostPort(cfg.Listen)
	if port == "" {
		port = "8080"
	}
	sup := supervisor.New()
	sup.Output = func(child, level, line string) {
		// The renderer's start script prefixes what its X server prints (docker/renderer/start.sh).
		// xkbcomp's keysym warnings at every start are not ours to fix: plain info.
		if rest, ok := strings.CutPrefix(line, "[xvfb] "); ok {
			child, line, level = "xvfb", rest, logs.LevelInfo
		}
		if level == "" {
			level, line = logs.ChildLine(line)
		}
		sink.Log(child, level, "", line)
	}
	// valhalla is started per generation, blue/green, by features (cmd/mapthing/valhalla.go).
	// MAPTHING_RENDERER=off: no renderer child at all, so no Xvfb or MapLibre Native in memory,
	// and readiness has no renderer to wait for.
	if !cfg.RendererOff {
		sup.Add(supervisor.Child{
			Name:      "renderer",
			Command:   cfg.RendererCmd,
			HealthURL: getenv("MAPTHING_RENDERER_HEALTH_URL", cfg.RendererURL+"/healthz"),
			Env: []string{
				"MAPTHING_INTERNAL_TOKEN=" + cfg.InternalToken,
				"MAPTHING_URL=http://127.0.0.1:" + port,
			},
		})
	}

	// On-demand terrain: off unless a region or "outside built regions" is set to it.
	od, odAreas, err := newOnDemand(ctx, cfg, st)
	if err != nil {
		return err
	}
	topt := tiles.Options{
		DataDir:           cfg.DataDir,
		Assets:            assets.FS(),
		PublicURL:         cfg.PublicURL,
		RendererURL:       cfg.RendererURL,
		RendererOff:       cfg.RendererOff,
		BasemapOptional:   !cfg.Serve.Tiles,
		Stats:             ss,
		RasterCacheBytes:  cfg.RasterCacheMB << 20,
		ImageryCacheBytes: cfg.ImageryCacheMB << 20,
		PackCacheBytes:    cfg.PackCacheMB << 20,
		PackMaxTiles:      cfg.PackMaxTiles,
		ImageryOverride:   cfg.ImageryURL != "",
	}
	if od != nil {
		topt.OnDemand, topt.ColdGate = od, coldGate(au)
		topt.OnDemandMinZoom, topt.OnDemandMaxZoom = ondemand.MinZoom, ondemand.MaxZoom
	}
	ts, err := tiles.New(topt)
	if err != nil {
		return fmt.Errorf("tiles: %w", err)
	}
	if od != nil {
		od.SetBuilt(ts.HasTerrain) // a warm job leaves built tiles alone
	}

	var f *features
	watcher := &tiles.Watcher{DataDir: cfg.DataDir}
	watcher.OnSwap = func(ctx context.Context, oldID, newID string) {
		// Zero downtime: tiles reopen at once; valhalla switches blue/green once the new
		// instance answers; the renderer drops its styles and pools (no restart).
		ts.Reload()
		if od != nil {
			od.Refresh(ctx) // the new generation may have built (or dropped) a region's terrain
		}
		f.swap(ctx, newID)
		runner.Hub().Publish(jobs.Event{Type: "generation", Data: map[string]any{"id": newID, "previous": oldID}})
	}
	watcher.Prime()

	deps := server.Deps{
		DataDir: cfg.DataDir, Store: st, Auth: au, Keys: ks, Stats: ss, Jobs: runner,
		Scheduler: sched, Supervisor: sup, Tiles: ts, Assets: assets.FS(), UI: web.Dist(), Logs: sink,
		Info: map[string]any{"serve": cfg.Serve.List(), "renderer": map[bool]string{false: "on", true: "off"}[cfg.RendererOff],
			"public_url": cfg.PublicURL, "trusted_proxy": cfg.TrustedProxy},
	}
	deps.Settings = func() []config.Setting { return config.Describe(config.Environ(), sink.Redact) }
	deps.Versions = imageVersions(cfg)
	upd := &update.Checker{Image: cfg.UpdateImage, Off: cfg.UpdateCheckOff, Store: st,
		Revision: os.Getenv("MAPTHING_IMAGE_REVISION"), Created: os.Getenv("MAPTHING_IMAGE_CREATED"),
		Client: &http.Client{Timeout: 20 * time.Second}}
	deps.Update = upd.Status
	go upd.Run(ctx)
	f = &features{cfg: cfg, store: st, runner: runner, sched: sched, sup: sup, watcher: watcher, tiles: ts,
		auth: au, ondemand: od, ondemandAreas: odAreas}
	if err := f.wire(ctx, &deps); err != nil {
		return err
	}

	if err := runner.Start(ctx); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	go ss.Run(ctx, 30*time.Second)
	limiterDone := make(chan struct{})
	go func() { // quota use and refusal counts, so quotas survive a restart
		defer close(limiterDone)
		ks.Run(ctx, 10*time.Second)
	}()
	go watcher.Run(ctx, 2*time.Second)
	sup.Start(ctx)
	f.start(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(deps).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE streams and large archive/pack downloads are long-lived.
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("mapthing listening", "addr", cfg.Listen, "data", cfg.DataDir, "version", server.Version, "generation", ts.Generation(),
			"serve", cfg.Serve.List(), "renderer", !cfg.RendererOff, "public_url", cfg.PublicURL, "trusted_proxy", cfg.TrustedProxy)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
	}
	shutdown(srv, ks, limiterDone, stop, 15*time.Second)
	return nil
}

// minLogLevel is the least severe level the log keeps: debug only with MAPTHING_DEBUG.
func minLogLevel() string {
	if os.Getenv("MAPTHING_DEBUG") != "" {
		return logs.LevelDebug
	}
	return logs.LevelInfo
}

func logLevel() slog.Level {
	if os.Getenv("MAPTHING_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// shutdown stops the server in an order that loses no quota use: let requests finish for up
// to grace, cut off the ones that have not (a long pack download), stop the background work,
// wait for the limiter's own last flush, and then write once more whatever was charged while
// the server drained. That last flush has its own time: the drain may have used all of grace.
func shutdown(srv *http.Server, ks *keys.Service, limiterDone <-chan struct{}, stop func(), grace time.Duration) {
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	err := srv.Shutdown(sctx)
	cancel()
	if err != nil {
		slog.Warn("shutdown: requests still running after the grace period were cut off", "grace", grace.String())
		_ = srv.Close()
	}
	stop() // cancels jobs, scheduler, supervisor children, stats and the limiter (final flushes)
	select {
	case <-limiterDone:
	case <-time.After(6 * time.Second):
	}
	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fcancel()
	if err := ks.Flush(fctx); err != nil {
		slog.Warn("rate limits: flush at shutdown", "err", err)
	}
	time.Sleep(500 * time.Millisecond)
}
