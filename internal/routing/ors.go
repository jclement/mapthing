package routing

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OpenRouteService v2 compatible subset.
//
// Error codes follow ORS: each endpoint has a base (directions 2000, isochrones 3000,
// matrix 6000, snap 8000); +0 invalid JSON, +1 missing parameter, +2 invalid parameter
// format, +3 invalid parameter value, +4 request exceeds server limit, +9 route not found,
// +10 point not found, +99 unknown.

const (
	orsDirections = 2000
	orsIsochrones = 3000
	orsMatrix     = 6000
	orsSnap       = 8000

	orsInvalidJSON  = 0
	orsMissing      = 1
	orsFormat       = 2
	orsValue        = 3
	orsLimit        = 4
	orsRouteNotFnd  = 9
	orsPointNotFnd  = 10
	orsUnknown      = 99
	orsDefaultRadM  = 350.0
	orsMaxRadiusM   = 10000.0
	orsAttribution  = "© openrouteservice-compatible API by mapthing | © OpenStreetMap contributors"
	orsEngineVer    = "mapthing-valhalla"
	orsMaxIsoRanges = 5
)

type orsErr struct {
	status int
	code   int
	msg    string
}

func writeORSError(w http.ResponseWriter, e orsErr) {
	writeJSON(w, e.status, map[string]any{
		"error": map[string]any{"code": e.code, "message": e.msg},
		"info": map[string]any{
			"engine":    map[string]any{"version": orsEngineVer},
			"timestamp": time.Now().UnixMilli(),
		},
	})
}

func orsBad(base, offset int, format string, args ...any) orsErr {
	return orsErr{status: http.StatusBadRequest, code: base + offset, msg: fmt.Sprintf(format, args...)}
}

// orsProfile maps an ORS profile to a costing.
func orsProfile(base int, p string) (string, *orsErr) {
	switch p {
	case "driving-car":
		return "auto", nil
	case "driving-hgv":
		return "truck", nil
	}
	e := orsBad(base, orsValue, "Parameter 'profile' has incorrect value of '%s'. Supported: driving-car, driving-hgv.", p)
	return "", &e
}

// orsLocations converts [[lon,lat],...] plus radiuses into Locations.
func orsLocations(base int, coords [][]float64, radiuses []float64, defRadius float64) ([]Location, *orsErr) {
	if len(radiuses) > 0 && len(radiuses) != len(coords) {
		e := orsBad(base, orsValue, "Parameter 'radiuses' must have the same number of values as there are coordinates.")
		return nil, &e
	}
	locs := make([]Location, len(coords))
	for i, c := range coords {
		if len(c) < 2 {
			e := orsBad(base, orsFormat, "Parameter 'coordinates' has incorrect format: coordinate %d needs [lon, lat].", i)
			return nil, &e
		}
		if c[0] < -180 || c[0] > 180 || c[1] < -90 || c[1] > 90 {
			e := orsBad(base, orsValue, "Parameter 'coordinates' has incorrect value: coordinate %d is out of range.", i)
			return nil, &e
		}
		r := defRadius
		if len(radiuses) > 0 {
			r = radiuses[i]
		}
		switch {
		case r == -1:
			r = -1 // unlimited
		case r <= 0:
			e := orsBad(base, orsValue, "Parameter 'radiuses' has incorrect value: %v (positive metres, or -1).", r)
			return nil, &e
		case r > orsMaxRadiusM:
			e := orsBad(base, orsLimit, "Parameter 'radiuses' exceeds the server limit of %.0f metres.", orsMaxRadiusM)
			return nil, &e
		}
		locs[i] = Location{Lon: c[0], Lat: c[1], RadiusM: r}
	}
	return locs, nil
}

// orsFromError maps a routing error to an ORS error for endpoint base.
func orsFromError(base int, err error, locs []Location) orsErr {
	re := asError(err)
	switch re.Code {
	case CodeNoRoad:
		i := re.Index
		if i < 0 {
			i = 0
		}
		var l Location
		if i < len(locs) {
			l = locs[i]
		}
		radius := "unlimited"
		if l.RadiusM > 0 {
			radius = strconv.FormatFloat(l.RadiusM, 'f', 1, 64) + " meters"
		}
		return orsErr{http.StatusNotFound, base + orsPointNotFnd,
			fmt.Sprintf("Could not find routable point within a radius of %s of specified coordinate %d: %.7f %.7f.", radius, i, l.Lon, l.Lat)}
	case CodeNoRoute:
		msg := "Route could not be found."
		if len(locs) >= 2 {
			a, b := locs[0], locs[len(locs)-1]
			msg = fmt.Sprintf("Route could not be found - Unable to find a route between points 1 (%.7f %.7f) and %d (%.7f %.7f).",
				a.Lon, a.Lat, len(locs), b.Lon, b.Lat)
		}
		return orsErr{http.StatusNotFound, base + orsRouteNotFnd, msg}
	case CodeLimit:
		return orsErr{http.StatusBadRequest, base + orsLimit, re.Msg}
	case CodeBadRequest:
		return orsErr{http.StatusBadRequest, base + orsValue, re.Msg}
	}
	return orsErr{http.StatusInternalServerError, base + orsUnknown, re.Msg}
}

func orsMetadata(service, profile string, query any) map[string]any {
	return map[string]any{
		"attribution": orsAttribution,
		"service":     service,
		"timestamp":   time.Now().UnixMilli(),
		"query":       query,
		"engine":      map[string]any{"version": orsEngineVer},
	}
}

// unitFactor converts metres to the requested ORS unit.
func unitFactor(base int, u string) (float64, *orsErr) {
	switch u {
	case "", "m":
		return 1, nil
	case "km":
		return 0.001, nil
	case "mi":
		return 1 / 1609.344, nil
	}
	e := orsBad(base, orsValue, "Parameter 'units' has incorrect value of '%s'.", u)
	return 0, &e
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }
func r1(v float64) float64 { return math.Round(v*10) / 10 }

// ---- directions ------------------------------------------------------------

type orsDirReq struct {
	Coordinates  [][]float64 `json:"coordinates"`
	Radiuses     []float64   `json:"radiuses"`
	Preference   string      `json:"preference"`
	Units        string      `json:"units"`
	Instructions *bool       `json:"instructions"`
	Geometry     *bool       `json:"geometry"`
	ExtraInfo    []string    `json:"extra_info"`
	Options      struct {
		AvoidFeatures []string `json:"avoid_features"`
	} `json:"options"`
	AlternativeRoutes *struct {
		TargetCount int `json:"target_count"`
	} `json:"alternative_routes"`
}

// orsStepType maps a Valhalla maneuver type to an ORS instruction type.
func orsStepType(t int) int {
	switch t {
	case 1, 2, 3:
		return 11 // depart
	case 4, 5, 6:
		return 10 // goal
	case 9, 18, 20, 37:
		return 5 // slight right
	case 10:
		return 1
	case 11:
		return 3
	case 12, 13:
		return 9 // u-turn
	case 14:
		return 2
	case 15:
		return 0
	case 16, 19, 21, 38:
		return 4 // slight left
	case 23:
		return 13 // keep right
	case 24:
		return 12 // keep left
	case 26:
		return 7 // enter roundabout
	case 27:
		return 8 // exit roundabout
	}
	return 6 // straight / continue
}

// orsSurfaceCode maps a Valhalla surface to the ORS surface extra_info code.
func orsSurfaceCode(raw string) int {
	switch raw {
	case "paved_smooth", "paved", "paved_rough":
		return 1
	case "compacted":
		return 8
	case "gravel":
		return 10
	case "dirt":
		return 11
	case "path":
		return 12
	}
	return 0
}

func (h *Handlers) orsDirectionsGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parse := func(s string) ([]float64, error) {
		parts := strings.Split(s, ",")
		if len(parts) != 2 {
			return nil, fmt.Errorf("want lon,lat")
		}
		lon, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		lat, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("want lon,lat")
		}
		return []float64{lon, lat}, nil
	}
	if q.Get("start") == "" || q.Get("end") == "" {
		writeORSError(w, orsBad(orsDirections, orsMissing, "Parameters 'start' and 'end' are required."))
		return
	}
	s, err1 := parse(q.Get("start"))
	e, err2 := parse(q.Get("end"))
	if err1 != nil || err2 != nil {
		writeORSError(w, orsBad(orsDirections, orsFormat, "Parameters 'start' and 'end' must be lon,lat."))
		return
	}
	h.orsRunDirections(w, r, orsDirReq{Coordinates: [][]float64{s, e}}, true)
}

func (h *Handlers) orsDirections(geojson bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var dr orsDirReq
		if err := decodeBody(r, &dr); err != nil {
			writeORSError(w, orsBad(orsDirections, orsInvalidJSON, "Unable to parse JSON request: %v", err))
			return
		}
		h.orsRunDirections(w, r, dr, geojson)
	}
}

func (h *Handlers) orsRunDirections(w http.ResponseWriter, r *http.Request, dr orsDirReq, geojson bool) {
	costing, oe := orsProfile(orsDirections, r.PathValue("profile"))
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	if len(dr.Coordinates) < 2 {
		writeORSError(w, orsBad(orsDirections, orsMissing, "Parameter 'coordinates' needs at least two points."))
		return
	}
	if len(dr.Coordinates) > 50 {
		writeORSError(w, orsBad(orsDirections, orsLimit, "Request exceeds the maximum of 50 waypoints."))
		return
	}
	locs, oe := orsLocations(orsDirections, dr.Coordinates, dr.Radiuses, orsDefaultRadM)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	k, oe := unitFactor(orsDirections, dr.Units)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	req := Request{Locations: locs, Profile: costing, Shortest: dr.Preference == "shortest"}
	for _, f := range dr.Options.AvoidFeatures {
		switch f {
		case "highways":
			req.AvoidHighways = true
		case "tollways":
			req.AvoidTolls = true
		case "ferries":
			req.AvoidFerries = true
		case "unpavedroads", "tracks":
			req.AvoidUnpaved = true
		default:
			writeORSError(w, orsBad(orsDirections, orsValue, "Parameter 'avoid_features' has incorrect value of '%s'.", f))
			return
		}
	}
	if dr.AlternativeRoutes != nil && dr.AlternativeRoutes.TargetCount > 1 {
		req.AlternatesMax = min(dr.AlternativeRoutes.TargetCount-1, 2)
	}
	routes, err := h.C.Routes(r.Context(), req)
	if err != nil {
		writeORSError(w, orsFromError(orsDirections, err, locs))
		return
	}
	instructions := dr.Instructions == nil || *dr.Instructions
	wantSurface := false
	for _, x := range dr.ExtraInfo {
		if x == "surface" {
			wantSurface = true
		}
	}
	query := map[string]any{"coordinates": dr.Coordinates, "profile": r.PathValue("profile"), "format": "json"}
	if geojson {
		query["format"] = "geojson"
	}
	var all [][2]float64
	if geojson {
		features := make([]any, 0, len(routes))
		for _, rt := range routes {
			all = append(all, rt.Geometry...)
			b := bbox(rt.Geometry)
			features = append(features, map[string]any{
				"bbox":       b[:],
				"type":       "Feature",
				"properties": orsRouteProps(rt, k, instructions, wantSurface),
				"geometry":   lineString{Type: "LineString", Coordinates: rt.Geometry},
			})
		}
		b := bbox(all)
		writeJSON(w, http.StatusOK, map[string]any{
			"type":     "FeatureCollection",
			"bbox":     b[:],
			"features": features,
			"metadata": orsMetadata("routing", r.PathValue("profile"), query),
		})
		return
	}
	out := make([]any, 0, len(routes))
	for _, rt := range routes {
		all = append(all, rt.Geometry...)
		props := orsRouteProps(rt, k, instructions, wantSurface)
		b := bbox(rt.Geometry)
		props["bbox"] = b[:]
		if dr.Geometry == nil || *dr.Geometry {
			props["geometry"] = EncodePolyline(rt.Geometry, 5)
		}
		out = append(out, props)
	}
	b := bbox(all)
	writeJSON(w, http.StatusOK, map[string]any{
		"bbox":     b[:],
		"routes":   out,
		"metadata": orsMetadata("routing", r.PathValue("profile"), query),
	})
}

func orsRouteProps(rt *Route, k float64, instructions, wantSurface bool) map[string]any {
	segments := make([]any, 0, len(rt.Legs))
	for _, l := range rt.Legs {
		seg := map[string]any{"distance": r1(l.DistanceM * k), "duration": r1(l.DurationS)}
		if instructions {
			steps := make([]any, 0, len(l.Maneuvers))
			for _, m := range l.Maneuvers {
				name := m.Street
				if name == "" {
					name = "-"
				}
				steps = append(steps, map[string]any{
					"distance":    r1(m.DistanceM * k),
					"duration":    r1(m.DurationS),
					"type":        orsStepType(m.Type),
					"instruction": m.Instruction,
					"name":        name,
					"way_points":  []int{m.From, m.To},
				})
			}
			seg["steps"] = steps
		}
		segments = append(segments, seg)
	}
	wayPoints := []int{0}
	for _, l := range rt.Legs {
		wayPoints = append(wayPoints, l.To)
	}
	props := map[string]any{
		"segments":   segments,
		"summary":    map[string]any{"distance": r1(rt.DistanceM * k), "duration": r1(rt.DurationS)},
		"way_points": wayPoints,
	}
	if wantSurface {
		props["extras"] = map[string]any{"surface": orsSurfaceExtra(rt, k)}
	}
	return props
}

// orsSurfaceExtra builds the ORS extras.surface block (values + summary).
func orsSurfaceExtra(rt *Route, k float64) map[string]any {
	values := [][3]int{}
	dist := map[int]float64{}
	var total float64
	for _, s := range rt.Surfaces {
		code := orsSurfaceCode(s.Raw)
		if n := len(values); n > 0 && values[n-1][2] == code && values[n-1][1] == s.From {
			values[n-1][1] = s.To
		} else {
			values = append(values, [3]int{s.From, s.To, code})
		}
		d := pathLength(rt.Geometry, s.From, s.To)
		dist[code] += d
		total += d
	}
	codes := make([]int, 0, len(dist))
	for code := range dist {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	summary := []any{}
	for _, code := range codes {
		d := dist[code]
		amount := 0.0
		if total > 0 {
			amount = r2(d / total * 100)
		}
		summary = append(summary, map[string]any{"value": code, "distance": r1(d * k), "amount": amount})
	}
	return map[string]any{"values": values, "summary": summary}
}

// ---- matrix ----------------------------------------------------------------

type orsMatrixReq struct {
	Locations    [][]float64     `json:"locations"`
	Sources      json.RawMessage `json:"sources"`
	Destinations json.RawMessage `json:"destinations"`
	Metrics      []string        `json:"metrics"`
	Units        string          `json:"units"`
}

// orsIndexes parses ORS "sources"/"destinations": absent or ["all"] = every index.
func orsIndexes(raw json.RawMessage, n int, name string) ([]int, *orsErr) {
	all := func() []int {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	if len(raw) == 0 || string(raw) == "null" {
		return all(), nil
	}
	var strs []string
	if json.Unmarshal(raw, &strs) == nil {
		if len(strs) == 1 && strs[0] == "all" {
			return all(), nil
		}
		out := make([]int, 0, len(strs))
		for _, s := range strs {
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= n {
				e := orsBad(orsMatrix, orsValue, "Parameter '%s' has incorrect value of '%s'.", name, s)
				return nil, &e
			}
			out = append(out, i)
		}
		return out, nil
	}
	var ints []int
	if err := json.Unmarshal(raw, &ints); err != nil {
		e := orsBad(orsMatrix, orsFormat, "Parameter '%s' has incorrect format.", name)
		return nil, &e
	}
	for _, i := range ints {
		if i < 0 || i >= n {
			e := orsBad(orsMatrix, orsValue, "Parameter '%s' has incorrect value of '%d'.", name, i)
			return nil, &e
		}
	}
	return ints, nil
}

func (h *Handlers) orsMatrix(w http.ResponseWriter, r *http.Request) {
	var mr orsMatrixReq
	if err := decodeBody(r, &mr); err != nil {
		writeORSError(w, orsBad(orsMatrix, orsInvalidJSON, "Unable to parse JSON request: %v", err))
		return
	}
	profile := r.PathValue("profile")
	costing, oe := orsProfile(orsMatrix, profile)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	if len(mr.Locations) < 2 {
		writeORSError(w, orsBad(orsMatrix, orsMissing, "Parameter 'locations' needs at least two points."))
		return
	}
	locs, oe := orsLocations(orsMatrix, mr.Locations, nil, orsDefaultRadM)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	src, oe := orsIndexes(mr.Sources, len(locs), "sources")
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	dst, oe := orsIndexes(mr.Destinations, len(locs), "destinations")
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	if len(src)*len(dst) > 2500 {
		writeORSError(w, orsBad(orsMatrix, orsLimit, "Request exceeds the maximum of 2500 routes (sources × destinations)."))
		return
	}
	k, oe := unitFactor(orsMatrix, mr.Units)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	metrics := map[string]bool{}
	for _, m := range mr.Metrics {
		if m != "distance" && m != "duration" {
			writeORSError(w, orsBad(orsMatrix, orsValue, "Parameter 'metrics' has incorrect value of '%s'.", m))
			return
		}
		metrics[m] = true
	}
	if len(metrics) == 0 {
		metrics["duration"] = true
	}

	// Valhalla fails the whole matrix when any point cannot be snapped: find such points
	// first so the answer names the coordinate, as ORS does.
	snaps, err := h.C.Locate(r.Context(), locs, costing)
	if err != nil {
		writeORSError(w, orsFromError(orsMatrix, err, locs))
		return
	}
	for i, s := range snaps {
		if s == nil {
			writeORSError(w, orsFromError(orsMatrix, &Error{Code: CodeNoRoad, Index: i}, locs))
			return
		}
	}
	pick := func(idx []int) []Location {
		out := make([]Location, len(idx))
		for i, j := range idx {
			out[i] = locs[j]
		}
		return out
	}
	m, err := h.C.Matrix(r.Context(), pick(src), pick(dst), costing)
	if err != nil {
		writeORSError(w, orsFromError(orsMatrix, err, locs))
		return
	}
	snapInfo := func(idx []int) []any {
		out := make([]any, len(idx))
		for i, j := range idx {
			s := snaps[j]
			e := map[string]any{"location": []float64{s.Lon, s.Lat}, "snapped_distance": r2(s.DistanceM)}
			if s.Name != "" {
				e["name"] = s.Name
			}
			out[i] = e
		}
		return out
	}
	resp := map[string]any{
		"sources":      snapInfo(src),
		"destinations": snapInfo(dst),
		"metadata":     orsMetadata("matrix", profile, mr),
	}
	scale := func(t [][]*float64, f float64) [][]*float64 {
		out := make([][]*float64, len(t))
		for i, row := range t {
			out[i] = make([]*float64, len(row))
			for j, v := range row {
				if v != nil {
					x := r2(*v * f)
					out[i][j] = &x
				}
			}
		}
		return out
	}
	if metrics["duration"] {
		resp["durations"] = scale(m.Durations, 1)
	}
	if metrics["distance"] {
		resp["distances"] = scale(m.Distances, k)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- snap ------------------------------------------------------------------

type orsSnapReq struct {
	Locations [][]float64 `json:"locations"`
	Radius    float64     `json:"radius"`
}

func (h *Handlers) orsSnap(w http.ResponseWriter, r *http.Request) {
	var sr orsSnapReq
	if err := decodeBody(r, &sr); err != nil {
		writeORSError(w, orsBad(orsSnap, orsInvalidJSON, "Unable to parse JSON request: %v", err))
		return
	}
	profile := r.PathValue("profile")
	costing, oe := orsProfile(orsSnap, profile)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	if len(sr.Locations) == 0 {
		writeORSError(w, orsBad(orsSnap, orsMissing, "Parameter 'locations' is missing."))
		return
	}
	if sr.Radius == 0 {
		writeORSError(w, orsBad(orsSnap, orsMissing, "Parameter 'radius' is missing."))
		return
	}
	radiuses := make([]float64, len(sr.Locations))
	for i := range radiuses {
		radiuses[i] = sr.Radius
	}
	locs, oe := orsLocations(orsSnap, sr.Locations, radiuses, orsDefaultRadM)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	snaps, err := h.C.Locate(r.Context(), locs, costing)
	if err != nil {
		writeORSError(w, orsFromError(orsSnap, err, locs))
		return
	}
	out := make([]any, len(snaps))
	for i, s := range snaps {
		if s == nil {
			continue // ORS returns null for a location with no road within the radius
		}
		e := map[string]any{"location": []float64{s.Lon, s.Lat}, "snapped_distance": r2(s.DistanceM)}
		if s.Name != "" {
			e["name"] = s.Name
		}
		out[i] = e
	}
	writeJSON(w, http.StatusOK, map[string]any{"locations": out, "metadata": orsMetadata("snap", profile, sr)})
}

// ---- isochrones ------------------------------------------------------------

type orsIsoReq struct {
	Locations    [][]float64 `json:"locations"`
	Range        []float64   `json:"range"`
	RangeType    string      `json:"range_type"`
	Interval     float64     `json:"interval"`
	Units        string      `json:"units"`
	LocationType string      `json:"location_type"`
	Smoothing    *float64    `json:"smoothing"`
	Attributes   []string    `json:"attributes"`
}

const (
	maxIsoTimeS     = 7200
	maxIsoDistanceM = 200000
	maxIsoLocations = 5
)

func (h *Handlers) orsIsochrones(w http.ResponseWriter, r *http.Request) {
	var ir orsIsoReq
	if err := decodeBody(r, &ir); err != nil {
		writeORSError(w, orsBad(orsIsochrones, orsInvalidJSON, "Unable to parse JSON request: %v", err))
		return
	}
	profile := r.PathValue("profile")
	costing, oe := orsProfile(orsIsochrones, profile)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	if len(ir.Locations) == 0 {
		writeORSError(w, orsBad(orsIsochrones, orsMissing, "Parameter 'locations' is missing."))
		return
	}
	if len(ir.Locations) > maxIsoLocations {
		writeORSError(w, orsBad(orsIsochrones, orsLimit, "Request exceeds the maximum of %d locations.", maxIsoLocations))
		return
	}
	if len(ir.Range) == 0 {
		writeORSError(w, orsBad(orsIsochrones, orsMissing, "Parameter 'range' is missing."))
		return
	}
	byDistance := false
	switch ir.RangeType {
	case "", "time":
	case "distance":
		byDistance = true
	default:
		writeORSError(w, orsBad(orsIsochrones, orsValue, "Parameter 'range_type' has incorrect value of '%s'.", ir.RangeType))
		return
	}
	k := 1.0 // request units → metres (distance only)
	if byDistance {
		f, oe := unitFactor(orsIsochrones, ir.Units)
		if oe != nil {
			writeORSError(w, *oe)
			return
		}
		k = 1 / f
	}
	values := ir.Range
	if len(values) == 1 && ir.Interval > 0 {
		values = nil
		for v := ir.Interval; v < ir.Range[0]; v += ir.Interval {
			values = append(values, v)
		}
		values = append(values, ir.Range[0])
	}
	if len(values) > orsMaxIsoRanges {
		writeORSError(w, orsBad(orsIsochrones, orsLimit, "Request exceeds the maximum of %d ranges.", orsMaxIsoRanges))
		return
	}
	engineVals := make([]float64, len(values))
	for i, v := range values {
		ev := v * k
		limit := float64(maxIsoTimeS)
		if byDistance {
			limit = maxIsoDistanceM
		}
		if ev <= 0 || ev > limit {
			writeORSError(w, orsBad(orsIsochrones, orsLimit, "Parameter 'range' value %v is out of bounds (max %v).", v, limit/k))
			return
		}
		engineVals[i] = ev
	}
	locs, oe := orsLocations(orsIsochrones, ir.Locations, nil, -1)
	if oe != nil {
		writeORSError(w, *oe)
		return
	}
	reverse := ir.LocationType == "destination"
	features := []any{}
	var all [][2]float64
	for gi, loc := range locs {
		contours, err := h.C.Isochrone(r.Context(), loc, costing, engineVals, byDistance, reverse)
		if err != nil {
			writeORSError(w, orsFromError(orsIsochrones, err, []Location{loc}))
			return
		}
		for _, c := range contours {
			all = append(all, geometryPoints(c.Geometry)...)
			features = append(features, map[string]any{
				"type": "Feature",
				"properties": map[string]any{
					"group_index": gi,
					"value":       r2(c.Value / k),
					"center":      []float64{loc.Lon, loc.Lat},
				},
				"geometry": c.Geometry,
			})
		}
	}
	b := bbox(all)
	writeJSON(w, http.StatusOK, map[string]any{
		"type":     "FeatureCollection",
		"bbox":     b[:],
		"features": features,
		"metadata": orsMetadata("isochrones", profile, ir),
	})
}

// geometryPoints returns every vertex of a GeoJSON Polygon or MultiPolygon (for a bbox).
func geometryPoints(raw json.RawMessage) [][2]float64 {
	var g struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if json.Unmarshal(raw, &g) != nil {
		return nil
	}
	var out [][2]float64
	switch g.Type {
	case "Polygon":
		var rings [][][2]float64
		if json.Unmarshal(g.Coordinates, &rings) == nil {
			for _, ring := range rings {
				out = append(out, ring...)
			}
		}
	case "MultiPolygon":
		var polys [][][][2]float64
		if json.Unmarshal(g.Coordinates, &polys) == nil {
			for _, p := range polys {
				for _, ring := range p {
					out = append(out, ring...)
				}
			}
		}
	}
	return out
}
