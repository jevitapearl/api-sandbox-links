# Setup (local dev)

Verified environment: Linux, Go 1.25, Node 24, Docker 29 + Compose 2.x, nixpacks 1.x.
The steps below target this box but transfer to any Linux/macOS dev machine.

> There is **no Makefile** despite older references to `make bootstrap`/`make up` —
> use the commands below directly. `go.mod` lives in `backend/`, so build from
> `backend/`; the backend reads `.env` relative to its **working directory**, so
> run the binary from the repo root.

## 1. Prerequisites

- Docker engine running (needed for nixpacks builds and sandbox containers).
- **Nixpacks**: `curl -sSL https://nixpacks.com/install.sh | bash`, then confirm
  with `nixpacks --version`. Point the backend at it via `NIXPACKS_BIN` (defaults
  to `nixpacks` on PATH).
- Redis is provided by compose; the host's own `:6379`, if any, is never used.

## 2. Environment — `.env`

Copy `.env.example` (the one at the **repo root**) to `.env` and fill it in:

```bash
cp .env.example .env
```

Required values (backend refuses to start without them):

| Key | Value / note |
|---|---|
| `DATABASE_URL` | `postgres://user:pass@localhost:5432/sandbox_platform` — matches `docker-compose.dev.yml` |
| `REDIS_URL` | `redis://localhost:6380` |
| `SECRETS_ENCRYPTION_KEY` | `openssl rand -base64 32`. Rotating it invalidates stored env vars and GitHub tokens |

Dev-auth mode vs real OAuth — leave `GITHUB_CLIENT_ID` empty for dev auth; the API
auto-seeds a `devuser` and every `requireUser` request is auto-assigned it (no
OAuth round-trip). Set real `GITHUB_CLIENT_ID`/`GITHUB_CLIENT_SECRET` to enable
GitHub single sign-on and push-from-sandbox (section 5).

The rest of the keys are optional overrides (`DOCKER_HOST`, `SANDBOX_WORK_DIR`,
`BACKEND_PORT`, `GATEWAY_PORT`, `FRONTEND_URL`, `SANDBOX_BASE_DOMAIN`,
`NIXPACKS_BIN`, `GEMINI_API_KEY`, `MAX_LIFETIME_SECONDS`). Defaults live in
`backend/internal/config/config.go`.

## 3. Infrastructure — `docker-compose.dev.yml`

```bash
docker compose -f docker-compose.dev.yml up -d
```

| Service | Host port | Notes |
|---|---|---|
| Postgres | `5432` | platform metadata (`user:pass`, db `sandbox_platform`) |
| Redis | `6380` | hot state (idle TTL keys, newest live metric per sandbox) |
| Traefik | `80`, `8081` | sandbox edge (`:80`), dashboard (`:8081`) |

Traefik runs with the Docker provider **disabled** and a file-provider wildcard
router (`traefik/dynamic.yml`) that sends every `*.sandbox.localhost` host to the
backend's gateway on the host at `:8090` (via `extra_hosts: host.docker.internal`).
Dashboard: `http://127.0.0.1:8081/dashboard/`.

## 4. Backend

Build from `backend/` (that is where `go.mod` lives):

```bash
cd api-sandbox-links/backend
go build ./... && go vet ./...
go test -race ./...     # see the note below
go build -o server ./cmd/server
```

The tests need no backend process, but two suites reach for real infrastructure
and skip themselves when it is absent:

- `internal/orchestrator/logs_integration_test.go` drives a real **Docker**
  daemon, starting throwaway containers to check the frame demux, the tail
  backfill, and teardown. Honours `ASL_TEST_IMAGE` (default `node:20-alpine`) and
  `DOCKER_HOST`. Point it at a small local image so it does not pull 800 MB.
- `internal/db/queries_test.go` needs a real **Postgres**, because the migration,
  the `ON CONFLICT` clause and the newest-N window are not things a unit test can
  check. Set `ASL_TEST_DATABASE_URL`:

  ```bash
  docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=pw postgres:16-alpine
  ASL_TEST_DATABASE_URL='postgres://postgres:pw@localhost:55432/postgres?sslmode=disable' \
    go test ./internal/db/
  ```

Then run from the **repo root** (godotenv loads `./.env` from the CWD — running
from `backend/` makes config fail):

```bash
cd api-sandbox-links
./backend/server &              # :8080 API + :8090 gateway
```

Health check:

```bash
curl http://localhost:8080/api/health
# {"status":"ok","time":"…","dev_auth":true}
```

To stop the backend, `kill $(pgrep -f './backend/server')` (never `pkill -f
"./backend/server"` from a shell whose own command line matches the pattern).

## 5. Frontend

```bash
cd frontend
npm install
npm run dev      # http://localhost:3000
```

- `NEXT_PUBLIC_API_BASE` defaults to `http://localhost:8080`; override in
  `frontend/.env.local` (that file's template is `frontend/.env.example`). An
  empty or whitespace-only value is treated as unset, since the WebSocket panels
  would otherwise build a relative URL the browser refuses to open.
- The build gate is `npm run build` (Next 16 App Router, Tailwind v4).
- Lint/typecheck: `npm run lint` and `npx tsc --noEmit`.
- Frontend unit tests: `npm test` (Vitest, `npm run test:watch` to iterate).
  The suites cover the pure helpers in `src/lib` — reconnect policy in
  `ws.test.ts` and frame normalization in `frames.test.ts` — and need neither a
  DOM nor a running backend. A component test will need `environment: "jsdom"`
  plus `@vitejs/plugin-react` and `@testing-library/react`; see
  `frontend/node_modules/next/dist/docs/01-app/02-guides/testing/vitest.md`.

## 6. GitHub OAuth (optional; skip to stay in dev auth)

1. Create an OAuth App at GitHub → Settings → Developer settings → OAuth Apps.
2. Homepage URL: `http://localhost:3000`. Authorization callback URL must **byte
   -for-byte match** `GITHUB_OAUTH_CALLBACK_URL` from `.env`
   (`http://localhost:8080/api/auth/github/callback`) — no trailing slash, same
   host/port. Mismatches surface as GitHub's `redirect_uri is not associated
   with this application`.
3. Set `GITHUB_CLIENT_ID`/`GITHUB_CLIENT_SECRET` in `.env`, restart the backend.
4. Flow: `/api/auth/github` → GitHub consent → `/api/auth/github/callback` →
   `asl_session` cookie set → redirect to `http://localhost:3000/dashboard?authed=1`.
   Scopes requested: `read:user user:email repo`.

## 7. First end-to-end run

1. Open the dashboard → paste `https://github.com/heroku/node-js-sample`
   (branch `master`) → **Create**.
2. Watch status go `queued → building → running` (first-ever nixpacks build can
   take a couple of minutes; it installs the Nix provider).
3. Open the **public URL** (`<subdomain>.sandbox.localhost` → `:80` → Traefik →
   gateway `:8090` → container) — you should get the app's response.
4. Wait out the idle TTL (or set `idle_timeout_seconds` low when creating), then
   hit the URL again → observe wake-on-request flipping back to `running`.
5. Use the editor tab to change a file → **Save & redeploy** → a `save_redeploy`
   deployment fires; the **Git** tab can commit & push to a `sandbox-edits-*`
   branch (push needs real OAuth).

## Troubleshooting

- `config: DATABASE_URL and REDIS_URL are required` → you launched from the wrong
  directory; start the binary from `api-sandbox-links/`.
- Backend can't reach Postgres (`connection refused` on 5432) though the container
  looks healthy → the port mapping silently dropped; fix with
  `docker compose -f docker-compose.dev.yml up -d --force-recreate postgres`.
- `relation "env_vars" does not exist` → a GORM table-name override is missing;
  `EnvVar`/`LifetimeWarning` map to `sandbox_*` tables (see DATABASE.md).
- Deployed link shows **"Sandbox container is not responding"** → the app
  hardcodes a port the platform didn't predict. The deploy pipeline probes
  listening ports and records the real one (see ARCHITECTURE.md); worktrees that
  bind odd ports may need a redeploy after that fix lands.
- `curl` to `localhost` silently going through an HTTP proxy → append
  `--noproxy '*'`; Traefik logs may show host `-` (no router) for the same reason.
- Docker provider router errors in the Traefik logs are **expected** (the provider
  is disabled by design).
- `/api/auth/github` returns 501 → OAuth config missing; either fill
  `GITHUB_CLIENT_ID`/`SECRET` or use dev auth mode.