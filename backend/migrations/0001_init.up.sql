-- 0001_init.up.sql — initial platform schema for API Sandbox Links.
-- Matches section 4 of BUILD_PROMPT.md verbatim.

-- Users (GitHub-authenticated)
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_id       BIGINT UNIQUE NOT NULL,
    username        TEXT NOT NULL,
    email           TEXT,
    avatar_url      TEXT,
    github_token    BYTEA NOT NULL,      -- encrypted at rest (AES via app layer)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Repositories the user has connected
CREATE TABLE repositories (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    github_url      TEXT NOT NULL,
    default_branch  TEXT NOT NULL DEFAULT 'main',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, github_url)
);

-- Core entity: a sandbox instance
CREATE TABLE sandboxes (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id         UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    parent_sandbox_id     UUID REFERENCES sandboxes(id) ON DELETE SET NULL,
    branch_name           TEXT NOT NULL,
    subdomain             TEXT UNIQUE NOT NULL,
    status                TEXT NOT NULL DEFAULT 'queued',
                          -- queued | building | running | hibernated | failed | expired | deleted
    container_id          TEXT,
    image_tag             TEXT,
    internal_port         INTEGER,
    idle_timeout_seconds    INTEGER NOT NULL DEFAULT 900,
    last_request_at        TIMESTAMPTZ,

    -- Lifetime / auto-destruction (separate concept from idle_timeout above.
    -- idle_timeout pauses; expires_at destroys permanently.)
    lifetime_seconds        INTEGER NOT NULL DEFAULT 86400,
    expires_at               TIMESTAMPTZ NOT NULL,
    destroyed_at             TIMESTAMPTZ,
    destruction_reason       TEXT,

    detected_language      TEXT,
    build_config            JSONB,
    ai_suggested_env        JSONB,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_sandboxes_status ON sandboxes(status);
CREATE INDEX idx_sandboxes_parent ON sandboxes(parent_sandbox_id);
CREATE INDEX idx_sandboxes_expires_at ON sandboxes(expires_at) WHERE destroyed_at IS NULL;

-- Lifetime warning tracking, so the destroyer worker doesn't re-send the same
-- 24h/1h/5min warning repeatedly on every tick
CREATE TABLE sandbox_lifetime_warnings (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id     UUID NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    threshold      TEXT NOT NULL,   -- '24h' | '1h' | '5m' (or proportional equivalents)
    sent_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (sandbox_id, threshold)
);

-- Env vars per sandbox
CREATE TABLE sandbox_env_vars (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id      UUID NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    key             TEXT NOT NULL,
    value           BYTEA NOT NULL,   -- encrypted
    is_ai_suggested BOOLEAN NOT NULL DEFAULT false,
    UNIQUE (sandbox_id, key)
);

-- Sidecar databases (one per sandbox that needs one)
CREATE TABLE sandbox_databases (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id      UUID NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    engine          TEXT NOT NULL,     -- postgres | mysql | sqlite
    container_id    TEXT,
    connection_url  BYTEA NOT NULL,    -- encrypted
    forked_from     UUID REFERENCES sandbox_databases(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Deploy/build history
CREATE TABLE deployments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id    UUID NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    trigger       TEXT NOT NULL,   -- initial | save_redeploy | github_push | manual
    status        TEXT NOT NULL,   -- pending | building | success | failed
    log_excerpt   TEXT,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ
);

-- Resource usage snapshots (periodic flush from Redis)
CREATE TABLE resource_snapshots (
    id                   BIGSERIAL PRIMARY KEY,
    sandbox_id           UUID NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    cpu_percent          REAL NOT NULL,
    memory_used_bytes    BIGINT NOT NULL,
    memory_limit_bytes   BIGINT NOT NULL,
    network_rx_bytes     BIGINT NOT NULL,
    network_tx_bytes     BIGINT NOT NULL,
    recorded_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_resource_snapshots_sandbox_time ON resource_snapshots(sandbox_id, recorded_at);

-- File edit tracking
CREATE TABLE file_edits (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id        UUID NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    file_path         TEXT NOT NULL,
    edited_by         UUID NOT NULL REFERENCES users(id),
    edited_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    pushed_to_github  BOOLEAN NOT NULL DEFAULT false
);