# Database

PostgreSQL via GORM, with a hand-written SQL migration as the source of truth:

- **`backend/migrations/0001_init.up.sql`** — full schema. Applied on startup by
  `db.Migrate` (recorded in `schema_migrations`).
- **`backend/migrations/0003_lifetime_starts_on_activation.up.sql`** — adds
  `sandboxes.activated_at`, and moves the lifetime clock from `created_at` to the
  first transition into `running`. See "When the lifetime starts" below.
- **`backend/migrations/0002_snapshot_dedup.up.sql`** — adds the unique index on
  `resource_snapshots (sandbox_id, recorded_at)` that makes the live-metrics
  flush idempotent, deleting any duplicate rows the index would otherwise reject.
  Never edit an already-applied migration; add a new one.
- **`backend/internal/db/models.go`** — GORM models mapping back onto those
  tables. Schema changes always land as a new migration file first; keep the
  models in sync.

> The migration header says it "matches BUILD_PROMPT.md verbatim" — don't trust
> that; the file (and this doc) reflect the current schema.

## GORM table-name caveats

GORM pluralizes model names unless `TableName()` overrides them. The overrides
exist because those two don't pluralize to the migration's names:

| Model | Table |
|---|---|
| `EnvVar` | `sandbox_env_vars` |
| `LifetimeWarning` | `sandbox_lifetime_warnings` |
| `Repository` | `repositories` |

Everything else (`users`, `sandboxes`, `sandbox_databases`, `deployments`,
`resource_snapshots`, `file_edits`) pluralizes normally. Forgetting an override
produces a runtime `relation "env_vars" does not exist` on the next save.

## Tables

| Table | Columns / notes |
|---|---|
| `users` | `id uuid pk`, `github_id bigint unique`, `username`, `email`, `avatar_url`, `github_token bytea not null` (AES-GCM encrypted), `created_at`. |
| `repositories` | per-user repo registry: `user_id` fk, `github_url`, `default_branch`, `created_at`, `unique(user_id, github_url)`. Created on first sandbox for that repo. |
| `sandboxes` | `id`, `repository_id` fk, `parent_sandbox_id` (fork), `branch_name`, `subdomain unique`, `status`, `container_id`, `image_tag`, `internal_port`, `idle_timeout_seconds` (default 900), `last_request_at`, `lifetime_seconds` (default 86400), `expires_at`, `activated_at`, `destroyed_at`, `destruction_reason`, `detected_language`, `build_config jsonb`, `ai_suggested_env jsonb`, `created_at`, `updated_at`. Indexes on `status`, `parent_sandbox_id`, partial on `expires_at WHERE destroyed_at IS NULL`. |
| `sandbox_env_vars` | `id`, `sandbox_id` fk, `key`, `value bytea` (encrypted), `is_ai_suggested`, `unique(sandbox_id, key)`. |
| `sandbox_databases` | sidecar DBs: `sandbox_id` fk, `engine` (`postgres\|mysql\|sqlite`), `container_id`, `connection_url bytea` (encrypted), `forked_from`, `created_at`. One per sandbox; persisted across redeploys. |
| `sandbox_lifetime_warnings` | dedup of sent warnings: `sandbox_id` fk, `threshold` (`24h\|1h\|5m` or proportional), `sent_at`, `unique(sandbox_id, threshold)`. |
| `deployments` | per-deploy history: `deployment` – `id`, `sandbox_id` fk, `trigger` (`initial\|save_redeploy\|github_push\|manual\|fork`), `status` (`pending\|building\|success\|failed`), `log_excerpt`, `started_at`, `finished_at`. |
| `resource_snapshots` | metric samples written every 10s while a sandbox runs: `cpu_percent real` (computed from counter deltas, not Docker's raw totals), `memory_used_bytes` (page-cache-adjusted), `memory_limit_bytes`, `network_rx_bytes`, `network_tx_bytes` (per-sample deltas summed across interfaces), `recorded_at`; index `(sandbox_id, recorded_at)`. Append-only while the sandbox runs, pruned on teardown and after **7 days** by the hourly snapshot reaper. Backs `GET /stats/history?range=1h\|6h\|24h\|7d`. A unique index on `(sandbox_id, recorded_at)` makes the 10s flush idempotent, so a retried flush cannot double-count a sample. |
| `file_edits` | optional audit trail of editor saves: `sandbox_id`, `file_path`, `edited_by` → `users`, `edited_at`, `pushed_to_github`. |

There are **no** `github_commits`, `sandbox_stats_rollups`, or
`lifetime_warning_dismissals` tables despite earlier docs; those features are not
in the current schema.

## Session storage

Auth is **stateless**: the cookie `asl_session` is an AES-GCM ciphertext of the
user's GitHub numeric ID (`SECRETS_ENCRYPTION_KEY`, 32 bytes base64 — set in
`.env`). There is no session table; the ID is decrypted and looked up in `users`
per request. Dev auth mode skips cookies entirely and fabricates the `devuser`.

## Data flow notes

- Redis holds only transient state: per-sandbox idle TTL keys `lr:<id>` (the
  idle-reaper contract; touched by the gateway on traffic and set at deploy time)
  and the newest live metric per sandbox, `stats:latest:<id>` (10s TTL, written
  by the stats collector, read by the 10s flush worker). Nothing durable lives in
  Redis. The TTL is deliberate: if a container's stats stream dies the key
  expires instead of leaving a frozen reading to be persisted as if it were live.
- Env vars and DB connection URLs are encrypted at rest with the same
  `EncryptionKey` used for the session cookie; they are decrypted only when
  assembling container env.
- Forks copy the parent's working tree into a new volume, then write a
  `parent_sandbox_id` pointer on the new sandbox (and `forked_from` on a sidecar
  DB if one is copied); the parent's deployments/snapshots are untouched.
- Destroys cascade in `TeardownSandbox`: container → sidecar DB → volume →
  network, plus pruning of deployments/snapshots.