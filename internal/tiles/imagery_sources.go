package tiles

// Imagery sources for /tiles/imagery/{z}/{x}/{y}.jpg. mapthing proxies and caches tiles from
// openly licensed imagery services; this file decides which upstream serves a given tile.
// Licences and the reasoning behind the choice are in docs/imagery.md.
//
// Default chain:
//   - z <= 8 anywhere, and z <= 16 inside the United States: USGS The National Map
//     "USGSImageryOnly" (public domain; NAIP-derived in the lower 48).
//   - elsewhere (Canada etc.) z <= 14: EOX Sentinel-2 cloudless 2016 (CC BY 4.0). Later EOX
//     mosaics are CC BY-NC-SA and are not used by default.
//
// MAPTHING_IMAGERY_URL replaces the chain with one template ({z} {x} {y}, {-y} for TMS rows,
// {q} for a Bing-style quadkey); MAPTHING_IMAGERY_ATTRIBUTION and MAPTHING_IMAGERY_MAXZOOM go
// with it. Pointing it at a service whose terms forbid proxying or caching (Esri World Imagery,
// Google, Bing, Mapbox) is the operator's responsibility.

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ImagerySource is one upstream XYZ imagery service.
type ImagerySource struct {
	Name        string
	URL         string // template: {z} {x} {y} {-y} {q}
	Attribution string
	MinZoom     int
	MaxZoom     int
}

// Built-in sources.
var (
	ImageryUSGS = ImagerySource{
		Name:        "usgs-imagery-only",
		URL:         "https://basemap.nationalmap.gov/arcgis/rest/services/USGSImageryOnly/MapServer/tile/{z}/{y}/{x}",
		Attribution: "Imagery: USGS The National Map (public domain)",
		MinZoom:     0,
		MaxZoom:     16,
	}
	ImageryS2Cloudless2016 = ImagerySource{
		Name:        "eox-s2cloudless-2016",
		URL:         "https://tiles.maps.eox.at/wmts/1.0.0/s2cloudless_3857/default/g/{z}/{y}/{x}.jpg",
		Attribution: "Imagery: Sentinel-2 cloudless - https://s2maps.eu by EOX IT Services GmbH (Contains modified Copernicus Sentinel data 2016), CC BY 4.0",
		MinZoom:     0,
		MaxZoom:     14,
	}
)

// usGlobalMaxZoom: up to this zoom the USGS service has (coarse) imagery for the whole world,
// so low zooms come from one source and look uniform.
const usGlobalMaxZoom = 8

// ImageryChooser picks the upstream for a tile.
type ImageryChooser struct {
	override *ImagerySource
}

// NewImageryChooser builds a chooser from configuration read through getenv.
func NewImageryChooser(getenv func(string) string) *ImageryChooser {
	c := &ImageryChooser{}
	if u := strings.TrimSpace(getenv("MAPTHING_IMAGERY_URL")); u != "" {
		src := ImagerySource{Name: "custom", URL: u, Attribution: getenv("MAPTHING_IMAGERY_ATTRIBUTION"), MaxZoom: 19}
		if mz, err := strconv.Atoi(getenv("MAPTHING_IMAGERY_MAXZOOM")); err == nil && mz >= 0 && mz <= 24 {
			src.MaxZoom = mz
		}
		c.override = &src
	}
	return c
}

// Source returns the source that serves tile z/x/y, or ok=false when no configured source
// covers it (outside the zoom range, or an invalid tile).
func (c *ImageryChooser) Source(z, x, y int) (ImagerySource, bool) {
	if z < 0 || z > 24 || x < 0 || y < 0 || x >= 1<<z || y >= 1<<z {
		return ImagerySource{}, false
	}
	if c.override != nil {
		return *c.override, z >= c.override.MinZoom && z <= c.override.MaxZoom
	}
	if z <= usGlobalMaxZoom {
		return ImageryUSGS, true
	}
	lon, lat := tileCenterLonLat(z, x, y)
	if inUS(lon, lat) {
		return ImageryUSGS, z <= ImageryUSGS.MaxZoom
	}
	return ImageryS2Cloudless2016, z <= ImageryS2Cloudless2016.MaxZoom
}

// URL returns the upstream URL and attribution for tile z/x/y.
func (c *ImageryChooser) URL(z, x, y int) (url, attribution string, ok bool) {
	src, ok := c.Source(z, x, y)
	if !ok {
		return "", "", false
	}
	return expandTemplate(src.URL, z, x, y), src.Attribution, true
}

// Attribution is the attribution text for the configured imagery, for the style's imagery
// source and the UI footer.
func (c *ImageryChooser) Attribution() string {
	if c.override != nil {
		return c.override.Attribution
	}
	return ImageryUSGS.Attribution + "; " + ImageryS2Cloudless2016.Attribution
}

var (
	defaultChooserOnce sync.Once
	defaultChooser     *ImageryChooser
)

func defaultImagery() *ImageryChooser {
	defaultChooserOnce.Do(func() { defaultChooser = NewImageryChooser(os.Getenv) })
	return defaultChooser
}

// ImageryURL chooses the upstream for tile z/x/y using the process environment.
func ImageryURL(z, x, y int) (url string, attribution string, ok bool) {
	return defaultImagery().URL(z, x, y)
}

// ImageryAttribution is the attribution for the imagery configured in the environment.
func ImageryAttribution() string { return defaultImagery().Attribution() }

func expandTemplate(t string, z, x, y int) string {
	r := strings.NewReplacer(
		"{z}", strconv.Itoa(z),
		"{x}", strconv.Itoa(x),
		"{y}", strconv.Itoa(y),
		"{-y}", strconv.Itoa((1<<z)-1-y),
		"{q}", quadkey(z, x, y),
	)
	return r.Replace(t)
}

func quadkey(z, x, y int) string {
	var b strings.Builder
	for i := z; i > 0; i-- {
		d := byte('0')
		m := 1 << (i - 1)
		if x&m != 0 {
			d++
		}
		if y&m != 0 {
			d += 2
		}
		b.WriteByte(d)
	}
	return b.String()
}

func tileCenterLonLat(z, x, y int) (lon, lat float64) {
	n := math.Exp2(float64(z))
	lon = (float64(x)+0.5)/n*360 - 180
	lat = math.Atan(math.Sinh(math.Pi*(1-2*(float64(y)+0.5)/n))) * 180 / math.Pi
	return lon, lat
}

// Coarse US outlines (lon, lat). Accuracy of tens of km is enough: it only decides which
// upstream to ask, and the cost of a wrong guess near the border is one blank tile.
var usPolygons = [][][2]float64{
	// lower 48: west coast, 49th parallel, Great Lakes, St Lawrence, Maine, east coast, Gulf,
	// Rio Grande, Mexico border
	{
		{-125.0, 48.5}, {-123.3, 49.0}, {-95.15, 49.0}, {-95.15, 49.4}, {-94.6, 48.7},
		{-89.6, 48.0}, {-84.8, 46.9}, {-83.4, 46.0}, {-82.4, 45.3}, {-82.5, 43.0},
		{-83.1, 42.0}, {-79.0, 42.5}, {-79.0, 43.3}, {-76.5, 43.6}, {-74.7, 45.0},
		{-71.5, 45.0}, {-70.0, 46.7}, {-69.2, 47.5}, {-67.8, 47.1}, {-67.0, 44.8},
		{-66.5, 44.5}, {-70.0, 41.0}, {-75.5, 35.0}, {-80.0, 24.0}, {-82.0, 24.0},
		{-90.0, 28.5}, {-97.2, 25.8}, {-99.5, 27.5}, {-101.4, 29.8}, {-103.0, 29.0},
		{-104.7, 29.9}, {-106.5, 31.8}, {-108.2, 31.3}, {-111.1, 31.3}, {-114.8, 32.5},
		{-117.1, 32.5}, {-118.5, 33.0}, {-125.0, 40.0},
	},
	{{-180, 51}, {-141, 51}, {-141, 72}, {-180, 72}}, // Alaska west of the 141st meridian
	{ // Alaska panhandle
		{-141, 60.3}, {-139, 60.3}, {-137.5, 59.2}, {-135.5, 59.8}, {-133.4, 58.4},
		{-131.8, 56.6}, {-130, 55.9}, {-130, 54.6}, {-136, 54.6}, {-141, 58},
	},
	{{172, 51}, {180, 51}, {180, 54}, {172, 54}},                     // Aleutians west of 180
	{{-160.6, 18.7}, {-154.6, 18.7}, {-154.6, 22.4}, {-160.6, 22.4}}, // Hawaii
	{{-68.0, 17.6}, {-64.5, 17.6}, {-64.5, 18.6}, {-68.0, 18.6}},     // Puerto Rico, USVI
}

func inUS(lon, lat float64) bool {
	for _, p := range usPolygons {
		if pointInPolygon(lon, lat, p) {
			return true
		}
	}
	return false
}

func pointInPolygon(x, y float64, poly [][2]float64) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		xi, yi := poly[i][0], poly[i][1]
		xj, yj := poly[j][0], poly[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

// ImageryUpstream returns the upstream URL for an imagery tile (used by the imagery proxy).
func ImageryUpstream(z, x, y int) (url string, ok bool) {
	u, _, ok := ImageryURL(z, x, y)
	return u, ok
}
