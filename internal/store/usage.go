package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UsageKey identifies one usage counter.
type UsageKey struct {
	KeyID int64
	Day   string // YYYY-MM-DD UTC
	Kind  string
}

// UsageRow is one stored usage counter.
type UsageRow struct {
	KeyID int64  `json:"key_id"`
	Day   string `json:"day"`
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

// CacheKey identifies one cache counter.
type CacheKey struct {
	Cache string
	Day   string
}

// CacheRow is one stored cache counter.
type CacheRow struct {
	Cache  string `json:"cache"`
	Day    string `json:"day"`
	Hits   int64  `json:"hits"`
	Misses int64  `json:"misses"`
}

// AddUsage adds deltas to usage and cache counters in one transaction.
func (s *Store) AddUsage(ctx context.Context, usage map[UsageKey]int64, cache map[CacheKey][2]int64) error {
	if len(usage) == 0 && len(cache) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for k, n := range usage {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage (key_id, day, kind, count) VALUES (?, ?, ?, ?)
			ON CONFLICT(key_id, day, kind) DO UPDATE SET count = count + excluded.count`, k.KeyID, k.Day, k.Kind, n); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for k, hm := range cache {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cache_stats (cache, day, hits, misses) VALUES (?, ?, ?, ?)
			ON CONFLICT(cache, day) DO UPDATE SET hits = hits + excluded.hits, misses = misses + excluded.misses`, k.Cache, k.Day, hm[0], hm[1]); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// UsageSince returns usage rows with day >= since (YYYY-MM-DD).
func (s *Store) UsageSince(ctx context.Context, since string) ([]UsageRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT key_id, day, kind, count FROM usage WHERE day >= ? ORDER BY day, key_id, kind`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.KeyID, &r.Day, &r.Kind, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KeyDayTotal returns the total stored requests for a key on a day (all kinds).
func (s *Store) KeyDayTotal(ctx context.Context, keyID int64, day string) (int64, error) {
	var n sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT SUM(count) FROM usage WHERE key_id = ? AND day = ?`, keyID, day).Scan(&n)
	return n.Int64, err
}

// CacheSince returns cache rows with day >= since.
func (s *Store) CacheSince(ctx context.Context, since string) ([]CacheRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT cache, day, hits, misses FROM cache_stats WHERE day >= ? ORDER BY day, cache`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CacheRow{}
	for rows.Next() {
		var r CacheRow
		if err := rows.Scan(&r.Cache, &r.Day, &r.Hits, &r.Misses); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateSession stores a session by token hash.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, expires time.Time) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sessions (token_hash, created_at, expires_at) VALUES (?, ?, ?)`,
		tokenHash, time.Now().Unix(), expires.Unix())
	return err
}

// SessionValid reports whether a session exists and has not expired.
func (s *Store) SessionValid(ctx context.Context, tokenHash string) (bool, error) {
	var exp int64
	err := s.DB.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().Unix() < exp, nil
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// PurgeSessions deletes expired sessions.
func (s *Store) PurgeSessions(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	return err
}
