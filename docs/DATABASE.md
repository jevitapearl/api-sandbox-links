# Database

PostgreSQL via GORM with a hand-written SQL migration (`backend/migrations/0001_init.up.sql`) as the source of truth. All writes use the GORM models mapping **back onto those tables**.

## Migrations

- `backend/migrations/0001_init.up.sql` — full schema; applied on startup by `db.Migrate` (records in `schema_migrations`).
- GORM model `TableName()` overrides exist for the two tables whose names don't pluralize:
  - `EnvVar` → **`sandbox_env_vars`**
  - `LifetimeWarning` → **`sandbox_lifetime_warnings`**
  - Everything else (`sandboxes`, `users`, `deployments`, `github_commits`, `resource_snapshots`, `sandbox_stats_rollups`) pluralizes normally.

Do not mix naming: forgetting an override produces a runtime `relation "env_vars" does not exist` when a save DB call runs.

## Tables

| Table | Columns / notes |
|---|---|
| `users` | `id uuid pk`, `github_id bigint unique`, `username`, `avatar_url`, `github_token bytea not null` (encrypted), `created_at`. |
| `sandboxes` | `id uuid pk`, `user_id uuid fk`, `repo_url`, `branch`, `subdomain unique`, `status` (queued/building/running/hibernated/failed/expired/deleted), `detected_language`, `container_id`, `volume_name`, `network_id`, `image_name`, `port`, `database` (auto/postgres/mysql/none), `lifetime_seconds`, `idle_timeout_seconds`, `expires_at`, `created_at`, `last_request_at`; `parent_sandbox_id uuid nullable` (fork). |
| `sandbox_env_vars` | `id uuid pk`, `sandbox_id fk`, `key`, `value` (encrypted), `created_at`. |
| `deployments` | `id uuid pk`, `sandbox_id fk`, `trigger` (initial/save_redeploy/manual/fork_redeploy), `status` (queued/building/success/failed), `commit_sha`, `log_excerpt text`, `started_at`, `finished_at`. |
| `resource_snapshots` | per-sandbox 30s metrics: `cpu_pct`, `memory_used_bytes`, `memory_limit_bytes`, `recorded_at`. |
| `sandbox_stats_rollups` | hourly/daily aggregates (not yet surfaced in the dashboard). |
| `github_commits` | record of push commits: `commit_sha`, `branch`, `direct boolean`, `message`, `pushed_by_user_id`, `sandbox_id`, `pushed_at`. |
| `lifetime_warnings` | `{ stage, due_at, sent_at }` — staging points (24h / 1h / 5m / percentiles) for deadline notices. |
| `lifetime_warning_dismissals` | per-user per-sandbox dismissal bookkeeping. |

## Session storage

Auth is **stateless**: the cookie `asl_session` is an AES-GCM ciphertext of the user's GitHub ID (`SECRETS_ENCRYPTION_KEY`, 32 bytes base64, generated in `.env`). No session table exists; the ID is looked up in `users`.

## Data flow notes

- Redis holds only transient state: per-sandbox idle TTL keys `lr:<id>` (the idle-reaper contract) and sidecar DB readiness flags. Nothing durable.
- Fork copies the parent's working tree into a new volume then writes a `parent_sandbox_id` pointer; the parent's `github_commits`/`deployments` stay untouched.
- `sandbox_stats_rollups` writes are candidates for a roomba/pruning job — currently history is capped to the last ~30m of `resource_snapshots`.