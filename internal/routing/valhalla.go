package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultURL is where the supervised valhalla_service listens (docker/valhalla/overrides.json).
const DefaultURL = "http://127.0.0.1:8002"

// URLFromEnv returns MAPTHING_VALHALLA_URL, or DefaultURL.
func URLFromEnv() string {
	if v := os.Getenv("MAPTHING_VALHALLA_URL"); v != "" {
		return v
	}
	return DefaultURL
}

// Default costing behaviour. Tracks (farm and lease roads) stay usable with a moderate
// penalty so remote sites are reachable; avoid_unpaved removes them except at the ends.
const (
	defaultUseTracks = 0.3
	unlimitedRadiusM = 200000 // what a negative radius means
)

// Client talks to valhalla_service over HTTP. It implements Router.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Log     *slog.Logger
}

// New returns a client for the Valhalla service at baseURL.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 130 * time.Second},
		Log:     slog.Default(),
	}
}

var _ Router = (*Client)(nil)

// Ready reports whether Valhalla answers /status.
func (c *Client) Ready(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/status", nil)
	if err != nil {
		return false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ---- wire types -----------------------------------------------------------

type vLocation struct {
	Lat          float64 `json:"lat"`
	Lon          float64 `json:"lon"`
	Type         string  `json:"type,omitempty"`
	SearchCutoff float64 `json:"search_cutoff,omitempty"`
}

type vError struct {
	ErrorCode  int    `json:"error_code"`
	Error      string `json:"error"`
	StatusCode int    `json:"status_code"`
}

type vSummary struct {
	Length float64 `json:"length"` // km
	Time   float64 `json:"time"`   // s
}

type vManeuver struct {
	Type             int      `json:"type"`
	Instruction      string   `json:"instruction"`
	StreetNames      []string `json:"street_names"`
	Length           float64  `json:"length"`
	Time             float64  `json:"time"`
	BeginShapeIndex  int      `json:"begin_shape_index"`
	EndShapeIndex    int      `json:"end_shape_index"`
	BeginStreetNames []string `json:"begin_street_names"`
}

type vLeg struct {
	Summary   vSummary    `json:"summary"`
	Maneuvers []vManeuver `json:"maneuvers"`
	Shape     string      `json:"shape"`
}

type vTrip struct {
	Legs    []vLeg   `json:"legs"`
	Summary vSummary `json:"summary"`
}

type vRouteResponse struct {
	Trip       vTrip `json:"trip"`
	Alternates []struct {
		Trip vTrip `json:"trip"`
	} `json:"alternates"`
}

type vTraceResponse struct {
	Edges []struct {
		Surface         string `json:"surface"`
		BeginShapeIndex int    `json:"begin_shape_index"`
		EndShapeIndex   int    `json:"end_shape_index"`
	} `json:"edges"`
}

// ---- transport ------------------------------------------------------------

// post sends body to /action and decodes a 200 answer into out.
func (c *Client) post(ctx context.Context, action string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+action, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Code: CodeUnavailable, Msg: "routing engine unreachable", Index: -1}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return &Error{Code: CodeUnavailable, Msg: "reading engine response: " + err.Error(), Index: -1}
	}
	if resp.StatusCode != http.StatusOK {
		return valhallaError(resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Code: CodeUnavailable, Msg: "decoding engine response: " + err.Error(), Index: -1}
	}
	return nil
}

// valhallaError maps a Valhalla error body to *Error.
func valhallaError(status int, body []byte) *Error {
	var ve vError
	if json.Unmarshal(body, &ve) != nil || ve.ErrorCode == 0 {
		return &Error{Code: CodeUnavailable, Msg: fmt.Sprintf("engine returned HTTP %d", status), Index: -1}
	}
	e := &Error{Msg: ve.Error, Index: -1}
	switch c := ve.ErrorCode; {
	case c == 171:
		e.Code = CodeNoRoad
	case c == 170 || c == 442 || c == 443:
		e.Code = CodeNoRoute
	case c >= 150 && c <= 169:
		e.Code = CodeLimit
	case status >= 500:
		e.Code = CodeUnavailable
	default:
		e.Code = CodeBadRequest
	}
	return e
}

// Raw forwards a request to Valhalla unchanged (the pass-through endpoint).
func (c *Client) Raw(ctx context.Context, method, action string, query url.Values, body io.Reader) (*http.Response, error) {
	u := c.BaseURL + "/" + action
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(req)
}

// ---- requests -------------------------------------------------------------

func costingName(profile string) (string, error) {
	switch profile {
	case "", "auto", "car", "driving-car":
		return "auto", nil
	case "truck", "hgv", "driving-hgv":
		return "truck", nil
	}
	return "", &Error{Code: CodeBadRequest, Msg: fmt.Sprintf("unknown profile %q (auto or truck)", profile), Index: -1}
}

func toVLocations(locs []Location, typ string) []vLocation {
	out := make([]vLocation, len(locs))
	for i, l := range locs {
		out[i] = vLocation{Lat: l.Lat, Lon: l.Lon, Type: typ}
		switch {
		case l.RadiusM > 0:
			out[i].SearchCutoff = l.RadiusM
		case l.RadiusM < 0:
			out[i].SearchCutoff = unlimitedRadiusM
		}
	}
	return out
}

// costingOptions builds Valhalla costing_options for a request.
func costingOptions(costing string, r Request) map[string]any {
	o := map[string]any{"use_tracks": defaultUseTracks}
	if r.AvoidUnpaved {
		o["use_tracks"] = 0.0
		o["exclude_unpaved"] = true
	}
	if r.AvoidHighways {
		o["use_highways"] = 0.0
	}
	if r.AvoidTolls {
		o["use_tolls"] = 0.0
	}
	if r.AvoidFerries {
		o["use_ferry"] = 0.0
	}
	if r.Shortest {
		o["shortest"] = true
	}
	return map[string]any{costing: o}
}

func validate(locs []Location, min int) error {
	if len(locs) < min {
		return &Error{Code: CodeBadRequest, Msg: fmt.Sprintf("need at least %d locations", min), Index: -1}
	}
	for i, l := range locs {
		if l.Lat < -90 || l.Lat > 90 || l.Lon < -180 || l.Lon > 180 {
			return &Error{Code: CodeBadRequest, Msg: fmt.Sprintf("location %d is not a valid lat/lon", i), Index: i}
		}
	}
	return nil
}

// Route plans the best route. It implements Router.
func (c *Client) Route(ctx context.Context, r Request) (*Route, error) {
	rs, err := c.Routes(ctx, r)
	if err != nil {
		return nil, err
	}
	return rs[0], nil
}

// Routes plans the best route and up to r.AlternatesMax alternatives.
func (c *Client) Routes(ctx context.Context, r Request) ([]*Route, error) {
	if err := validate(r.Locations, 2); err != nil {
		return nil, err
	}
	costing, err := costingName(r.Profile)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"locations":          toVLocations(r.Locations, "break"),
		"costing":            costing,
		"costing_options":    costingOptions(costing, r),
		"directions_options": map[string]any{"units": "kilometers"},
	}
	if r.AlternatesMax > 0 && len(r.Locations) == 2 { // Valhalla: alternates only for 2 points
		body["alternates"] = r.AlternatesMax
	}
	var vr vRouteResponse
	if err := c.post(ctx, "route", body, &vr); err != nil {
		return nil, c.locateFailure(ctx, err, r.Locations, costing)
	}
	trips := []vTrip{vr.Trip}
	for _, a := range vr.Alternates {
		trips = append(trips, a.Trip)
	}
	routes := make([]*Route, 0, len(trips))
	for _, t := range trips {
		rt, err := tripToRoute(t)
		if err != nil {
			return nil, err
		}
		c.addSurfaces(ctx, rt, costing)
		routes = append(routes, rt)
	}
	return routes, nil
}

// locateFailure fills in which location could not be snapped for a no_road error.
func (c *Client) locateFailure(ctx context.Context, err error, locs []Location, costing string) error {
	var re *Error
	if !errors.As(err, &re) || re.Code != CodeNoRoad {
		return err
	}
	snaps, lerr := c.Locate(ctx, locs, costing)
	if lerr != nil {
		return err
	}
	for i, s := range snaps {
		if s == nil {
			re.Index = i
			break
		}
	}
	return re
}

// tripToRoute converts a Valhalla trip, concatenating the leg shapes into one geometry.
func tripToRoute(t vTrip) (*Route, error) {
	rt := &Route{
		DistanceM: t.Summary.Length * 1000,
		DurationS: t.Summary.Time,
	}
	for _, l := range t.Legs {
		pts, err := DecodePolyline(l.Shape, 6)
		if err != nil {
			return nil, &Error{Code: CodeUnavailable, Msg: "bad shape from engine: " + err.Error(), Index: -1}
		}
		offset := 0
		if len(rt.Geometry) > 0 && len(pts) > 0 {
			// The leg starts where the previous one ended: share that vertex.
			offset = len(rt.Geometry) - 1
			pts = pts[1:]
		}
		rt.Geometry = append(rt.Geometry, pts...)
		leg := Leg{
			DistanceM: l.Summary.Length * 1000,
			DurationS: l.Summary.Time,
			From:      offset,
			To:        len(rt.Geometry) - 1,
		}
		for _, m := range l.Maneuvers {
			street := ""
			if len(m.StreetNames) > 0 {
				street = m.StreetNames[0]
			} else if len(m.BeginStreetNames) > 0 {
				street = m.BeginStreetNames[0]
			}
			leg.Maneuvers = append(leg.Maneuvers, Maneuver{
				Instruction: m.Instruction,
				DistanceM:   m.Length * 1000,
				DurationS:   m.Time,
				Street:      street,
				Type:        m.Type,
				From:        offset + m.BeginShapeIndex,
				To:          offset + m.EndShapeIndex,
			})
		}
		rt.Legs = append(rt.Legs, leg)
	}
	return rt, nil
}

// SurfaceClass maps a Valhalla edge surface to paved | gravel | dirt | unknown.
func SurfaceClass(s string) string {
	switch s {
	case "paved_smooth", "paved", "paved_rough":
		return SurfacePaved
	case "compacted", "gravel":
		return SurfaceGravel
	case "dirt", "path":
		return SurfaceDirt
	}
	return SurfaceUnknown
}

// addSurfaces walks the route's own shape with trace_attributes to learn the surface of
// every edge, then fills Surfaces and SurfaceBreakdown. A failure leaves the whole
// distance as unknown rather than failing the route.
func (c *Client) addSurfaces(ctx context.Context, rt *Route, costing string) {
	rt.SurfaceBreakdown = map[string]float64{SurfaceUnknown: rt.DistanceM}
	rt.Surfaces = []SurfaceSpan{{From: 0, To: len(rt.Geometry) - 1, Class: SurfaceUnknown}}
	if len(rt.Geometry) < 2 {
		return
	}
	body := map[string]any{
		"encoded_polyline": EncodePolyline(rt.Geometry, 6),
		"shape_match":      "edge_walk",
		"costing":          costing,
		"filters": map[string]any{
			"attributes": []string{"edge.surface", "edge.begin_shape_index", "edge.end_shape_index"},
			"action":     "include",
		},
	}
	var tr vTraceResponse
	if err := c.post(ctx, "trace_attributes", body, &tr); err != nil || len(tr.Edges) == 0 {
		c.Log.Debug("routing: surface lookup failed; reporting unknown", "err", err)
		return
	}
	applySurfaces(rt, tr)
}

func applySurfaces(rt *Route, tr vTraceResponse) {
	spans := make([]SurfaceSpan, 0, len(tr.Edges))
	raw := map[string]float64{}
	var total float64
	for _, e := range tr.Edges {
		cls := SurfaceClass(e.Surface)
		if n := len(spans); n > 0 && spans[n-1].Raw == e.Surface && spans[n-1].To == e.BeginShapeIndex {
			spans[n-1].To = e.EndShapeIndex
		} else {
			spans = append(spans, SurfaceSpan{From: e.BeginShapeIndex, To: e.EndShapeIndex, Class: cls, Raw: e.Surface})
		}
		d := pathLength(rt.Geometry, e.BeginShapeIndex, e.EndShapeIndex)
		raw[cls] += d
		total += d
	}
	if total <= 0 {
		return
	}
	// Scale the shape-measured lengths to the engine's reported distance.
	k := rt.DistanceM / total
	rt.SurfaceBreakdown = map[string]float64{}
	for cls, d := range raw {
		rt.SurfaceBreakdown[cls] = round1(d * k)
	}
	rt.Surfaces = spans
}

func round1(v float64) float64 {
	if v < 0 {
		return -round1(-v)
	}
	return float64(int64(v*10+0.5)) / 10
}

// ---- locate / matrix / isochrone -------------------------------------------

// Snap is where a location snapped to the road network.
type Snap struct {
	Lon, Lat  float64
	DistanceM float64
	Name      string
}

type vLocateResult struct {
	Edges []struct {
		CorrelatedLat float64 `json:"correlated_lat"`
		CorrelatedLon float64 `json:"correlated_lon"`
		Distance      float64 `json:"distance"`
		EdgeInfo      struct {
			Names []string `json:"names"`
		} `json:"edge_info"`
	} `json:"edges"`
}

// Locate snaps each location to the nearest routable edge within its radius.
// A nil entry means nothing routable was found.
func (c *Client) Locate(ctx context.Context, locs []Location, costing string) ([]*Snap, error) {
	if err := validate(locs, 1); err != nil {
		return nil, err
	}
	body := map[string]any{"locations": toVLocations(locs, ""), "costing": costing, "verbose": true}
	var res []vLocateResult
	if err := c.post(ctx, "locate", body, &res); err != nil {
		return nil, err
	}
	out := make([]*Snap, len(locs))
	for i := range locs {
		if i >= len(res) || len(res[i].Edges) == 0 {
			continue
		}
		best := res[i].Edges[0]
		for _, e := range res[i].Edges[1:] {
			if e.Distance < best.Distance {
				best = e
			}
		}
		s := &Snap{Lon: best.CorrelatedLon, Lat: best.CorrelatedLat}
		s.DistanceM = haversine([2]float64{locs[i].Lon, locs[i].Lat}, [2]float64{s.Lon, s.Lat})
		if r := locs[i].RadiusM; r > 0 && s.DistanceM > r {
			continue
		}
		for _, e := range res[i].Edges {
			if len(e.EdgeInfo.Names) > 0 {
				s.Name = e.EdgeInfo.Names[0]
				break
			}
		}
		out[i] = s
	}
	return out, nil
}

// Matrix is a many-to-many table. Nil cells are unreachable.
type Matrix struct {
	Distances [][]*float64 // metres
	Durations [][]*float64 // seconds
}

// Matrix computes distances and durations from every source to every target.
func (c *Client) Matrix(ctx context.Context, sources, targets []Location, profile string) (*Matrix, error) {
	if err := validate(sources, 1); err != nil {
		return nil, err
	}
	if err := validate(targets, 1); err != nil {
		return nil, err
	}
	costing, err := costingName(profile)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"sources":         toVLocations(sources, ""),
		"targets":         toVLocations(targets, ""),
		"costing":         costing,
		"costing_options": costingOptions(costing, Request{}),
		"units":           "kilometers",
	}
	var res struct {
		SourcesToTargets [][]struct {
			FromIndex int      `json:"from_index"`
			ToIndex   int      `json:"to_index"`
			Time      *float64 `json:"time"`
			Distance  *float64 `json:"distance"`
		} `json:"sources_to_targets"`
	}
	if err := c.post(ctx, "sources_to_targets", body, &res); err != nil {
		return nil, err
	}
	m := &Matrix{Distances: make([][]*float64, len(sources)), Durations: make([][]*float64, len(sources))}
	for i := range sources {
		m.Distances[i] = make([]*float64, len(targets))
		m.Durations[i] = make([]*float64, len(targets))
	}
	for _, row := range res.SourcesToTargets {
		for _, cell := range row {
			if cell.FromIndex < 0 || cell.FromIndex >= len(sources) || cell.ToIndex < 0 || cell.ToIndex >= len(targets) {
				continue
			}
			if cell.Distance != nil {
				d := *cell.Distance * 1000
				m.Distances[cell.FromIndex][cell.ToIndex] = &d
			}
			if cell.Time != nil {
				t := *cell.Time
				m.Durations[cell.FromIndex][cell.ToIndex] = &t
			}
		}
	}
	return m, nil
}

// Contour is one isochrone polygon.
type Contour struct {
	Value    float64         // seconds or metres, as asked
	Geometry json.RawMessage // GeoJSON Polygon or MultiPolygon
}

// Isochrone computes reachability polygons around one location. Values are seconds when
// byDistance is false, metres otherwise. Contours come back in ascending value order.
func (c *Client) Isochrone(ctx context.Context, loc Location, profile string, values []float64, byDistance, reverse bool) ([]Contour, error) {
	if err := validate([]Location{loc}, 1); err != nil {
		return nil, err
	}
	costing, err := costingName(profile)
	if err != nil {
		return nil, err
	}
	contours := make([]map[string]float64, len(values))
	for i, v := range values {
		if byDistance {
			contours[i] = map[string]float64{"distance": v / 1000}
		} else {
			contours[i] = map[string]float64{"time": v / 60}
		}
	}
	body := map[string]any{
		"locations":       toVLocations([]Location{loc}, ""),
		"costing":         costing,
		"costing_options": costingOptions(costing, Request{}),
		"contours":        contours,
		"polygons":        true,
		"reverse":         reverse,
	}
	var res struct {
		Features []struct {
			Properties struct {
				Contour float64 `json:"contour"`
			} `json:"properties"`
			Geometry json.RawMessage `json:"geometry"`
		} `json:"features"`
	}
	if err := c.post(ctx, "isochrone", body, &res); err != nil {
		return nil, err
	}
	out := make([]Contour, 0, len(res.Features))
	for _, f := range res.Features {
		v := f.Properties.Contour * 60
		if byDistance {
			v = f.Properties.Contour * 1000
		}
		out = append(out, Contour{Value: math.Round(v*1000) / 1000, Geometry: f.Geometry})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out, nil
}
