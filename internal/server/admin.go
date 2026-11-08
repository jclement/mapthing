package server

// Admin API handlers. The JSON shapes are the ones the admin UI uses (web/src/api.ts) and are
// documented in docs/architecture.md, "Admin UI and admin API shapes".

import (
	"context"
	"database/sql"
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

// CatalogRegion is what the catalog knows about an installable region (see Deps.ValidateRegion).
type CatalogRegion struct {
	Name             string
	Parent           string
	BBox             []float64 // [w,s,e,n]
	ExtractBytes     int64
	ElevationSources []string
}

// ---------- keys ----------

type keyLimitsJSON struct {
	RPM     *int     `json:"rpm"`
	RPD     *int     `json:"rpd"`
	Origins []string `json:"origins"`
}

type keyUsageJSON struct {
	Today int64   `json:"today"`
	Total int64   `json:"total"`
	Daily []int64 `json:"daily"` // last 14 UTC days, oldest first
}

type keyJSON struct {
	ID         string        `json:"id"`
	Label      string        `json:"label"`
	Prefix     string        `json:"prefix"`
	CreatedAt  time.Time     `json:"created_at"`
	LastUsedAt *time.Time    `json:"last_used_at"`
	RevokedAt  *time.Time    `json:"revoked_at,omitempty"`
	Limits     keyLimitsJSON `json:"limits"`
	Usage      keyUsageJSON  `json:"usage"`
}

const keyDays = 14

func limitsJSON(l store.Limits) keyLimitsJSON {
	out := keyLimitsJSON{Origins: l.AllowedOrigins}
	if out.Origins == nil {
		out.Origins = []string{}
	}
	if l.RPM > 0 {
		v := l.RPM
		out.RPM = &v
	}
	if l.RPD > 0 {
		v := l.RPD
		out.RPD = &v
	}
	return out
}

// keyUsage returns per-key usage: today, all-time total and the last 14 days.
func (s *Server) keyUsage(ctx context.Context) (map[int64]*keyUsageJSON, error) {
	rows, err := s.d.Store.UsageSince(ctx, "0000-00-00")
	if err != nil {
		return nil, err
	}
	pending, _ := s.d.Stats.Pending()
	for k, n := range pending {
		rows = append(rows, store.UsageRow{KeyID: k.KeyID, Day: k.Day, Kind: k.Kind, Count: n})
	}
	now := time.Now().UTC()
	dayIdx := map[string]int{}
	for i := range keyDays {
		dayIdx[now.AddDate(0, 0, -(keyDays-1-i)).Format("2006-01-02")] = i
	}
	out := map[int64]*keyUsageJSON{}
	for _, r := range rows {
		u := out[r.KeyID]
		if u == nil {
			u = &keyUsageJSON{Daily: make([]int64, keyDays)}
			out[r.KeyID] = u
		}
		u.Total += r.Count
		if i, ok := dayIdx[r.Day]; ok {
			u.Daily[i] += r.Count
			if i == keyDays-1 {
				u.Today += r.Count
			}
		}
	}
	return out, nil
}

func keyView(k *store.APIKey, usage map[int64]*keyUsageJSON) keyJSON {
	v := keyJSON{ID: strconv.FormatInt(k.ID, 10), Label: k.Label, Prefix: k.Prefix, CreatedAt: k.CreatedAt,
		LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt, Limits: limitsJSON(k.Limits),
		Usage: keyUsageJSON{Daily: make([]int64, keyDays)}}
	if u := usage[k.ID]; u != nil {
		v.Usage = *u
	}
	return v
}

// listKeys: GET /admin/api/keys → {keys:[…]}. Revoked keys are left out (?revoked=1 includes them).
func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := s.d.Store.Keys(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	usage, err := s.keyUsage(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	all := r.URL.Query().Get("revoked") == "1"
	out := []keyJSON{}
	for _, k := range ks {
		if k.RevokedAt != nil && !all {
			continue
		}
		out = append(out, keyView(k, usage))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
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
	usage, _ := s.keyUsage(r.Context())
	writeJSON(w, http.StatusOK, keyView(k, usage))
}

// keyBody is the create/edit body: {label, rpm, rpd, origins} (null or 0 = unlimited).
type keyBody struct {
	Label   *string   `json:"label"`
	RPM     *int      `json:"rpm"`
	RPD     *int      `json:"rpd"`
	Origins *[]string `json:"origins"`
}

func (b keyBody) apply(l store.Limits) store.Limits {
	if b.RPM != nil {
		l.RPM = *b.RPM
	}
	if b.RPD != nil {
		l.RPD = *b.RPD
	}
	if b.Origins != nil {
		l.AllowedOrigins = *b.Origins
	}
	return l
}

// readKeyBody decodes a key body; rpm/rpd may be null (unlimited), which leaves the pointer
// nil, so null is mapped to 0 explicitly via a raw pass.
func readKeyBody(w http.ResponseWriter, r *http.Request) (keyBody, bool) {
	var raw map[string]json.RawMessage
	if !readJSON(w, r, &raw) {
		return keyBody{}, false
	}
	var b keyBody
	zero := 0
	for name, ptr := range map[string]**int{"rpm": &b.RPM, "rpd": &b.RPD} {
		v, ok := raw[name]
		if !ok {
			continue
		}
		if string(v) == "null" {
			*ptr = &zero
			continue
		}
		var n int
		if json.Unmarshal(v, &n) != nil || n < 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", name+" must be a non-negative integer or null")
			return b, false
		}
		*ptr = &n
	}
	if v, ok := raw["label"]; ok {
		var l string
		if json.Unmarshal(v, &l) != nil {
			writeError(w, r, http.StatusBadRequest, "bad_request", "label must be a string")
			return b, false
		}
		b.Label = &l
	}
	if v, ok := raw["origins"]; ok && string(v) != "null" {
		var o []string
		if json.Unmarshal(v, &o) != nil {
			writeError(w, r, http.StatusBadRequest, "bad_request", "origins must be a list of strings")
			return b, false
		}
		b.Origins = &o
	}
	return b, true
}

func validLabel(l string) bool { return strings.TrimSpace(l) != "" && len(l) <= 200 }

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	b, ok := readKeyBody(w, r)
	if !ok {
		return
	}
	if b.Label == nil || !validLabel(*b.Label) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "label is required (1-200 characters)")
		return
	}
	k, secret, err := s.d.Keys.Create(r.Context(), strings.TrimSpace(*b.Label), b.apply(store.Limits{}))
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": keyView(k, nil), "secret": secret})
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	b, ok := readKeyBody(w, r)
	if !ok {
		return
	}
	k, err := s.d.Store.Key(r.Context(), id)
	if err != nil {
		storeError(w, r, err)
		return
	}
	label := k.Label
	if b.Label != nil {
		if !validLabel(*b.Label) {
			writeError(w, r, http.StatusBadRequest, "bad_request", "label must be 1-200 characters")
			return
		}
		label = strings.TrimSpace(*b.Label)
	}
	if err := s.d.Keys.Update(r.Context(), id, label, b.apply(k.Limits)); err != nil {
		storeError(w, r, err)
		return
	}
	s.getKey(w, r)
}

// revokeKey: DELETE /admin/api/keys/{id} → 204. The key stops working at once; its row stays.
func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.d.Keys.Revoke(r.Context(), id); err != nil {
		storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- jobs and builds ----------

func (s *Server) jobRow(ctx context.Context, row *store.JobRow) store.JobRow {
	if cur, ok := s.d.Jobs.Current(); ok && cur.ID == row.ID {
		return cur
	}
	_ = ctx
	return *row
}

// listJobs: GET /admin/api/jobs → {jobs:[…]} newest first.
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	js, err := s.d.Store.Jobs(r.Context(), limit)
	if err != nil {
		storeError(w, r, err)
		return
	}
	out := make([]jobs.JobJSON, 0, len(js))
	for _, j := range js {
		out = append(out, jobs.View(s.jobRow(r.Context(), j)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// getJob: GET /admin/api/jobs/{id} → job + log tail.
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
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	logs, err := s.d.Store.JobLogTail(r.Context(), id, tail)
	if err != nil {
		storeError(w, r, err)
		return
	}
	lines := make([]jobs.LogJSON, 0, len(logs))
	for _, l := range logs {
		lines = append(lines, jobs.LogView(l))
	}
	writeJSON(w, http.StatusOK, struct {
		jobs.JobJSON
		Log []jobs.LogJSON `json:"log"`
	}{jobs.View(s.jobRow(r.Context(), j)), lines})
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
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) eventSnapshot() []jobs.Event {
	var ev []jobs.Event
	if cur, ok := s.d.Jobs.Current(); ok {
		ev = append(ev, jobs.Event{Type: "job", Data: jobs.View(cur)})
	}
	return ev
}

// startBuild: POST /admin/api/build {refresh_downloads} → 202 job (409 if one is queued/running).
func (s *Server) startBuild(w http.ResponseWriter, r *http.Request) {
	var b struct {
		RefreshDownloads bool `json:"refresh_downloads"`
		Refresh          bool `json:"refresh"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &b) {
		return
	}
	if !s.d.Jobs.Registered(scheduler.BuildJob) {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "the build pipeline is not available in this binary")
		return
	}
	row, err := s.d.Jobs.Enqueue(r.Context(), scheduler.BuildJob, map[string]any{"refresh": b.RefreshDownloads || b.Refresh}, jobs.EnqueueOptions{Trigger: "manual", Unique: true})
	if errors.Is(err, jobs.ErrBusy) {
		writeError(w, r, http.StatusConflict, "busy", err.Error())
		return
	}
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, jobs.View(*row))
}

// ---------- generations ----------

type manifestJSON struct {
	Regions []manifestRegionJSON `json:"regions"`
	Sizes   map[string]int64     `json:"sizes"` // basemap, terrain, contours, routing, total
	Timings map[string]float64   `json:"timings"`
	Checks  []checkJSON          `json:"checks"`
	Raw     json.RawMessage      `json:"raw,omitempty"` // the pipeline's manifest as written
}

type manifestRegionJSON struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	DataDate *string   `json:"data_date"`
	BBox     []float64 `json:"bbox,omitempty"`
}

type checkJSON struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// pipelineManifest is the subset of the build's manifest.json that the admin API reads.
type pipelineManifest struct {
	Regions []struct {
		ID              string    `json:"id"`
		Name            string    `json:"name"`
		BBox            []float64 `json:"bbox"`
		SourceTimestamp string    `json:"source_timestamp"`
	} `json:"regions"`
	Stages []struct {
		Name      string  `json:"name"`
		DurationS float64 `json:"duration_s"`
		Skipped   bool    `json:"skipped"`
	} `json:"stages"`
	Checks []struct {
		Name   string `json:"name"`
		Region string `json:"region"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	} `json:"checks"`
	CreatedAt time.Time `json:"created_at"`
}

func readManifest(genDir string) (*pipelineManifest, json.RawMessage) {
	b, err := os.ReadFile(filepath.Join(genDir, "manifest.json"))
	if err != nil || !json.Valid(b) {
		return &pipelineManifest{}, nil
	}
	var m pipelineManifest
	_ = json.Unmarshal(b, &m)
	return &m, b
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

func normaliseManifest(genDir string) manifestJSON {
	m, raw := readManifest(genDir)
	out := manifestJSON{Regions: []manifestRegionJSON{}, Timings: map[string]float64{}, Checks: []checkJSON{}, Raw: raw}
	for _, r := range m.Regions {
		mr := manifestRegionJSON{ID: r.ID, Name: r.Name, BBox: r.BBox}
		if r.SourceTimestamp != "" {
			ts := r.SourceTimestamp
			mr.DataDate = &ts
		}
		out.Regions = append(out.Regions, mr)
	}
	for _, st := range m.Stages {
		if !st.Skipped {
			out.Timings[st.Name] = st.DurationS
		}
	}
	for _, c := range m.Checks {
		name := c.Name
		if c.Region != "" {
			name += " (" + c.Region + ")"
		}
		out.Checks = append(out.Checks, checkJSON{Name: name, OK: c.OK, Detail: c.Detail})
	}
	basemap := fileSize(filepath.Join(genDir, "basemap.pmtiles"))
	terrain := fileSize(filepath.Join(genDir, "terrain.pmtiles"))
	routing := dirSize(filepath.Join(genDir, "valhalla"))
	contours := fileSize(filepath.Join(genDir, "contours.pmtiles"))
	out.Sizes = map[string]int64{"basemap": basemap, "terrain": terrain, "contours": contours, "routing": routing, "total": dirSize(genDir)}
	return out
}

type generationJSON struct {
	ID        string       `json:"id"`
	CreatedAt time.Time    `json:"created_at"`
	Status    string       `json:"status"` // live | kept
	Manifest  manifestJSON `json:"manifest"`
}

// generations: GET /admin/api/generations → {generations:[…]} (those on disk), newest first.
func (s *Server) generations(w http.ResponseWriter, r *http.Request) {
	live, _, _ := tiles.CurrentGeneration(s.d.DataDir)
	out := []generationJSON{}
	ents, _ := os.ReadDir(filepath.Join(s.d.DataDir, "generations"))
	for _, e := range ents {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		dir := filepath.Join(s.d.DataDir, "generations", e.Name())
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		g := generationJSON{ID: e.Name(), Status: "kept", Manifest: normaliseManifest(dir)}
		if m, _ := readManifest(dir); !m.CreatedAt.IsZero() {
			g.CreatedAt = m.CreatedAt.UTC()
		} else if fi, err := e.Info(); err == nil {
			g.CreatedAt = fi.ModTime().UTC()
		}
		if e.Name() == live {
			g.Status = "live"
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"generations": out})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h, err := s.d.Store.History(r.Context(), r.URL.Query().Get("kind"), limit)
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": h})
}

// ---------- schedule ----------

type scheduleRegion struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	st := s.d.Scheduler.State()
	regions, err := s.d.Store.Regions(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	rs := make([]scheduleRegion, 0, len(regions))
	for _, rg := range regions {
		rs = append(rs, scheduleRegion{ID: rg.ID, Enabled: rg.Refresh})
	}
	hist, err := s.d.Store.History(r.Context(), "schedule", 20)
	if err != nil {
		storeError(w, r, err)
		return
	}
	upcoming := []time.Time{}
	if st.Enabled {
		upcoming, _ = scheduler.NextRuns(st.Cron, time.Now(), 5)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cron": st.Cron, "enabled": st.Enabled, "refresh_downloads": s.d.Scheduler.RefreshDownloads(r.Context()),
		"regions": rs,
		// extras (read-only)
		"next_run": st.NextRun, "last_run": st.LastRun, "default_cron": st.Default, "upcoming": upcoming,
		"timezone": "UTC", "history": hist,
	})
}

// putSchedule: PUT {cron, enabled, refresh_downloads, regions:[{id,enabled}]}; any field may be
// left out. Read-only extras sent back by a client are ignored.
func (s *Server) putSchedule(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Cron             *string          `json:"cron"`
		Enabled          *bool            `json:"enabled"`
		RefreshDownloads *bool            `json:"refresh_downloads"`
		Regions          []scheduleRegion `json:"regions"`
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
	for _, rg := range b.Regions {
		if err := s.d.Store.SetRegionRefresh(r.Context(), rg.ID, rg.Enabled); err != nil && !errors.Is(err, store.ErrNotFound) {
			storeError(w, r, err)
			return
		}
	}
	if b.RefreshDownloads != nil {
		if err := s.d.Scheduler.SetRefreshDownloads(r.Context(), *b.RefreshDownloads); err != nil {
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

// ---------- regions ----------

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

type regionJSON struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	CatalogID        *string      `json:"catalog_id"`
	Parent           string       `json:"parent,omitempty"`
	BBox             []float64    `json:"bbox"`
	Polygon          [][2]float64 `json:"polygon,omitempty"`
	Status           string       `json:"status"` // pending | installed | failed
	ExtractBytes     int64        `json:"extract_bytes"`
	DiskBytes        int64        `json:"disk_bytes"`
	DataDate         *string      `json:"data_date"`
	DownloadedAt     *time.Time   `json:"downloaded_at"`
	ElevationSources []string     `json:"elevation_sources"`
	Refresh          bool         `json:"refresh"`
	AddedAt          time.Time    `json:"added_at"`
}

// regionMeta is the JSON core keeps in regions.meta (from the catalog at add time).
type regionMeta struct {
	ExtractBytes     int64    `json:"extract_bytes,omitempty"`
	ElevationSources []string `json:"elevation_sources,omitempty"`
}

// downloadMeta is the subset of the pipeline's downloads/<region>/meta.json read here.
type downloadMeta struct {
	Size            int64     `json:"size"`
	SourceTimestamp string    `json:"source_timestamp"`
	DownloadedAt    time.Time `json:"downloaded_at"`
}

// regionContext is what every region view needs, computed once per request.
type regionContext struct {
	live       map[string]manifestRegionJSON
	lastFailed bool // the latest finished build failed
	downloads  map[string]int64
}

func (s *Server) regionContext(ctx context.Context) regionContext {
	rc := regionContext{live: map[string]manifestRegionJSON{}, downloads: s.diskCtl.get().Downloads}
	if _, dir, ok := tiles.CurrentGeneration(s.d.DataDir); ok {
		for _, r := range normaliseManifest(dir).Regions {
			rc.live[r.ID] = r
		}
	}
	var status string
	err := s.d.Store.DB.QueryRowContext(ctx, `SELECT status FROM jobs WHERE name = ? AND status IN ('succeeded','failed') ORDER BY id DESC LIMIT 1`, scheduler.BuildJob).Scan(&status)
	rc.lastFailed = err == nil && status == "failed"
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = err
	}
	return rc
}

var (
	regionIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)*$`)
	slugRE     = regexp.MustCompile(`[^a-z0-9]+`)
)

func (s *Server) regionView(rg *store.Region, rc regionContext) regionJSON {
	v := regionJSON{ID: rg.ID, Name: rg.Name, Parent: rg.Parent, BBox: rg.BBox, Status: "pending",
		ElevationSources: []string{}, Refresh: rg.Refresh, AddedAt: rg.AddedAt}
	if rg.Kind == "extract" {
		id := rg.ID
		v.CatalogID = &id
	}
	var meta regionMeta
	_ = json.Unmarshal(rg.Meta, &meta)
	v.ExtractBytes = meta.ExtractBytes
	if meta.ElevationSources != nil {
		v.ElevationSources = meta.ElevationSources
	}
	v.Polygon = polygonRing(rg.Polygon)
	if b, err := os.ReadFile(filepath.Join(s.d.DataDir, "downloads", filepath.FromSlash(rg.ID), "meta.json")); err == nil {
		var dm downloadMeta
		if json.Unmarshal(b, &dm) == nil {
			if dm.SourceTimestamp != "" {
				ts := dm.SourceTimestamp
				v.DataDate = &ts
			}
			if !dm.DownloadedAt.IsZero() {
				t := dm.DownloadedAt.UTC()
				v.DownloadedAt = &t
			}
			if v.ExtractBytes == 0 {
				v.ExtractBytes = dm.Size
			}
		}
	}
	v.DiskBytes = rc.downloads[rg.ID]
	if lr, ok := rc.live[rg.ID]; ok {
		v.Status = "installed"
		if v.DataDate == nil {
			v.DataDate = lr.DataDate
		}
		if len(v.BBox) != 4 && len(lr.BBox) == 4 {
			v.BBox = lr.BBox
		}
	} else if rc.lastFailed {
		v.Status = "failed"
	}
	if len(v.BBox) != 4 {
		v.BBox = bboxOf(v.Polygon)
	}
	return v
}

// polygonRing accepts a stored polygon as [[lon,lat],…] or a GeoJSON Polygon and returns the
// outer ring.
func polygonRing(raw json.RawMessage) [][2]float64 {
	if len(raw) == 0 {
		return nil
	}
	var ring [][2]float64
	if json.Unmarshal(raw, &ring) == nil {
		return ring
	}
	var g struct {
		Type        string           `json:"type"`
		Coordinates [][][2]float64   `json:"coordinates"`
		Geometry    *json.RawMessage `json:"geometry"`
	}
	if json.Unmarshal(raw, &g) == nil {
		if g.Geometry != nil {
			return polygonRing(*g.Geometry)
		}
		if g.Type == "Polygon" && len(g.Coordinates) > 0 {
			return g.Coordinates[0]
		}
	}
	return nil
}

func bboxOf(ring [][2]float64) []float64 {
	if len(ring) == 0 {
		return []float64{0, 0, 0, 0}
	}
	b := []float64{ring[0][0], ring[0][1], ring[0][0], ring[0][1]}
	for _, p := range ring {
		b[0], b[1] = min(b[0], p[0]), min(b[1], p[1])
		b[2], b[3] = max(b[2], p[0]), max(b[3], p[1])
	}
	return b
}

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	rs, err := s.d.Store.Regions(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	rc := s.regionContext(r.Context())
	out := make([]regionJSON, 0, len(rs))
	for _, rg := range rs {
		out = append(out, s.regionView(rg, rc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"regions": out})
}

func (s *Server) getRegion(w http.ResponseWriter, r *http.Request) {
	rg, err := s.d.Store.Region(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.regionView(rg, s.regionContext(r.Context())))
}

// addRegion: POST {"id":"north-america/us/texas"} for a catalog extract, or
// {"name":"Permian core","bbox":[w,s,e,n]} / {"name":…,"polygon":[[lon,lat],…]} for a custom
// area → 201 region; 409 when the id is already installed.
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
	rg := &store.Region{Refresh: true, Parent: b.Parent, Meta: json.RawMessage("{}")}
	if b.Refresh != nil {
		rg.Refresh = *b.Refresh
	}
	custom := len(b.BBox) > 0 || (len(b.Polygon) > 0 && string(b.Polygon) != "null")
	if custom {
		if strings.TrimSpace(b.Name) == "" {
			writeError(w, r, http.StatusBadRequest, "bad_request", "a custom region needs a name")
			return
		}
		ring := polygonRing(b.Polygon)
		if len(b.Polygon) > 0 && string(b.Polygon) != "null" && len(ring) < 3 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "polygon must be [[lon,lat],…] (3+ points) or a GeoJSON Polygon")
			return
		}
		if len(b.BBox) == 0 {
			b.BBox = bboxOf(ring)
		}
		if len(b.BBox) != 4 || b.BBox[0] >= b.BBox[2] || b.BBox[1] >= b.BBox[3] || b.BBox[0] < -180 || b.BBox[2] > 180 || b.BBox[1] < -90 || b.BBox[3] > 90 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "bbox must be [w,s,e,n] with w<e and s<n")
			return
		}
		rg.Kind, rg.Name, rg.BBox = "custom", strings.TrimSpace(b.Name), b.BBox
		if len(ring) > 0 {
			rg.Polygon, _ = json.Marshal(ring)
		}
		rg.ID = b.ID
		if rg.ID == "" {
			rg.ID = "custom/" + strings.Trim(slugRE.ReplaceAllString(strings.ToLower(rg.Name), "-"), "-")
		}
	} else {
		rg.Kind, rg.ID, rg.Name = "extract", b.ID, b.Name
	}
	if !regionIDRE.MatchString(rg.ID) || len(rg.ID) > 200 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid region id")
		return
	}
	if _, err := s.d.Store.Region(r.Context(), rg.ID); err == nil {
		writeError(w, r, http.StatusConflict, "exists", "that region is already installed")
		return
	}
	if rg.Kind == "extract" && s.d.ValidateRegion != nil {
		info, err := s.d.ValidateRegion(r.Context(), rg.ID)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "unknown_region", err.Error())
			return
		}
		if rg.Name == "" {
			rg.Name = info.Name
		}
		if rg.Parent == "" {
			rg.Parent = info.Parent
		}
		if len(info.BBox) == 4 {
			rg.BBox = info.BBox
		}
		rg.Meta, _ = json.Marshal(regionMeta{ExtractBytes: info.ExtractBytes, ElevationSources: info.ElevationSources})
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
	writeJSON(w, http.StatusCreated, s.regionView(saved, s.regionContext(r.Context())))
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

// deleteRegion: DELETE /admin/api/regions/{id} → 204. The region leaves the map at the next build.
func (s *Server) deleteRegion(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Store.DeleteRegion(r.Context(), r.PathValue("id")); err != nil {
		storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	w.WriteHeader(http.StatusNoContent)
}
