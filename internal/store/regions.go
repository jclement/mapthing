package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Region is an installed region.
type Region struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"` // extract | custom
	Parent    string          `json:"parent,omitempty"`
	BBox      []float64       `json:"bbox,omitempty"`    // [w,s,e,n]
	Polygon   json.RawMessage `json:"polygon,omitempty"` // GeoJSON geometry
	Refresh   bool            `json:"refresh"`
	AddedAt   time.Time       `json:"added_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Meta      json.RawMessage `json:"meta"`
}

const regionCols = `id, name, kind, parent, bbox, polygon, refresh, added_at, updated_at, meta`

func scanRegion(sc interface{ Scan(...any) error }) (*Region, error) {
	var r Region
	var bbox, poly, meta string
	var refresh int
	var added, updated int64
	if err := sc.Scan(&r.ID, &r.Name, &r.Kind, &r.Parent, &bbox, &poly, &refresh, &added, &updated, &meta); err != nil {
		return nil, err
	}
	if bbox != "" {
		_ = json.Unmarshal([]byte(bbox), &r.BBox)
	}
	if poly != "" {
		r.Polygon = json.RawMessage(poly)
	}
	if meta == "" {
		meta = "{}"
	}
	r.Meta = json.RawMessage(meta)
	r.Refresh = refresh != 0
	r.AddedAt = time.Unix(added, 0).UTC()
	r.UpdatedAt = time.Unix(updated, 0).UTC()
	return &r, nil
}

// Regions lists installed regions by ID.
func (s *Store) Regions(ctx context.Context) ([]*Region, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+regionCols+` FROM regions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Region{}
	for rows.Next() {
		r, err := scanRegion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Region returns one region.
func (s *Store) Region(ctx context.Context, id string) (*Region, error) {
	r, err := scanRegion(s.DB.QueryRowContext(ctx, `SELECT `+regionCols+` FROM regions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// UpsertRegion inserts or replaces a region's definition (AddedAt is kept on update).
func (s *Store) UpsertRegion(ctx context.Context, r *Region) error {
	var bbox string
	if len(r.BBox) == 4 {
		b, _ := json.Marshal(r.BBox)
		bbox = string(b)
	}
	meta := string(r.Meta)
	if meta == "" {
		meta = "{}"
	}
	if r.Kind == "" {
		r.Kind = "extract"
	}
	now := time.Now().Unix()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO regions (id, name, kind, parent, bbox, polygon, refresh, added_at, updated_at, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, kind = excluded.kind, parent = excluded.parent, bbox = excluded.bbox,
			polygon = excluded.polygon, refresh = excluded.refresh, updated_at = excluded.updated_at, meta = excluded.meta`,
		r.ID, r.Name, r.Kind, r.Parent, bbox, string(r.Polygon), boolInt(r.Refresh), now, now, meta)
	return err
}

// SetRegionRefresh toggles whether a region is part of scheduled refreshes.
func (s *Store) SetRegionRefresh(ctx context.Context, id string, refresh bool) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE regions SET refresh = ?, updated_at = ? WHERE id = ?`, boolInt(refresh), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// SetRegionMeta replaces a region's meta JSON (pipeline: source date, sizes).
func (s *Store) SetRegionMeta(ctx context.Context, id string, meta json.RawMessage) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE regions SET meta = ?, updated_at = ? WHERE id = ?`, string(meta), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// DeleteRegion removes a region.
func (s *Store) DeleteRegion(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM regions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// Schedule is the refresh schedule.
type Schedule struct {
	Cron      string    `json:"cron"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Schedule returns the schedule row.
func (s *Store) Schedule(ctx context.Context) (*Schedule, error) {
	var sc Schedule
	var en int
	var up int64
	if err := s.DB.QueryRowContext(ctx, `SELECT cron, enabled, updated_at FROM schedule WHERE id = 1`).Scan(&sc.Cron, &en, &up); err != nil {
		return nil, err
	}
	sc.Enabled = en != 0
	sc.UpdatedAt = time.Unix(up, 0).UTC()
	return &sc, nil
}

// SetSchedule updates the schedule row.
func (s *Store) SetSchedule(ctx context.Context, cron string, enabled bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE schedule SET cron = ?, enabled = ?, updated_at = ? WHERE id = 1`, cron, boolInt(enabled), time.Now().Unix())
	return err
}

// HistoryEntry is one notable event.
type HistoryEntry struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	Kind       string          `json:"kind"`
	OK         bool            `json:"ok"`
	Message    string          `json:"message"`
	JobID      *int64          `json:"job_id,omitempty"`
	Generation string          `json:"generation,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
}

// AddHistory records an event.
func (s *Store) AddHistory(ctx context.Context, e HistoryEntry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	d := string(e.Details)
	if d == "" {
		d = "{}"
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO history (at, kind, ok, message, job_id, generation, details) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.At.Unix(), e.Kind, boolInt(e.OK), e.Message, e.JobID, e.Generation, d)
	return err
}

// History lists recent events, newest first; kind "" means all kinds.
func (s *Store) History(ctx context.Context, kind string, limit int) ([]HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, at, kind, ok, message, job_id, generation, details FROM history`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HistoryEntry{}
	for rows.Next() {
		var e HistoryEntry
		var at int64
		var ok int
		var job sql.NullInt64
		var details string
		if err := rows.Scan(&e.ID, &at, &e.Kind, &ok, &e.Message, &job, &e.Generation, &details); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		e.OK = ok != 0
		if job.Valid {
			e.JobID = &job.Int64
		}
		e.Details = json.RawMessage(details)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Generation is a recorded build.
type Generation struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	Status    string          `json:"status"`
	JobID     *int64          `json:"job_id,omitempty"`
	SizeBytes int64           `json:"size_bytes"`
	Manifest  json.RawMessage `json:"manifest"`
}

// UpsertGeneration records or updates a generation.
func (s *Store) UpsertGeneration(ctx context.Context, g *Generation) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}
	m := string(g.Manifest)
	if m == "" {
		m = "{}"
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO generations (id, created_at, status, job_id, size_bytes, manifest) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET status = excluded.status, job_id = COALESCE(excluded.job_id, job_id),
			size_bytes = excluded.size_bytes, manifest = excluded.manifest`,
		g.ID, g.CreatedAt.Unix(), g.Status, g.JobID, g.SizeBytes, m)
	return err
}

// Generations lists recorded generations, newest first.
func (s *Store) Generations(ctx context.Context) ([]*Generation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, created_at, status, job_id, size_bytes, manifest FROM generations ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Generation{}
	for rows.Next() {
		var g Generation
		var created int64
		var job sql.NullInt64
		var m string
		if err := rows.Scan(&g.ID, &created, &g.Status, &job, &g.SizeBytes, &m); err != nil {
			return nil, err
		}
		g.CreatedAt = time.Unix(created, 0).UTC()
		if job.Valid {
			g.JobID = &job.Int64
		}
		g.Manifest = json.RawMessage(m)
		out = append(out, &g)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
