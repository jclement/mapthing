package ondemand

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/jclement/mapthing/internal/contours"
	"github.com/jclement/mapthing/internal/elevation"
)

// Contours on demand: where contour lines were not built, a contour tile is
// drawn at request time from on-demand terrain, with the build's own
// contouring code, and cached like a terrain tile.
//
// One deliberate difference from built contours: the build contours every
// zoom from the deepest terrain (z13); on demand a tile is contoured from
// terrain one or two zooms deeper than itself (ContourTerrainZoom). A z10
// tile then needs 36 terrain tiles instead of 100, so a first view takes
// seconds, not most of a minute. The price is that on-demand z10 lines are
// slightly smoother than built ones.

// Contour zooms served on demand: the zooms a default build makes.
const (
	ContourMinZoom = contours.DefaultMinZoom
	ContourMaxZoom = contours.DefaultMaxZoom
)

// contourRev is part of every cached contour tile's version (see renderRev).
const contourRev = 1

// ContourTerrainZoom is the terrain zoom a contour tile is drawn from: two
// zooms deeper, as deep as on-demand terrain goes.
func ContourTerrainZoom(z int) int { return min(z+2, MaxZoom) }

// BuiltTerrain gives the manager the live archive's terrain: tile returns a
// built tile's bytes (false when the archive has none), and generation names
// the live generation. A contour tile at the edge of a built region takes the
// built tiles on that side, so its lines meet the built region's ground.
type BuiltTerrain struct {
	Tile       func(z, x, y int) ([]byte, bool)
	Generation func() string
}

// SetBuiltTerrain tells the manager how to read built terrain.
func (m *Manager) SetBuiltTerrain(b BuiltTerrain) {
	m.builtTerrain.Store(&b)
	m.SetBuilt(func(z, x, y int) bool {
		_, ok := b.Tile(z, x, y)
		return ok
	})
}

// AllowsContour reports whether contour tile z/x/y may be drawn on demand.
func (m *Manager) AllowsContour(z, x, y int) bool {
	if m.opt.ContoursOff || z < ContourMinZoom || z > ContourMaxZoom {
		return false
	}
	w, s, e, n := elevation.TileBounds(z, x, y)
	return m.Svc.Policy().AllowsBox(elevation.BBox{w, s, e, n})
}

func contourKey(z, x, y int) string { return fmt.Sprintf("contour/%d/%d/%d", z, x, y) }

// contourVersion condenses everything a contour tile is made from: the
// release of each source file over its terrain mosaic, and the generation
// whose built tiles may sit in that mosaic.
func (m *Manager) contourVersion(ctx context.Context, z, x, y int) ([8]byte, error) {
	tz := ContourTerrainZoom(z)
	span := 1 << (tz - z)
	w, _, _, n := elevation.TileBounds(tz, x*span-1, y*span-1)
	_, s, e, _ := elevation.TileBounds(tz, (x+1)*span, (y+1)*span)
	files, err := m.Svc.candidates(ctx, elevation.BBox{w, s, e, n})
	if err != nil {
		return [8]byte{}, err
	}
	gen := ""
	if b := m.builtTerrain.Load(); b != nil && b.Generation != nil {
		gen = b.Generation()
	}
	tv := versionOf(files)
	sum := sha256.Sum256(fmt.Appendf(nil, "contours %d %d %x %s", contourRev, tz, tv, gen))
	var v [8]byte
	copy(v[:], sum[:])
	return v, nil
}

// HasContour reports whether the contour tile is cached, current as far as is
// known, without asking the hosts anything.
func (m *Manager) HasContour(z, x, y int) bool {
	ver, err := m.contourVersion(offline(context.Background()), z, x, y)
	if err != nil {
		return false
	}
	_, ok := m.Svc.stored(contourKey(z, x, y), ver)
	return ok
}

// Contour returns contour tile z/x/y as a gzipped vector tile (nil when no
// line crosses it or there is no terrain), from the cache when it was drawn
// from the current terrain, and drawn now otherwise. cold reports whether it
// had to be drawn. The caller decides whether on demand applies (AllowsContour).
func (m *Manager) Contour(ctx context.Context, z, x, y int) (mvt []byte, cold bool, err error) {
	if z < ContourMinZoom || z > ContourMaxZoom || x < 0 || y < 0 || x >= 1<<z || y >= 1<<z {
		return nil, false, fmt.Errorf("ondemand: no such contour tile")
	}
	s := m.Svc
	ver, err := m.contourVersion(ctx, z, x, y)
	if err != nil {
		countFailure(s, err)
		return nil, true, err
	}
	key := contourKey(z, x, y)
	if b, ok := s.stored(key, ver); ok {
		s.n.contourHits.Add(1)
		return b, false, nil
	}
	v, err := s.flight.do(ctx, fmt.Sprintf("%s@%x", key, ver), func(ctx context.Context) (any, error) {
		if b, ok := s.stored(key, ver); ok {
			return b, nil
		}
		tz := ContourTerrainZoom(z)
		policy := s.Policy()
		built := m.builtTerrain.Load()
		// The mosaic's terrain tiles are fetched together first, then read back.
		span := 1 << (tz - z)
		var need [][2]int
		for ty := y*span - 1; ty <= (y+1)*span; ty++ {
			for tx := x*span - 1; tx <= (x+1)*span; tx++ {
				if ty < 0 || ty >= 1<<tz || tx < 0 || tx >= 1<<tz {
					continue
				}
				if built != nil {
					if _, ok := built.Tile(tz, tx, ty); ok {
						continue
					}
				}
				if policy.AllowsTile(tz, tx, ty) {
					need = append(need, [2]int{tx, ty})
				}
			}
		}
		if err := each(ctx, need, func(t [2]int) error {
			_, _, err := s.Tile(ctx, tz, t[0], t[1])
			return err
		}); err != nil {
			return nil, err
		}
		data, err := guard("contours", func() ([]byte, error) {
			return contours.Tile(func(tx, ty int) (*elevation.Tile, error) {
				var png []byte
				if built != nil {
					png, _ = built.Tile(tz, tx, ty)
				}
				if png == nil && policy.AllowsTile(tz, tx, ty) {
					var err error
					if png, _, err = s.Tile(ctx, tz, tx, ty); err != nil {
						return nil, err
					}
				}
				if len(png) == 0 {
					return nil, nil
				}
				return elevation.DecodeTile(png)
			}, tz, z, x, y, contours.Options{MinZoom: ContourMinZoom, MaxZoom: ContourMaxZoom})
		})
		if err != nil {
			return nil, err
		}
		if s.opt.Store != nil {
			_ = s.opt.Store.Put(key, append(ver[:], data...))
		}
		s.n.contoursBuilt.Add(1)
		return data, nil
	})
	if err != nil {
		countFailure(s, err)
		return nil, true, err
	}
	b, _ := v.([]byte)
	return b, true, nil
}
