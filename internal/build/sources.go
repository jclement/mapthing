package build

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// Ocean records where a generation's ocean and land polygons came from (manifest "ocean").
const (
	OceanOSM          = "osm"                // osmdata.openstreetmap.de split polygons (or a mirror)
	OceanNaturalEarth = "approximate:ne-10m" // Natural Earth 10m fallback, clipped to the regions
	OceanMissing      = "missing"            // placeholders: no ocean drawn at high zoom
)

// planetilerSource is one auxiliary input of the Protomaps profile. Files are cached in
// /data/cache/planetiler and reused by every build; Planetiler finds them there and does not
// download again.
type planetilerSource struct {
	name, file, url, env string
	ocean                bool  // water/land polygons: failure falls back instead of failing the build
	approxBytes          int64 // download size, for the disk guard
}

var planetilerSources = []planetilerSource{
	{"natural earth", "natural_earth_vector.gpkg.zip", "https://naciscdn.org/naturalearth/packages/natural_earth_vector.gpkg.zip", "MAPTHING_NATURAL_EARTH_URL", false, 470 << 20},
	{"water polygons", "water-polygons-split-3857.zip", "https://osmdata.openstreetmap.de/download/water-polygons-split-3857.zip", "MAPTHING_WATER_POLYGONS_URL", true, 900 << 20},
	{"land polygons", "land-polygons-split-3857.zip", "https://osmdata.openstreetmap.de/download/land-polygons-split-3857.zip", "MAPTHING_LAND_POLYGONS_URL", true, 800 << 20},
	{"landcover", "daylight-landcover.gpkg", "https://r2-public.protomaps.com/datasets/daylight-landcover.gpkg", "MAPTHING_LANDCOVER_URL", false, 40 << 20},
	{"qrank", "qrank.csv.gz", "https://qrank.toolforge.org/download/qrank.csv.gz", "MAPTHING_QRANK_URL", false, 130 << 20},
	{"pgf encoding", "pgf-encoding.zip", "https://wipfli.github.io/pgf-encoding/pgf-encoding.zip", "MAPTHING_PGF_ENCODING_URL", false, 1 << 20},
}

// sourceBackoff is the wait before each retry of a failed source download.
var sourceBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

// fallbackRetryBudget caps the total retry time for a source that has a fallback (the ocean
// polygons), and failedHostTTL is how long a host that exhausted it is skipped by later builds
// of the same process (a long-running server retries it twice a day).
var (
	fallbackRetryBudget = 60 * time.Second
	failedHostTTL       = 12 * time.Hour
)

// ensureSources makes every auxiliary source available in the cache. It returns the ocean
// status and, when the OSM polygons could not be had, paths to substitute water/land zips.
func (s *state) ensureSources(ctx context.Context, wd string) (ocean, waterZip, landZip string, err error) {
	dir := s.b.sourcesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	oceanOK := true
	for i, src := range planetilerSources {
		s.rep.Progress(float64(i)/float64(len(planetilerSources))*0.1, "sources: "+src.name)
		path := filepath.Join(dir, src.file)
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			continue
		}
		srcURL := src.url
		if v := os.Getenv(src.env); v != "" {
			srcURL = v
		}
		budget := time.Duration(0)
		if src.ocean {
			budget = fallbackRetryBudget
		}
		if err := s.downloadRetry(ctx, srcURL, path, src.name, budget); err != nil {
			if src.ocean {
				s.logf("sources: %s unavailable after retries (%v)", src.name, err)
				oceanOK = false
				continue
			}
			return "", "", "", fmt.Errorf("%s from %s: %w (set %s to a mirror)", src.name, srcURL, err, src.env)
		}
	}
	if oceanOK {
		return OceanOSM, "", "", nil
	}
	return s.oceanFallback(ctx, wd)
}

// downloadRetry downloads with backoff. budget > 0 caps the total time spent (the source has
// a fallback); such a source's host is then remembered as failed and skipped for failedHostTTL.
func (s *state) downloadRetry(ctx context.Context, rawURL, path, name string, budget time.Duration) error {
	host := hostOf(rawURL)
	if budget > 0 {
		if at, ok := s.b.hostFailedAt(host); ok {
			return fmt.Errorf("%s failed at %s; not retrying until %s", host,
				at.Format(time.RFC3339), at.Add(failedHostTTL).Format(time.RFC3339))
		}
	}
	start := time.Now()
	var err error
	for attempt := 0; ; attempt++ {
		s.logf("sources: downloading %s from %s", name, rawURL)
		if err = s.downloadFile(ctx, rawURL, path, name); err == nil {
			return nil
		}
		if attempt >= len(sourceBackoff) || ctx.Err() != nil {
			break
		}
		wait := sourceBackoff[attempt]
		if budget > 0 && time.Since(start)+wait > budget {
			break
		}
		s.logf("sources: %s failed (%v); retrying in %s", name, err, wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	if budget > 0 && ctx.Err() == nil {
		s.b.markHostFailed(host)
	}
	return err
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

func (b *Builder) hostFailedAt(host string) (time.Time, bool) {
	b.hostMu.Lock()
	defer b.hostMu.Unlock()
	at, ok := b.failedHosts[host]
	if !ok || time.Since(at) > failedHostTTL {
		return time.Time{}, false
	}
	return at, true
}

func (b *Builder) markHostFailed(host string) {
	b.hostMu.Lock()
	defer b.hostMu.Unlock()
	if b.failedHosts == nil {
		b.failedHosts = map[string]time.Time{}
	}
	b.failedHosts[host] = time.Now()
}

func (s *state) downloadFile(ctx context.Context, rawURL, path, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.b.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	part := path + ".part"
	_, n, err := s.copyWithProgress(part, resp.Body, resp.ContentLength, name)
	if err == nil && resp.ContentLength > 0 && n != resp.ContentLength {
		err = fmt.Errorf("short download: %d of %d bytes", n, resp.ContentLength)
	}
	if err != nil {
		_ = os.Remove(part)
		return err
	}
	return os.Rename(part, path)
}

// oceanFallback builds water and land shapefile zips for the regions' area from Natural Earth
// 10m (already cached for the low zooms), clipped to the regions' bbox plus a margin. The
// coastline is coarse at street zooms but never floods land. If even that fails, tiny
// placeholders let the build finish without high-zoom ocean.
func (s *state) oceanFallback(ctx context.Context, wd string) (string, string, string, error) {
	out := filepath.Join(wd, "ocean")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", "", "", err
	}
	water := filepath.Join(out, "water-polygons-split-3857.zip")
	land := filepath.Join(out, "land-polygons-split-3857.zip")
	b := unionBBox(s.opt.Regions, 0.5)
	ne := "/vsizip/" + filepath.Join(s.b.sourcesDir(), "natural_earth_vector.gpkg.zip") + "/packages/natural_earth_vector.gpkg"
	err := func() error {
		for _, l := range []struct{ layer, name, zip string }{
			{"ne_10m_ocean", "water", water}, {"ne_10m_land", "land", land},
		} {
			shpDir := filepath.Join(out, l.name+"-polygons-split-3857")
			_ = os.RemoveAll(shpDir)
			if err := os.MkdirAll(shpDir, 0o755); err != nil {
				return err
			}
			bb := []string{fmt.Sprint(b[0]), fmt.Sprint(b[1]), fmt.Sprint(b[2]), fmt.Sprint(b[3])}
			args := append([]string{"-f", "ESRI Shapefile", "-t_srs", "EPSG:3857", "-spat"}, bb...)
			args = append(args, "-clipsrc")
			args = append(args, bb...)
			args = append(args, filepath.Join(shpDir, l.name+"_polygons.shp"), ne, l.layer)
			if err := s.exec(ctx, out, s.b.Tools.Ogr2ogr, args...); err != nil {
				return err
			}
			if err := zipDir(shpDir, l.zip); err != nil {
				return err
			}
		}
		return nil
	}()
	if err == nil {
		s.logf("ocean: using Natural Earth 10m polygons clipped to %v (approximate coastline)", b)
		return OceanNaturalEarth, water, land, nil
	}
	s.logf("ocean: Natural Earth fallback failed (%v); building without high-zoom ocean", err)
	if err := writePlaceholderZip(water, "water"); err != nil {
		return "", "", "", err
	}
	if err := writePlaceholderZip(land, "land"); err != nil {
		return "", "", "", err
	}
	return OceanMissing, water, land, nil
}

func unionBBox(rs []catalog.Region, margin float64) catalog.BBox {
	b := catalog.BBox{180, 90, -180, -90}
	for _, r := range rs {
		b[0], b[1] = math.Min(b[0], r.BBox[0]), math.Min(b[1], r.BBox[1])
		b[2], b[3] = math.Max(b[2], r.BBox[2]), math.Max(b[3], r.BBox[3])
	}
	return catalog.BBox{
		math.Max(-180, b[0]-margin), math.Max(-85, b[1]-margin),
		math.Min(180, b[2]+margin), math.Min(85, b[3]+margin),
	}
}

// zipDir zips dir's files under a folder named after dir (the layout Planetiler expects).
func zipDir(dir, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	ents, err := os.ReadDir(dir)
	if err != nil {
		_ = f.Close()
		return err
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		w, err := zw.Create(filepath.Base(dir) + "/" + e.Name())
		if err != nil {
			_ = f.Close()
			return err
		}
		src, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			_ = f.Close()
			return err
		}
		_, err = io.Copy(w, src)
		_ = src.Close()
		if err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writePlaceholderZip writes a minimal valid shapefile (one tiny polygon far off Antarctica, in
// EPSG:3857) so Planetiler's shapefile source opens without drawing anything.
func writePlaceholderZip(dst, name string) error {
	dir := filepath.Join(filepath.Dir(dst), name+"-placeholder", name+"-polygons-split-3857")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeTinyShapefile(filepath.Join(dir, name+"_polygons")); err != nil {
		return err
	}
	return zipDir(dir, dst)
}
