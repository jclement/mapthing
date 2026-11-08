// Package routing plans routes on the live generation's Valhalla graph and
// exposes Google Routes and OpenRouteService compatible shapes on top.
package routing

import "context"

// Location is a WGS84 waypoint.
type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	// RadiusM lets a waypoint snap to a road up to this far away (0 = engine default).
	RadiusM float64 `json:"radius_m,omitempty"`
}

// Request is a route request.
type Request struct {
	Locations     []Location `json:"locations"`
	Profile       string     `json:"profile"` // "auto" | "truck"
	AvoidUnpaved  bool       `json:"avoid_unpaved,omitempty"`
	AlternatesMax int        `json:"alternates,omitempty"`
}

// Route is a planned route.
type Route struct {
	Geometry         [][2]float64       `json:"-"` // lon,lat
	DistanceM        float64            `json:"distance_m"`
	DurationS        float64            `json:"duration_s"`
	SurfaceBreakdown map[string]float64 `json:"surface_breakdown_m"` // paved/gravel/dirt/unknown → metres
	Legs             []Leg              `json:"legs"`
}

// Leg is one waypoint-to-waypoint part of a route.
type Leg struct {
	DistanceM float64    `json:"distance_m"`
	DurationS float64    `json:"duration_s"`
	Maneuvers []Maneuver `json:"maneuvers,omitempty"`
}

// Maneuver is one turn instruction.
type Maneuver struct {
	Instruction string  `json:"instruction"`
	DistanceM   float64 `json:"distance_m"`
	DurationS   float64 `json:"duration_s"`
	Street      string  `json:"street,omitempty"`
}

// ErrNoRoute and ErrNoRoad distinguish "no path" from "point not near any road".
type Error struct {
	Code string // "no_route" | "no_road" | "unavailable"
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

// Router is implemented by the Valhalla client.
type Router interface {
	Route(ctx context.Context, req Request) (*Route, error)
	Ready(ctx context.Context) bool
}
