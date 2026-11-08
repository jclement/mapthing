package tiles

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func tileAt(lon, lat float64, z int) (int, int) {
	n := math.Exp2(float64(z))
	x := int((lon + 180) / 360 * n)
	y := int((1 - math.Asinh(math.Tan(lat*math.Pi/180))/math.Pi) / 2 * n)
	return x, y
}

func TestImagerySourceChoice(t *testing.T) {
	c := NewImageryChooser(func(string) string { return "" })
	cases := []struct {
		name     string
		lon, lat float64
		z        int
		want     string // source name, "" = not served
	}{
		{"Permian TX z12", -102.25, 31.9, 12, "usgs-imagery-only"},
		{"Permian TX z16", -102.25, 31.9, 16, "usgs-imagery-only"},
		{"Permian TX z17 beyond USGS", -102.25, 31.9, 17, ""},
		{"Haynesville LA z14", -93.7, 32.3, 14, "usgs-imagery-only"},
		{"Delaware z15", -75.52, 39.16, 15, "usgs-imagery-only"},
		{"Detroit z12", -83.05, 42.33, 12, "usgs-imagery-only"},
		{"Juneau AK z12", -134.4, 58.3, 12, "usgs-imagery-only"},
		{"Anchorage AK z12", -149.9, 61.2, 12, "usgs-imagery-only"},
		{"Honolulu z12", -157.86, 21.3, 12, "usgs-imagery-only"},
		{"Calgary AB z12", -114.07, 51.05, 12, "eox-s2cloudless-2016"},
		{"Calgary AB z6 (global USGS)", -114.07, 51.05, 6, "usgs-imagery-only"},
		{"Fort St John BC z13", -120.85, 56.25, 13, "eox-s2cloudless-2016"},
		{"London ON z12", -81.25, 42.98, 12, "eox-s2cloudless-2016"},
		{"Whitehorse YT z12", -135.05, 60.72, 12, "eox-s2cloudless-2016"},
		{"Calgary AB z15 beyond S2", -114.07, 51.05, 15, ""},
		{"Monterrey MX z12", -100.3, 25.67, 12, "eox-s2cloudless-2016"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, y := tileAt(tc.lon, tc.lat, tc.z)
			src, ok := c.Source(tc.z, x, y)
			got := ""
			if ok {
				got = src.Name
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImageryURL(t *testing.T) {
	c := NewImageryChooser(func(string) string { return "" })
	x, y := tileAt(-102.25, 31.9, 12)
	u, attr, ok := c.URL(12, x, y)
	if !ok || u != "https://basemap.nationalmap.gov/arcgis/rest/services/USGSImageryOnly/MapServer/tile/12/"+strconv.Itoa(y)+"/"+strconv.Itoa(x) {
		t.Fatalf("url %q ok=%v", u, ok)
	}
	if !strings.Contains(attr, "USGS") {
		t.Fatalf("attribution %q", attr)
	}
	for _, bad := range [][3]int{{-1, 0, 0}, {3, 8, 0}, {3, 0, 8}, {25, 0, 0}, {2, -1, 0}} {
		if _, _, ok := c.URL(bad[0], bad[1], bad[2]); ok {
			t.Fatalf("tile %v accepted", bad)
		}
	}
	if !strings.Contains(c.Attribution(), "EOX") || !strings.Contains(c.Attribution(), "USGS") {
		t.Fatalf("default attribution %q", c.Attribution())
	}
}

func TestImageryOverride(t *testing.T) {
	env := map[string]string{
		"MAPTHING_IMAGERY_URL":         "https://img.example/{z}/{x}/{y}.jpg?tms={-y}&q={q}",
		"MAPTHING_IMAGERY_ATTRIBUTION": "Example imagery",
		"MAPTHING_IMAGERY_MAXZOOM":     "13",
	}
	c := NewImageryChooser(func(k string) string { return env[k] })
	u, attr, ok := c.URL(3, 5, 1)
	if !ok || u != "https://img.example/3/5/1.jpg?tms=6&q=103" || attr != "Example imagery" {
		t.Fatalf("got %q %q %v", u, attr, ok)
	}
	if _, _, ok := c.URL(14, 0, 0); ok {
		t.Fatal("z14 served beyond MAPTHING_IMAGERY_MAXZOOM")
	}
	if c.Attribution() != "Example imagery" {
		t.Fatalf("attribution %q", c.Attribution())
	}
}

func TestQuadkey(t *testing.T) {
	for _, tc := range []struct {
		z, x, y int
		want    string
	}{{0, 0, 0, ""}, {1, 1, 0, "1"}, {1, 0, 1, "2"}, {3, 3, 5, "213"}} {
		if got := quadkey(tc.z, tc.x, tc.y); got != tc.want {
			t.Errorf("quadkey(%d,%d,%d) = %q, want %q", tc.z, tc.x, tc.y, got, tc.want)
		}
	}
}
