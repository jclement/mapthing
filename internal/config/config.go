// Package config reads mapthing's configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the process configuration.
type Config struct {
	DataDir       string // MAPTHING_DATA, default /data
	Listen        string // MAPTHING_LISTEN, default :8080
	AdminUser     string // ADMIN_USER (required)
	AdminPassword string // ADMIN_PASSWORD (required)
	PublicURL     string // MAPTHING_PUBLIC_URL, used in styles/TileJSON; default derived from request

	ValhallaURL string   // MAPTHING_VALHALLA_URL, default http://127.0.0.1:8002
	ValhallaCmd []string // MAPTHING_VALHALLA_CMD, default "valhalla_service {config} 1" ({config} = per-instance config)
	RendererURL string   // MAPTHING_RENDERER_URL, default http://127.0.0.1:8081
	RendererCmd []string // MAPTHING_RENDERER_CMD, default "node /opt/renderer/server.js"

	// RendererOff: MAPTHING_RENDERER=off. No renderer child (so no Xvfb or MapLibre Native);
	// raster tile endpoints answer 404 renderer_off. Implied when tiles are not served.
	RendererOff bool
	// Serve is which parts of a generation this server serves (MAPTHING_SERVE, default all).
	Serve Serve

	RasterCacheMB  int64  // MAPTHING_RASTER_CACHE_MB, default 2048
	ImageryCacheMB int64  // MAPTHING_IMAGERY_CACHE_MB, default 4096
	PackCacheMB    int64  // MAPTHING_PACK_CACHE_MB, default 4096
	PackMaxTiles   uint64 // MAPTHING_PACK_MAX_TILES, default 2000000
	LogMaxMB       int64  // MAPTHING_LOG_MAX_MB, default 50: total size of the log files in <data>/logs

	// InternalToken authenticates the local renderer's requests (MAPTHING_INTERNAL_TOKEN;
	// generated at startup when unset and passed to the renderer child).
	InternalToken string
	// ImageryURL is an operator imagery override (MAPTHING_IMAGERY_URL), read by the imagery
	// source selection; core only uses it to know an override is active.
	ImageryURL string
}

// FromEnv loads Config from environment variables.
func FromEnv() (Config, error) {
	c := Config{
		DataDir:       getenv("MAPTHING_DATA", "/data"),
		Listen:        getenv("MAPTHING_LISTEN", ":8080"),
		AdminUser:     os.Getenv("ADMIN_USER"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		PublicURL:     strings.TrimRight(os.Getenv("MAPTHING_PUBLIC_URL"), "/"),
		ValhallaURL:   getenv("MAPTHING_VALHALLA_URL", "http://127.0.0.1:8002"),
		RendererURL:   getenv("MAPTHING_RENDERER_URL", "http://127.0.0.1:8081"),
	}
	c.InternalToken = os.Getenv("MAPTHING_INTERNAL_TOKEN")
	c.ImageryURL = os.Getenv("MAPTHING_IMAGERY_URL")
	c.ValhallaCmd = strings.Fields(getenv("MAPTHING_VALHALLA_CMD", "valhalla_service {config} 1"))
	c.RendererCmd = strings.Fields(getenv("MAPTHING_RENDERER_CMD", "node /opt/renderer/server.js"))
	var errs []error
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("MAPTHING_RENDERER"))); v {
	case "", "on", "1", "true":
	case "off", "0", "false", "none":
		c.RendererOff = true
	default:
		errs = append(errs, fmt.Errorf("MAPTHING_RENDERER must be on or off, not %q", v))
	}
	var err error
	if c.Serve, err = ParseServe(os.Getenv("MAPTHING_SERVE")); err != nil {
		errs = append(errs, err)
	}
	if !c.Serve.Tiles {
		c.RendererOff = true // nothing to render from
	}
	num := func(k string, def int64) int64 {
		v := os.Getenv(k)
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s must be a non-negative integer", k))
			return def
		}
		return n
	}
	c.RasterCacheMB = num("MAPTHING_RASTER_CACHE_MB", 2048)
	c.ImageryCacheMB = num("MAPTHING_IMAGERY_CACHE_MB", 4096)
	c.PackCacheMB = num("MAPTHING_PACK_CACHE_MB", 4096)
	c.PackMaxTiles = uint64(num("MAPTHING_PACK_MAX_TILES", 2_000_000))
	if c.LogMaxMB = num("MAPTHING_LOG_MAX_MB", 50); c.LogMaxMB == 0 {
		errs = append(errs, errors.New("MAPTHING_LOG_MAX_MB must be at least 1"))
	}
	if c.AdminUser == "" || c.AdminPassword == "" {
		errs = append(errs, errors.New("ADMIN_USER and ADMIN_PASSWORD must be set"))
	}
	return c, errors.Join(errs...)
}

// Serve says which parts of a generation a server serves. A routing and elevation backend
// (MAPTHING_SERVE=routing,elevation) needs no basemap in its generations: tiles come from
// somewhere else, such as object storage behind an edge proxy.
type Serve struct {
	Tiles     bool // vector tiles, styles' data, packs; needs basemap.pmtiles
	Routing   bool // Valhalla and the routing APIs; needs valhalla/
	Elevation bool // elevation APIs; needs terrain.pmtiles
}

// ParseServe reads a MAPTHING_SERVE value: a comma-separated list of tiles, routing and
// elevation. Empty or "all" is everything.
func ParseServe(v string) (Serve, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || v == "all" {
		return Serve{Tiles: true, Routing: true, Elevation: true}, nil
	}
	var s Serve
	for _, part := range strings.Split(v, ",") {
		switch strings.TrimSpace(part) {
		case "tiles":
			s.Tiles = true
		case "routing":
			s.Routing = true
		case "elevation":
			s.Elevation = true
		case "":
		default:
			return Serve{}, fmt.Errorf("MAPTHING_SERVE: unknown part %q (want a list of tiles, routing, elevation)", strings.TrimSpace(part))
		}
	}
	if s == (Serve{}) {
		return Serve{}, errors.New("MAPTHING_SERVE names nothing to serve (want a list of tiles, routing, elevation)")
	}
	return s, nil
}

// List returns the served parts by name.
func (s Serve) List() []string {
	out := []string{}
	if s.Tiles {
		out = append(out, "tiles")
	}
	if s.Routing {
		out = append(out, "routing")
	}
	if s.Elevation {
		out = append(out, "elevation")
	}
	return out
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
