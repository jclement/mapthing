// Package config reads mapthing's configuration from the environment.
package config

import (
	"errors"
	"os"
)

// Config is the process configuration.
type Config struct {
	DataDir       string // MAPTHING_DATA, default /data
	Listen        string // MAPTHING_LISTEN, default :8080
	AdminUser     string // ADMIN_USER (required)
	AdminPassword string // ADMIN_PASSWORD (required)
	PublicURL     string // MAPTHING_PUBLIC_URL, used in styles/TileJSON; default derived from request
}

// FromEnv loads Config from environment variables.
func FromEnv() (Config, error) {
	c := Config{
		DataDir:       getenv("MAPTHING_DATA", "/data"),
		Listen:        getenv("MAPTHING_LISTEN", ":8080"),
		AdminUser:     os.Getenv("ADMIN_USER"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		PublicURL:     os.Getenv("MAPTHING_PUBLIC_URL"),
	}
	if c.AdminUser == "" || c.AdminPassword == "" {
		return c, errors.New("ADMIN_USER and ADMIN_PASSWORD must be set")
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
