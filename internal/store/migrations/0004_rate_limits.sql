-- Rate limiting that survives a restart. Additive.
--
-- key_quota: cost units a key has used in a period: a UTC day ('2026-11-11') or a UTC month
-- ('2026-11'). Daily and monthly quotas are checked against these.
CREATE TABLE key_quota (
    key_id INTEGER NOT NULL,
    period TEXT    NOT NULL,
    units  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, period)
);

-- key_limited: requests answered 429, per key (0 = no key: a global limit hit by the admin
-- session never happens, so 0 is unused), UTC day and scope (rate | ip | daily | monthly | global).
CREATE TABLE key_limited (
    key_id INTEGER NOT NULL,
    day    TEXT    NOT NULL,
    scope  TEXT    NOT NULL,
    count  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, day, scope)
);
