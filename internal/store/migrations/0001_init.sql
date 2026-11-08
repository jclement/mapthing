-- mapthing schema v1. Times are Unix seconds (UTC) unless noted.

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE api_keys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    label        TEXT    NOT NULL,
    prefix       TEXT    NOT NULL,            -- first characters of the secret, for display
    hash         TEXT    NOT NULL UNIQUE,     -- hex SHA-256 of the whole secret
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER,
    limits       TEXT    NOT NULL DEFAULT '{}' -- JSON: {"rpm":0,"rpd":0,"allowed_origins":[]}
);

-- usage: per key, per UTC day (YYYY-MM-DD), per kind. key_id 0 = admin session.
CREATE TABLE usage (
    key_id INTEGER NOT NULL,
    day    TEXT    NOT NULL,
    kind   TEXT    NOT NULL,
    count  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, day, kind)
);
CREATE INDEX usage_day ON usage (day);

-- cache_stats: hits and misses per cache (raster, imagery, pack) per UTC day.
CREATE TABLE cache_stats (
    cache  TEXT    NOT NULL,
    day    TEXT    NOT NULL,
    hits   INTEGER NOT NULL DEFAULT 0,
    misses INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (cache, day)
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,               -- hex SHA-256 of the cookie value
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

-- regions: what the admin installed. kind 'extract' (a catalog id such as us/texas) or
-- 'custom' (a bbox or polygon clipped from a parent extract).
CREATE TABLE regions (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL DEFAULT 'extract',
    parent     TEXT    NOT NULL DEFAULT '',
    bbox       TEXT    NOT NULL DEFAULT '',     -- JSON [w,s,e,n] or ''
    polygon    TEXT    NOT NULL DEFAULT '',     -- GeoJSON geometry or ''
    refresh    INTEGER NOT NULL DEFAULT 1,      -- included in scheduled refreshes
    added_at   INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    meta       TEXT    NOT NULL DEFAULT '{}'    -- JSON: source date, size, ... (pipeline-owned)
);

-- schedule: a single row.
CREATE TABLE schedule (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    cron       TEXT    NOT NULL,
    enabled    INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
INSERT INTO schedule (id, cron, enabled, updated_at) VALUES (1, '0 3 * * 0', 1, 0);

CREATE TABLE jobs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL,
    params      TEXT    NOT NULL DEFAULT '{}',
    trigger     TEXT    NOT NULL DEFAULT 'manual', -- manual | schedule | startup
    status      TEXT    NOT NULL,                  -- queued | running | succeeded | failed | cancelled
    stage       TEXT    NOT NULL DEFAULT '',
    pct         REAL    NOT NULL DEFAULT 0,
    message     TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    finished_at INTEGER
);
CREATE INDEX jobs_status ON jobs (status);

CREATE TABLE job_logs (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id INTEGER NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    at     INTEGER NOT NULL, -- Unix milliseconds
    line   TEXT    NOT NULL
);
CREATE INDEX job_logs_job ON job_logs (job_id, id);

-- generations: builds known to the server (the filesystem under /data/generations is the truth
-- for what exists; this records status and manifest for history).
CREATE TABLE generations (
    id         TEXT    PRIMARY KEY,
    created_at INTEGER NOT NULL,
    status     TEXT    NOT NULL,                -- building | live | kept | failed | deleted
    job_id     INTEGER,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    manifest   TEXT    NOT NULL DEFAULT '{}'
);

-- history: notable events (builds, swaps, schedule runs, failures) for the admin UI.
CREATE TABLE history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         INTEGER NOT NULL,
    kind       TEXT    NOT NULL,   -- job | schedule | generation | system
    ok         INTEGER NOT NULL,
    message    TEXT    NOT NULL,
    job_id     INTEGER,
    generation TEXT    NOT NULL DEFAULT '',
    details    TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX history_at ON history (at);
