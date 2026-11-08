package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Limits are per-key restrictions. Zero means unlimited; an empty AllowedOrigins allows any.
type Limits struct {
	RPM            int      `json:"rpm"`
	RPD            int      `json:"rpd"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// APIKey is a stored key. The secret itself is never stored, only its SHA-256.
type APIKey struct {
	ID         int64      `json:"id"`
	Label      string     `json:"label"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Limits     Limits     `json:"limits"`
}

const keyCols = `id, label, prefix, hash, created_at, last_used_at, revoked_at, limits`

func scanKey(sc interface{ Scan(...any) error }) (*APIKey, error) {
	var k APIKey
	var created int64
	var used, revoked sql.NullInt64
	var limits string
	if err := sc.Scan(&k.ID, &k.Label, &k.Prefix, &k.Hash, &created, &used, &revoked, &limits); err != nil {
		return nil, err
	}
	k.CreatedAt = time.Unix(created, 0).UTC()
	k.LastUsedAt = nullTime(used)
	k.RevokedAt = nullTime(revoked)
	_ = json.Unmarshal([]byte(limits), &k.Limits)
	if k.Limits.AllowedOrigins == nil {
		k.Limits.AllowedOrigins = []string{}
	}
	return &k, nil
}

// CreateKey inserts a key and returns it with its ID.
func (s *Store) CreateKey(ctx context.Context, label, prefix, hash string, lim Limits) (*APIKey, error) {
	lj, _ := json.Marshal(lim)
	now := time.Now().UTC()
	res, err := s.DB.ExecContext(ctx, `INSERT INTO api_keys (label, prefix, hash, created_at, limits) VALUES (?, ?, ?, ?, ?)`,
		label, prefix, hash, now.Unix(), string(lj))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Key(ctx, id)
}

// Key returns one key by ID.
func (s *Store) Key(ctx context.Context, id int64) (*APIKey, error) {
	k, err := scanKey(s.DB.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// KeyByHash returns a key by its secret's hash (revoked keys included; callers check).
func (s *Store) KeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	k, err := scanKey(s.DB.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// Keys lists all keys, newest first.
func (s *Store) Keys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// UpdateKey changes a key's label and limits.
func (s *Store) UpdateKey(ctx context.Context, id int64, label string, lim Limits) error {
	lj, _ := json.Marshal(lim)
	res, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET label = ?, limits = ? WHERE id = ?`, label, string(lj), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// RevokeKey marks a key revoked (idempotent).
func (s *Store) RevokeKey(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// TouchKeys records last-used times (batched by the stats flusher).
func (s *Store) TouchKeys(ctx context.Context, used map[int64]time.Time) error {
	if len(used) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for id, t := range used {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET last_used_at = MAX(COALESCE(last_used_at, 0), ?) WHERE id = ?`, t.Unix(), id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
