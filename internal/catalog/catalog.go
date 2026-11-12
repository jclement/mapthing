// Package catalog lists the regions mapthing can install: every Geofabrik extract (from
// Geofabrik's index-v1.json, cached under the data directory and refreshed weekly) plus custom
// regions clipped by bbox or polygon from the smallest Geofabrik extract that contains them.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultIndexURL is Geofabrik's extract index, with geometries.
const DefaultIndexURL = "https://download.geofabrik.de/index-v1.json"

// DefaultPrefix limits Entries to North America unless the caller asks for everything.
const DefaultPrefix = "north-america/"

const geofabrikBase = "https://download.geofabrik.de/"

// BBox is west, south, east, north in WGS84 degrees.
type BBox [4]float64

// Center returns the bbox centre as lon, lat.
func (b BBox) Center() [2]float64 { return [2]float64{(b[0] + b[2]) / 2, (b[1] + b[3]) / 2} }

// Area is the bbox area in square degrees (only used to rank extracts by size).
func (b BBox) Area() float64 { return math.Abs(b[2]-b[0]) * math.Abs(b[3]-b[1]) }

// Valid reports whether the bbox is a non-empty box inside WGS84 bounds.
func (b BBox) Valid() bool {
	return b[0] >= -180 && b[2] <= 180 && b[1] >= -90 && b[3] <= 90 && b[0] < b[2] && b[1] < b[3]
}

// Entry is one installable Geofabrik extract.
type Entry struct {
	ID               string   `json:"id"`     // e.g. north-america/us/texas
	Name             string   `json:"name"`   // e.g. Texas
	Parent           string   `json:"parent"` // parent entry id, "" for continents
	BBox             BBox     `json:"bbox"`
	PBFURL           string   `json:"pbf_url"`
	UpdatesURL       string   `json:"updates_url,omitempty"` // has state.txt
	Size             int64    `json:"size,omitempty"`        // bytes, from HEAD (0 = unknown)
	ElevationSources []string `json:"elevation_sources,omitempty"`

	geom multiPolygon
}

// Region is something a build installs: a Geofabrik extract or a custom clip of one.
type Region struct {
	ID         string `json:"id"` // Geofabrik id, or custom/<slug>
	Name       string `json:"name"`
	BBox       BBox   `json:"bbox"`
	PBFURL     string `json:"pbf_url,omitempty"`
	UpdatesURL string `json:"updates_url,omitempty"`
	SizeHint   int64  `json:"size_hint,omitempty"` // bytes of the extract (parent extract for custom)
	// Polygon is the region's outline as lon/lat rings, read with the even-odd rule (outer rings
	// and holes of every part). Geofabrik regions: the extract boundary from index-v1.json.
	// Custom regions: the drawn polygon, or the bbox as a rectangle.
	Polygon [][][2]float64 `json:"polygon,omitempty"`
	// Custom regions only.
	Custom *Custom `json:"custom,omitempty"`
	// TerrainOnDemand: the operator set this region's terrain to on demand. The terrain build
	// then makes only its low zooms; deeper tiles and elevations are read from the DEM hosts
	// when asked for (internal/ondemand).
	TerrainOnDemand bool `json:"terrain_on_demand,omitempty"`
}

// Custom describes a clipped region.
type Custom struct {
	// Parents are the Geofabrik extracts it is clipped from (each clipped, then merged).
	Parents []Region `json:"parents"`
}

// Catalog loads and caches the Geofabrik index.
type Catalog struct {
	DataDir  string        // cache lives in DataDir/catalog
	IndexURL string        // default DefaultIndexURL
	MaxAge   time.Duration // refresh interval, default 7 days
	Client   *http.Client
	// Coverage returns the elevation source ids that cover a bbox. Wired by the server to the
	// elevation package; nil lists none.
	Coverage func(BBox) []string

	mu      sync.RWMutex
	entries []Entry
	byID    map[string]int
	sizes   map[string]sizeRec
	fetched time.Time
}

type sizeRec struct {
	Size    int64     `json:"size"`
	Checked time.Time `json:"checked"`
}

// New returns a catalog caching under dataDir.
func New(dataDir string) *Catalog {
	return &Catalog{DataDir: dataDir}
}

func (c *Catalog) dir() string { return filepath.Join(c.DataDir, "catalog") }

func (c *Catalog) indexPath() string { return filepath.Join(c.dir(), "geofabrik-index-v1.json") }

func (c *Catalog) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (c *Catalog) maxAge() time.Duration {
	if c.MaxAge > 0 {
		return c.MaxAge
	}
	return 7 * 24 * time.Hour
}

// Load reads the cached index, fetching a fresh copy when the cache is missing or older than
// MaxAge. A failed refresh falls back to the stale cache.
func (c *Catalog) Load(ctx context.Context) error {
	path := c.indexPath()
	st, statErr := os.Stat(path)
	if statErr != nil || time.Since(st.ModTime()) > c.maxAge() {
		if err := c.fetch(ctx, path); err != nil {
			if statErr != nil {
				return fmt.Errorf("catalog: fetch index: %w", err)
			}
			// Stale cache is better than nothing.
		}
	}
	return c.loadFile(path)
}

// Ensure loads the index once (Load) if nothing is loaded yet.
func (c *Catalog) Ensure(ctx context.Context) error {
	c.mu.RLock()
	loaded := len(c.entries) > 0
	c.mu.RUnlock()
	if loaded {
		return nil
	}
	return c.Load(ctx)
}

// Refresh forces a re-download of the index.
func (c *Catalog) Refresh(ctx context.Context) error {
	if err := c.fetch(ctx, c.indexPath()); err != nil {
		return err
	}
	return c.loadFile(c.indexPath())
}

func (c *Catalog) fetch(ctx context.Context, path string) error {
	url := c.IndexURL
	if url == "" {
		url = DefaultIndexURL
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Validate before replacing the cache.
	data, err := os.ReadFile(tmp)
	if err == nil {
		_, err = parseIndex(data)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("index invalid: %w", err)
	}
	return os.Rename(tmp, path)
}

func (c *Catalog) loadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	entries, err := parseIndex(data)
	if err != nil {
		return err
	}
	st, _ := os.Stat(path)
	sizes := c.readSizes()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = entries
	c.byID = make(map[string]int, len(entries))
	for i := range entries {
		c.byID[entries[i].ID] = i
	}
	c.sizes = sizes
	if st != nil {
		c.fetched = st.ModTime()
	}
	return nil
}

// FetchedAt is when the cached index was downloaded.
func (c *Catalog) FetchedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetched
}

// Filter selects catalog entries.
type Filter struct {
	All    bool   // every continent; otherwise Prefix (default north-america/)
	Prefix string // id prefix
}

// Entries returns matching entries sorted by id, with cached sizes and elevation coverage.
func (c *Catalog) Entries(f Filter) []Entry {
	prefix := f.Prefix
	if prefix == "" && !f.All {
		prefix = DefaultPrefix
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Entry
	for _, e := range c.entries {
		if prefix != "" && !strings.HasPrefix(e.ID, prefix) && e.ID+"/" != prefix {
			continue
		}
		out = append(out, c.decorate(e))
	}
	return out
}

// Get returns one entry.
func (c *Catalog) Get(id string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	i, ok := c.byID[id]
	if !ok {
		return Entry{}, false
	}
	return c.decorate(c.entries[i]), true
}

func (c *Catalog) decorate(e Entry) Entry {
	if s, ok := c.sizes[e.ID]; ok {
		e.Size = s.Size
	}
	if c.Coverage != nil {
		e.ElevationSources = c.Coverage(e.BBox)
	}
	return e
}

// Region resolves a Geofabrik entry id to a buildable Region.
func (c *Catalog) Region(id string) (Region, error) {
	e, ok := c.Get(id)
	if !ok {
		return Region{}, fmt.Errorf("catalog: unknown region %q", id)
	}
	return e.Region(), nil
}

// Region converts an entry to a Region.
func (e Entry) Region() Region {
	return Region{ID: e.ID, Name: e.Name, BBox: e.BBox, PBFURL: e.PBFURL, UpdatesURL: e.UpdatesURL,
		SizeHint: e.Size, Polygon: e.geom.rings()}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a name into an id-safe slug.
func Slug(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "region"
	}
	return s
}

// ErrNotCovered means no Geofabrik extract covers any of the requested custom area.
var ErrNotCovered = errors.New("catalog: no extract contains that area")

// CustomRegion builds a custom Region clipped by bbox or polygon (one of them) from the cheapest
// set of Geofabrik extracts that together cover it (see cover.go). Sizes come from the cache:
// call PlanCustom (or FillSizes) first when they may be missing.
func (c *Catalog) CustomRegion(name string, bbox *BBox, polygon [][][2]float64) (Region, error) {
	r, _, err := c.CustomRegionPlan(name, bbox, polygon)
	return r, err
}

// CustomRegionPlan is CustomRegion that also returns the cover plan.
func (c *Catalog) CustomRegionPlan(name string, bbox *BBox, polygon [][][2]float64) (Region, CoverPlan, error) {
	if strings.TrimSpace(name) == "" {
		return Region{}, CoverPlan{}, errors.New("catalog: custom region needs a name")
	}
	poly, box, err := areaPolygon(bbox, polygon)
	if err != nil {
		return Region{}, CoverPlan{}, err
	}
	plan, err := c.plan(poly)
	if err != nil {
		return Region{}, CoverPlan{}, err
	}
	var parents []Region
	for _, p := range plan.Parents {
		e, ok := c.Get(p.ID)
		if !ok {
			return Region{}, CoverPlan{}, fmt.Errorf("catalog: parent %s vanished", p.ID)
		}
		pr := e.Region()
		pr.SizeHint = p.Bytes // the parent keeps its outline: the disk guard sizes the clip by area share
		parents = append(parents, pr)
	}
	return Region{
		ID:       "custom/" + Slug(name),
		Name:     name,
		BBox:     box,
		SizeHint: plan.TotalBytes,
		Polygon:  poly,
		Custom:   &Custom{Parents: parents},
	}, plan, nil
}

// --- index parsing ---

type indexDoc struct {
	Features []struct {
		Properties struct {
			ID     string            `json:"id"`
			Parent string            `json:"parent"`
			Name   string            `json:"name"`
			URLs   map[string]string `json:"urls"`
		} `json:"properties"`
		Geometry *struct {
			Type        string          `json:"type"`
			Coordinates json.RawMessage `json:"coordinates"`
		} `json:"geometry"`
	} `json:"features"`
}

// IDFromURL derives a path-style id (north-america/us/texas) from a Geofabrik PBF URL.
func IDFromURL(pbf string) string {
	s := strings.TrimPrefix(pbf, geofabrikBase)
	s = strings.TrimPrefix(s, "http://download.geofabrik.de/")
	return strings.TrimSuffix(s, "-latest.osm.pbf")
}

func parseIndex(data []byte) ([]Entry, error) {
	var doc indexDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Features) == 0 {
		return nil, errors.New("no features")
	}
	shortToID := map[string]string{}
	var entries []Entry
	for _, f := range doc.Features {
		p := f.Properties
		pbf := p.URLs["pbf"]
		if pbf == "" {
			continue
		}
		e := Entry{ID: IDFromURL(pbf), Name: p.Name, PBFURL: pbf, UpdatesURL: p.URLs["updates"]}
		if f.Geometry != nil {
			g, err := parseGeometry(f.Geometry.Type, f.Geometry.Coordinates)
			if err == nil {
				e.geom = g
				e.BBox = g.bbox()
			}
		}
		shortToID[p.ID] = e.ID
		e.Parent = p.Parent // short id for now
		entries = append(entries, e)
	}
	byID := map[string]bool{}
	for _, e := range entries {
		byID[e.ID] = true
	}
	for i := range entries {
		e := &entries[i]
		e.Parent = shortToID[e.Parent]
		// Geofabrik lists US states as "us/<state>" with parent "north-america" and the short
		// id as name. Use the nearest path ancestor that is an entry (north-america/us) and a
		// readable name.
		for p := path.Dir(e.ID); p != "." && p != "/"; p = path.Dir(p) {
			if byID[p] {
				if strings.HasPrefix(p, e.Parent+"/") || e.Parent == "" {
					e.Parent = p
				}
				break
			}
		}
		if strings.Contains(e.Name, "/") || e.Name == "" {
			e.Name = titleFromSlug(path.Base(e.ID))
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

// titleFromSlug turns "new-mexico" into "New Mexico".
func titleFromSlug(s string) string {
	words := strings.Split(s, "-")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
