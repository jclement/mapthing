package routing

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Handlers serves the native, pass-through, Google Routes and ORS routing endpoints.
type Handlers struct {
	C   *Client
	Log *slog.Logger
}

// NewHandlers returns handlers backed by c.
func NewHandlers(c *Client) *Handlers { return &Handlers{C: c, Log: slog.Default()} }

// Mount registers every routing endpoint on mux. wrap applies auth and accounting; kind is
// one of "route", "matrix", "isochrone", "snap", "valhalla".
func (h *Handlers) Mount(mux *http.ServeMux, wrap func(kind string, h http.Handler) http.Handler) {
	if wrap == nil {
		wrap = func(_ string, h http.Handler) http.Handler { return h }
	}
	add := func(pattern, kind string, f http.HandlerFunc) { mux.Handle(pattern, wrap(kind, f)) }

	add("POST /api/v1/route", "route", h.nativeRoute)
	// Methods are explicit: a method-less pattern conflicts with the server's "GET /" UI route.
	// The pass-through is charged as the operation it runs, so /valhalla/isochrone costs and
	// is limited exactly like /api/v1/isochrone (ValhallaKind). Each kind has its own wrapped
	// handler; the action in the path picks one.
	pass := map[string]http.Handler{}
	for _, kind := range []string{"route", "matrix", "isochrone", "snap", "elevation", "valhalla"} {
		pass[kind] = wrap(kind, http.HandlerFunc(h.passthrough))
	}
	byAction := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pass[ValhallaKind(r.PathValue("action"))].ServeHTTP(w, r)
	})
	mux.Handle("GET /api/v1/valhalla/{action}", byAction)
	mux.Handle("POST /api/v1/valhalla/{action}", byAction)

	add("POST /directions/v2:computeRoutes", "route", h.googleComputeRoutes)

	for _, p := range []string{"/ors/v2/directions/{profile}", "/ors/v2/directions/{profile}/json"} {
		add("POST "+p, "route", h.orsDirections(false))
	}
	add("POST /ors/v2/directions/{profile}/geojson", "route", h.orsDirections(true))
	add("GET /ors/v2/directions/{profile}", "route", h.orsDirectionsGET)
	add("POST /ors/v2/matrix/{profile}", "matrix", h.orsMatrix)
	add("POST /ors/v2/matrix/{profile}/json", "matrix", h.orsMatrix)
	add("POST /ors/v2/snap/{profile}", "snap", h.orsSnap)
	add("POST /ors/v2/snap/{profile}/json", "snap", h.orsSnap)
	add("POST /ors/v2/isochrones/{profile}", "isochrone", h.orsIsochrones)
	add("POST /ors/v2/isochrones/{profile}/geojson", "isochrone", h.orsIsochrones)
}

const maxBody = 8 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	return dec.Decode(v)
}

// asError converts any error into *Error.
func asError(err error) *Error {
	var re *Error
	if errors.As(err, &re) {
		return re
	}
	return &Error{Code: CodeUnavailable, Msg: err.Error(), Index: -1}
}

// httpStatus is the HTTP status for a native error code.
func httpStatus(code string) int {
	switch code {
	case CodeNoRoute, CodeNoRoad:
		return http.StatusNotFound
	case CodeLimit, CodeBadRequest:
		return http.StatusBadRequest
	}
	return http.StatusServiceUnavailable
}

// ---- native ---------------------------------------------------------------

type nativeError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Index   *int   `json:"location_index,omitempty"`
	} `json:"error"`
}

func writeNativeError(w http.ResponseWriter, err error) {
	re := asError(err)
	var body nativeError
	body.Error.Code = re.Code
	body.Error.Message = re.Msg
	if re.Index >= 0 {
		i := re.Index
		body.Error.Index = &i
	}
	writeJSON(w, httpStatus(re.Code), body)
}

// nativeFeature is the POST /api/v1/route response: a GeoJSON Feature.
type nativeFeature struct {
	Type       string           `json:"type"`
	BBox       [4]float64       `json:"bbox"`
	Geometry   lineString       `json:"geometry"`
	Properties nativeProperties `json:"properties"`
}

type lineString struct {
	Type        string       `json:"type"`
	Coordinates [][2]float64 `json:"coordinates"`
}

type nativeProperties struct {
	*Route
	Alternates []nativeFeature `json:"alternates,omitempty"`
}

func toNativeFeature(rt *Route) nativeFeature {
	return nativeFeature{
		Type:       "Feature",
		BBox:       bbox(rt.Geometry),
		Geometry:   lineString{Type: "LineString", Coordinates: rt.Geometry},
		Properties: nativeProperties{Route: rt},
	}
}

func (h *Handlers) nativeRoute(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := decodeBody(r, &req); err != nil {
		writeNativeError(w, &Error{Code: CodeBadRequest, Msg: "invalid JSON: " + err.Error(), Index: -1})
		return
	}
	routes, err := h.C.Routes(r.Context(), req)
	if err != nil {
		writeNativeError(w, err)
		return
	}
	f := toNativeFeature(routes[0])
	for _, alt := range routes[1:] {
		f.Properties.Alternates = append(f.Properties.Alternates, toNativeFeature(alt))
	}
	writeJSON(w, http.StatusOK, f)
}

// ---- pass-through -----------------------------------------------------------

// passthroughActions are the Valhalla actions the pass-through forwards.
var passthroughActions = map[string]bool{
	"route": true, "optimized_route": true, "sources_to_targets": true, "isochrone": true,
	"trace_route": true, "trace_attributes": true, "locate": true, "height": true,
	"expansion": true, "centroid": true, "status": true,
}

// ValhallaKind is the usage kind a pass-through action is charged and limited as: the kind of
// the dedicated endpoint that runs the same operation, and "valhalla" for actions without one
// (map matching, expansion, centroid, status, and anything unknown, which the handler refuses).
func ValhallaKind(action string) string {
	switch action {
	case "route":
		return "route"
	case "sources_to_targets", "optimized_route": // an optimised route computes a matrix first
		return "matrix"
	case "isochrone":
		return "isochrone"
	case "locate":
		return "snap"
	case "height":
		return "elevation"
	}
	return "valhalla"
}

func (h *Handlers) passthrough(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if !passthroughActions[action] {
		writeNativeError(w, &Error{Code: CodeBadRequest, Msg: "unsupported valhalla action " + action, Index: -1})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeNativeError(w, &Error{Code: CodeBadRequest, Msg: "use GET or POST", Index: -1})
		return
	}
	q := r.URL.Query()
	q.Del("key") // the API key never reaches the engine
	var body io.Reader
	if r.Method == http.MethodPost {
		body = io.LimitReader(r.Body, maxBody)
	}
	resp, err := h.C.Raw(r.Context(), r.Method, action, q, body)
	if err != nil {
		writeNativeError(w, &Error{Code: CodeUnavailable, Msg: "routing engine unreachable", Index: -1})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, k := range []string{"Content-Type", "Content-Encoding"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" || strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
