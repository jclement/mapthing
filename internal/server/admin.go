package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/scheduler"
	"github.com/jclement/mapthing/internal/store"
	"github.com/jclement/mapthing/internal/tiles"
)

// --- keys ---

type keyView struct {
	*store.APIKey
	Usage keyUsage `json:"usage"`
}

type keyUsage struct {
	Today  int64            `json:"today"`
	Last30 int64            `json:"last_30d"`
	ByKind map[string]int64 `json:"by_kind_30d"`
}

func (s *Server) keyUsage(r *http.Request) (map[int64]*keyUsage, error) {
	now := time.Now().UTC()
	since := now.AddDate(0, 0, -29).Format("2006-01-02")
	today := now.Format("2006-01-02")
	rows, err := s.d.Store.UsageSince(r.Context(), since)
	if err != nil {
		return nil, err
	}
	pending, _ := s.d.Stats.Pending()
	for k, n := range pending {
		rows = append(rows, store.UsageRow{KeyID: k.KeyID, Day: k.Day, Kind: k.Kind, Count: n})
	}
	out := map[int64]*keyUsage{}
	for _, row := range rows {
		u := out[row.KeyID]
		if u == nil {
			u = &keyUsage{ByKind: map[string]int64{}}
			out[row.KeyID] = u
		}
		u.Last30 += row.Count
		u.ByKind[row.Kind] += row.Count
		if row.Day == today {
			u.Today += row.Count
		}
	}
	return out, nil
}

func (s *Server) view(k *store.APIKey, usage map[int64]*keyUsage) keyView {
	v := keyView{APIKey: k, Usage: keyUsage{ByKind: map[string]int64{}}}
	if u := usage[k.ID]; u != nil {
		v.Usage = *u
	}
	return v
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := s.d.Store.Keys(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	usage, err := s.keyUsage(r)
	if err != nil {
		storeError(w, r, err)
		return
	}
	out := make([]keyView, 0, len(ks))
	for _, k := range ks {
		out = append(out, s.view(k, usage))
	}
	writeJSON(w, http.StatusOK, out)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid id")
		return 0, false
	}
	return id, true
}

func (s *Server) getKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	k, err := s.d.Store.Key(r.Context(), id)
	if err != nil {
		storeError(w, r, err)
		return
	}
	usage, _ := s.keyUsage(r)
	writeJSON(w, http.StatusOK, s.view(k, usage))
}

type keyBody struct {
	Label  *string       `json:"label"`
	Limits *store.Limits `json:"limits"`
}

func validLabel(l string) bool { return strings.TrimSpace(l) != "" && len(l) <= 200 }

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var b keyBody
	if !readJSON(w, r, &b) {
		return
	}
	if b.Label == nil || !validLabel(*b.Label) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "label is required (1-200 characters)")
		return
	}
	lim := store.Limits{}
	if b.Limits != nil {
		lim = *b.Limits
	}
	k, secret, err := s.d.Keys.Create(r.Context(), strings.TrimSpace(*b.Label), lim)
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": s.view(k, nil), "secret": secret,
		"note": "Store the secret now: it is not shown again."})
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var b keyBody
	if !readJSON(w, r, &b) {
		return
	}
	k, err := s.d.Store.Key(r.Context(), id)
	if err != nil {
		storeError(w, r, err)
		return
	}
	label, lim := k.Label, k.Limits
	if b.Label != nil {
		if !validLabel(*b.Label) {
			writeError(w, r, http.StatusBadRequest, "bad_request", "label must be 1-200 characters")
			return
		}
		label = strings.TrimSpace(*b.Label)
	}
	if b.Limits != nil {
		lim = *b.Limits
	}
	if err := s.d.Keys.Update(r.Context(), id, label, lim); err != nil {
		storeError(w, r, err)
		return
	}
	s.getKey(w, r)
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.d.Keys.Revoke(r.Context(), id); err != nil {
		storeError(w, r, err)
		return
	}
	s.getKey(w, r)
}

// --- jobs and builds ---

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	js, err := s.d.Store.Jobs(r.Context(), limit)
	if err != nil {
		storeError(w, r, err)
		return
	}
	if cur, ok := s.d.Jobs.Current(); ok {
		for i, j := range js {
			if j.ID == cur.ID {
				js[i] = &cur
			}
		}
	}
	writeJSON(w, http.StatusOK, js)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	j, err := s.d.Store.Job(r.Context(), id)
	if err != nil {
		storeError(w, r, err)
		return
	}
	if cur, ok := s.d.Jobs.Current(); ok && cur.ID == id {
		j = &cur
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	logs, err := s.d.Store.JobLogTail(r.Context(), id, tail)
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j, "log": logs})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.d.Jobs.Cancel(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusConflict, "not_cancellable", "job is not queued or running")
			return
		}
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) eventSnapshot() []jobs.Event {
	var ev []jobs.Event
	if cur, ok := s.d.Jobs.Current(); ok {
		ev = append(ev, jobs.Event{Type: "job", Data: cur})
	}
	if s.d.Tiles != nil {
		ev = append(ev, jobs.Event{Type: "generation", Data: map[string]any{"id": s.d.Tiles.Generation()}})
	}
	return ev
}

func (s *Server) startBuild(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Refresh bool `json:"refresh"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &b) {
		return
	}
	if !s.d.Jobs.Registered(scheduler.BuildJob) {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "the build pipeline is not available in this binary")
		return
	}
	row, err := s.d.Jobs.Enqueue(r.Context(), scheduler.BuildJob, map[string]any{"refresh": b.Refresh}, jobs.EnqueueOptions{Trigger: "manual", Unique: true})
	if errors.Is(err, jobs.ErrBusy) {
		writeError(w, r, http.StatusConflict, "busy", err.Error())
		return
	}
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, row)
}

// generationView merges what is on disk with what the store recorded.
type generationView struct {
	ID        string          `json:"id"`
	Live      bool            `json:"live"`
	Status    string          `json:"status"`
	CreatedAt *time.Time      `json:"created_at,omitempty"`
	SizeBytes int64           `json:"size_bytes"`
	OnDisk    bool            `json:"on_disk"`
	Manifest  json.RawMessage `json:"manifest,omitempty"`
}

func (s *Server) generations(w http.ResponseWriter, r *http.Request) {
	live, _, _ := tiles.CurrentGeneration(s.d.DataDir)
	byID := map[string]*generationView{}
	recs, err := s.d.Store.Generations(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	for _, g := range recs {
		t := g.CreatedAt
		byID[g.ID] = &generationView{ID: g.ID, Status: g.Status, CreatedAt: &t, SizeBytes: g.SizeBytes, Manifest: g.Manifest}
	}
	sizes := s.diskCtl.get().Generations
	ents, _ := os.ReadDir(filepath.Join(s.d.DataDir, "generations"))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		v := byID[e.Name()]
		if v == nil {
			v = &generationView{ID: e.Name(), Status: "kept"}
			if fi, err := e.Info(); err == nil {
				t := fi.ModTime().UTC()
				v.CreatedAt = &t
			}
			byID[e.Name()] = v
		}
		v.OnDisk = true
		if n, ok := sizes[e.Name()]; ok {
			v.SizeBytes = n
		}
		if m, err := os.ReadFile(filepath.Join(s.d.DataDir, "generations", e.Name(), "manifest.json")); err == nil && json.Valid(m) {
			v.Manifest = m
		}
	}
	out := make([]*generationView, 0, len(byID))
	for _, v := range byID {
		v.Live = v.ID == live
		if v.Live {
			v.Status = "live"
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].CreatedAt, out[j].CreatedAt
		if ti == nil || tj == nil {
			return out[i].ID > out[j].ID
		}
		return ti.After(*tj)
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h, err := s.d.Store.History(r.Context(), r.URL.Query().Get("kind"), limit)
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// --- schedule ---

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	st := s.d.Scheduler.State()
	hist, err := s.d.Store.History(r.Context(), "schedule", 20)
	if err != nil {
		storeError(w, r, err)
		return
	}
	regions, err := s.d.Store.Regions(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	refresh := map[string]bool{}
	for _, rg := range regions {
		refresh[rg.ID] = rg.Refresh
	}
	var upcoming []time.Time
	if st.Enabled {
		upcoming, _ = scheduler.NextRuns(st.Cron, time.Now(), 5)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cron": st.Cron, "enabled": st.Enabled, "next_run": st.NextRun, "last_run": st.LastRun,
		"default_cron": st.Default, "upcoming": upcoming, "timezone": "UTC", "history": hist, "regions": refresh,
	})
}

func (s *Server) putSchedule(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Cron    *string         `json:"cron"`
		Enabled *bool           `json:"enabled"`
		Regions map[string]bool `json:"regions"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	st := s.d.Scheduler.State()
	expr, enabled := st.Cron, st.Enabled
	if b.Cron != nil {
		expr = strings.TrimSpace(*b.Cron)
	}
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	if _, err := scheduler.Parse(expr); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_cron", err.Error())
		return
	}
	for id, on := range b.Regions {
		if err := s.d.Store.SetRegionRefresh(r.Context(), id, on); err != nil {
			storeError(w, r, err)
			return
		}
	}
	if err := s.d.Scheduler.Set(r.Context(), expr, enabled); err != nil {
		storeError(w, r, err)
		return
	}
	s.getSchedule(w, r)
}

// --- regions ---

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	if s.d.Catalog == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "the region catalog is not available")
		return
	}
	c, err := s.d.Catalog(r.Context())
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "catalog_error", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	rs, err := s.d.Store.Regions(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (s *Server) getRegion(w http.ResponseWriter, r *http.Request) {
	rg, err := s.d.Store.Region(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rg)
}

var (
	regionIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)*$`)
	slugRE     = regexp.MustCompile(`[^a-z0-9]+`)
)

// addRegion: POST {"id":"us/texas"} for a catalog extract, or
// {"name":"Permian core","bbox":[w,s,e,n]|"polygon":{GeoJSON},"parent":"us/texas"} for a custom area.
func (s *Server) addRegion(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID      string          `json:"id"`
		Name    string          `json:"name"`
		Parent  string          `json:"parent"`
		BBox    []float64       `json:"bbox"`
		Polygon json.RawMessage `json:"polygon"`
		Refresh *bool           `json:"refresh"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	rg := &store.Region{Refresh: true, Parent: b.Parent}
	if b.Refresh != nil {
		rg.Refresh = *b.Refresh
	}
	custom := len(b.BBox) > 0 || len(b.Polygon) > 0
	switch {
	case custom:
		if strings.TrimSpace(b.Name) == "" {
			writeError(w, r, http.StatusBadRequest, "bad_request", "a custom region needs a name")
			return
		}
		if len(b.BBox) > 0 {
			if len(b.BBox) != 4 || b.BBox[0] >= b.BBox[2] || b.BBox[1] >= b.BBox[3] || b.BBox[0] < -180 || b.BBox[2] > 180 || b.BBox[1] < -90 || b.BBox[3] > 90 {
				writeError(w, r, http.StatusBadRequest, "bad_request", "bbox must be [w,s,e,n] with w<e and s<n")
				return
			}
		}
		if len(b.Polygon) > 0 && !json.Valid(b.Polygon) {
			writeError(w, r, http.StatusBadRequest, "bad_request", "polygon must be a GeoJSON geometry")
			return
		}
		rg.Kind, rg.Name, rg.BBox, rg.Polygon = "custom", strings.TrimSpace(b.Name), b.BBox, b.Polygon
		rg.ID = b.ID
		if rg.ID == "" {
			rg.ID = "custom/" + strings.Trim(slugRE.ReplaceAllString(strings.ToLower(rg.Name), "-"), "-")
		}
	default:
		rg.Kind, rg.ID, rg.Name = "extract", b.ID, b.Name
	}
	if !regionIDRE.MatchString(rg.ID) || len(rg.ID) > 200 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid region id")
		return
	}
	if rg.Kind == "extract" && s.d.ValidateRegion != nil {
		name, parent, err := s.d.ValidateRegion(r.Context(), rg.ID)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "unknown_region", err.Error())
			return
		}
		if rg.Name == "" {
			rg.Name = name
		}
		if rg.Parent == "" {
			rg.Parent = parent
		}
	}
	if rg.Name == "" {
		rg.Name = rg.ID
	}
	if err := s.d.Store.UpsertRegion(r.Context(), rg); err != nil {
		storeError(w, r, err)
		return
	}
	saved, err := s.d.Store.Region(r.Context(), rg.ID)
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) patchRegion(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Refresh *bool `json:"refresh"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Refresh != nil {
		if err := s.d.Store.SetRegionRefresh(r.Context(), r.PathValue("id"), *b.Refresh); err != nil {
			storeError(w, r, err)
			return
		}
	}
	s.getRegion(w, r)
}

func (s *Server) deleteRegion(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Store.DeleteRegion(r.Context(), r.PathValue("id")); err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "the region leaves the map at the next build"})
}

func (s *Server) restartChild(w http.ResponseWriter, r *http.Request) {
	if s.d.Supervisor == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "no supervisor")
		return
	}
	if err := s.d.Supervisor.Restart(r.PathValue("name")); err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}
