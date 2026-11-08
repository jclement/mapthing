package elevation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Limits on request size.
const (
	MaxPoints       = 10000 // /api/v1/elevation
	MaxGoogleCount  = 512   // Google: locations per request and samples per path
	maxBodyBytes    = 4 << 20
	maxProfilePathV = 50000 // vertices in a profile request
)

// Handler serves the elevation HTTP API.
type Handler struct {
	Svc Service
}

// NewHandler returns the HTTP handler set for svc.
func NewHandler(svc Service) *Handler { return &Handler{Svc: svc} }

// Mount registers the elevation routes (core's server.Mountable). wrap applies
// authentication, rate limits and usage counting under a kind; every route
// here counts as "elevation", the Google-compatible one included. A nil wrap
// mounts the handlers bare (tests).
func (h *Handler) Mount(mux *http.ServeMux, wrap func(kind string, h http.Handler) http.Handler) {
	if wrap == nil {
		wrap = func(_ string, h http.Handler) http.Handler { return h }
	}
	const kind = "elevation"
	mux.Handle("GET /api/v1/elevation", wrap(kind, http.HandlerFunc(h.points)))
	mux.Handle("POST /api/v1/elevation", wrap(kind, http.HandlerFunc(h.points)))
	mux.Handle("POST /api/v1/elevation/profile", wrap(kind, http.HandlerFunc(h.profile)))
	mux.Handle("GET /api/v1/elevation/sources", wrap(kind, http.HandlerFunc(h.sources)))
	mux.Handle("GET /maps/api/elevation/json", wrap(kind, http.HandlerFunc(h.google)))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func apiError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.As(err, new(*internalError)):
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type internalError struct{ err error }

func (e *internalError) Error() string { return e.err.Error() }

// --- /api/v1/elevation --------------------------------------------------------

type pointsRequest struct {
	Points []LonLat `json:"points"`
}

type pointsResponse struct {
	Results []Sample     `json:"results"`
	Sources []SourceInfo `json:"sources"`
}

// points: POST {"points":[[lon,lat],..]} or GET ?points=lon,lat|lon,lat
func (h *Handler) points(w http.ResponseWriter, r *http.Request) {
	var req pointsRequest
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
			apiError(w, fmt.Errorf("invalid JSON body: %w", err))
			return
		}
	} else {
		q := r.URL.Query().Get("points")
		if q == "" {
			apiError(w, errors.New(`missing "points" (lon,lat|lon,lat…)`))
			return
		}
		pts, err := parsePairs(q, false)
		if err != nil {
			apiError(w, err)
			return
		}
		req.Points = pts
	}
	if len(req.Points) == 0 {
		apiError(w, errors.New("no points"))
		return
	}
	if len(req.Points) > MaxPoints {
		apiError(w, fmt.Errorf("too many points (%d > %d)", len(req.Points), MaxPoints))
		return
	}
	for _, p := range req.Points {
		if err := checkLonLat(p); err != nil {
			apiError(w, err)
			return
		}
	}
	res, err := h.Svc.Points(r.Context(), req.Points)
	if err != nil {
		apiError(w, wrapInternal(err))
		return
	}
	writeJSON(w, http.StatusOK, pointsResponse{Results: res, Sources: usedSources(h.Svc, res)})
}

func usedSources(svc Service, res []Sample) []SourceInfo {
	used := map[string]bool{}
	for _, s := range res {
		used[s.Source] = true
	}
	out := []SourceInfo{}
	for _, s := range svc.Sources() {
		if used[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

func wrapInternal(err error) error {
	if errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &internalError{err}
}

// --- /api/v1/elevation/profile ------------------------------------------------

type profileRequest struct {
	Path        []LonLat `json:"path"`
	Polyline    string   `json:"polyline"`
	Precision   int      `json:"precision"` // polyline precision, default 5 (6 for polyline6)
	IntervalM   float64  `json:"interval_m"`
	Samples     int      `json:"samples"`
	HysteresisM float64  `json:"hysteresis_m"`
}

func (h *Handler) profile(w http.ResponseWriter, r *http.Request) {
	var req profileRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
		apiError(w, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	path := req.Path
	if req.Polyline != "" {
		if len(path) > 0 {
			apiError(w, errors.New(`give "path" or "polyline", not both`))
			return
		}
		if req.Precision != 0 && (req.Precision < 1 || req.Precision > 7) {
			apiError(w, errors.New("precision must be 1..7"))
			return
		}
		var err error
		if path, err = DecodePolyline(req.Polyline, req.Precision); err != nil {
			apiError(w, fmt.Errorf("polyline: %w", err))
			return
		}
	}
	if len(path) == 0 {
		apiError(w, errors.New(`missing "path" or "polyline"`))
		return
	}
	if len(path) > maxProfilePathV {
		apiError(w, fmt.Errorf("path has too many vertices (%d > %d)", len(path), maxProfilePathV))
		return
	}
	for _, p := range path {
		if err := checkLonLat(p); err != nil {
			apiError(w, err)
			return
		}
	}
	if req.Samples < 0 || req.Samples > MaxProfileSamples {
		apiError(w, fmt.Errorf("samples must be 0..%d", MaxProfileSamples))
		return
	}
	if req.IntervalM < 0 || math.IsNaN(req.IntervalM) {
		apiError(w, errors.New("interval_m must be positive"))
		return
	}
	if req.IntervalM > 0 && req.IntervalM < 1 {
		req.IntervalM = 1
	}
	p, err := h.Svc.Profile(r.Context(), path, ProfileOptions{IntervalM: req.IntervalM, Samples: req.Samples, HysteresisM: req.HysteresisM})
	if err != nil {
		apiError(w, wrapInternal(err))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) sources(w http.ResponseWriter, r *http.Request) {
	s := h.Svc.Sources()
	if s == nil {
		s = []SourceInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": s})
}

// --- Google Elevation API compatibility ------------------------------------------

type googleLocation struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type googleResult struct {
	Elevation  float64        `json:"elevation"`
	Location   googleLocation `json:"location"`
	Resolution float64        `json:"resolution"`
}

type googleResponse struct {
	Results      []googleResult `json:"results"`
	Status       string         `json:"status"`
	ErrorMessage string         `json:"error_message,omitempty"`
}

// google implements GET /maps/api/elevation/json. Like Google:
//   - locations=lat,lng|lat,lng… or locations=enc:<polyline>
//   - path=lat,lng|… or path=enc:<polyline>, with samples=N (required, ≤512)
//   - at most 512 locations; errors use the documented status strings.
//
// Points no source covers come back with elevation 0 and resolution -1 (the
// Google shape has no null); if no point at all is covered the status is
// DATA_NOT_AVAILABLE.
func (h *Handler) google(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, code, msg string) {
		writeJSON(w, status, googleResponse{Results: []googleResult{}, Status: code, ErrorMessage: msg})
	}
	q := r.URL.Query()
	locs, path, samplesStr := q.Get("locations"), q.Get("path"), q.Get("samples")
	var samples []Sample
	var err error
	switch {
	case locs != "" && path != "":
		fail(http.StatusBadRequest, "INVALID_REQUEST", "Specify either locations or path, not both.")
		return
	case locs != "":
		pts, perr := parseGoogleCoords(locs)
		if perr != nil {
			fail(http.StatusBadRequest, "INVALID_REQUEST", perr.Error())
			return
		}
		if len(pts) > MaxGoogleCount {
			fail(http.StatusBadRequest, "INVALID_REQUEST", fmt.Sprintf("At most %d locations per request.", MaxGoogleCount))
			return
		}
		samples, err = h.Svc.Points(r.Context(), pts)
	case path != "":
		pts, perr := parseGoogleCoords(path)
		if perr != nil {
			fail(http.StatusBadRequest, "INVALID_REQUEST", perr.Error())
			return
		}
		if samplesStr == "" {
			fail(http.StatusBadRequest, "INVALID_REQUEST", "Path requests require the samples parameter.")
			return
		}
		n, serr := strconv.Atoi(samplesStr)
		if serr != nil || n < 1 || n > MaxGoogleCount {
			fail(http.StatusBadRequest, "INVALID_REQUEST", fmt.Sprintf("samples must be an integer from 1 to %d.", MaxGoogleCount))
			return
		}
		if len(pts) < 2 && n > 1 {
			fail(http.StatusBadRequest, "INVALID_REQUEST", "A path needs at least two points.")
			return
		}
		var p *Profile
		p, err = h.Svc.Profile(r.Context(), pts, ProfileOptions{Samples: n})
		if p != nil {
			samples = p.Samples
		}
	default:
		fail(http.StatusBadRequest, "INVALID_REQUEST", "Missing locations or path parameter.")
		return
	}
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			fail(http.StatusOK, "DATA_NOT_AVAILABLE", "No elevation data is loaded.")
			return
		}
		fail(http.StatusInternalServerError, "UNKNOWN_ERROR", err.Error())
		return
	}
	resp := googleResponse{Status: "OK", Results: make([]googleResult, len(samples))}
	covered := 0
	for i, s := range samples {
		gr := googleResult{Location: googleLocation{Lat: s.Lat, Lng: s.Lon}, Resolution: -1}
		if s.Elevation != nil {
			gr.Elevation = *s.Elevation
			gr.Resolution = s.Resolution
			covered++
		}
		resp.Results[i] = gr
	}
	if covered == 0 && len(samples) > 0 {
		resp.Status = "DATA_NOT_AVAILABLE"
		resp.ErrorMessage = "No elevation data covers the requested locations."
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseGoogleCoords parses "lat,lng|lat,lng" or "enc:<polyline>".
func parseGoogleCoords(s string) ([]LonLat, error) {
	if strings.HasPrefix(s, "enc:") {
		pts, err := DecodePolyline(strings.TrimPrefix(s, "enc:"), 5)
		if err != nil {
			return nil, fmt.Errorf("invalid encoded polyline: %w", err)
		}
		if len(pts) == 0 {
			return nil, errors.New("empty encoded polyline")
		}
		for _, p := range pts {
			if err := checkLonLat(p); err != nil {
				return nil, err
			}
		}
		return pts, nil
	}
	return parsePairs(s, true)
}

// parsePairs parses "a,b|a,b…". latFirst selects lat,lng (Google) over lon,lat.
func parsePairs(s string, latFirst bool) ([]LonLat, error) {
	parts := strings.Split(s, "|")
	pts := make([]LonLat, 0, len(parts))
	for _, part := range parts {
		ab := strings.Split(strings.TrimSpace(part), ",")
		if len(ab) != 2 {
			return nil, fmt.Errorf("invalid coordinate %q", part)
		}
		a, err1 := strconv.ParseFloat(strings.TrimSpace(ab[0]), 64)
		b, err2 := strconv.ParseFloat(strings.TrimSpace(ab[1]), 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("invalid coordinate %q", part)
		}
		p := LonLat{a, b}
		if latFirst {
			p = LonLat{b, a}
		}
		if err := checkLonLat(p); err != nil {
			return nil, err
		}
		pts = append(pts, p)
	}
	return pts, nil
}

func checkLonLat(p LonLat) error {
	if math.IsNaN(p[0]) || math.IsNaN(p[1]) || p[0] < -180 || p[0] > 180 || p[1] < -90 || p[1] > 90 {
		return fmt.Errorf("coordinate out of range: lat %v, lng %v", p[1], p[0])
	}
	return nil
}
