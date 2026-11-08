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
}

// ProfileOptions controls path sampling.
type ProfileOptions struct {
	IntervalM float64 // sample spacing along the path; 0 = default (30 m)
	Samples   int     // if > 0, exactly this many evenly spaced samples (Google semantics)
	// HysteresisM is the threshold for the smoothed gain/loss: a climb or
	// descent only counts once it exceeds this many metres. 0 = default (2 m);
	// negative = no smoothing (smoothed equals raw).
	HysteresisM float64
}

// Profile is an elevation profile along a path.
type Profile struct {
	Samples []Sample `json:"samples"`
	LengthM float64  `json:"length_m"`
	// GainM and LossM are smoothed with ProfileOptions.HysteresisM so DEM
	// noise on flat ground does not add up; GainRawM and LossRawM sum every
	// sample-to-sample change.
	GainM       float64  `json:"gain_m"`
	LossM       float64  `json:"loss_m"`
	GainRawM    float64  `json:"gain_raw_m"`
	LossRawM    float64  `json:"loss_raw_m"`
	HysteresisM float64  `json:"hysteresis_m"`
	MinM        float64  `json:"min_m"`
	MaxM        float64  `json:"max_m"`
	Sources     []string `json:"sources"`
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
