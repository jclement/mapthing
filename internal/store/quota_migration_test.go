package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Review 3, finding 4: upgrading in the middle of a day does not hand keys with an old
// "requests per day" limit their day again. Migration 0005 carries today's and this month's
// request counts over as used units, once.
func TestUpgradeCarriesUsageIntoQuotas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mapthing.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	// The database as a version without quotas left it: usage rows, no quota rows, and
	// migration 5 not applied yet. Dates as SQLite sees them, so the test is right at any hour.
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	var today, month, earlier string
	if err := s.DB.QueryRowContext(ctx, `SELECT strftime('%Y-%m-%d','now'), strftime('%Y-%m','now'), strftime('%Y-%m-%d','now','-40 days')`).Scan(&today, &month, &earlier); err != nil {
		t.Fatal(err)
	}
	must(`DELETE FROM key_quota`)
	must(`DELETE FROM schema_migrations WHERE version = 5`)
	must(`INSERT INTO usage (key_id, day, kind, count) VALUES (1, ?, 'tile', 4000), (1, ?, 'route', 1000), (2, ?, 'tile', 7), (1, ?, 'tile', 90000), (0, ?, 'tile', 5)`,
		today, today, today, earlier, today)
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	used := func(id int64, period string) int64 {
		t.Helper()
		n, err := s.QuotaUsed(ctx, id, period)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// One unit per request, which is what a request cost under requests-per-day.
	if got := used(1, today); got != 5000 {
		t.Fatalf("key 1 today: %d units, want the 5000 requests it already made", got)
	}
	if got := used(1, month); got != 5000 {
		t.Errorf("key 1 this month: %d (the usage of 40 days ago is another month)", got)
	}
	if got := used(2, today); got != 7 {
		t.Errorf("key 2 today: %d", got)
	}
	if got := used(0, today); got != 0 {
		t.Errorf("requests without a key were given a quota row: %d", got)
	}
	// So a key that had used up "5000 a day" is still out after the upgrade.
	lim := Limits{RPD: 5000}.Normalise()
	if lim.DailyQuota != 5000 || used(1, today) < lim.DailyQuota {
		t.Fatalf("limit %+v, used %d", lim, used(1, today))
	}
	// It happens once: opening the database again adds nothing, even with more usage since.
	must(`UPDATE usage SET count = count + 100 WHERE key_id = 1 AND day = ? AND kind = 'tile'`, today)
	_ = s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if n, _ := s2.QuotaUsed(ctx, 1, today); n != 5000 {
		t.Errorf("after reopening: %d, want 5000", n)
	}
	// And a period that already has a quota row is never overwritten.
	if _, err := s2.DB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 5`); err != nil {
		t.Fatal(err)
	}
	if err := s2.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := s2.QuotaUsed(ctx, 1, today); n != 5000 {
		t.Errorf("a second run of the migration changed an existing row: %d", n)
	}
}
