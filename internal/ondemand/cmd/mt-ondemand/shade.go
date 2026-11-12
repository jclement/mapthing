package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"sync"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
)

// shade renders nx×ny on-demand tiles starting at z/x0/y0 as one hillshade
// image, with each pixel's source tinted, to look for seams between tiles,
// files and sources by eye.
func shade(ctx context.Context, z, x0, y0, nx, ny int, out string) error {
	s := ondemand.New(ondemand.Options{})
	const n = elevation.TileSize
	w, h := nx*n, ny*n
	elev := make([]float32, w*h)
	src := make([]uint8, w*h)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ferr error
	sem := make(chan struct{}, 6)
	for ty := 0; ty < ny; ty++ {
		for tx := 0; tx < nx; tx++ {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				r, err := s.Render(ctx, z, x0+tx, y0+ty)
				if err != nil {
					mu.Lock()
					ferr = err
					mu.Unlock()
					return
				}
				for j := 0; j < n; j++ {
					copy(elev[(ty*n+j)*w+tx*n:], r.Elev[j*n:(j+1)*n])
					copy(src[(ty*n+j)*w+tx*n:], r.Src[j*n:(j+1)*n])
				}
			}()
		}
	}
	wg.Wait()
	if ferr != nil {
		return ferr
	}
	_, lat := elevation.WorldPixelToLonLat(0, float64(y0*n+h/2), z)
	px := elevation.GroundResolution(z, lat)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	at := func(x, y int) float64 {
		x, y = max(0, min(w-1, x)), max(0, min(h-1, y))
		return float64(elev[y*w+x])
	}
	// Light from the north-west, 45° up; relief exaggerated so plains show too.
	const exaggerate = 3.0
	tints := [5][3]float64{{0.6, 0.6, 0.6}, {1, 0.92, 0.92}, {0.92, 1, 0.92}, {0.92, 0.92, 1}, {1, 1, 0.9}}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := at(x, y)
			if math.IsNaN(c) {
				img.Set(x, y, color.RGBA{40, 60, 90, 255})
				continue
			}
			dzdx := (at(x+1, y) - at(x-1, y)) / (2 * px) * exaggerate
			dzdy := (at(x, y+1) - at(x, y-1)) / (2 * px) * exaggerate
			if math.IsNaN(dzdx) {
				dzdx = 0
			}
			if math.IsNaN(dzdy) {
				dzdy = 0
			}
			// surface normal · light
			lx, ly, lz := -0.5, -0.5, 0.7071
			nrm := math.Sqrt(dzdx*dzdx + dzdy*dzdy + 1)
			v := (-dzdx*lx - dzdy*ly + lz) / nrm
			v = math.Max(0, math.Min(1, 0.15+0.85*v))
			t := tints[min(4, int(src[y*w+x]))]
			img.Set(x, y, color.RGBA{uint8(255 * v * t[0]), uint8(255 * v * t[1]), uint8(255 * v * t[2]), 255})
		}
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	req, by, _ := s.Upstream().Counters()
	fmt.Printf("%d×%d tiles at z%d: %d requests, %.1f MB\n", nx, ny, z, req, float64(by)/1e6)
	return png.Encode(f, img)
}
