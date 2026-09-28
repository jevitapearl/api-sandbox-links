-- 0003_lifetime_starts_on_activation.up.sql
--
-- The lifetime is a promise about how long a *usable link* lives, so the clock
-- must not run while the sandbox is still being built. Previously expires_at was
-- fixed at created_at + lifetime_seconds, so a 5-minute sandbox whose build took
-- 3 minutes was destroyed 2 minutes after it came up — and a build longer than
-- the whole lifetime was destroyed before it ever went live.
--
-- activated_at records the moment the sandbox first reached 'running'.
-- expires_at is rebased to activated_at + lifetime_seconds on that transition
-- only, so a redeploy (which also passes through building -> running) does not
-- hand out another full lifetime every time.
--
-- expires_at stays NOT NULL and is still set at creation: it remains the deadline
-- for a sandbox that never activates, so a build that hangs or fails cannot leak
-- a row forever.
ALTER TABLE sandboxes
    ADD COLUMN activated_at TIMESTAMPTZ;

COMMENT ON COLUMN sandboxes.activated_at IS
    'When the sandbox first reached running. NULL until then. The lifetime clock starts here, not at created_at.';

-- The expiry reaper scans on expires_at, not activated_at, so no new index is
-- needed. This one exists only for "which sandboxes are still building", which
-- is a support question rather than a hot path.
CREATE INDEX idx_sandboxes_pending_activation
    ON sandboxes (created_at)
    WHERE activated_at IS NULL AND destroyed_at IS NULL;
