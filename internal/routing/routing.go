// Package routing plans routes on the live generation's Valhalla graph and
// exposes Google Routes and OpenRouteService compatible shapes on top.
package routing

import "context"

// Location is a WGS84 waypoint.
type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	// RadiusM is how far from the point a road may be and still be snapped to
	// (0 = engine default, 35 km; negative = unlimited, capped at 200 km).
	RadiusM float64 `json:"radius_m,omitempty"`
}

// Request is a route request.
type Request struct {
	Locations     []Location `json:"locations"`
	Profile       string     `json:"profile"` // "auto" | "truck"
	AvoidUnpaved  bool       `json:"avoid_unpaved,omitempty"`
	AvoidHighways bool       `json:"avoid_highways,omitempty"`
	AvoidTolls    bool       `json:"avoid_tolls,omitempty"`
	AvoidFerries  bool       `json:"avoid_ferries,omitempty"`
	Shortest      bool       `json:"shortest,omitempty"` // minimise distance instead of time
	AlternatesMax int        `json:"alternates,omitempty"`
}

// Surface classes reported in Route.SurfaceBreakdown.
const (
	SurfacePaved   = "paved"
	SurfaceGravel  = "gravel"
	SurfaceDirt    = "dirt"
	SurfaceUnknown = "unknown"
)

// Route is a planned route.
type Route struct {
	Geometry         [][2]float64       `json:"-"` // lon,lat
	DistanceM        float64            `json:"distance_m"`
	DurationS        float64            `json:"duration_s"`
	SurfaceBreakdown map[string]float64 `json:"surface_breakdown"` // paved/gravel/dirt/unknown → metres
	Surfaces         []SurfaceSpan      `json:"-"`
	Legs             []Leg              `json:"legs"`
}

// SurfaceSpan is the surface of Geometry[From..To] (indices into Route.Geometry).
type SurfaceSpan struct {
	From, To int
	Class    string // paved | gravel | dirt | unknown
	Raw      string // the engine's own surface value (e.g. "compacted")
}

// Leg is one waypoint-to-waypoint part of a route.
type Leg struct {
	DistanceM float64    `json:"distance_m"`
	DurationS float64    `json:"duration_s"`
	Maneuvers []Maneuver `json:"maneuvers,omitempty"`
	// From and To index Route.Geometry: the leg is Geometry[From..To].
	From int `json:"-"`
	To   int `json:"-"`
}

// Maneuver is one turn instruction.
type Maneuver struct {
	Instruction string  `json:"instruction"`
	DistanceM   float64 `json:"distance_m"`
	DurationS   float64 `json:"duration_s"`
	Street      string  `json:"street,omitempty"`
	// Type is the Valhalla maneuver type; From/To index Route.Geometry.
	Type int `json:"type"`
	From int `json:"-"`
	To   int `json:"-"`
}

// Error is a routing failure the API can explain to the caller.
type Error struct {
	Code  string // see the Code* constants
	Msg   string
	Index int // the location that caused it, or -1 when unknown
}

// Error codes.
const (
	CodeNoRoute     = "no_route"    // every point snapped, but no path joins them
	CodeNoRoad      = "no_road"     // a point is not within its radius of any road
	CodeLimit       = "limit"       // the request exceeds a service limit
	CodeBadRequest  = "bad_request" // the request is malformed
	CodeUnavailable = "unavailable" // the engine is down or failed
)

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

// Router is implemented by the Valhalla client.
type Router interface {
	Route(ctx context.Context, req Request) (*Route, error)
	Ready(ctx context.Context) bool
}
