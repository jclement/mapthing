package server

// Admin API handlers. The JSON shapes are the ones the admin UI uses (web/src/api.ts) and are
// documented in docs/architecture.md, "Admin UI and admin API shapes".

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/logs"
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

// CatalogParent is one Geofabrik extract a custom region is clipped from.
type CatalogParent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	Estimated bool   `json:"estimated,omitempty"` // size guessed from area (not yet known)
}

// CustomEstimate is what the pipeline plans for a custom area (see Deps.EstimateCustom).
type CustomEstimate struct {
	Parents          []CatalogParent `json:"parents"`
	TotalBytes       int64           `json:"total_bytes"` // download size: every parent extract
	Estimated        bool            `json:"estimated,omitempty"`
	SingleBest       *CatalogParent  `json:"single_best,omitempty"` // cheapest single extract covering it
	ElevationSources []string        `json:"elevation_sources"`
}

// ErrNotCovered is returned by EstimateCustom when no extract covers the area.
var ErrNotCovered = errors.New("no extract covers that area")

// estimateCustom: POST /admin/api/catalog/estimate {bbox:[w,s,e,n]} or {polygon:[[lon,lat],…]}
// → the parent extracts and total download for a custom region, before adding it.
func (s *Server) estimateCustom(w http.ResponseWriter, r *http.Request) {
	if s.d.EstimateCustom == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "the region catalog is not available")
		return
	}
	var b struct {
		BBox    []float64       `json:"bbox"`
		Polygon json.RawMessage `json:"polygon"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	ring := polygonRing(b.Polygon)
	if len(b.BBox) == 0 && len(ring) >= 3 {
		b.BBox = bboxOf(ring)
	}
	if !validBBox(b.BBox) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "bbox must be [w,s,e,n] with w<e and s<n, or polygon [[lon,lat],…] (3+ points)")
		return
	}
	est, err := s.d.EstimateCustom(r.Context(), b.BBox, ring)
	if errors.Is(err, ErrNotCovered) {
		writeError(w, r, http.StatusBadRequest, "not_covered", "no Geofabrik extract covers that area")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "catalog_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, est)
}

func validBBox(b []float64) bool {
	return len(b) == 4 && b[0] < b[2] && b[1] < b[3] && b[0] >= -180 && b[2] <= 180 && b[1] >= -90 && b[3] <= 90
}

// ---------- keys ----------

// keyLimitsJSON is a key's limits. null means unlimited. Rates and quotas are in cost units
// (a request costs its kind's weight: GET /admin/api/limits). rpm and rpd are the older request
// counts, still accepted on write (rps = rpm/60 with a burst of rpm; daily_quota = rpd) and
// derived on read.
type keyLimitsJSON struct {
	RPM          *int     `json:"rpm"`
	RPD          *int     `json:"rpd"`
	RPS          *float64 `json:"rps"`
	Burst        *int     `json:"burst"`
	DailyQuota   *int64   `json:"daily_quota"`
	MonthlyQuota *int64   `json:"monthly_quota"`
	IPRPS        *float64 `json:"ip_rps"`
	IPBurst      *int     `json:"ip_burst"`
	Origins      []string `json:"origins"`
}

type keyUsageJSON struct {
	Today int64   `json:"today"`
	Total int64   `json:"total"`
	Daily []int64 `json:"daily"` // requests, last 14 UTC days, oldest first
	// Against the limits: cost units used in the current UTC day and month, and what is left
	// in the rate bucket now (null without a rate limit).
	UnitsToday    int64  `json:"units_today"`
	UnitsMonth    int64  `json:"units_month"`
	RateRemaining *int64 `json:"rate_remaining"`
	// Requests answered 429: today, per day (last 14, oldest first), today by which limit,
	// and the most recent ones (newest last; these are lost at a restart).
	LimitedToday  int64            `json:"limited_today"`
	LimitedDaily  []int64          `json:"limited_daily"`
	LimitedScopes map[string]int64 `json:"limited_scopes"`
	Recent        []keys.Refusal   `json:"recent_limited"`
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
	l = l.Normalise()
	out := keyLimitsJSON{Origins: l.AllowedOrigins}
	if out.Origins == nil {
		out.Origins = []string{}
	}
	if l.RPS > 0 {
		rps, burst, rpm := l.RPS, l.Burst, int(math.Round(l.RPS*60))
		out.RPS, out.Burst, out.RPM = &rps, &burst, &rpm
	}
	if l.DailyQuota > 0 {
		q, rpd := l.DailyQuota, int(l.DailyQuota)
		out.DailyQuota, out.RPD = &q, &rpd
	}
	if l.MonthlyQuota > 0 {
		q := l.MonthlyQuota
		out.MonthlyQuota = &q
	}
	if l.IPRPS > 0 {
		rps, burst := l.IPRPS, l.IPBurst
		out.IPRPS, out.IPBurst = &rps, &burst
	}
	return out
}

func emptyUsage() *keyUsageJSON {
	return &keyUsageJSON{Daily: make([]int64, keyDays), LimitedDaily: make([]int64, keyDays),
		LimitedScopes: map[string]int64{}, Recent: []keys.Refusal{}}
}

// keyUsage returns per-key usage: requests today, in total and over the last 14 days, and
// the requests that were refused.
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
	get := func(id int64) *keyUsageJSON {
		u := out[id]
		if u == nil {
			u = emptyUsage()
			out[id] = u
		}
		return u
	}
	for _, r := range rows {
		u := get(r.KeyID)
		u.Total += r.Count
		if i, ok := dayIdx[r.Day]; ok {
			u.Daily[i] += r.Count
			if i == keyDays-1 {
				u.Today += r.Count
			}
		}
	}
	since := now.AddDate(0, 0, -(keyDays - 1)).Format("2006-01-02")
	limited, err := s.d.Store.LimitedSince(ctx, since)
	if err != nil {
		return nil, err
	}
	for k, n := range s.d.Keys.PendingLimited() {
		limited = append(limited, store.LimitedRow{KeyID: k.KeyID, Day: k.Day, Scope: k.Scope, Count: n})
	}
	for _, r := range limited {
		i, ok := dayIdx[r.Day]
		if !ok {
			continue
		}
		u := get(r.KeyID)
		u.LimitedDaily[i] += r.Count
		if i == keyDays-1 {
			u.LimitedToday += r.Count
			u.LimitedScopes[r.Scope] += r.Count
		}
	}
	return out, nil
}

func (s *Server) keyView(ctx context.Context, k *store.APIKey, usage map[int64]*keyUsageJSON) keyJSON {
	v := keyJSON{ID: strconv.FormatInt(k.ID, 10), Label: k.Label, Prefix: k.Prefix, CreatedAt: k.CreatedAt,
		LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt, Limits: limitsJSON(k.Limits), Usage: *emptyUsage()}
	if u := usage[k.ID]; u != nil {
		v.Usage = *u
	}
	live := s.d.Keys.UsageOf(ctx, k)
	v.Usage.UnitsToday, v.Usage.UnitsMonth, v.Usage.Recent = live.UnitsToday, live.UnitsMonth, live.Limited
	if live.RateRemaining >= 0 {
		n := live.RateRemaining
		v.Usage.RateRemaining = &n
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
		out = append(out, s.keyView(r.Context(), k, usage))
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
	writeJSON(w, http.StatusOK, s.keyView(r.Context(), k, usage))
}

// keyBody is the create/edit body: {label, rps, burst, daily_quota, monthly_quota, ip_rps,
// ip_burst, origins}, plus the older rpm and rpd. A field left out is unchanged; null or 0
// removes that limit.
type keyBody struct {
	Label   *string
	Origins *[]string
	nums    map[string]float64 // the numeric fields that were present (null → 0)
}

var keyNumFields = []string{"rpm", "rpd", "rps", "burst", "daily_quota", "monthly_quota", "ip_rps", "ip_burst"}

func (b keyBody) apply(l store.Limits) store.Limits {
	l = l.Normalise()
	set := func(name string) (float64, bool) { v, ok := b.nums[name]; return v, ok }
	// The older fields first, so the current ones win when both are sent.
	if v, ok := set("rpm"); ok {
		l.RPS, l.Burst = v/60, int(v)
	}
	if v, ok := set("rpd"); ok {
		l.DailyQuota = int64(v)
	}
	if v, ok := set("rps"); ok {
		l.RPS = v
		if _, hasBurst := set("burst"); !hasBurst {
			if _, legacy := set("rpm"); !legacy {
				l.Burst = 0 // the default for the new rate
			}
		}
	}
	if v, ok := set("burst"); ok {
		l.Burst = int(v)
	}
	if v, ok := set("daily_quota"); ok {
		l.DailyQuota = int64(v)
	}
	if v, ok := set("monthly_quota"); ok {
		l.MonthlyQuota = int64(v)
	}
	if v, ok := set("ip_rps"); ok {
		l.IPRPS = v
		if _, hasBurst := set("ip_burst"); !hasBurst {
			l.IPBurst = 0
		}
	}
	if v, ok := set("ip_burst"); ok {
		l.IPBurst = int(v)
	}
	if l.RPS <= 0 {
		l.Burst = 0
	}
	if l.IPRPS <= 0 {
		l.IPBurst = 0
	}
	if b.Origins != nil {
		l.AllowedOrigins = *b.Origins
	}
	return l.Normalise()
}

// readKeyBody decodes a key body. A limit may be a non-negative number or null (no limit).
func readKeyBody(w http.ResponseWriter, r *http.Request) (keyBody, bool) {
	var raw map[string]json.RawMessage
	if !readJSON(w, r, &raw) {
		return keyBody{}, false
	}
	b := keyBody{nums: map[string]float64{}}
	for _, name := range keyNumFields {
		v, ok := raw[name]
		if !ok {
			continue
		}
		if string(v) == "null" {
			b.nums[name] = 0
			continue
		}
		var n float64
		whole := name != "rps" && name != "ip_rps"
		if json.Unmarshal(v, &n) != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || n > 1e12 || (whole && n != math.Trunc(n)) {
			kind := "a non-negative number"
			if whole {
				kind = "a non-negative whole number"
			}
			writeError(w, r, http.StatusBadRequest, "bad_request", name+" must be "+kind+" or null")
			return b, false
		}
		b.nums[name] = n
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
	writeJSON(w, http.StatusCreated, map[string]any{"key": s.keyView(r.Context(), k, nil), "secret": secret})
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

// getLimits: GET /admin/api/limits → {weights, global, kinds, defaults}: what each kind of
// request costs, and the server-wide limits by kind.
func (s *Server) getLimits(w http.ResponseWriter, _ *http.Request) {
	cfg := s.d.Keys.Config()
	writeJSON(w, http.StatusOK, map[string]any{"weights": cfg.Weights, "global": cfg.Global, "kinds": keys.Kinds, "defaults": keys.DefaultConfig()})
}

// putLimits: PUT /admin/api/limits {weights:{kind:n}, global:{kind:{rps,burst}}} → the same
// shape as GET. A kind left out of weights keeps the default weight; a kind left out of
// global (or with rps 0) has no server-wide limit. 400 bad_limits.
func (s *Server) putLimits(w http.ResponseWriter, r *http.Request) {
	var c keys.Config
	if !readJSON(w, r, &c) {
		return
	}
	if err := s.d.Keys.SetConfig(r.Context(), c); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_limits", err.Error())
		return
	}
	s.getLimits(w, r)
}

// ---------- jobs and builds ----------

// redact masks secrets in text on its way out. Jobs redact what they store (jobs.Runner.Redact),
// but rows written by an older version, or by a runner without a redactor, are still raw.
func (s *Server) redact(text string) string {
	if text == "" {
		return text
	}
	if s.d.Logs != nil {
		return s.d.Logs.Redact(text)
	}
	return logs.Redact(text)
}

// jobView is jobs.View with the error and stage details masked.
func (s *Server) jobView(row store.JobRow) jobs.JobJSON {
	v := jobs.View(row)
	v.Error = s.redact(v.Error)
	if len(v.Stages) > 0 {
		st := make([]store.StageRow, len(v.Stages))
		copy(st, v.Stages)
		for i := range st {
			st[i].Detail = s.redact(st[i].Detail)
		}
		v.Stages = st
	}
	return v
}

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
		out = append(out, s.jobView(s.jobRow(r.Context(), j)))
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
		l.Line = s.redact(l.Line)
		lines = append(lines, jobs.LogView(l))
	}
	writeJSON(w, http.StatusOK, struct {
		jobs.JobJSON
		Log []jobs.LogJSON `json:"log"`
	}{s.jobView(s.jobRow(r.Context(), j)), lines})
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
		ev = append(ev, jobs.Event{Type: "job", Data: s.jobView(cur)})
	}
	return ev
}

// startBuild: POST /admin/api/build {refresh_downloads, rebuild_terrain} → 202 job (409 if one
// is queued/running). rebuild_terrain builds the elevation tiles again even when the live
// generation's could be reused.
func (s *Server) startBuild(w http.ResponseWriter, r *http.Request) {
	var b struct {
		RefreshDownloads bool `json:"refresh_downloads"`
		Refresh          bool `json:"refresh"`
		RebuildTerrain   bool `json:"rebuild_terrain"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &b) {
		return
	}
	if !s.d.Jobs.Registered(scheduler.BuildJob) {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "the build pipeline is not available in this binary")
		return
	}
	row, err := s.d.Jobs.Enqueue(r.Context(), scheduler.BuildJob, map[string]any{"refresh": b.RefreshDownloads || b.Refresh, "rebuild_terrain": b.RebuildTerrain}, jobs.EnqueueOptions{Trigger: "manual", Unique: true})
	if errors.Is(err, jobs.ErrBusy) {
		writeError(w, r, http.StatusConflict, "busy", err.Error())
		return
	}
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.jobView(*row))
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

// GenerationState is what the pipeline knows about one generation directory (see
// Deps.GenerationInventory).
type GenerationState struct {
	ID       string
	Status   string // live | previous | kept | building | incoming | invalid
	Owned    bool
	Valid    bool
	Problems []string
	Bytes    int64
}

type generationJSON struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// live: being served. previous: the one kept for rollback. kept: another complete one.
	// building: a build is writing it. incoming: copied in, never activated here. invalid:
	// incomplete or damaged (problems says why).
	Status   string       `json:"status"`
	Owned    bool         `json:"owned"`
	Problems []string     `json:"problems,omitempty"`
	Bytes    int64        `json:"bytes"` // size on disk
	Manifest manifestJSON `json:"manifest"`
	// What the server will accept for it right now.
	CanActivate bool `json:"can_activate"`
	CanDelete   bool `json:"can_delete"`
}

// generations: GET /admin/api/generations → {generations:[…], disk:{…}}: every generation
// directory, newest first.
func (s *Server) generations(w http.ResponseWriter, r *http.Request) {
	live, _, _ := tiles.CurrentGeneration(s.d.DataDir)
	states := map[string]GenerationState{}
	var diskBytes int64
	inventory := s.d.GenerationInventory != nil
	if inventory {
		var list []GenerationState
		list, diskBytes = s.d.GenerationInventory()
		for _, g := range list {
			states[g.ID] = g
		}
	}
	busy := s.d.Jobs != nil && s.d.Jobs.Busy(r.Context(), scheduler.BuildJob)
	out := []generationJSON{}
	var sum int64
	ents, _ := os.ReadDir(filepath.Join(s.d.DataDir, "generations"))
	for _, e := range ents {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue // a copy in flight (docs/operations.md), not a generation yet
		}
		dir := filepath.Join(s.d.DataDir, "generations", e.Name())
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		g := generationJSON{ID: e.Name(), Status: "kept", Owned: true, Manifest: normaliseManifest(dir)}
		if m, _ := readManifest(dir); !m.CreatedAt.IsZero() {
			g.CreatedAt = m.CreatedAt.UTC()
		} else if fi, err := e.Info(); err == nil {
			g.CreatedAt = fi.ModTime().UTC()
		}
		g.Bytes = g.Manifest.Sizes["total"]
		valid := true
		if st, ok := states[e.Name()]; ok {
			g.Status, g.Owned, g.Problems, valid = st.Status, st.Owned, st.Problems, st.Valid
		} else if inventory {
			// Not a real directory (a symbolic link): never activated or pruned.
			g.Status, g.Owned, valid = "invalid", false, false
			g.Problems = []string{"a symbolic link, not a generation directory"}
		}
		if e.Name() == live {
			g.Status = "live"
		}
		g.CanActivate = valid && g.Status != "live" && g.Status != "building" && !busy && s.d.ActivateGeneration != nil
		g.CanDelete = g.Status != "live" && g.Status != "building" && s.d.DeleteGeneration != nil
		sum += g.Bytes
		out = append(out, g)
	}
	if !inventory {
		diskBytes = sum
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	disk := s.diskCtl.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"generations": out,
		"disk": map[string]any{
			// bytes: what the generations occupy, a file shared by hard link (reused terrain)
			// counted once. sum_bytes: the sizes above added up.
			"bytes": diskBytes, "sum_bytes": sum,
			"free_bytes": disk.FreeBytes, "total_bytes": disk.TotalBytes,
		},
	})
}

// deleteGeneration: DELETE /admin/api/generations/{id} → 204. 404 no_generation; 409 live
// (the one being served) or busy (a build is writing it, or it is being activated).
func (s *Server) deleteGeneration(w http.ResponseWriter, r *http.Request) {
	if s.d.DeleteGeneration == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "this server cannot delete generations")
		return
	}
	id := r.PathValue("id")
	err := s.d.DeleteGeneration(r.Context(), id)
	var ae *ActivateError
	if errors.As(err, &ae) {
		writeError(w, r, ae.Status, ae.Code, ae.Message)
		return
	}
	if err != nil {
		slog.Error("delete generation", "generation", id, "err", err)
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	slog.Info("generation deleted", "generation", id)
	_ = s.d.Store.AddHistory(r.Context(), store.HistoryEntry{Kind: "generation", OK: true, Message: "generation " + id + " deleted by hand", Generation: id})
	s.diskCtl.reset()
	w.WriteHeader(http.StatusNoContent)
}

// Activation is the result of making a generation live.
type Activation struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"` // live
	AlreadyLive bool     `json:"already_live"`
	Previous    *string  `json:"previous"` // the generation that was live before, if any
	Removed     []string `json:"removed"`  // generations retention deleted
}

// ActivateError is a refused activation: the HTTP status, an error code and what is wrong.
type ActivateError struct {
	Status   int
	Code     string
	Message  string
	Problems []string
}

func (e *ActivateError) Error() string { return e.Message }

// activateGeneration: POST /admin/api/generations/{id}/activate → 200 Activation. The
// generation directory must already be under <data>/generations/<id> (built here, or copied
// in). 404 no_generation, 409 busy (a build is running), 422 invalid_generation (with
// problems[]) or routing_failed; a refusal leaves the live generation serving. Activating the
// live generation is a no-op that answers 200 with already_live.
func (s *Server) activateGeneration(w http.ResponseWriter, r *http.Request) {
	if s.d.ActivateGeneration == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "this server cannot activate generations")
		return
	}
	res, err := s.d.ActivateGeneration(r.Context(), r.PathValue("id"))
	var ae *ActivateError
	if errors.As(err, &ae) {
		e := map[string]any{"code": ae.Code, "message": ae.Message}
		if len(ae.Problems) > 0 {
			e["problems"] = ae.Problems
		}
		writeJSON(w, ae.Status, map[string]any{"error": e})
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if res.Removed == nil {
		res.Removed = []string{}
	}
	if !res.AlreadyLive {
		msg := "generation " + res.ID + " made live by hand"
		if res.Previous != nil {
			msg += " (was " + *res.Previous + ")"
		}
		slog.Info("generation activated", "generation", res.ID, "removed", res.Removed)
		_ = s.d.Store.AddHistory(r.Context(), store.HistoryEntry{Kind: "generation", OK: true, Message: msg, Generation: res.ID})
		s.diskCtl.reset()
	}
	res.Status = "live"
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h, err := s.d.Store.History(r.Context(), r.URL.Query().Get("kind"), limit)
	if err != nil {
		storeError(w, r, err)
		return
	}
	for i := range h {
		h[i].Message = s.redact(h[i].Message)
		if len(h[i].Details) > 0 {
			// Details are JSON: mask the strings inside rather than the encoded text.
			var d any
			if json.Unmarshal(h[i].Details, &d) == nil {
				h[i].Details, _ = json.Marshal(redactJSON(d, s.redact))
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": h})
}

// redactJSON applies redact to every string in a decoded JSON value.
func redactJSON(v any, redact func(string) string) any {
	switch x := v.(type) {
	case string:
		return redact(x)
	case []any:
		for i, e := range x {
			x[i] = redactJSON(e, redact)
		}
	case map[string]any:
		for k, e := range x {
			x[k] = redactJSON(e, redact)
		}
	}
	return v
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
	def, err := s.d.Store.Schedule(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cron": st.Cron, "enabled": st.Enabled, "refresh_downloads": s.d.Scheduler.RefreshDownloads(r.Context()),
		// defaults for regions that follow this schedule
		"window": def.Window, "only_changed": def.OnlyChanged,
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
		Window           *string          `json:"window"`
		OnlyChanged      *bool            `json:"only_changed"`
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
	if b.Window != nil || b.OnlyChanged != nil {
		def, err := s.d.Store.Schedule(r.Context())
		if err != nil {
			storeError(w, r, err)
			return
		}
		window, only := def.Window, def.OnlyChanged
		if b.Window != nil {
			window = *b.Window
		}
		if b.OnlyChanged != nil {
			only = *b.OnlyChanged
		}
		if err := s.d.Scheduler.SetDefaults(r.Context(), window, only); err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_window", err.Error())
			return
		}
	}
	// The older per-region switch: off is mode never, on puts a never region back on the default.
	regions, err := s.d.Store.Regions(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	current := map[string]bool{}
	for _, rg := range regions {
		current[rg.ID] = rg.Refresh
	}
	for _, rg := range b.Regions {
		if was, ok := current[rg.ID]; !ok || was == rg.Enabled {
			continue // unchanged: leave the region's own mode alone
		}
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
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	CatalogID        *string         `json:"catalog_id"`
	Parent           string          `json:"parent,omitempty"`
	BBox             []float64       `json:"bbox"`
	Polygon          [][2]float64    `json:"polygon,omitempty"`
	Status           string          `json:"status"` // pending | installed | failed
	ExtractBytes     int64           `json:"extract_bytes"`
	DiskBytes        int64           `json:"disk_bytes"`
	DataDate         *string         `json:"data_date"`
	DownloadedAt     *time.Time      `json:"downloaded_at"`
	ElevationSources []string        `json:"elevation_sources"`
	Parents          []CatalogParent `json:"parents,omitempty"` // custom regions: extracts clipped from
	Refresh          bool            `json:"refresh"`           // false exactly when the mode is never
	AddedAt          time.Time       `json:"added_at"`
	RefreshPolicy    refreshJSON     `json:"refresh_policy"`
	RefreshCost      refreshCostJSON `json:"refresh_cost"`
}

// refreshJSON is a region's refresh setting, what it resolves to, and where it stands.
type refreshJSON struct {
	// The region's own setting.
	Mode        string `json:"mode"` // default | never | daily | weekly | monthly | cron
	Cron        string `json:"cron,omitempty"`
	Window      string `json:"window,omitempty"` // "HH:MM-HH:MM" UTC; "" = the default's
	OnlyChanged bool   `json:"only_changed"`
	// What applies once the default schedule is taken into account.
	Effective struct {
		Cron        *string `json:"cron"` // null: never refreshed
		Window      string  `json:"window"`
		OnlyChanged bool    `json:"only_changed"`
		Inherited   bool    `json:"inherited"` // from the default schedule
	} `json:"effective"`
	// NextDue: the next schedule time. NextBuild: when a refresh can start (inside the window).
	NextDue   *time.Time `json:"next_due"`
	NextBuild *time.Time `json:"next_build"`
	Due       bool       `json:"due"`     // the schedule time has passed
	Waiting   bool       `json:"waiting"` // due, but outside the build window
	// The last look at the source.
	LastCheckedAt *time.Time `json:"last_checked_at"`
	LastCheck     string     `json:"last_check,omitempty"` // changed | unchanged | error
	LastDetail    string     `json:"last_check_detail,omitempty"`
}

// refreshCostJSON is what refreshing this region costs: its own download, and a build, which
// is over all regions together whichever of them is due.
type refreshCostJSON struct {
	DownloadBytes int64 `json:"download_bytes"`
	// BuildSeconds is how long the last build's basemap, routing and other stages took
	// without the terrain, which a refresh reuses; null before the first build.
	BuildSeconds *float64 `json:"build_seconds"`
}

// regionMeta is the JSON core keeps in regions.meta (from the catalog at add time).
type regionMeta struct {
	ExtractBytes     int64           `json:"extract_bytes,omitempty"`
	ElevationSources []string        `json:"elevation_sources,omitempty"`
	Parents          []CatalogParent `json:"parents,omitempty"`
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
	schedule   *store.Schedule
	now        time.Time
	// buildSeconds: the live generation's build time without its terrain stage; nil if unknown.
	buildSeconds *float64
}

func (s *Server) regionContext(ctx context.Context) regionContext {
	rc := regionContext{live: map[string]manifestRegionJSON{}, downloads: s.diskCtl.get().Downloads, now: time.Now().UTC()}
	if sc, err := s.d.Store.Schedule(ctx); err == nil {
		rc.schedule = sc
	} else {
		rc.schedule = &store.Schedule{Cron: scheduler.DefaultCron}
	}
	if _, dir, ok := tiles.CurrentGeneration(s.d.DataDir); ok {
		m := normaliseManifest(dir)
		for _, r := range m.Regions {
			rc.live[r.ID] = r
		}
		var secs float64
		for stage, d := range m.Timings {
			if stage != "terrain" && stage != "download" {
				secs += d
			}
		}
		if secs > 0 {
			rc.buildSeconds = &secs
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
	v.Parents = meta.Parents
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
	v.RefreshPolicy = refreshView(rg, rc.schedule, rc.now)
	v.RefreshCost = refreshCostJSON{DownloadBytes: v.ExtractBytes, BuildSeconds: rc.buildSeconds}
	return v
}

func refreshView(rg *store.Region, def *store.Schedule, now time.Time) refreshJSON {
	st := scheduler.StatusFor(rg, def, now)
	out := refreshJSON{Mode: st.Policy.Mode, Cron: rg.RefreshCron, Window: rg.RefreshWindow, OnlyChanged: rg.RefreshOnlyChanged,
		NextDue: st.NextDue, NextBuild: st.NextBuild, Due: st.Due, Waiting: st.Waiting,
		LastCheckedAt: rg.CheckedAt, LastCheck: rg.CheckResult, LastDetail: rg.CheckDetail}
	if st.Policy.Cron != "" {
		c := st.Policy.Cron
		out.Effective.Cron = &c
	}
	out.Effective.Window = st.Policy.Window.String()
	out.Effective.OnlyChanged = st.Policy.OnlyChanged
	out.Effective.Inherited = st.Policy.Inherited
	return out
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
	if rg.Kind == "custom" && s.d.EstimateCustom != nil {
		est, err := s.d.EstimateCustom(r.Context(), rg.BBox, polygonRing(rg.Polygon))
		if errors.Is(err, ErrNotCovered) {
			writeError(w, r, http.StatusBadRequest, "not_covered", "no Geofabrik extract covers that area")
			return
		}
		if err == nil {
			ids := make([]string, len(est.Parents))
			for i, p := range est.Parents {
				ids[i] = p.ID
			}
			if rg.Parent == "" {
				rg.Parent = strings.Join(ids, ",")
			}
			rg.Meta, _ = json.Marshal(regionMeta{ExtractBytes: est.TotalBytes, ElevationSources: est.ElevationSources, Parents: est.Parents})
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
	writeJSON(w, http.StatusCreated, s.regionView(saved, s.regionContext(r.Context())))
}

// patchRegion: PATCH /admin/api/regions/{id} {refresh_policy:{mode,cron,window,only_changed}}
// (or the older {refresh: bool}) → region. 400 bad_refresh for a mode, cron or window that
// does not parse.
func (s *Server) patchRegion(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Refresh       *bool `json:"refresh"`
		RefreshPolicy *struct {
			Mode        string `json:"mode"`
			Cron        string `json:"cron"`
			Window      string `json:"window"`
			OnlyChanged *bool  `json:"only_changed"`
		} `json:"refresh_policy"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if p := b.RefreshPolicy; p != nil {
		mode := strings.ToLower(strings.TrimSpace(p.Mode))
		if !scheduler.ValidMode(mode) {
			writeError(w, r, http.StatusBadRequest, "bad_refresh", "mode must be default, never, daily, weekly, monthly or cron")
			return
		}
		expr := ""
		if mode == scheduler.ModeCron {
			expr = strings.TrimSpace(p.Cron)
			if _, err := scheduler.Parse(expr); err != nil || expr == "" {
				writeError(w, r, http.StatusBadRequest, "bad_refresh", "mode cron needs a 5-field cron expression (UTC), for example 0 2 * * 1-5")
				return
			}
		}
		win, err := scheduler.ParseWindow(p.Window)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_refresh", err.Error())
			return
		}
		only := true
		if p.OnlyChanged != nil {
			only = *p.OnlyChanged
		}
		if err := s.d.Store.SetRegionRefreshPolicy(r.Context(), r.PathValue("id"), mode, expr, win.String(), only); err != nil {
			storeError(w, r, err)
			return
		}
	} else if b.Refresh != nil {
		if err := s.d.Store.SetRegionRefresh(r.Context(), r.PathValue("id"), *b.Refresh); err != nil {
			storeError(w, r, err)
			return
		}
	}
	s.getRegion(w, r)
}

// checkRegions: POST /admin/api/regions/check → {regions:[…]}: look at every refreshed region's
// source now and record whether it has something newer. Builds nothing. One check runs at a
// time; a call within a minute of the last answers with that check's results (header
// X-Mapthing-Check: recent) without asking the sources again.
func (s *Server) checkRegions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	asked, err := s.d.Scheduler.CheckAll(ctx)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	if !asked {
		// Within the cooldown of the last check: its results are what is returned.
		w.Header().Set("X-Mapthing-Check", "recent")
	}
	s.listRegions(w, r)
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
