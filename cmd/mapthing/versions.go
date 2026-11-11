package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/build"
	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/server"
)

// imageVersions returns the function behind the Settings page's version list: the software
// the image carries. What the image records about itself (Valhalla's version, the basemap
// profile's commit) comes from its environment; the rest is asked of each tool once, in the
// background, so the first request never waits on a slow `java -version`.
func imageVersions(cfg config.Config) func() []server.Component {
	tools := build.ToolsFromEnv()
	var (
		mu     sync.Mutex
		probed map[string]string
		once   sync.Once
	)
	probe := func() {
		got := map[string]string{
			"valhalla": toolVersion(tools.ValhallaService, "--version"),
			"java":     toolVersion(tools.Java, "-version"),
			"osmium":   toolVersion(tools.Osmium, "--version"),
			"gdal":     toolVersion("gdalwarp", "--version"),
			"node":     toolVersion("node", "--version"),
		}
		mu.Lock()
		probed = got
		mu.Unlock()
	}
	return func() []server.Component {
		once.Do(func() { go probe() })
		mu.Lock()
		p := probed
		mu.Unlock()
		pending := p == nil
		get := func(k string) string { return p[k] }
		orEnv := func(env, probedVersion string) string {
			if v := os.Getenv(env); v != "" && v != "unknown" {
				return v
			}
			return probedVersion
		}
		note := func(found string, what string) string {
			if found == "" && pending {
				return what + " (checking…)"
			}
			if found == "" {
				return what + " (not found in this environment)"
			}
			return what
		}
		valhalla := orEnv("MAPTHING_VALHALLA_VERSION", get("valhalla"))
		out := []server.Component{
			{Name: "Valhalla", Version: valhalla, Detail: note(valhalla, "routing engine and graph builder")},
		}
		planetiler := server.Component{Name: "Planetiler (Protomaps basemap)", Detail: "vector basemap builder, " + filepath.Base(tools.PlanetilerJar)}
		if ref := os.Getenv("MAPTHING_BASEMAPS_REF"); ref != "" {
			planetiler.Version = "basemaps " + shortRef(ref)
		} else if _, err := os.Stat(tools.PlanetilerJar); err != nil {
			planetiler.Detail = "vector basemap builder (not found in this environment)"
		}
		out = append(out, planetiler)
		rv, rdetail := rendererVersion(cfg)
		if node := get("node"); node != "" {
			rdetail += ", Node " + strings.TrimPrefix(node, "v")
		}
		out = append(out, server.Component{Name: "Renderer (MapLibre Native)", Version: rv, Detail: rdetail})
		for _, t := range []struct{ key, name, what string }{
			{"gdal", "GDAL", "elevation build"},
			{"osmium", "osmium", "merging and clipping extracts"},
			{"java", "Java", "runs Planetiler"},
		} {
			out = append(out, server.Component{Name: t.name, Version: get(t.key), Detail: note(get(t.key), t.what)})
		}
		if v := os.Getenv("MAPTHING_IMAGE_VERSION"); v != "" && v != "dev" {
			out = append(out, server.Component{Name: "Container image", Version: v,
				Detail: strings.TrimSpace("revision " + shortRef(os.Getenv("MAPTHING_IMAGE_REVISION")) + " " + os.Getenv("MAPTHING_IMAGE_CREATED"))})
		}
		return out
	}
}

func shortRef(ref string) string {
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

var versionRE = regexp.MustCompile(`\d+\.\d+(?:\.\d+)*(?:[-+._][0-9A-Za-z]+)*`)

// toolVersion runs `tool flag` and returns the first version number it prints ("" when the
// tool is not there or says nothing recognisable).
func toolVersion(tool, flag string) string {
	path, err := exec.LookPath(tool)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path, flag).CombinedOutput() //nolint:gosec // a configured tool and a fixed flag
	return parseVersion(string(out))
}

func parseVersion(out string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return versionRE.FindString(first)
}

// rendererVersion reads the MapLibre Native version the renderer was installed with.
func rendererVersion(cfg config.Config) (version, detail string) {
	if cfg.RendererOff {
		return "", "raster tiles: turned off (MAPTHING_RENDERER=off)"
	}
	dir := os.Getenv("MAPTHING_RENDERER_DIR")
	if dir == "" {
		dir = "/opt/renderer"
	}
	detail = "raster tiles"
	// The installed package says exactly what is running; the renderer's own manifest says
	// what was asked for.
	for _, p := range []string{
		filepath.Join(dir, "node_modules", "@maplibre", "maplibre-gl-native", "package.json"),
		filepath.Join(dir, "package.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var pkg struct {
			Name         string            `json:"name"`
			Version      string            `json:"version"`
			Dependencies map[string]string `json:"dependencies"`
		}
		if json.Unmarshal(b, &pkg) != nil {
			continue
		}
		if pkg.Name == "@maplibre/maplibre-gl-native" && pkg.Version != "" {
			return pkg.Version, detail
		}
		if v := pkg.Dependencies["@maplibre/maplibre-gl-native"]; v != "" {
			return v, detail
		}
	}
	return "", detail + " (not found in this environment)"
}
