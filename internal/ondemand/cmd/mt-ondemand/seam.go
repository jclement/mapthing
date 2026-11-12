package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"github.com/jclement/mapthing/internal/contours"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/pmtiles"
)

type memSettings map[string]string

func (m memSettings) Setting(_ context.Context, k string) (string, error) {
	if v, ok := m[k]; ok {
		return v, nil
	}
	return "", os.ErrNotExist
}
func (m memSettings) SetSetting(_ context.Context, k, v string) error { m[k] = v; return nil }

// seam draws the contours on both sides of a built region's edge, to look at
// the join by eye: the z10 tile at lon/lat is "built" (its z13 terrain written
// to an archive, as a region's build leaves it, and contoured with the build's
// code), and its eastern neighbour is drawn on demand. Built lines are blue,
// on-demand lines red; the edge is the grey line. One image per contour zoom.
func seam(ctx context.Context, lon, lat float64, dir, out string) error {
	px, py := elevation.LonLatToWorldPixel(lon, lat, 10)
	bx, by := int(px)/256, int(py)/256
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// 1. The built side: the 8×8 z13 terrain tiles of one z10 tile, plus that z10 tile
	// (the contour build looks for it), read from the sources.
	src := ondemand.New(ondemand.Options{})
	tpath := filepath.Join(dir, "terrain.pmtiles")
	w, err := pmtiles.Create(tpath)
	if err != nil {
		return err
	}
	add := func(z, x, y int, withSrc bool) error {
		r, err := src.Render(ctx, z, x, y)
		if err != nil {
			return err
		}
		if !withSrc {
			r.Src = nil
		}
		data, err := elevation.EncodeTile(r.Elev, r.Src)
		if err != nil {
			return err
		}
		return w.AddTile(uint8(z), uint32(x), uint32(y), data)
	}
	if err := add(10, bx, by, false); err != nil {
		return err
	}
	for ty := by * 8; ty < by*8+8; ty++ {
		for tx := bx * 8; tx < bx*8+8; tx++ {
			if err := add(13, tx, ty, true); err != nil {
				return err
			}
		}
	}
	wst, s, e, n := elevation.TileBounds(10, bx, by)
	meta, _ := json.Marshal(elevation.Metadata{Name: "seam", Format: "png", Encoding: "terrarium", TileSize: 256, MaxZoom: 13})
	if err := w.Commit(pmtiles.Header{TileType: pmtiles.TypePNG, TileCompression: pmtiles.CompressionNone, MinZoom: 10, MaxZoom: 13,
		MinLonE7: int32(wst * 1e7), MinLatE7: int32(s * 1e7), MaxLonE7: int32(e * 1e7), MaxLatE7: int32(n * 1e7)}, meta); err != nil {
		return err
	}
	cpath := filepath.Join(dir, "contours.pmtiles")
	if _, err := contours.Build(ctx, tpath, cpath, contours.Options{}); err != nil {
		return err
	}
	terrain, err := pmtiles.Open(tpath)
	if err != nil {
		return err
	}
	built, err := pmtiles.Open(cpath)
	if err != nil {
		return err
	}
	// 2. The on-demand side, with the built terrain known to it.
	m, err := ondemand.NewManager(ctx, ondemand.ManagerOptions{DataDir: dir, Settings: memSettings{}})
	if err != nil {
		return err
	}
	m.Svc.SetPolicy(ondemand.Policy{Outside: true})
	m.SetBuiltTerrain(ondemand.BuiltTerrain{Generation: func() string { return "seam" }, Tile: func(z, x, y int) ([]byte, bool) {
		data, ok, err := terrain.Get(uint8(z), uint32(x), uint32(y))
		return data, err == nil && ok && len(data) > 0
	}})
	// 3. Draw each zoom: the two eastern columns of the built tile and the two western
	// columns of its neighbour (at z10, the two tiles themselves).
	for z := 10; z <= 12; z++ {
		k := 1 << (z - 10)
		cols, rows := min(2*k, 4), min(k, 3)
		x0, y0 := (bx+1)*k-cols/2, by*k+(k-rows)/2
		const T = 384
		img := image.NewRGBA(image.Rect(0, 0, cols*T, rows*T))
		for i := range img.Pix {
			img.Pix[i] = 255
		}
		nb, nd := 0, 0
		for ty := 0; ty < rows; ty++ {
			for tx := 0; tx < cols; tx++ {
				x, y := x0+tx, y0+ty
				var gz []byte
				col := color.RGBA{200, 30, 30, 255}
				if x < (bx+1)*k {
					gz, _, _ = built.Get(uint8(z), uint32(x), uint32(y))
					col = color.RGBA{30, 60, 200, 255}
				} else if gz, _, err = m.Contour(ctx, z, x, y); err != nil {
					return err
				}
				if len(gz) == 0 {
					continue
				}
				raw, err := pmtiles.Decompress(gz, pmtiles.CompressionGzip)
				if err != nil {
					return err
				}
				layers, err := contours.Decode(raw)
				if err != nil {
					return err
				}
				for _, l := range layers {
					for _, f := range l.Features {
						c := col
						if major, _ := f.Props["major"].(bool); !major {
							c.A = 255
							c.R, c.G, c.B = c.R+(255-c.R)/3, c.G+(255-c.G)/3, c.B+(255-c.B)/3
						}
						for _, ln := range f.Lines {
							if x < (bx+1)*k {
								nb++
							} else {
								nd++
							}
							for i := 1; i < len(ln); i++ {
								line(img, tx*T+int(ln[i-1][0])*T/contours.Extent, ty*T+int(ln[i-1][1])*T/contours.Extent,
									tx*T+int(ln[i][0])*T/contours.Extent, ty*T+int(ln[i][1])*T/contours.Extent, c)
							}
						}
					}
				}
			}
		}
		for yy := 0; yy < rows*T; yy += 3 { // the edge of the built region
			img.Set(cols/2*T, yy, color.RGBA{120, 120, 120, 255})
		}
		f, err := os.Create(fmt.Sprintf("%s-z%d.png", out, z))
		if err != nil {
			return err
		}
		if err := png.Encode(f, img); err != nil {
			return err
		}
		_ = f.Close()
		fmt.Printf("z%d: %d built lines (blue, from z13 terrain), %d on-demand lines (red, from z%d terrain)\n", z, nb, nd, ondemand.ContourTerrainZoom(z))
	}
	req, bytes, _ := m.Svc.Upstream().Counters()
	fmt.Printf("on-demand side: %d requests, %.1f MB\n", req, float64(bytes)/1e6)
	return nil
}

func line(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx, dy := x1-x0, y1-y0
	steps := max(max(dx, -dx), max(dy, -dy), 1)
	for i := 0; i <= steps; i++ {
		img.SetRGBA(x0+dx*i/steps, y0+dy*i/steps, c)
	}
}

// contourCost times cold contour tiles at lon/lat for each zoom, each from an empty cache,
// then a neighbour (which shares terrain with it).
func contourCost(ctx context.Context, lon, lat float64, dir string) error {
	fmt.Println("| contour tile | terrain tiles it reads | cold: s, MB read, KB | its eastern neighbour after it: s, MB read |")
	fmt.Println("|---|---|---|---|")
	for z := ondemand.ContourMinZoom; z <= ondemand.ContourMaxZoom; z++ {
		m, err := ondemand.NewManager(ctx, ondemand.ManagerOptions{DataDir: filepath.Join(dir, fmt.Sprint("c", z)), Settings: memSettings{}})
		if err != nil {
			return err
		}
		m.Svc.SetPolicy(ondemand.Policy{Outside: true})
		px, py := elevation.LonLatToWorldPixel(lon, lat, z)
		x, y := int(px)/256, int(py)/256
		_, _ = m.Svc.Render(ctx, 13, x<<(13-z)+40, y<<(13-z)) // open connections
		_, b0, _ := m.Svc.Upstream().Counters()
		t0 := time.Now()
		mvt, _, err := m.Contour(ctx, z, x, y)
		if err != nil {
			return err
		}
		d1 := time.Since(t0)
		_, b1, _ := m.Svc.Upstream().Counters()
		t0 = time.Now()
		if _, _, err := m.Contour(ctx, z, x+1, y); err != nil {
			return err
		}
		d2 := time.Since(t0)
		_, b2, _ := m.Svc.Upstream().Counters()
		span := 1<<(ondemand.ContourTerrainZoom(z)-z) + 2
		fmt.Printf("| z%d | %d at z%d | %.1f s, %.1f MB, %.0f KB | %.1f s, %.1f MB |\n", z, span*span, ondemand.ContourTerrainZoom(z),
			d1.Seconds(), float64(b1-b0)/1e6, float64(len(mvt))/1e3, d2.Seconds(), float64(b2-b1)/1e6)
	}
	return nil
}
