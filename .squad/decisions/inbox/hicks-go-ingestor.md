# Decision: Go MQTT Ingestor (First Step of Go Rewrite)

**Date:** 2026-03-27
**Author:** Hicks
**Status:** Implemented

## What

Created a standalone Go MQTT ingestor service at `cmd/ingestor/`. This is a separate process from the Node.js web server that handles MQTT packet ingestion and writes to the same shared SQLite database.

## Why

Separating MQTT ingestion from the web server is the first step toward a Go rewrite. The ingestor is CPU-bound (packet decoding) and I/O-bound (DB writes) — Go handles both better than Node.js for this workload. A static binary also simplifies deployment (no `node_modules`).

## Architecture

- **Single binary**, no CGO (uses `modernc.org/sqlite` pure Go)
- Reads same `config.json` format as Node.js (mqttSources array)
- Shares SQLite DB with Node.js server (WAL mode for concurrent access)
- Handles Format 1 (raw packet) MQTT messages only — companion bridge format stays in Node.js for now
- Does NOT serve HTTP/WebSocket — web layer stays in Node.js

## What's Ported

- `decoder.js` → `decoder.go` (header, path, all payload types, advert with flags/lat/lon/name)
- `computeContentHash` → Go (SHA-256, path-independent)
- `db.js` v3 schema → `db.go` (transmissions, observations, nodes, observers — same column names)
- MQTT connection logic from `server.js` → `main.go` (multi-broker, reconnect, IATA filter)

## What's Not Ported

- Companion bridge format (Format 2)
- Channel key decryption
- WebSocket broadcast
- In-memory packet store / cache

## Tests

25 Go tests (decoder golden fixtures from production data + DB schema compatibility + E2E ingest pipeline). All passing.

## Impact

- No existing JS files modified
- Node.js server continues to work unchanged
- Both can run simultaneously, writing to the same DB
- Go 1.22+ required to build (system had Go 1.17 — Go 1.22.5 was installed)
