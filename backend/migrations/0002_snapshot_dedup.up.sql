-- 0002_snapshot_dedup.up.sql
--
-- resource_snapshots is written by the 10s flush worker, which reads one Redis
-- key per sandbox. If a collector restarts between two flushes, the key can hold
-- a sample the previous pass already inserted, so the same (sandbox_id,
-- recorded_at) can land twice. Duplicates are harmless to the chart but they
-- accumulate, and they make "one row per interval" untrue, which is exactly the
-- assumption the row-cap arithmetic in GET /stats/history relies on.
--
-- Deduplicate any existing rows first (keeping the lowest id, i.e. the first
-- write) so the unique index can be created on a database that has already been
-- running this migration's predecessor.
DELETE FROM resource_snapshots a
USING resource_snapshots b
WHERE a.sandbox_id = b.sandbox_id
  AND a.recorded_at = b.recorded_at
  AND a.id > b.id;

CREATE UNIQUE INDEX idx_resource_snapshots_unique
    ON resource_snapshots (sandbox_id, recorded_at);
