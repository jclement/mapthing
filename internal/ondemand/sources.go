package ondemand

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/jclement/mapthing/internal/elevation"
)

// The DEM sources read on demand are the ones the terrain build uses, in the
// same priority order (elevation.KnownSources): where a higher one has no
// data for a pixel, the next one is asked. internal/build/terrain_sources.go
// is the build's side of the same table.

const (
	url3DEP   = "https://prd-tnm.s3.amazonaws.com/StagedProducts/Elevation/13/TIFF/current/%s/USGS_13_%s.tif"
	urlGLO30  = "https://copernicus-dem-30m.s3.amazonaws.com/%[1]s/%[1]s.tif"
	urlMRDEM  = "https://canelevation-dem.s3.ca-central-1.amazonaws.com/mrdem-30/mrdem-30-dtm.tif"
	stacHRDEM = "https://datacube.services.geo.ca/stac/api/search"
)

// URLs lets tests point the sources at a local server.
type URLs struct {
	ThreeDEP, GLO30, MRDEM, HRDEMIndex string
}

// DefaultURLs are the public hosts.
func DefaultURLs() URLs {
	return URLs{ThreeDEP: url3DEP, GLO30: urlGLO30, MRDEM: urlMRDEM, HRDEMIndex: stacHRDEM}
}

// source is one DEM product and how to find its files for an area.
type source struct {
	def elevation.SourceDef
	// index is the per-pixel source number written into tiles (1-based
	// position in elevation.KnownSources).
	index uint8
	// project maps longitude/latitude to the product's CRS; nil when the
	// product is in geographic coordinates.
	project func(lon, lat float64) (x, y float64)
	// host is where the product is published, for the admin pages.
	host string
	// files lists the product's files that may hold data inside box. A file
	// that turns out not to exist upstream is dropped later.
	files func(ctx context.Context, s *Service, box elevation.BBox) ([]string, error)
}

func (s *source) covers(b elevation.BBox) bool {
	for _, c := range s.def.Coverage {
		if c.Intersects(b) {
			return true
		}
	}
	return false
}

func buildSources(u URLs) []*source {
	var out []*source
	for i, def := range elevation.KnownSources {
		src := &source{def: def, index: uint8(i + 1)}
		switch def.ID {
		case "hrdem":
			src.host = hostOf(u.MRDEM) // the mosaics sit beside MRDEM; the index names them
			src.project = canadaAtlas.forward
			src.files = func(ctx context.Context, s *Service, box elevation.BBox) ([]string, error) {
				items, err := s.hrdemIndex(ctx, u.HRDEMIndex)
				if err != nil {
					return nil, err
				}
				var urls []string
				for _, it := range items {
					if it.BBox.Intersects(box) {
						urls = append(urls, it.URL)
					}
				}
				return urls, nil
			}
		case "3dep13":
			src.host = hostOf(u.ThreeDEP)
			src.files = func(_ context.Context, _ *Service, box elevation.BBox) ([]string, error) {
				var urls []string
				eachCell(box, func(lon, lat int) {
					if lon < 0 { // 3DEP is western hemisphere only, named by north-west corner
						n := fmt.Sprintf("n%02dw%03d", lat+1, -lon)
						urls = append(urls, fmt.Sprintf(u.ThreeDEP, n, n))
					}
				})
				return urls, nil
			}
		case "mrdem":
			src.host = hostOf(u.MRDEM)
			src.project = canadaAtlas.forward
			src.files = func(context.Context, *Service, elevation.BBox) ([]string, error) {
				return []string{u.MRDEM}, nil
			}
		case "glo30":
			src.host = hostOf(u.GLO30)
			src.files = func(_ context.Context, _ *Service, box elevation.BBox) ([]string, error) {
				var urls []string
				eachCell(box, func(lon, lat int) {
					ns, ew := "N", "E"
					if lat < 0 {
						ns = "S"
					}
					if lon < 0 {
						ew = "W"
					}
					n := fmt.Sprintf("Copernicus_DSM_COG_10_%s%02d_00_%s%03d_00_DEM", ns, absInt(lat), ew, absInt(lon))
					urls = append(urls, fmt.Sprintf(u.GLO30, n))
				})
				return urls, nil
			}
		default:
			continue
		}
		out = append(out, src)
	}
	return out
}

// hostOf returns the host of a URL template (which url.Parse would refuse for its %s).
func hostOf(raw string) string {
	if _, rest, ok := strings.Cut(raw, "://"); ok {
		raw = rest
	}
	host, _, _ := strings.Cut(raw, "/")
	return host
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// eachCell calls fn with the south-west corner of every 1° cell that box
// touches, widened by a hair so a sample on a cell edge finds both
// neighbours.
func eachCell(box elevation.BBox, fn func(lon, lat int)) {
	const pad = 0.002
	for lat := int(math.Floor(box[1] - pad)); lat <= int(math.Floor(box[3]+pad)); lat++ {
		if lat < -90 || lat > 89 {
			continue
		}
		for lon := int(math.Floor(box[0] - pad)); lon <= int(math.Floor(box[2]+pad)); lon++ {
			if lon < -180 || lon > 179 {
				continue
			}
			fn(lon, lat)
		}
	}
}

// indexItem is one HRDEM mosaic.
type indexItem struct {
	ID   string         `json:"id"`
	BBox elevation.BBox `json:"bbox"`
	URL  string         `json:"url"`
}

// fetchHRDEMIndex lists the HRDEM mosaics from NRCan's STAC API (a few dozen items).
func fetchHRDEMIndex(ctx context.Context, up *Upstream, base string) ([]indexItem, error) {
	next := base + "?collections=" + url.QueryEscape("hrdem-mosaic-2m") + "&limit=100"
	var items []indexItem
	for page := 0; next != "" && page < 50; page++ {
		raw, err := up.Get(ctx, next, 32<<20)
		if err != nil {
			return nil, err
		}
		var body struct {
			Features []struct {
				ID     string    `json:"id"`
				BBox   []float64 `json:"bbox"`
				Assets map[string]struct {
					Href string `json:"href"`
				} `json:"assets"`
			} `json:"features"`
			Links []struct {
				Rel  string `json:"rel"`
				Href string `json:"href"`
			} `json:"links"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("%w: HRDEM index: %w", ErrUpstream, err)
		}
		for _, ft := range body.Features {
			a, ok := ft.Assets["dtm"]
			if !ok || len(ft.BBox) < 4 {
				continue
			}
			items = append(items, indexItem{ID: ft.ID, BBox: elevation.BBox{ft.BBox[0], ft.BBox[1], ft.BBox[2], ft.BBox[3]}, URL: a.Href})
		}
		next = ""
		for _, l := range body.Links {
			if l.Rel == "next" {
				next = l.Href
			}
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: HRDEM index is empty", ErrUpstream)
	}
	return items, nil
}
