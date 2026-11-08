// Command mapthing is a self-hosted map service: tiles, routing and elevation.
//
//	mapthing [serve]     run the server (default)
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
	"syscall"
	"time"

	"github.com/jclement/mapthing/assets"
	"github.com/jclement/mapthing/internal/auth"
	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/scheduler"
	"github.com/jclement/mapthing/internal/server"
	"github.com/jclement/mapthing/internal/stats"
	"github.com/jclement/mapthing/internal/store"
	"github.com/jclement/mapthing/internal/supervisor"
	"github.com/jclement/mapthing/internal/tiles"
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
	case "healthcheck":
		os.Exit(healthcheck())
	case "version", "--version", "-v":
		fmt.Println(server.Version)
	default:
		fmt.Fprintf(os.Stderr, "usage: mapthing [serve|healthcheck|version]\n")
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
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
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
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if cfg.InternalToken == "" {
		var b [24]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		cfg.InternalToken = hex.EncodeToString(b[:])
	}
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
	runner := jobs.NewRunner(st)
	sched := scheduler.New(st, runner)

	_, port, _ := net.SplitHostPort(cfg.Listen)
	if port == "" {
		port = "8080"
	}
	sup := supervisor.New(filepath.Join(cfg.DataDir, "logs"))
	valhallaConf := filepath.Join(cfg.DataDir, "current", "valhalla", "valhalla.json")
	sup.Add(supervisor.Child{
		Name:      "valhalla",
		Command:   cfg.ValhallaCmd,
		HealthURL: cfg.ValhallaURL + "/status",
		Precheck: func() error {
			if _, err := os.Stat(valhallaConf); err != nil {
				return errors.New("no routing graph yet (build a generation first)")
			}
			return nil
		},
	})
	sup.Add(supervisor.Child{
		Name:      "renderer",
		Command:   cfg.RendererCmd,
		HealthURL: getenv("MAPTHING_RENDERER_HEALTH_URL", cfg.RendererURL+"/health"),
		Env: []string{
			"MAPTHING_INTERNAL_TOKEN=" + cfg.InternalToken,
			"MAPTHING_URL=http://127.0.0.1:" + port,
		},
	})

	ts, err := tiles.New(tiles.Options{
		DataDir:           cfg.DataDir,
		Assets:            assets.FS(),
		PublicURL:         cfg.PublicURL,
		RendererURL:       cfg.RendererURL,
		Stats:             ss,
		RasterCacheBytes:  cfg.RasterCacheMB << 20,
		ImageryCacheBytes: cfg.ImageryCacheMB << 20,
		PackCacheBytes:    cfg.PackCacheMB << 20,
		PackMaxTiles:      cfg.PackMaxTiles,
		ImageryOverride:   cfg.ImageryURL != "",
	})
	if err != nil {
		return fmt.Errorf("tiles: %w", err)
	}

	watcher := &tiles.Watcher{DataDir: cfg.DataDir}
	watcher.OnSwap = func(ctx context.Context, oldID, newID string) {
		ts.Reload()
		if err := sup.RestartAll(ctx); err != nil {
			slog.Warn("restart children", "err", err)
		}
		runner.Hub().Publish(jobs.Event{Type: "generation", Data: map[string]any{"id": newID, "previous": oldID}})
	}
	watcher.Prime()

	deps := server.Deps{
		DataDir: cfg.DataDir, Store: st, Auth: au, Keys: ks, Stats: ss, Jobs: runner,
		Scheduler: sched, Supervisor: sup, Tiles: ts, Assets: assets.FS(), UI: web.Dist(),
	}
	f := features{cfg: cfg, store: st, runner: runner, sup: sup, watcher: watcher, tiles: ts}
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
	go watcher.Run(ctx, 2*time.Second)
	sup.Start(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(deps).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE streams and large archive/pack downloads are long-lived.
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("mapthing listening", "addr", cfg.Listen, "data", cfg.DataDir, "version", server.Version, "generation", ts.Generation())
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
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	stop() // cancels jobs, scheduler, supervisor children, stats (final flush)
	time.Sleep(500 * time.Millisecond)
	return nil
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
