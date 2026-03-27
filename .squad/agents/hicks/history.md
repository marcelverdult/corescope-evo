# Hicks — History

## Project Context

MeshCore Analyzer is a real-time LoRa mesh packet analyzer. Node.js + Express + SQLite backend, vanilla JS SPA frontend. Custom decoder.js fixes path_length bug from upstream library. In-memory packet store provides O(1) lookups for 30K+ packets. TTL response cache achieves 7,000× speedup on bulk health endpoint.

User: User

## Learnings

- Session started 2026-03-26. Team formed: Kobayashi (Lead), Hicks (Backend), Newt (Frontend), Bishop (Tester).
- Split the monolithic "Frontend coverage (instrumented Playwright)" CI step into 5 discrete steps: Instrument frontend JS, Start test server (with health-check poll replacing sleep 5), Run Playwright E2E tests, Extract coverage + generate report, Stop test server. Cleanup/report steps use `if: always()` so server shutdown happens even on test failure. Server PID shared across steps via .server.pid file. "Frontend E2E only" fast-path left untouched.
- Fixed memory explosion in packet-store.js: observations no longer duplicate transmission fields (hash, raw_hex, decoded_json, payload_type, route_type). Instead, observations store only `transmission_id` as a reference. Added `_enrichObs()` to hydrate observations at API boundaries (getById, getSiblings, enrichObservations). Replaced `.all()` with `.iterate()` for streaming load. Updated `_transmissionsForObserver()` to use transmission_id instead of hash. For a 185MB DB with 50K transmissions × 23 observations avg, this eliminates ~1.17M copies of hex dumps and JSON — projected ~2GB RAM savings.
- Built standalone Go MQTT ingestor (`cmd/ingestor/`). Ported decoder.js → Go (header parsing, path extraction, all payload types, advert decoding with flags/lat/lon/name). Ported db.js v3 schema (transmissions + observations + nodes + observers). Ported computeContentHash (SHA-256 based, path-independent). Uses modernc.org/sqlite (pure Go, no CGO) and paho.mqtt.golang. 25 tests passing (decoder golden fixtures from production data + DB schema compatibility). Supports same config.json format as Node.js server. Handles Format 1 (raw packet) messages; companion bridge format deferred. System Go was 1.17 — installed Go 1.22.5 to support modern dependencies.
