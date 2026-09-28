-- 0003_lifetime_starts_on_activation.down.sql
--
-- Dropping the column returns expires_at to being anchored at created_at, so any
-- sandbox currently in flight keeps the deadline it already has. Rows that were
-- never activated are unaffected: their expires_at was never rebased.
--
-- The index goes with the column, since its partial predicate references it.
DROP INDEX IF EXISTS idx_sandboxes_pending_activation;

ALTER TABLE sandboxes
    DROP COLUMN IF EXISTS activated_at;
