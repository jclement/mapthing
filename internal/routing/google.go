package routing

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Google Routes API (computeRoutes) compatible subset.

type gLatLng struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type gWaypoint struct {
	Location *struct {
		LatLng *gLatLng `json:"latLng"`
	} `json:"location"`
	PlaceID string `json:"placeId"`
	Address string `json:"address"`
	Via     bool   `json:"via"`
}

type gRequest struct {
	Origin                   *gWaypoint  `json:"origin"`
	Destination              *gWaypoint  `json:"destination"`
	Intermediates            []gWaypoint `json:"intermediates"`
	TravelMode               string      `json:"travelMode"`
	PolylineQuality          string      `json:"polylineQuality"`
	PolylineEncoding         string      `json:"polylineEncoding"`
	ComputeAlternativeRoutes bool        `json:"computeAlternativeRoutes"`
	RouteModifiers           struct {
		AvoidTolls    bool `json:"avoidTolls"`
		AvoidHighways bool `json:"avoidHighways"`
		AvoidFerries  bool `json:"avoidFerries"`
	} `json:"routeModifiers"`
}

type gError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func writeGoogleError(w http.ResponseWriter, status int, gstatus, msg string) {
	var e gError
	e.Error.Code, e.Error.Message, e.Error.Status = status, msg, gstatus
	writeJSON(w, status, e)
}

func (wp *gWaypoint) toLocation(name string) (Location, error) {
	if wp == nil {
		return Location{}, fmt.Errorf("%s is required", name)
	}
	if wp.Location == nil || wp.Location.LatLng == nil {
		if wp.Address != "" || wp.PlaceID != "" {
			return Location{}, fmt.Errorf("%s: address and placeId waypoints are not supported; use location.latLng", name)
		}
		return Location{}, fmt.Errorf("%s: location.latLng is required", name)
	}
	return Location{Lat: wp.Location.LatLng.Latitude, Lon: wp.Location.LatLng.Longitude}, nil
}

// googleProfile picks the costing: Google has no truck mode, so ?profile=truck selects it.
func googleProfile(r *http.Request, mode string) (string, error) {
	switch mode {
	case "", "DRIVE", "TRAVEL_MODE_UNSPECIFIED":
	default:
		return "", fmt.Errorf("travelMode %s is not supported (DRIVE only)", mode)
	}
	if p := r.URL.Query().Get("profile"); p != "" {
		return costingName(p)
	}
	return "auto", nil
}

func (h *Handlers) googleComputeRoutes(w http.ResponseWriter, r *http.Request) {
	var gr gRequest
	if err := decodeBody(r, &gr); err != nil {
		writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid JSON payload received. "+err.Error())
		return
	}
	req := Request{
		AvoidHighways: gr.RouteModifiers.AvoidHighways,
		AvoidTolls:    gr.RouteModifiers.AvoidTolls,
		AvoidFerries:  gr.RouteModifiers.AvoidFerries,
	}
	profile, err := googleProfile(r, gr.TravelMode)
	if err != nil {
		writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	req.Profile = profile
	o, err := gr.Origin.toLocation("origin")
	if err == nil {
		req.Locations = append(req.Locations, o)
		for i := range gr.Intermediates {
			var l Location
			if l, err = gr.Intermediates[i].toLocation(fmt.Sprintf("intermediates[%d]", i)); err != nil {
				break
			}
			req.Locations = append(req.Locations, l)
		}
	}
	if err == nil {
		var d Location
		if d, err = gr.Destination.toLocation("destination"); err == nil {
			req.Locations = append(req.Locations, d)
		}
	}
	if err != nil {
		writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if gr.ComputeAlternativeRoutes && len(gr.Intermediates) == 0 {
		req.AlternatesMax = 2
	}
	switch gr.PolylineEncoding {
	case "", "ENCODED_POLYLINE", "POLYLINE_ENCODING_UNSPECIFIED", "GEO_JSON_LINESTRING":
	default:
		writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "unknown polylineEncoding "+gr.PolylineEncoding)
		return
	}

	routes, err := h.C.Routes(r.Context(), req)
	if err != nil {
		re := asError(err)
		switch re.Code {
		case CodeNoRoute, CodeNoRoad:
			writeJSON(w, http.StatusOK, map[string]any{}) // Google answers "no route" with an empty object
		case CodeLimit, CodeBadRequest:
			writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", re.Msg)
		default:
			writeGoogleError(w, http.StatusServiceUnavailable, "UNAVAILABLE", re.Msg)
		}
		return
	}
	resp := googleResponse(routes, gr)
	mask := r.Header.Get("X-Goog-FieldMask")
	if mask == "" {
		mask = r.URL.Query().Get("$fields")
	}
	if mask == "" {
		mask = r.URL.Query().Get("fields")
	}
	writeJSON(w, http.StatusOK, applyFieldMask(resp, mask))
}

// googleResponse builds the computeRoutes response as generic JSON (so it can be masked).
func googleResponse(routes []*Route, gr gRequest) map[string]any {
	out := make([]any, 0, len(routes))
	for i, rt := range routes {
		label := "DEFAULT_ROUTE"
		if i > 0 {
			label = "DEFAULT_ROUTE_ALTERNATE"
		}
		legs := make([]any, 0, len(rt.Legs))
		for _, l := range rt.Legs {
			pts := rt.Geometry[l.From : l.To+1]
			legs = append(legs, map[string]any{
				"distanceMeters": int(math.Round(l.DistanceM)),
				"duration":       gDuration(l.DurationS),
				"staticDuration": gDuration(l.DurationS),
				"polyline":       gPolyline(pts, gr),
				"startLocation":  map[string]any{"latLng": gLL(pts[0])},
				"endLocation":    map[string]any{"latLng": gLL(pts[len(pts)-1])},
			})
		}
		b := bbox(rt.Geometry)
		out = append(out, map[string]any{
			"distanceMeters": int(math.Round(rt.DistanceM)),
			"duration":       gDuration(rt.DurationS),
			"staticDuration": gDuration(rt.DurationS),
			"polyline":       gPolyline(rt.Geometry, gr),
			"legs":           legs,
			"viewport": map[string]any{
				"low":  gLatLng{Latitude: b[1], Longitude: b[0]},
				"high": gLatLng{Latitude: b[3], Longitude: b[2]},
			},
			"routeLabels": []string{label},
		})
	}
	return map[string]any{"routes": out}
}

func gDuration(s float64) string { return strconv.FormatInt(int64(math.Round(s)), 10) + "s" }

func gLL(p [2]float64) gLatLng { return gLatLng{Latitude: p[1], Longitude: p[0]} }

func gPolyline(pts [][2]float64, gr gRequest) map[string]any {
	if gr.PolylineQuality == "OVERVIEW" {
		pts = simplify(pts, 20)
	}
	if gr.PolylineEncoding == "GEO_JSON_LINESTRING" {
		return map[string]any{"geoJsonLinestring": lineString{Type: "LineString", Coordinates: pts}}
	}
	return map[string]any{"encodedPolyline": EncodePolyline(pts, 5)}
}

// applyFieldMask prunes v to the comma-separated field paths in mask ("*" or "" keeps all).
// Paths are dotted ("routes.legs.duration"); arrays are transparent.
func applyFieldMask(v map[string]any, mask string) any {
	mask = strings.TrimSpace(mask)
	if mask == "" || mask == "*" {
		return v
	}
	tree := map[string]any{}
	for _, p := range strings.Split(mask, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		node := tree
		parts := strings.Split(p, ".")
		for i, part := range parts {
			if part == "*" {
				// "routes.*" keeps everything below routes.
				for k := range node {
					delete(node, k)
				}
				node["*"] = true
				break
			}
			if i == len(parts)-1 {
				node[part] = true
				break
			}
			child, ok := node[part].(map[string]any)
			if !ok {
				if node[part] == true {
					break // already keeping the whole subtree
				}
				child = map[string]any{}
				node[part] = child
			}
			node = child
		}
	}
	pruned := prune(v, tree)
	if pruned == nil {
		return map[string]any{}
	}
	return pruned
}

func prune(v any, tree map[string]any) any {
	if tree["*"] == true {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, sub := range tree {
			val, ok := x[k]
			if !ok {
				continue
			}
			if sub == true {
				out[k] = val
			} else if p := prune(val, sub.(map[string]any)); p != nil {
				out[k] = p
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			if p := prune(e, tree); p != nil {
				out = append(out, p)
			}
		}
		return out
	default:
		// A struct value under a path that goes deeper: round-trip through JSON.
		b, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		var m any
		if json.Unmarshal(b, &m) != nil {
			return nil
		}
		if _, ok := m.(map[string]any); !ok {
			return nil
		}
		return prune(m, tree)
	}
}
