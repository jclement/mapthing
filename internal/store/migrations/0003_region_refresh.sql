-- Per-region refresh. Each region has its own refresh setting; the schedule row is the default
-- for regions that do not set one. Additive, so an older version still reads the tables.
--
-- refresh_mode: default (follow the schedule row) | never | daily | weekly | monthly | cron.
-- The old regions.refresh flag is kept in step (0 exactly when the mode is never).
ALTER TABLE regions ADD COLUMN refresh_mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE regions ADD COLUMN refresh_cron TEXT NOT NULL DEFAULT '';         -- for mode cron (5 fields, UTC)
ALTER TABLE regions ADD COLUMN refresh_window TEXT NOT NULL DEFAULT '';       -- 'HH:MM-HH:MM' UTC, or '' (the default's)
ALTER TABLE regions ADD COLUMN refresh_only_changed INTEGER NOT NULL DEFAULT 1; -- used when the mode is not default
-- refresh_handled_at: when this region's refresh was last decided (built, or found unchanged).
-- Its next refresh is the first schedule time after it. NULL until the scheduler first sees it.
ALTER TABLE regions ADD COLUMN refresh_handled_at INTEGER;
-- The last look at the region's source: when, what was found (changed | unchanged | error), detail.
ALTER TABLE regions ADD COLUMN checked_at INTEGER;
ALTER TABLE regions ADD COLUMN check_result TEXT NOT NULL DEFAULT '';
ALTER TABLE regions ADD COLUMN check_detail TEXT NOT NULL DEFAULT '';
UPDATE regions SET refresh_mode = 'never' WHERE refresh = 0;

-- Defaults for regions in mode default: an optional build window and "only when the source changed".
ALTER TABLE schedule ADD COLUMN build_window TEXT NOT NULL DEFAULT '';
ALTER TABLE schedule ADD COLUMN only_changed INTEGER NOT NULL DEFAULT 1;
