# CoreScope-EVO — Roadmap

Accurate current-state + backlog, derived from a full codebase audit (replaces the
stale root plan docs — see "Plan-doc status" below).

## Current state

CoreScope-EVO is a **mature, production-deployed** MeshCore packet analyzer.

- **Backend** — Go, three binaries: `cmd/ingestor` (MQTT daemon, multi-broker,
  batched transactional writes), `cmd/server` (HTTP API — ~60 endpoints — +
  in-memory store over SQLite + WebSocket hub), `cmd/decrypt` (CLI). Normalized
  `transmissions`/`observations` schema. Custom MeshCore decoder, channel
  decryption, ed25519 validation, neighbor graph, path inspection, clock-skew,
  analytics. Well tested (~116 Go test files, race-instrumented).
- **Frontend** — vanilla-JS hash-routed SPA, 19 routes (home, packets, map, live,
  nodes, channels, tools, observers, analytics, perf, audio-lab, mc-keygen,
  per-node detail …). White-label customizer, WebSocket live feed, Leaflet map.
- **Infra** — multi-stage Docker, supervisord (4 service combos), Caddy, CI
  (`deploy.yml`) gating `go-test → e2e-test → build-and-publish → deploy`,
  multi-arch GHCR publish. Deployed to `analyzer.kiekr.app` via Coolify.

The project is not "half-built" — the backlog below is gaps, polish, and debt,
not missing core features.

## Plan-doc status — archive these

The root plan docs are **done or stale** and should be moved to `docs/archive/`:

| Doc | Status |
|-----|--------|
| `BUILD_PLAN.md` | STALE — describes the removed Node.js stack. Retire. |
| `DEDUP-DESIGN.md`, `DEDUP-MIGRATION-PLAN.md` | DONE — normalized schema shipped. |
| `NODE-ANALYTICS-PLAN.md` | DONE — `#/nodes/:pubkey/analytics` shipped. |
| `NEW_USER_SPEC.md` | DONE — home page matches the spec. |
| `CUSTOMIZATION-PLAN.md` | Phase 1–2 DONE; residual bugs → P1 below. |
| `AUDIO-PLAN.md` | DONE except the percussion layer → P2 below. |
| `AUDIO-WORKBENCH.md` | M1 done; M2–M5 → P2 below. |
| `docs/specs/*` | All DONE except `deployment-simplification` (P1) and `rf-health-dashboard` (P2). |
| `docs/go-migration.md` | STALE — says Go images unpublished; GHCR pipeline is live. Rewrite. |

## Backlog

### P0 — correctness / safety
- **Clamp `limit`/`offset` to a hard maximum** on every list endpoint. `queryInt`
  (`cmd/server/routes.go`) and `QueryPackets` (`store.go`) enforce only a floor;
  `?limit=10000000` builds that many maps — unbounded response size / memory.

### P1 — real gaps & honesty
- **mc-keygen acceleration is dead.** GPU path broken + hidden; WASM worker broken
  + disabled (still spun up, unused). Decide: fix properly, or remove the dead
  WASM/GPU code and the misleading "WASM/WebGPU accelerated" copy on the tools
  card (`app.js`). CPU keygen works.
- **Docker `compose pull` path is broken.** All `docker-compose*.yml` use `build:`,
  none reference the published `ghcr.io/...` image; `docker-compose.simple.yml`
  (per `deployment-simplification.md`) is missing. Add it; repoint compose files
  at the GHCR image.
- **Inconsistent page states** — loading/empty/error UI is ad-hoc; `perf.js` shows
  a bare "Loading…". Standardize.
- **`/api/backup`** streams the whole SQLite file with no size guard or rate limit.

### P2 — features (genuinely outstanding)
- **Audio percussion layer** (kick/snare/hihat) — `AUDIO-PLAN.md`, no code yet.
- **Audio Workbench M2–M5** — override sliders, A/B compare, sequence editor, live
  annotation (`AUDIO-WORKBENCH.md`).
- **`/metrics` Prometheus endpoint** — referenced by two specs, not implemented.
- **RF-health dashboard** — confirm whether the analytics "RF" tab satisfies
  `rf-health-dashboard.md` or a dedicated status-grid view is still wanted.
- **Customizer click-to-identify inspector** — the one unbuilt customizer item.

### P3 — tech debt & cleanup
- **Split `cmd/server/store.go` (~9000 lines)** — extract analytics, channel, and
  distance-index code into separate files.
- **Schema-migration framework** — replace the ad-hoc `ensureXColumn` calls in
  `cmd/server/main.go` (ordering hazards noted in comments) with one migration
  table, like the ingestor already has.
- **Test hygiene** — ~50 root `test-*.js` run in neither `test-all.sh` nor CI;
  49 use brittle `fs.readFileSync().includes()` source-grep assertions. Prune
  dead, convert brittle ones to behavioral, wire the curated remainder into CI.
- **Remove dead code** — unused mc-keygen WASM workers, hidden GPU UI,
  `geofilter-draft.js`, `audio-v1-constellation.js`.
- **De-duplicate** the nav route lists hand-synced across `bottom-nav.js` /
  `nav-drawer.js`; the repeated SQL `IN`-clause builders (~8× in `db.go`).
- **Docs** — rewrite `go-migration.md`, retire `BUILD_PLAN.md`, fix README test
  counts, archive the done plan docs.
- **Frontend structure** (longer-term) — huge files (`analytics.js`, `live.js`,
  `packets.js` each 3000+ lines), no build step, heavy `window.*` global use.
