package server

import (
	"errors"
	"net/http"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
)

// On-demand terrain (internal/ondemand): terrain and elevation read from the public DEM
// hosts when first asked for, for regions set to it and, optionally, for everywhere outside
// built regions. Off unless the operator turns it on.

// ondemandStatus: GET /admin/api/ondemand → ondemand.Status: the configuration, where it
// applies, cache use, counters and the health of each DEM host.
func (s *Server) ondemandStatus(w http.ResponseWriter, r *http.Request) {
	if s.d.OnDemand == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "on-demand terrain is not part of this server (it serves neither tiles nor elevation)")
		return
	}
	writeJSON(w, http.StatusOK, s.d.OnDemand.Status())
}

// putOndemand: PUT /admin/api/ondemand {outside: "off"|"on_demand", cache_mb} → status.
// 400 bad_request for a value out of range.
func (s *Server) putOndemand(w http.ResponseWriter, r *http.Request) {
	if s.d.OnDemand == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "on-demand terrain is not part of this server")
		return
	}
	c := s.d.OnDemand.Config()
	var b struct {
		Outside *string `json:"outside"`
		CacheMB *int64  `json:"cache_mb"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Outside != nil {
		c.Outside = *b.Outside
	}
	if b.CacheMB != nil {
		c.CacheMB = *b.CacheMB
	}
	if err := c.Validate(); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.d.OnDemand.SetConfig(r.Context(), c); err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.d.OnDemand.Status())
}

// clearOndemand: POST /admin/api/ondemand/clear → status, with the cache emptied.
func (s *Server) clearOndemand(w http.ResponseWriter, r *http.Request) {
	if s.d.OnDemand == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "on-demand terrain is not part of this server")
		return
	}
	s.d.OnDemand.Clear()
	writeJSON(w, http.StatusOK, s.d.OnDemand.Status())
}

// ondemandOverview is the part of the overview about on-demand terrain: nil when it is off
// everywhere, so the page shows nothing for a server that does not use it.
func (s *Server) ondemandOverview() any {
	if s.d.OnDemand == nil {
		return nil
	}
	st := s.d.OnDemand.Status()
	if !st.Enabled && st.Cache.Bytes == 0 {
		return nil
	}
	return st
}

// warmBody is what the warm endpoints take: a region set to on demand, or a box.
type warmBody struct {
	Region  string    `json:"region"`
	BBox    []float64 `json:"bbox"`
	MaxZoom int       `json:"max_zoom"`
}

// warmRequest reads and resolves a warm request; it answers the error itself.
func (s *Server) warmRequest(w http.ResponseWriter, r *http.Request) (ondemand.WarmRequest, bool) {
	var req ondemand.WarmRequest
	if s.d.OnDemand == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "on-demand terrain is not part of this server")
		return req, false
	}
	var b warmBody
	if !readJSON(w, r, &b) {
		return req, false
	}
	req.MaxZoom = b.MaxZoom
	if req.MaxZoom == 0 {
		req.MaxZoom = ondemand.MaxZoom
	}
	switch {
	case b.Region != "":
		a, ok := s.d.OnDemand.Region(b.Region)
		if !ok {
			writeError(w, r, http.StatusBadRequest, "bad_request", "that region's terrain is not on demand")
			return req, false
		}
		req.BBox, req.Rings, req.Label = a.BBox, a.Rings, a.ID
		if rg, err := s.d.Store.Region(r.Context(), a.ID); err == nil && rg.Name != "" {
			req.Label = rg.Name
		}
	case len(b.BBox) == 4:
		req.BBox = elevation.BBox{b.BBox[0], b.BBox[1], b.BBox[2], b.BBox[3]}
		req.Label = "a drawn box"
	default:
		writeError(w, r, http.StatusBadRequest, "bad_request", "give a region or a bbox [west, south, east, north]")
		return req, false
	}
	return req, true
}

func warmError(w http.ResponseWriter, r *http.Request, err error) {
	var we *ondemand.ErrWarm
	switch {
	case errors.As(err, &we):
		writeError(w, r, http.StatusBadRequest, "bad_request", we.Msg)
	case errors.Is(err, ondemand.ErrWarmBusy):
		writeError(w, r, http.StatusConflict, "busy", err.Error())
	default:
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
	}
}

// warmEstimate: POST /admin/api/ondemand/warm/estimate {region | bbox, max_zoom} →
// ondemand.WarmEstimate: how many tiles, how many are cached already, what fetching the rest
// costs, and whether the cache can hold the area. Nothing is fetched.
func (s *Server) warmEstimate(w http.ResponseWriter, r *http.Request) {
	req, ok := s.warmRequest(w, r)
	if !ok {
		return
	}
	est, err := s.d.OnDemand.EstimateWarm(req)
	if err != nil {
		warmError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, est)
}

// warmStart: POST /admin/api/ondemand/warm {region | bbox, max_zoom} → 202 ondemand.WarmStatus.
// 400 when on demand applies nowhere in the area or the cache cannot hold it; 409 while a
// warm job runs.
func (s *Server) warmStart(w http.ResponseWriter, r *http.Request) {
	req, ok := s.warmRequest(w, r)
	if !ok {
		return
	}
	st, err := s.d.OnDemand.StartWarm(req)
	if err != nil {
		warmError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

// warmCancel: POST /admin/api/ondemand/warm/cancel → status. What was fetched stays cached.
func (s *Server) warmCancel(w http.ResponseWriter, r *http.Request) {
	if s.d.OnDemand == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "on-demand terrain is not part of this server")
		return
	}
	s.d.OnDemand.CancelWarm()
	writeJSON(w, http.StatusOK, s.d.OnDemand.Status())
}
