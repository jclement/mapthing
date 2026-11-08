package build

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// gdalRunner runs GDAL command-line tools. In the container they are on PATH.
// For local development set MAPTHING_GDAL_DOCKER to a GDAL image (for example
// ghcr.io/osgeo/gdal:ubuntu-small-3.11.4): each command then runs in a
// throwaway container with the given directories mounted at the same paths.
type gdalRunner struct {
	image  string   // docker image, or "" to exec directly
	mounts []string // absolute directories to bind-mount (docker mode)
}

// gdalEnv tunes GDAL for range-reading COGs over HTTPS.
var gdalEnv = []string{
	"CPL_VSIL_CURL_ALLOWED_EXTENSIONS=.tif,.TIF,.tiff,.vrt",
	"GDAL_HTTP_MAX_RETRY=6",
	"GDAL_HTTP_RETRY_DELAY=3",
	"GDAL_HTTP_TIMEOUT=120",
	"GDAL_HTTP_MULTIPLEX=YES",
	"GDAL_HTTP_VERSION=2",
	"VSI_CACHE=TRUE",
	"VSI_CACHE_SIZE=67108864",
	"GDAL_CACHEMAX=512",
	"CPL_VSIL_CURL_CHUNK_SIZE=1048576",
}

func newGDALRunner(mounts ...string) *gdalRunner {
	return &gdalRunner{image: os.Getenv("MAPTHING_GDAL_DOCKER"), mounts: mounts}
}

func (g *gdalRunner) run(ctx context.Context, tool string, args ...string) error {
	var cmd *exec.Cmd
	if g.image != "" {
		dargs := []string{"run", "--rm", "--network", "host", "-u", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())}
		for _, e := range gdalEnv {
			dargs = append(dargs, "-e", e)
		}
		for _, m := range g.mounts {
			dargs = append(dargs, "-v", m+":"+m)
		}
		dargs = append(dargs, g.image, tool)
		cmd = exec.CommandContext(ctx, "docker", append(dargs, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, tool, args...)
		cmd.Env = append(os.Environ(), gdalEnv...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = "…" + msg[len(msg)-2000:]
		}
		return fmt.Errorf("%s %s: %w: %s", tool, strings.Join(args, " "), err, msg)
	}
	return nil
}

// available reports whether gdalwarp can be run.
func (g *gdalRunner) available(ctx context.Context) error {
	if g.image != "" {
		_, err := exec.LookPath("docker")
		return err
	}
	_, err := exec.LookPath("gdalwarp")
	if err != nil {
		return fmt.Errorf("gdalwarp not on PATH (the container image must include GDAL ≥ 3.6; for local runs set MAPTHING_GDAL_DOCKER)")
	}
	return nil
}
