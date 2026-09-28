-- 0002_snapshot_dedup.down.sql
--
-- Only the unique index is dropped. The rows deleted by the up migration are not
-- recreated: they were duplicates of a row that still exists, so rolling this
-- back restores the constraint but not the discarded copies. Re-inserting them
-- would reintroduce the duplicates this migration exists to remove.

DROP INDEX IF EXISTS idx_resource_snapshots_unique;
