# Setup (local dev)

Verified environment: Linux, Go 1.25.0, Node 24, Docker 29.1.3 + Compose 2.40.3, nixpacks 1.41.0. Everything below targets this box but the steps transfer.

## 1. Prerequisites

- Docker engine running (needed for nixpacks builds and sandbox containers).
- **Nixpacks CLI**: `curl -sSL https://nixpacks.com/install.sh | bash` then check `nixpacks --version`. Point the backend at it with `NIXPACKS_BIN` (defaults to `nixpacks` on PATH).
- A **Redis** — compose brings one up on `:6380` (the host `:6379` may already have a system Redis; backend defaults to the compose endpoint).

## 2. Environment — `api-sandbox-links/.env`

Copy `backend/.env.example` to `./.env` at the repo root of `api-sandbox-links`:

```bash
DATABASE_URL=postgres://sandbox:sandbox@localhost:5433/sandbox_dev?sslmode=disable
REDIS_ADDR=localhost:6380
GATEWAY_PORT=8090
SECRETS_ENCRYPTION_KEY=<32 random bytes, base64>
GITHUB_CLIENT_ID=
GITHUB_CLIENT_SECRET=
NIXPACKS_BIN=/home/you/.local/bin/nixpacks
```

- `SECRETS_ENCRYPTION_KEY`: `openssl rand -base64 32`. Changing it invalidates stored env vars / commit tokens.
- **Dev auth**: leave `GITHUB_CLIENT_ID` empty → the API auto-creates/seeds `devuser` on first authed call, and `github_token` is stubbed. Set real values for OAuth single sign-on (commit & push then works with your token).

> The backend loads `./.env` via godotenv relative to its working directory — **start it from `api-sandbox-links/`**, not from `backend/`, or config fails.

## 3. Backend

```bash
cd api-sandbox-links
go build ./... && go vet ./...
go build -o backend/server ./backend/cmd/server
./backend/server &          # starts :8080 (API) + :8090 (gateway)
```

Health check: `curl http://localhost:8080/api/health` → `{"status":"ok","dev_auth":true}`.

## 4. Infrastructure — `docker-compose.dev.yml`

```bash
docker compose -f docker-compose.dev.yml up -d
```

Brings up Postgres (`:5433`), Redis (`:6380`), and Traefik (`:80`, dashboard `:8081`). The compose file also mounts `traefik/dynamic.yml` (file-provider edge router) and gives Traefik `host.docker.internal:host-gateway` so it can reach the gateway's `:8090` on the host.

**Traefik dashboard**: `http://127.0.0.1:8081/dashboard/` (the API `--api.insecure` listener is pinned to the `traefik` entrypoint `:8081`, not docker's default `:8080`).

## 5. Frontend

```bash
cd frontend
npm install
npm run dev      # http://localhost:3000
```

- `NEXT_PUBLIC_API_BASE` defaults to `http://localhost:8080`.
- `next.config.ts` sets `devIndicators: false` by default; the build gate is `npm run build` (Next 16 typed routes, Tailwind v4).

## 6. First end-to-end run

1. Open the dashboard → paste `https://github.com/heroku/node-js-sample` (branch `master`) → **Create**.
2. Watch status go `queued → building → running` (first-ever nixpacks build can take a couple of minutes; it installs the Nix provider).
3. Open the **public URL** (`<subdomain>.localhost` → `:80` → Traefik → gateway `:8090` → container) — you should get `Hello World!`.
4. Wait out the idle TTL or set `idle_timeout_seconds` low in the create form, then hit the URL again → observe wake-on-request (~1s) bounce back to `running`.
5. Use the editor tab to change a file → **Save & redeploy** → a `save_redeploy` deployment fires; the **Git** tab can commit & push to a `sandbox-edits-*` branch.

## Troubleshooting

- `relation "env_vars" does not exist` → GORM table-name override missing (see DATABASE.md); models `EnvVar`/`LifetimeWarning` need explicit names.
- Backend exits at startup with "DATABASE_URL and REDIS_URL are required" → you launched from the wrong directory; start from `api-sandbox-links/`.
- Traefik access log shows host `-` (no router) → request host isn't `*.sandbox.localhost` exactly; use `--noproxy '*'` on curl in proxied environments.
- Docker provider router errors in the Traefik logs are expected (disabled — see ARCHITECTURE.md); the file provider owns routing.
- `curl` to `localhost` silently going through an HTTP proxy → append `--noproxy '*'`.