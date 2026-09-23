# API

Base URL: `http://localhost:8080`. All sandbox routes require the session cookie (`asl_session`) set by GitHub OAuth sign-in; in **dev auth mode** any request is auto-assigned the `devuser`.

Common conventions:

- JSON in/out; errors are `{ "error": "<message>" }` with an appropriate status code.
- REST statuses: `200` OK, `201` created, `400` validation, `401`/`403` auth, `404` missing, `409` conflict (e.g. `git.ErrNoChanges`).
- Sandbox lifecycle fields shared by every sandbox object:

```jsonc
{
  "id": "6a90fec3-8aad-4024-b83e-1cb76d867c9f",
  "subdomain": "node-js-sample-7e9fd5",
  "repo_url": "https://github.com/heroku/node-js-sample",
  "branch": "master",
  "status": "running",               // queued|building|running|hibernated|failed|expired|deleted
  "detected_language": "nodejs",
  "container_id": "…",
  "url": "http://node-js-sample-7e9fd5.sandbox.localhost",
  "created_at": "…", "expires_at": "…",
  "lifetime_seconds": 86400, "idle_timeout_seconds": 900,
  "parent_sandbox_id": null
}
```

## Auth

| Method | Path | Description |
|---|---|---|
| GET | `/api/auth/github` | Redirect to GitHub OAuth (disabled → dev auto-login in dev mode). |
| GET | `/api/auth/github/callback` | OAuth callback; sets `asl_session`, redirects to `/`. |
| GET | `/api/me` | Current user `{ id, github_id, username, avatar_url, dev }`. |
| POST | `/api/auth/logout` | Clears session. |
| GET | `/api/health` | `{ status, time, dev_auth }` — no auth required. |

## Sandboxes

| Method | Path | Description |
|---|---|---|
| GET | `/api/sandboxes/` | List current user's sandboxes (array in `sandboxes`). |
| POST | `/api/sandboxes/` | Create. Body: `{ repo_url, branch?, database?: "auto"\|"postgres"\|"mysql"\|"none", lifetime_seconds, idle_timeout_seconds, env_vars? }`. 201 with sandbox. |
| GET | `/api/sandboxes/{id}` | Fetch one sandbox. |
| POST | `/api/sandboxes/{id}/actions/extend` | Extend lifetime by ±24h (clamped to 7d max). Returns sandbox. |
| POST | `/api/sandboxes/{id}/actions/destroy` | Teardown container/volume/network/deployments. Returns sandbox (status `deleted`). |
| POST | `/api/sandboxes/{id}/fork` | Clone a running/hibernated sandbox into a sibling container with new owner. Returns the new sandbox. |
| GET | `/api/sandboxes/{id}/deployments` | `{ deployments: [{ id, trigger, status, started_at, finished_at, log_excerpt }] }`. |
| POST | `/api/sandboxes/{id}/deploy` | Trigger a redeploy from the current working tree (volume). `{ queued: true }`. |
| GET | `/api/sandboxes/{id}/warnings` | `{ warnings: [{ stage, due_at, sent_at }] }`. |
| GET | `/api/sandboxes/{id}/stats/history` | `{ snapshots: [{ recorded_at, cpu_pct, memory_used_bytes, memory_limit_bytes }] }` (last ~30m). |

## File editor (Phase F)

| Method | Path | Description |
|---|---|---|
| GET | `/api/sandboxes/{id}/files` | Working-tree file tree (`.git` excluded). `{ files: [{ path, dir? }] }`. |
| GET | `/api/sandboxes/{id}/file?path=<rel>` | File content. `{ path, content, binary, encoding }`. Paths are `safeJoin`-validated (traversal → 400). |
| PUT | `/api/sandboxes/{id}/file` | Body `{ path, content, is_base64? }` → writes into the volume and **enqueues a save-redeploy** to the container. `{ saved: true, redeploying: true }`. |

## Commit & push (Phase G)

| Method | Path | Description |
|---|---|---|
| POST | `/api/sandboxes/{id}/commit` | Body `{ message, branch?, direct_to_original?, force? }`. Commits the whole working tree and pushes. Default `branch` is `sandbox-edits-<subdomain>` (never touches your real branch); `direct_to_original + force` explicitly overrides. Returns `{ commit, branch, direct, pushed_at }`. `409` when nothing to commit. Requires a real GitHub token (OAuth). |

## WebSockets (authed)

Both use **text frames** with JSON events.

### Logs — `GET /api/sandboxes/{id}/logs`

```jsonc
{ "kind": "log",  "stream": "stdout", "line": "listening on :3000" }
{ "kind": "status", "status": "no-container" }     // sandbox has no running container
```

Server demuxes docker's 8-byte header frames into lines (1 MiB guard), streams `Tail: 200` historical lines first, then follows live output. The client closes the connection to stop following.

### Stats — `GET /api/sandboxes/{id}/stats`

```jsonc
{ "kind": "stats", "stats": { "recorded_at": "…", "cpu_pct": 12.4, "memory_used_bytes": 8125000, "memory_limit_bytes": 268435456 } }
```

Emitter restarts the container's metric loop every 30s and replays the current snapshot on (re)connect.

## Gateway (not REST)

`:8090` is a reverse proxy, not part of this API surface. It also exposes `GET /` for health probes and a host-handling path that performs wake-on-request before forwarding `*.sandbox.localhost`.

## Errors

- Create: 400 when `repo_url` missing/unsupported protocol, lifetime < 300s or > 7d, unknown database.
- Files: 400 on path traversal, 404 when the working tree/volume isn't materialized.
- Commit: 404 sandbox/volume missing; 409 `ErrNoChanges`; 502 if the decrypted token doesn't authenticate with GitHub.