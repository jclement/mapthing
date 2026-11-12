package ondemand

import (
	"sync/atomic"

	"github.com/jclement/mapthing/internal/elevation"
)

// Zoom range served on demand. Zooms below MinZoom are always built ahead
// (they are small, and a zoomed-out map must not wait on a dozen DEM files per
// tile); SampleZoom is the deepest, as in a default build.
const (
	MinZoom = 9
	MaxZoom = SampleZoom
)

// Area is a region whose terrain is read on demand.
type Area struct {
	ID   string
	BBox elevation.BBox
	// Rings is the outline as lon/lat rings (even-odd rule); empty means the box.
	Rings [][][2]float64
}

// Policy says where on-demand terrain applies.
type Policy struct {
	// Outside: everywhere no built terrain covers, the whole world.
	Outside bool
	// Areas are the regions set to on demand.
	Areas []Area
}

// Enabled reports whether on demand applies anywhere.
func (p Policy) Enabled() bool { return p.Outside || len(p.Areas) > 0 }

// AllowsBox reports whether on demand applies somewhere in a lon/lat box.
func (p Policy) AllowsBox(b elevation.BBox) bool {
	if p.Outside {
		return true
	}
	for _, a := range p.Areas {
		if !a.BBox.Intersects(b) {
			continue
		}
		if len(a.Rings) == 0 || elevation.RingsIntersectBox(a.Rings, b) {
			return true
		}
	}
	return false
}

// AllowsTile reports whether tile z/x/y may be built on demand.
func (p Policy) AllowsTile(z, x, y int) bool {
	if z < MinZoom || z > MaxZoom || !p.Enabled() {
		return false
	}
	w, s, e, n := elevation.TileBounds(z, x, y)
	return p.AllowsBox(elevation.BBox{w, s, e, n})
}

type policyBox struct{ v atomic.Pointer[Policy] }

// SetPolicy replaces where on demand applies. Cached tiles are kept: a region
// switched back to on demand finds them again.
func (s *Service) SetPolicy(p Policy) { s.policy.v.Store(&p) }

// Policy returns the current policy.
func (s *Service) Policy() Policy {
	if p := s.policy.v.Load(); p != nil {
		return *p
	}
	return Policy{}
}
