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

	RasterCacheMB  int64  // MAPTHING_RASTER_CACHE_MB, default 2048
	ImageryCacheMB int64  // MAPTHING_IMAGERY_CACHE_MB, default 4096
	PackCacheMB    int64  // MAPTHING_PACK_CACHE_MB, default 4096
	PackMaxTiles   uint64 // MAPTHING_PACK_MAX_TILES, default 2000000

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
	if c.AdminUser == "" || c.AdminPassword == "" {
		errs = append(errs, errors.New("ADMIN_USER and ADMIN_PASSWORD must be set"))
	}
	return c, errors.Join(errs...)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
