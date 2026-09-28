# API Sandbox Links — Documentation

Turn any public GitHub repository into a disposable, time-limited, remotely-accessed
sandbox. This is the index to all project docs.

| Doc | Read when… | Source of truth in code |
|---|---|---|
| [SETUP.md](SETUP.md) | Getting a dev environment running end-to-end: env vars, build order, ports, OAuth, troubleshooting. | `backend/internal/config/config.go`, `docker-compose.dev.yml`, `.env.example` |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Understanding the two-listener split, routing, wake-on-request, the build pipeline, sidecar databases, and reapers. | `backend/internal/orchestrator/`, `backend/internal/proxy/`, `traefik/dynamic.yml` |
| [API.md](API.md) | Calling the REST/WebSocket surface: routes, request bodies, response shapes, errors. | `backend/internal/api/router.go` + handlers |
| [DATABASE.md](DATABASE.md) | The Postgres schema, migrations, and the GORM models that map onto it. | `backend/migrations/0001_init.up.sql`, `backend/internal/db/models.go` |
| [diagrams/sequence-live-stats.mmd](diagrams/sequence-live-stats.mmd) | Tracing one live metrics session: WS handshake, Docker stream, Redis newest-value key, 10s Postgres flush, 7d prune. | `backend/internal/api/websocket.go`, `backend/internal/orchestrator/stats.go` |

## How the docs stay honest

- Every claim above is cross-referenced against a concrete code path so a doc can
  be verified or fixed quickly when behaviour changes.
- **Ports and env keys are normative** in `backend/internal/config/config.go` and
  `docker-compose.dev.yml`; the README and SETUP.md summarize but never override,
  defaulting to those two files when there is a conflict.
- Schema changes must land as a new file in `backend/migrations/` first; update
  this index's links and DATABASE.md when they do.

## Quick orientation

```
README.md        pitch + quick start + pointers here
docs/
  README.md      (this file)
  SETUP.md       local development runbook
  ARCHITECTURE.md  design overview (Mermaid diagrams inline)
  API.md         reference for the :8080 control plane
  DATABASE.md    schema reference
  diagrams/      standalone Mermaid sources for the longer flows
```

The high-level flow: `Browser → :3000 (Next.js) → API :8080` for control plane
(REST + WebSockets), and `Browser → :80 (Traefik) → gateway :8090 → sandbox
container` for sandbox traffic. Details in ARCHITECTURE.md.