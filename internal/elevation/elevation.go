// Package elevation answers elevation queries for points and paths from the
// terrain tiles of the live generation.
package elevation

import "context"

// LonLat is a WGS84 coordinate, longitude first (GeoJSON order).
type LonLat [2]float64

// Sample is one elevation answer.
type Sample struct {
	Lon        float64  `json:"lon"`
	Lat        float64  `json:"lat"`
	Elevation  *float64 `json:"elevation_m"` // nil when no data covers the point
	Source     string   `json:"source,omitempty"`
	Resolution float64  `json:"resolution_m,omitempty"`
	DistanceM  float64  `json:"distance_m,omitempty"` // along-path distance (profiles only)
	// Structure is "bridge" or "tunnel" when a deck-mode profile interpolated
	// this sample across a structure (the DEM value is then not the road level).
	Structure string `json:"structure,omitempty"`
}

// Bridge modes for profiles.
const (
	// BridgesGround samples the terrain everywhere (default). Right for buried
	// pipe, which goes under a creek or canal rather than over it.
	BridgesGround = "ground"
	// BridgesDeck interpolates linearly across bridges and tunnels the path
	// map-matches onto, approximating the road level a vehicle follows.
	BridgesDeck = "deck"
)

// StructureSpan is one stretch of a deck-mode profile that was interpolated.
type StructureSpan struct {
	StartM float64 `json:"start_m"` // along-path distance of the first structure sample
	EndM   float64 `json:"end_m"`   // ... and of the last
	Kind   string  `json:"kind"`    // bridge | tunnel
}

// StructureFinder reports, for points along a road path, which lie on a
// bridge or tunnel ("" = ground or not matched), and the fraction of points it
// could match. routing.Client implements it with Valhalla trace_attributes.
type StructureFinder interface {
	Structures(ctx context.Context, pts [][2]float64) (kinds []string, matched float64, err error)
}

// ProfileOptions controls path sampling.
type ProfileOptions struct {
	IntervalM float64 // sample spacing along the path; 0 = default (30 m)
	Samples   int     // if > 0, exactly this many evenly spaced samples (Google semantics)
	// SmoothWindowM is the width of the running-median filter applied to the
	// elevation series before gain/loss (by along-path distance, centred).
	// It removes narrow features a vehicle does not climb: bridge and culvert
	// dips in a bare-earth DEM, embankment edges, single-pixel noise.
	// 0 = default (DefaultSmoothWindowM); negative = off.
	SmoothWindowM float64
	// HysteresisM is the threshold applied after the median: a climb or
	// descent only counts once it exceeds this many metres. 0 = default (2 m);
	// negative = off.
	HysteresisM float64
	// Bridges is BridgesGround ("" = ground) or BridgesDeck.
	Bridges string
}

// Profile is an elevation profile along a path.
type Profile struct {
	Samples []Sample `json:"samples"`
	LengthM float64  `json:"length_m"`
	// GainM and LossM are computed from the elevation series after a running
	// median over SmoothWindowM metres and then HysteresisM hysteresis, so
	// DEM noise and bridge dips do not add up; GainRawM and LossRawM sum
	// every sample-to-sample change of the unsmoothed series. Samples always
	// carry the unsmoothed elevations.
	GainM         float64 `json:"gain_m"`
	LossM         float64 `json:"loss_m"`
	GainRawM      float64 `json:"gain_raw_m"`
	LossRawM      float64 `json:"loss_raw_m"`
	SmoothWindowM float64 `json:"smooth_window_m"`
	HysteresisM   float64 `json:"hysteresis_m"`
	// Bridges echoes the mode used; StructureSpans lists the interpolated
	// stretches (deck only); Warning explains a deck request that fell back to
	// ground.
	Bridges        string          `json:"bridges"`
	StructureSpans []StructureSpan `json:"structure_spans,omitempty"`
	Warning        string          `json:"warning,omitempty"`
	MinM           float64         `json:"min_m"`
	MaxM           float64         `json:"max_m"`
	Sources        []string        `json:"sources"`
}

// SourceInfo describes one DEM source available in the live generation.
type SourceInfo struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ResolutionM float64 `json:"resolution_m"`
	License     string  `json:"license"`
	Attribution string  `json:"attribution"`
}

// Service is implemented by the terrain-tile reader.
type Service interface {
	Points(ctx context.Context, pts []LonLat) ([]Sample, error)
	Profile(ctx context.Context, path []LonLat, opt ProfileOptions) (*Profile, error)
	Sources() []SourceInfo
}
