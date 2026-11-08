// Command mapthing is a self-hosted map service: tiles, routing and elevation.
package main

import (
	"fmt"
	"os"

	"github.com/jclement/mapthing/internal/config"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mapthing:", err)
		os.Exit(2)
	}
	fmt.Printf("mapthing: data=%s listen=%s (server not wired yet)\n", cfg.DataDir, cfg.Listen)
}
