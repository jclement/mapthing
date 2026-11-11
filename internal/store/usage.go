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

// QuotaKey identifies a key's usage in one period: a UTC day (2006-01-02) or month (2006-01).
type QuotaKey struct {
	KeyID  int64
	Period string
}

// LimitedKey identifies a count of refused requests.
type LimitedKey struct {
	KeyID int64
	Day   string
	Scope string // rate | ip | daily | monthly | global
}

// AddQuota adds cost units and refused-request counts in one transaction.
func (s *Store) AddQuota(ctx context.Context, units map[QuotaKey]int64, limited map[LimitedKey]int64) error {
	if len(units) == 0 && len(limited) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for k, n := range units {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_quota (key_id, period, units) VALUES (?, ?, ?)
			ON CONFLICT(key_id, period) DO UPDATE SET units = units + excluded.units`, k.KeyID, k.Period, n); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for k, n := range limited {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_limited (key_id, day, scope, count) VALUES (?, ?, ?, ?)
			ON CONFLICT(key_id, day, scope) DO UPDATE SET count = count + excluded.count`, k.KeyID, k.Day, k.Scope, n); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// QuotaUsed returns the stored cost units of a key in a period.
func (s *Store) QuotaUsed(ctx context.Context, keyID int64, period string) (int64, error) {
	var n sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT units FROM key_quota WHERE key_id = ? AND period = ?`, keyID, period).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n.Int64, err
}

// LimitedRow is a stored count of refused requests.
type LimitedRow struct {
	KeyID int64
	Day   string
	Scope string
	Count int64
}

// LimitedSince returns refused-request counts with day >= since.
func (s *Store) LimitedSince(ctx context.Context, since string) ([]LimitedRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT key_id, day, scope, count FROM key_limited WHERE day >= ? ORDER BY day`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []LimitedRow{}
	for rows.Next() {
		var r LimitedRow
		if err := rows.Scan(&r.KeyID, &r.Day, &r.Scope, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneQuota deletes daily rows older than keepDays and monthly rows older than 13 months.
func (s *Store) PruneQuota(ctx context.Context, now time.Time, keepDays int) error {
	day := now.UTC().AddDate(0, 0, -keepDays).Format("2006-01-02")
	month := now.UTC().AddDate(0, -13, 0).Format("2006-01")
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM key_quota WHERE (length(period) = 10 AND period < ?) OR (length(period) = 7 AND period < ?)`, day, month); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM key_limited WHERE day < ?`, day)
	return err
}
