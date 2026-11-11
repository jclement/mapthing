package config

import (
	"os"
	"sort"
	"strings"
)

// Setting describes one environment variable mapthing reads, with its effective value, for
// the admin Settings page.
type Setting struct {
	Name    string `json:"name"`
	Group   string `json:"group"`
	Value   string `json:"value"`             // the effective value; masked for a secret
	Default string `json:"default,omitempty"` // what applies when the variable is unset
	// Source is where the value came from: "env" (set in the container's environment),
	// "image" (the environment holds exactly what the published image sets, so nobody
	// changed it) or "default" (unset).
	Source string `json:"source"`
	Secret bool   `json:"secret,omitempty"`
	Set    bool   `json:"set"` // the variable is set and not empty
	Help   string `json:"help"`
}

type settingDef struct {
	name, group, def, help string
	secret                 bool
	// sensitive values are shown, but passed through the redactor first (a URL that may
	// carry a key).
	sensitive bool
}

// Groups, in the order the page shows them.
const (
	GroupAccess  = "Access"
	GroupServing = "Serving"
	GroupLimits  = "Caches and limits"
	GroupBuild   = "Builds"
	GroupTerrain = "Terrain and contours"
	GroupProcs   = "Processes"
	GroupTools   = "Build tools"
)

var settingDefs = []settingDef{
	{name: "ADMIN_USER", group: GroupAccess, def: "(required)", help: "The admin login name."},
	{name: "ADMIN_PASSWORD", group: GroupAccess, def: "(required)", secret: true, help: "The admin password."},
	{name: "MAPTHING_PUBLIC_URL", group: GroupAccess, def: "taken from each request", help: "The address clients use. Written into styles, TileJSON and key snippets. Set it behind a proxy or tunnel."},
	{name: "MAPTHING_TRUSTED_PROXY", group: GroupAccess, def: "off", help: "on: every request arrives through a proxy or tunnel, so the client address comes from CF-Connecting-IP or X-Forwarded-For."},
	{name: "MAPTHING_LISTEN", group: GroupAccess, def: ":8080", help: "The address the server listens on."},
	{name: "MAPTHING_INTERNAL_TOKEN", group: GroupAccess, def: "generated at each start", secret: true, help: "Shared with the renderer so it can read tiles without an API key."},

	{name: "MAPTHING_DATA", group: GroupServing, def: "/data", help: "The data folder: generations, downloads, caches, logs and the database."},
	{name: "MAPTHING_SERVE", group: GroupServing, def: "tiles,routing,elevation", help: "Which parts this server serves. A part left out is not served and not required of a generation."},
	{name: "MAPTHING_RENDERER", group: GroupServing, def: "on", help: "off runs without the raster renderer; raster tile requests then answer 404."},
	{name: "MAPTHING_IMAGERY_URL", group: GroupServing, def: "built-in open sources", sensitive: true, help: "A satellite imagery tile URL template to use instead of the built-in sources."},
	{name: "MAPTHING_IMAGERY_ATTRIBUTION", group: GroupServing, def: "", help: "Attribution text for MAPTHING_IMAGERY_URL."},
	{name: "MAPTHING_IMAGERY_MAXZOOM", group: GroupServing, def: "19", help: "Deepest zoom MAPTHING_IMAGERY_URL has."},
	{name: "MAPTHING_UPDATE_CHECK", group: GroupServing, def: "on", help: "off stops the daily question to the image registry whether a newer image exists. Nothing is sent but the request for the image's public description."},
	{name: "MAPTHING_UPDATE_IMAGE", group: GroupServing, def: "ghcr.io/jclement/mapthing:<MAPTHING_TAG or latest>", help: "The image and tag to compare with, if you run a copy from another registry."},
	{name: "MAPTHING_LOG_MAX_MB", group: GroupServing, def: "50", help: "Total size of the log files kept in the data folder."},
	{name: "MAPTHING_DEBUG", group: GroupServing, def: "off", help: "Any value turns on debug logging, including every request and the routing engine's per-request lines."},

	{name: "MAPTHING_RASTER_CACHE_MB", group: GroupLimits, def: "2048", help: "Disk cache of rendered raster tiles."},
	{name: "MAPTHING_IMAGERY_CACHE_MB", group: GroupLimits, def: "4096", help: "Disk cache of satellite imagery tiles."},
	{name: "MAPTHING_PACK_CACHE_MB", group: GroupLimits, def: "4096", help: "Disk cache of offline packs."},
	{name: "MAPTHING_PACK_MAX_TILES", group: GroupLimits, def: "2000000", help: "The largest offline pack a client may ask for, in tiles."},

	{name: "MAPTHING_DISK_GUARD_FACTOR", group: GroupBuild, def: "1", help: "Multiplies the free disk a build asks for before it starts. Below 1 accepts the risk of running out."},
	{name: "MAPTHING_PLANETILER_XMX", group: GroupBuild, def: "60% of available memory", help: "Java heap for the basemap build, for example 12g."},
	{name: "MAPTHING_WATER_POLYGONS_URL", group: GroupBuild, def: "osmdata.openstreetmap.de", sensitive: true, help: "Mirror for the ocean polygons the basemap build downloads once."},
	{name: "MAPTHING_LAND_POLYGONS_URL", group: GroupBuild, def: "osmdata.openstreetmap.de", sensitive: true, help: "Mirror for the land polygons."},
	{name: "MAPTHING_NATURAL_EARTH_URL", group: GroupBuild, def: "built-in", sensitive: true, help: "Mirror for the Natural Earth data."},
	{name: "MAPTHING_LANDCOVER_URL", group: GroupBuild, def: "built-in", sensitive: true, help: "Mirror for the landcover data."},
	{name: "MAPTHING_QRANK_URL", group: GroupBuild, def: "built-in", sensitive: true, help: "Mirror for the QRank place-importance file."},
	{name: "MAPTHING_PGF_ENCODING_URL", group: GroupBuild, def: "built-in", sensitive: true, help: "Mirror for the font encoding data."},

	{name: "MAPTHING_TERRAIN_MAXZOOM", group: GroupTerrain, def: "chosen from the best source (12 or 13)", help: "Forces the deepest terrain zoom."},
	{name: "MAPTHING_TERRAIN_MAX_TILES", group: GroupTerrain, def: "400000", help: "Tile budget at the deepest zoom; the zoom is lowered to fit."},
	{name: "MAPTHING_TERRAIN_QUANTUM_M", group: GroupTerrain, def: "0.25", help: "Elevation rounding step at the deepest zoom, in metres."},
	{name: "MAPTHING_TERRAIN_WORKERS", group: GroupTerrain, def: "from the CPU count", help: "Parallel terrain workers."},
	{name: "MAPTHING_CONTOURS_MAXZOOM", group: GroupTerrain, def: "12", help: "Deepest contour zoom. 0 turns contours off."},
	{name: "MAPTHING_ELEVATION_CACHE", group: GroupTerrain, def: "<data>/elevation/cache", help: "Where downloaded elevation data is kept between builds."},

	{name: "MAPTHING_VALHALLA_URL", group: GroupProcs, def: "http://127.0.0.1:8002", help: "Where the routing engine listens. The next port is used too, for swaps without downtime."},
	{name: "MAPTHING_VALHALLA_CMD", group: GroupProcs, def: "valhalla_service {config} 1", help: "How the routing engine is started."},
	{name: "MAPTHING_RENDERER_URL", group: GroupProcs, def: "http://127.0.0.1:8081", help: "Where the raster renderer listens."},
	{name: "MAPTHING_RENDERER_CMD", group: GroupProcs, def: "node /opt/renderer/server.js", help: "How the raster renderer is started."},
	{name: "MAPTHING_RENDERER_HEALTH_URL", group: GroupProcs, def: "<renderer URL>/healthz", help: "The renderer's health probe."},

	{name: "MAPTHING_JAVA", group: GroupTools, def: "java", help: "Java, for Planetiler."},
	{name: "MAPTHING_PLANETILER_JAR", group: GroupTools, def: "/opt/mapthing/protomaps-basemap.jar", help: "The basemap builder."},
	{name: "MAPTHING_OSMIUM", group: GroupTools, def: "osmium", help: "Merges and clips OpenStreetMap extracts."},
	{name: "MAPTHING_OGR2OGR", group: GroupTools, def: "ogr2ogr", help: "GDAL's vector tool."},
	{name: "MAPTHING_VALHALLA_BUILD_CONFIG", group: GroupTools, def: "valhalla_build_config", help: "Routing graph build."},
	{name: "MAPTHING_VALHALLA_BUILD_ADMINS", group: GroupTools, def: "valhalla_build_admins", help: "Routing graph build."},
	{name: "MAPTHING_VALHALLA_BUILD_TILES", group: GroupTools, def: "valhalla_build_tiles", help: "Routing graph build."},
	{name: "MAPTHING_VALHALLA_BUILD_EXTRACT", group: GroupTools, def: "valhalla_build_extract", help: "Routing graph build."},
	{name: "MAPTHING_VALHALLA_SERVICE", group: GroupTools, def: "valhalla_service", help: "The routing engine, used to check a new graph."},
	{name: "MAPTHING_VALHALLA_OVERRIDES", group: GroupTools, def: "/usr/share/mapthing/valhalla/overrides.json", help: "Routing settings merged over Valhalla's defaults."},
}

// internalVars are set by the image or passed between mapthing's own processes; they are not
// settings, and not unrecognised either.
var internalVars = map[string]bool{
	"MAPTHING_IMAGE_ID": true, "MAPTHING_IMAGE_VERSION": true, "MAPTHING_IMAGE_REVISION": true,
	"MAPTHING_IMAGE_CREATED": true, "MAPTHING_VALHALLA_VERSION": true, "MAPTHING_BASEMAPS_REF": true,
	"MAPTHING_RENDERER_DIR": true, "MAPTHING_ASSETS": true, "MAPTHING_URL": true, "MAPTHING_TAG": true,
	"MAPTHING_GDAL_DOCKER": true, // development: run GDAL from an image
}

// KnownVar reports whether name is a setting Describe lists or one of mapthing's own
// internal variables.
func KnownVar(name string) bool {
	if internalVars[name] {
		return true
	}
	for _, d := range settingDefs {
		if d.name == name {
			return true
		}
	}
	return false
}

// imageValues are the variables the published image sets itself (docker/Dockerfile, ENV). A
// variable holding exactly this value was not set by the operator.
var imageValues = map[string]string{
	"MAPTHING_DATA":                "/data",
	"MAPTHING_LISTEN":              ":8080",
	"MAPTHING_PLANETILER_JAR":      "/opt/mapthing/protomaps-basemap.jar",
	"MAPTHING_VALHALLA_OVERRIDES":  "/usr/share/mapthing/valhalla/overrides.json",
	"MAPTHING_RENDERER_CMD":        "/opt/renderer/start.sh",
	"MAPTHING_RENDERER_HEALTH_URL": "http://127.0.0.1:8081/healthz",
}

// SecretMask is shown in place of a secret that is set.
const SecretMask = "••••••••" //nolint:gosec // the mask shown instead of a secret, not a secret

// secretLooking reports whether a variable name suggests its value is a secret.
func secretLooking(name string) bool {
	n := strings.ToLower(name)
	for _, w := range []string{"password", "passwd", "secret", "token", "key", "credential"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// Describe returns every setting with its effective value and where it came from, in page
// order, followed by any MAPTHING_* variable that is set but not one mapthing reads (a typo,
// usually), in group "Not recognised". Secrets are never returned: a set secret is SecretMask.
// redact is applied to values that may carry a credential (URLs); nil leaves them as they are.
func Describe(environ []string, redact func(string) string) []Setting {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	known := map[string]bool{}
	out := make([]Setting, 0, len(settingDefs))
	for _, d := range settingDefs {
		known[d.name] = true
		v, set := env[d.name]
		set = set && v != ""
		s := Setting{Name: d.name, Group: d.group, Default: d.def, Secret: d.secret, Set: set, Help: d.help, Source: "default"}
		switch {
		case set && d.secret:
			s.Value, s.Source = SecretMask, "env"
		case set:
			s.Value, s.Source = v, "env"
			if imageValues[d.name] == v {
				s.Source = "image"
			}
			if d.sensitive && redact != nil {
				s.Value = redact(v)
			}
		case d.secret:
			s.Value = ""
		default:
			s.Value = d.def
		}
		out = append(out, s)
	}
	var extra []Setting
	for k, v := range env {
		if !strings.HasPrefix(k, "MAPTHING_") || known[k] || internalVars[k] || v == "" {
			continue
		}
		if strings.HasPrefix(k, "MAPTHING_TEST_") || strings.HasPrefix(k, "MAPTHING_FAKE_") {
			continue
		}
		s := Setting{Name: k, Group: "Not recognised", Source: "env", Set: true,
			Help: "mapthing does not read this variable. Check the name."}
		if secretLooking(k) {
			s.Value, s.Secret = SecretMask, true
		} else if redact != nil {
			s.Value = redact(v)
		} else {
			s.Value = v
		}
		extra = append(extra, s)
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Name < extra[j].Name })
	return append(out, extra...)
}

// Environ is os.Environ, replaceable in tests.
var Environ = os.Environ
