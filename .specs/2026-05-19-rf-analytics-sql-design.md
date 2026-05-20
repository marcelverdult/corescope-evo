# RF Analytics — SQL Backend (Pilot)

**Date:** 2026-05-19
**Status:** Design approved, pending spec review
**Scope:** RF analytics only. Pilot for a 7-group analytics rearchitecture.

## Context & motivation

CoreScope analytics (RF, topology, channels, distance, subpaths, hash, node-health)
compute by iterating the **in-memory `PacketStore`**. There is no SQL aggregation
path and no sqlite-only mode (`sqliteOnly` is hardcoded `false`). `packetStore.retentionHours`
bounds how much loads into RAM; anything not in RAM is invisible to analytics.

Consequences:
- The store must hold all data analytics need → unbounded RAM growth → OOM on small
  hosts (live `analyzer.kiekr.app` OOM'd its 3.8 GB host on 2026-05-19).
- Heavy endpoints (`neighbor-graph` ~10 s, `clock-skew` ~9 s) are slow.
- Analytics can only ever show the `retentionHours` window, never full history.

**Goal:** move analytics off the in-memory store onto SQLite queries. Removes the
RAM ceiling and makes full history queryable regardless of host size.

This document specifies the **RF analytics pilot** — the first of 7 groups. It
establishes the template (query module shape, parity harness, rollout toggle) the
other 6 groups reuse, each as its own spec.

## Decisions

- **Full rearchitecture** is the goal, done group-by-group.
- **`PacketStore` stays** — for live/recent data only (map, WebSocket, `/api/packets`).
  Analytics stop depending on it. Once all 7 groups are migrated, `retentionHours`
  becomes a small fixed live-window, no longer a data-visibility knob.
- **Parity = statistically equivalent.** Aggregates (count/avg/min/max/stddev,
  group-by buckets) match exactly. Percentiles and the SNR/RSSI scatter may differ
  where SQL uses sorted-column fetch / histogram binning; asserted within tolerance.
- **Approach C (hybrid).** SQL does exact aggregates; percentiles via sorted
  single-column fetch; scatter via SQL 2D histogram. Flat memory regardless of DB size.

## Architecture

New file `cmd/server/analytics_rf_sql.go`:

```
computeAnalyticsRFSQL(db *DB, region string, window TimeWindow) (map[string]interface{}, error)
```

- Returns the same `map[string]interface{}` shape as the current `computeAnalyticsRF`.
- Takes `*DB` — **no `PacketStore` data dependency**.
- The existing TTL cache wrapper `GetAnalyticsRFWithWindow` is unchanged: on cache
  miss it calls the SQL computer instead of the in-memory one. Cache keys and
  invalidation stay as-is.
- Config toggle `analytics.sqlBackend` (bool, default `false`) selects old vs new
  per request.
- Old `computeAnalyticsRF` stays in place as the **parity reference**, deleted only
  after cutover.

Blast radius: one new file, one cache-miss branch, one config flag.

## Query design

Schema facts:
- `observations`: `snr REAL`, `rssi REAL`, `timestamp INTEGER` (Unix epoch **seconds**),
  `transmission_id`, `observer_idx`.
- `transmissions`: `hash TEXT UNIQUE`, `payload_type`, `raw_hex`, `first_seen TEXT` (RFC3339).
- Existing indexes: `idx_observations_timestamp`, `idx_observations_ts_obs`,
  `idx_transmissions_first_seen`, `idx_transmissions_payload_type`.

~6 queries, run only on cache miss. All windowed; region adds `observer_idx IN (…)`.

1. **Core aggregates** — one scan of `observations`: `COUNT`, `AVG/MIN/MAX` of snr &
   rssi, plus `SUM(x)` and `SUM(x*x)`. stddev = `sqrt(SUM(x²)/n − avg²)`, exact.
   Also `MIN/MAX(timestamp)`, total obs count.
2. **Payload-type distribution** — `transmissions GROUP BY payload_type` (hash is
   `UNIQUE` → already per-packet).
3. **Hourly counts + signal-over-time** — merged: `GROUP BY strftime('%Y-%m-%dT%H',
   timestamp,'unixepoch')` → `COUNT(DISTINCT transmission_id)` + `AVG(snr)`. The
   `%Y-%m-%dT%H` format matches the old `ts[:13]` keys exactly.
4. **SNR-by-type** — `observations JOIN transmissions GROUP BY payload_type`, same
   SUM / SUM-of-squares trick.
5. **Scatter histogram** — `GROUP BY CAST(snr AS INT), CAST(rssi AS INT)` → ~hundreds
   of bins instead of millions of points.
6. **Percentiles** — `SELECT snr … ORDER BY snr` (and rssi), one REAL column; Go
   picks percentile indices. ~2.3 M floats ≈ 18 MB transient, freed after — bounded.

Window: `TimeWindow` RFC3339 → epoch seconds for `observations` queries;
`first_seen` string-compared for `transmissions` queries. Region: `resolveRegionObservers`
→ `observer_idx` set.

**New index:** composite `idx_observations_ts_snr_rssi (timestamp, snr, rssi)` makes
queries 1, 5, 6 index-only (no row lookups). Added via `ensureServerIndexes`
(`CREATE INDEX IF NOT EXISTS`).

## Error handling

- Query error → log + return error → HTTP 500. **No silent fallback** to the old
  path — that would hide bugs.
- Empty result set → zero-valued structure identical to the old empty output.
- `NULL` snr/rssi → SQL `COUNT(snr)` / `AVG(snr)` skip NULLs natively, matching the
  old `obs.SNR != nil` guards.

## Testing

Parity test `analytics_rf_sql_test.go`:
- Deterministic fixture — small SQLite DB built in test setup with known
  transmissions/observations. Load into an in-memory `PacketStore` (existing test
  helpers), run old `computeAnalyticsRF`; run `computeAnalyticsRFSQL` on the same DB.
- Assert: aggregates exact-equal; percentiles exact on shared data with ±tolerance
  for tie edges; scatter — new bin counts equal a binned projection of old points,
  totals match.
- Matrix: no-region / with-region × zero-window / 24h-window × empty dataset.
- Larger real-data sanity check against the backup DB — manual / CI-skippable
  (too big to commit).

Verification: `go build ./...` and `go test ./...` green before rollout.

## Rollout

1. Ship `computeAnalyticsRFSQL` + new index behind `analytics.sqlBackend` (default
   `false`). Parity tests + build green.
2. Deploy to `analyzer.kiekr.app`, flip flag on, eyeball the RF analytics page.
3. Flip default to `true`; delete old `computeAnalyticsRF` + the flag.

## Non-goals / follow-ups

- The other 6 analytics groups (topology/neighbor-graph, channels, distance,
  subpath, hash, node-health) — each its own spec→plan→implement cycle, reusing
  this pilot's template. Not specified here.
- Eliminating `PacketStore` — it stays for live/recent data.
- Changing the analytics JSON response shape or the frontend.
- Fixing the `estimateStore*Bytes` accounting bug — tracked separately.
