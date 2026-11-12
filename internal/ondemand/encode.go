package ondemand

import (
	"math"

	"github.com/jclement/mapthing/internal/elevation"
)

const (
	// SampleZoom is the zoom whose tiles answer elevation queries and carry
	// the per-pixel source number: the terrain build's default max zoom.
	SampleZoom = 13
	// quantum is the rounding step at SampleZoom and deeper, doubling per
	// zoom out up to quantumMax: the build's rule (build.TerrainQuantumM), for
	// the same reason (PNG cannot compress sub-decimetre noise).
	quantum    = 0.25
	quantumMax = 8.0
)

// Encode writes the tile as a Terrarium PNG, rounded as the build rounds.
func (r *Rendered) Encode(z int) ([]byte, error) {
	step := quantum
	if z < SampleZoom {
		step = math.Min(quantum*math.Exp2(float64(SampleZoom-z)), quantumMax)
	}
	q := make([]float32, len(r.Elev))
	for i, v := range r.Elev {
		if v != v {
			q[i] = v
			continue
		}
		q[i] = float32(math.Round(float64(v)/step) * step)
	}
	var src []uint8
	if z >= SampleZoom {
		src = r.Src
	}
	return elevation.EncodeTile(q, src)
}
