package build

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Tools are the external programs a build runs. Defaults match the container image.
type Tools struct {
	Java                 string
	PlanetilerJar        string
	Osmium               string
	ValhallaBuildConfig  string
	ValhallaBuildAdmins  string
	ValhallaBuildTiles   string
	ValhallaBuildExtract string
	ValhallaService      string
	Ogr2ogr              string
	ValhallaOverrides    string // JSON deep-merged over valhalla_build_config output; "" = none
}

// ToolsFromEnv returns the default tool paths, overridable by MAPTHING_* variables.
func ToolsFromEnv() Tools {
	return Tools{
		Java:                 getenv("MAPTHING_JAVA", "java"),
		PlanetilerJar:        getenv("MAPTHING_PLANETILER_JAR", "/opt/mapthing/protomaps-basemap.jar"),
		Osmium:               getenv("MAPTHING_OSMIUM", "osmium"),
		ValhallaBuildConfig:  getenv("MAPTHING_VALHALLA_BUILD_CONFIG", "valhalla_build_config"),
		ValhallaBuildAdmins:  getenv("MAPTHING_VALHALLA_BUILD_ADMINS", "valhalla_build_admins"),
		ValhallaBuildTiles:   getenv("MAPTHING_VALHALLA_BUILD_TILES", "valhalla_build_tiles"),
		ValhallaBuildExtract: getenv("MAPTHING_VALHALLA_BUILD_EXTRACT", "valhalla_build_extract"),
		ValhallaService:      getenv("MAPTHING_VALHALLA_SERVICE", "valhalla_service"),
		Ogr2ogr:              getenv("MAPTHING_OGR2OGR", "ogr2ogr"),
		ValhallaOverrides:    getenv("MAPTHING_VALHALLA_OVERRIDES", "/usr/share/mapthing/valhalla/overrides.json"),
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// exec runs a tool, streaming its output lines to the log and tracking peak RSS.
func (s *state) exec(ctx context.Context, dir, name string, args ...string) error {
	_, err := s.execOut(ctx, dir, nil, name, args...)
	return err
}

// execOut is exec that also captures stdout into stdout (if non-nil) instead of logging it.
func (s *state) execOut(ctx context.Context, dir string, stdout io.Writer, name string, args ...string) (*os.ProcessState, error) {
	s.logf("$ %s %s", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	var wg sync.WaitGroup
	pipe := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			s.logf("%s", sc.Text())
		}
	}
	errR, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	wg.Add(1)
	go pipe(errR)
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		outR, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		go pipe(outR)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	wg.Wait()
	err = cmd.Wait()
	if ps := cmd.ProcessState; ps != nil {
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok && ru.Maxrss > s.peakKB {
			s.peakKB = ru.Maxrss // KiB on Linux
		}
	}
	if err != nil {
		return cmd.ProcessState, fmt.Errorf("%s: %w", name, err)
	}
	return cmd.ProcessState, nil
}
