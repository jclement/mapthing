-- Keys made before rate limits counted cost units had "requests per day" (rpd), counted in
-- the usage table, one per request. Their limit is now a daily quota checked against
-- key_quota, so without this an upgrade in the middle of a day would hand every key its whole
-- day again. Carry over, once, what each key has already used in the current UTC day and
-- month: one unit per request, which is what a request cost under the old limit.
--
-- A period that already has a row is left alone (INSERT OR IGNORE), and a migration runs
-- once, so nothing is ever added twice.
INSERT OR IGNORE INTO key_quota (key_id, period, units)
SELECT key_id, day, SUM(count) FROM usage
WHERE key_id > 0 AND day = strftime('%Y-%m-%d', 'now')
GROUP BY key_id, day;

INSERT OR IGNORE INTO key_quota (key_id, period, units)
SELECT key_id, substr(day, 1, 7), SUM(count) FROM usage
WHERE key_id > 0 AND substr(day, 1, 7) = strftime('%Y-%m', 'now')
GROUP BY key_id, substr(day, 1, 7);
