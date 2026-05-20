# Distance Analytics Rollup — Design

**Date:** 2026-05-20
**Status:** Design approved, pending spec review
**Scope:** Distance analytics (`/api/analytics/distance`). Pre-aggregates the
per-hop distance records the in-memory `computeAnalyticsDistance` scans on
every request.

## Context & motivation

`/api/analytics/distance` returns six aggregates over the in-memory
`distHops` + `distPaths` slices: `summary`, `topHops`, `topPaths`,
`catStats`, `distHistogram`, `distOverTime`. The in-memory `distHops` slice
is already "rollup-shaped" — each record is one `(from, to, dist, type,
snr, hash, timestamp, hour_bucket)` row — but the index lives only in RAM,
bounded by the in-memory retention window. The endpoint cannot show
full-DB distance analytics; it shows whatever the in-memory store still
holds.

This rollup migrates distance to the same pre-aggregated table pattern
already proven for RF, channels, and node-health: a background-maintained
SQL rollup serves any window over the full DB, flat-fast.

## Goal

Serve `/api/analytics/distance` from pre-aggregated SQL tables. Support an
optional `TimeWindow` (the other rollups already do). Stay flat-fast as
the DB grows. Keep the in-memory `computeAnalyticsDistance` as the
fallback + parity reference.

## Decisions

- **Three rollup tables.** `distance_hourly` (per-type aggregates),
  `distance_pair_hourly` (per node-pair best record), `distance_paths`
  (per-tx path detail). Plus a `distance_path_observers` mapping for
  region-filtered path reads, and `distance_rollup_meta` for the
  watermark. One per response responsibility.
- **Per-observer key with `-1` sentinel for global.** Each per-tx hop is
  written once with `observer_idx = -1` (the global row, deduped per hop)
  AND once per observer_idx that saw the tx (over-counted, for region
  reads). Region read sums per-observer rows over the region's observer
  set; no-region read uses the `-1` row. This avoids the
  RF-style over-count for the no-region case.
- **Fixed-bin distance histogram.** 25 bins, 0–300 km, 12 km width. Values
  clamp to the end bins (the in-memory compute already discards
  > 300 km as measurement noise). Slight change from the current dynamic
  bins; statistically equivalent — same trade-off RF accepted.
- **Fixed-bin SNR histogram** (per pair, for `medianSnr`). 50 bins,
  −30…+20 dB, 1 dB width — matches the RF rollup's SNR bins.
- **Companion path table for topPaths.** `distance_paths(hour, tx_id PK,
  total_dist, hop_count, hash, timestamp, hops_json)` — one row per
  transmission with a path. Indexed `(hour, total_dist)` so the
  top-20-by-distance query is an O(log n) range scan + limit. Bounded
  by transmission count (~96k now, ~few thousand/day growth).
- **Add `TimeWindow` support.** `GetAnalyticsDistanceWithWindow(region,
  window)` mirrors RF/channels. Frontend window selector works uniformly
  across all three groups.
- **Reuse generic helpers** — `rfEffectiveWindow`, `rfWindowHourBounds`,
  `rfRegionObserverIdxs`, `rfIntPlaceholders`, `rfBinIndex`,
  `rfPackBins`, `rfUnpackBins`, `rfAddBins`, `rfHistogramFromBins`.
- **Gated by the existing `analyticsSqlBackend` flag.** Wrapper branches:
  rollup if flag+ready, else in-memory `computeAnalyticsDistance`. The
  in-memory path stays as fallback and parity reference.
- Files: `distance_rollup.go`, `distance_rollup_maintain.go`,
  `distance_rollup_read.go`, `distance_rollup_test.go`. Log prefix
  `[distance-rollup]`.

## Section 1 — schema

```sql
-- Per-type aggregates: serves summary, catStats, distHistogram, distOverTime.
CREATE TABLE IF NOT EXISTS distance_hourly (
    hour         TEXT    NOT NULL,
    type         TEXT    NOT NULL,    -- 'R↔R' | 'C↔R' | 'C↔C'
    observer_idx INTEGER NOT NULL,    -- -1 = global (deduped), >=0 = per-observer (region)
    count        INTEGER NOT NULL DEFAULT 0,
    dist_sum     REAL    NOT NULL DEFAULT 0,
    dist_min     REAL,
    dist_max     REAL,
    dist_bins    BLOB,                -- 25-bin packed int16 LE, 0-300km/12km
    PRIMARY KEY (hour, type, observer_idx)
);
CREATE INDEX IF NOT EXISTS idx_distance_hourly_hour ON distance_hourly(hour);

-- Per-pair best record: serves topHops.
CREATE TABLE IF NOT EXISTS distance_pair_hourly (
    hour            TEXT    NOT NULL,
    pair_key        TEXT    NOT NULL,  -- min(fromPk)|max(toPk), unordered
    type            TEXT    NOT NULL,
    observer_idx    INTEGER NOT NULL,
    count           INTEGER NOT NULL DEFAULT 0,
    best_dist       REAL    NOT NULL DEFAULT 0,
    best_from_name  TEXT,
    best_from_pk    TEXT,
    best_to_name    TEXT,
    best_to_pk      TEXT,
    best_hash       TEXT,
    best_timestamp  TEXT,
    snr_max         REAL,
    snr_bins        BLOB,              -- 50-bin packed int16 LE, -30..+20 dB
    PRIMARY KEY (hour, pair_key, type, observer_idx)
);
CREATE INDEX IF NOT EXISTS idx_distance_pair_hourly_hour ON distance_pair_hourly(hour);

-- Per-tx path detail: serves topPaths (region-free).
CREATE TABLE IF NOT EXISTS distance_paths (
    hour       TEXT    NOT NULL,
    tx_id      INTEGER PRIMARY KEY,
    total_dist REAL    NOT NULL DEFAULT 0,
    hop_count  INTEGER NOT NULL DEFAULT 0,
    hash       TEXT,
    timestamp  TEXT,
    hops_json  TEXT                    -- JSON array of {fromName,fromPk,toName,toPk,dist}
);
CREATE INDEX IF NOT EXISTS idx_distance_paths_hour_dist ON distance_paths(hour, total_dist DESC);

-- Path → observers mapping: region-filtered topPaths.
CREATE TABLE IF NOT EXISTS distance_path_observers (
    tx_id        INTEGER NOT NULL,
    observer_idx INTEGER NOT NULL,
    PRIMARY KEY (tx_id, observer_idx)
);
CREATE INDEX IF NOT EXISTS idx_distance_path_observers_obs
    ON distance_path_observers(observer_idx);

CREATE TABLE IF NOT EXISTS distance_rollup_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
```

**Bin layout:**
- Distance: 25 bins, `[0, 12, 24, …, 288]` km, last bin catches ≥288 km.
- SNR: 50 bins, `[-30, -29, …, 19]` dB, end bins clamp.
- Packed as little-endian `int16` counts (existing `rfPackBins` /
  `rfUnpackBins`).

`dist_min`/`dist_max` are `NULL` when `count = 0` (a defensive case — the
recompute never writes a zero-count row).

## Section 2 — recompute, backfill, maintenance

**Single-hour recompute** (`recomputeDistanceRollupHour(rw, hour)`). The
raw read runs OUTSIDE the write tx, filters on the indexed
`transmissions.first_seen` RFC3339 range (mirrors channels/node-health):

1. `lo`, `hi` = hour bounds in RFC3339 (`'2026-05-20T10:00:00Z'`,
   `…T11:00:00Z'`).
2. Raw read (outside tx):
   ```sql
   SELECT t.id, t.first_seen, t.hash,
          o.observer_idx, o.path_json, o.resolved_path, o.snr
   FROM transmissions t
   JOIN observations o ON o.transmission_id = t.id
   WHERE t.first_seen >= ? AND t.first_seen < ?
     AND (o.path_json IS NOT NULL AND o.path_json != '' AND o.path_json != '[]')
   ```
   Group rows by `tx.id` in Go to collect (a) the distinct observer_idx
   set for that tx and (b) the per-tx SNR via `MAX(o.snr)` across rows
   (the in-memory `distHopRecord.SNR` is `tx.SNR`, a per-tx field
   aggregated from observations during ingest; `MAX` is the matching
   aggregator).
3. Bulk-load `nodes` GPS into a per-hour map:
   ```sql
   SELECT public_key, name, role, lat, lon FROM nodes WHERE lat IS NOT NULL AND lon IS NOT NULL
   ```
   Used to resolve hop pubkeys → `(name, role, lat, lon)`. Fetched once
   per recompute, not per tx.
4. Per tx (grouped by `tx.id` across the join rows):
   - Collect distinct observer_idxs.
   - Parse path: positional `resolved_path[i] ?? path_json[i]`.
   - Resolve each hop pubkey via the GPS map. Chain only nodes with GPS.
   - Compute pairwise haversine for consecutive GPS-known hops.
   - Drop hops > 300 km.
   - Classify each hop as `R↔R` / `C↔R` / `C↔C` from sender + hop roles
     (same rules as `computeDistancesForTx`).
5. For each hop:
   - Per `(type, observer_idx=-1)` and per `(type, oi)` for each observer
     that saw the tx: bump `count`, `dist_sum`, update `dist_min/max`,
     bin into `dist_bins`.
   - Per `(pair_key, type, observer_idx=-1)` and per `(pair_key, type,
     oi)`: bump `count`, update `best_dist` (max) and the
     corresponding `best_*` names/hash/timestamp, update `snr_max`, bin
     into `snr_bins`.
6. For each tx with ≥ 1 hop: build `distance_paths` row (`total_dist =
   sum of hop dists`, `hops_json`). For each observer that saw the tx:
   `distance_path_observers(tx_id, observer_idx)` row.
7. **Short write tx:** `DELETE` rows for this hour from
   `distance_hourly`, `distance_pair_hourly`, `distance_paths`, and
   `distance_path_observers` (via `WHERE tx_id IN (SELECT tx_id FROM
   distance_paths WHERE hour = ?)`); then `INSERT` the new rows;
   `COMMIT`. Scan every nullable column as `sql.Null*`.

**Maintenance** (`runDistanceRollupMaintenance`) — identical shape to RF
/ channels / node-health:

- Watermark on `observations.id` (key `distance_rollup_last_obs_id`).
  Tracks observations so a new observation on an old transmission
  re-touches its hour (covers new region coverage and SNR updates).
- Touched hours:
  ```sql
  SELECT DISTINCT strftime('%Y-%m-%dT%H', t.first_seen)
  FROM observations o JOIN transmissions t ON t.id = o.transmission_id
  WHERE o.id > ?
  ```
- For each touched hour: `recomputeDistanceRollupHour` + `time.Sleep(50ms)`.
- Advance watermark to `MAX(observations.id)`. Set the watermark key even
  on an empty DB so `distanceRollupReady` flips.
- Guard mutex (`distanceRollupMaintMu`, `TryLock`) serializes backfill
  vs the 5-min ticker.

**Backfill** (`backfillDistanceRollupAsync`) = maintenance from
watermark 0. Background goroutine at startup when the flag is on. Logs
`[distance-rollup] backfill complete in <d>`. In-memory fallback serves
the endpoint while backfill runs.

**Eventual consistency caveat.** Resolved path data (`resolved_path`)
and `nodes.lat`/`lon` updates for already-rolled hours don't retroactively
re-roll those hours. Same drift as node-rollup; same justification: the
in-memory `distHops` index has the identical async behavior. Documented.

## Section 3 — read path

`computeAnalyticsDistanceFromRollup(db, region, window)` returns the
same JSON map as `computeAnalyticsDistance`. Steps:

- `eff := rfEffectiveWindow(window)`; `sinceHour, untilHour :=
  rfWindowHourBounds(eff)`; `idxs := rfRegionObserverIdxs(db, region)`.
- Observer-set choice: `oi = -1` when `region == ""` (use global rows);
  else `oi IN (idxs)` (sum per-observer rows; documented over-count).
- **Aggregates** — one query against `distance_hourly`:
  ```sql
  SELECT type, SUM(count), SUM(dist_sum), MIN(dist_min), MAX(dist_max)
  FROM distance_hourly
  WHERE hour >= ? AND hour <= ? AND observer_idx <oi-clause>
  GROUP BY type
  ```
  → catStats per type (avg = sum/count, min, max). Median per type +
  global histogram come from packed-bin sums.
- **Histogram bins** — one query, sum the `dist_bins` BLOBs over the
  matched rows; unpack to 25-bin array; emit
  `{bins, min: dist_min, max: dist_max}` shape via
  `rfHistogramFromBins(bins, 0, 12)`.
- **Median per type** — interpolated from the per-type summed
  `dist_bins`.
- **distOverTime** — `GROUP BY hour` over `distance_hourly`:
  ```sql
  SELECT hour, SUM(count), SUM(dist_sum)
  FROM distance_hourly WHERE … GROUP BY hour ORDER BY hour
  ```
  → `[{hour, avg = sum/count, count}]`.
- **summary** — `totalHops = SUM(count)`,
  `avgDist = SUM(dist_sum)/SUM(count)`, `maxDist = MAX(dist_max)`.
  `totalPaths` from `SELECT COUNT(*) FROM distance_paths WHERE hour …`
  (no-region) or `… JOIN distance_path_observers …` (region).
- **topHops** — query `distance_pair_hourly`, group by `(pair_key, type)`
  over the window, pick the max-`best_dist` row per group, dedupe in Go,
  sort, take top 20. `bestSnr = MAX(snr_max)`. `medianSnr` from summed
  `snr_bins` per pair.
- **topPaths** — region-free:
  ```sql
  SELECT tx_id, total_dist, hop_count, hash, timestamp, hops_json
  FROM distance_paths
  WHERE hour >= ? AND hour <= ?
  ORDER BY total_dist DESC LIMIT 20
  ```
  Region-filtered: JOIN `distance_path_observers` on `tx_id`, restrict
  `observer_idx IN (idxs)`, GROUP BY `tx_id` (a path may belong to
  multiple matching observers), then ORDER + LIMIT.

**Wrapper** — `GetAnalyticsDistanceWithWindow(region, window)`:

```go
if s.analyticsSQLBackend && s.db != nil && distanceRollupReady(s.db.conn) {
    if r, err := computeAnalyticsDistanceFromRollup(s.db, region, window); err == nil {
        return r
    } else {
        log.Printf("[distance-rollup] read failed, falling back: %v", err)
    }
}
return s.computeAnalyticsDistance(region, window)  // existing in-memory, extended for window
```

The in-memory `computeAnalyticsDistance` is extended to accept (and
optionally apply) a `TimeWindow`, mirroring the channels evolution. The
existing `distCache` TTL cache wraps the result.

The `/api/analytics/distance` JSON response shape is unchanged; the
frontend is untouched.

## Section 4 — testing & perf gate

- **Unit tests** — single-hour recompute (raw → aggregates per type
  + per pair + paths, with observer_idx=-1 vs per-observer); positional
  `resolved_path` ?? `path_json` hop resolution; maintenance
  (touched-hours + watermark); read-path assembly (window math, region
  filtering, topHops dedup, topPaths ORDER BY, fixed-bin median).
- **Parity test** — `GetAnalyticsDistanceWithWindow` (rollup) vs
  in-memory `computeAnalyticsDistance` on a small fixture with known
  GPS-tagged nodes and a path. `summary.totalHops/avgDist/maxDist`
  exact; `catStats.count/avg` exact; `distHistogram` fixed-bin shape
  (different from in-memory's dynamic-bin shape — assert against
  recomputed fixed bins). Use an explicit `TimeWindow` covering the
  fixture.
- **Perf gate** — synthetic DB ~50k transmissions with paths over 7
  days, build the rollup, time
  `computeAnalyticsDistanceFromRollup` for region-free and a single-region
  query; assert < 500 ms each.
- **Maintenance test** — insert new observations, run the maintenance
  job, assert rollup updates and watermark advances.

Fixture conventions: `observations.timestamp` epoch seconds
(`1779098400 = 2026-05-18T10:00:00Z`); `transmissions.first_seen`
RFC3339 text. Recompute buckets by `first_seen`. Use an explicit
`TimeWindow`, never `TimeWindow{}`.

## Section 5 — rollout

- Schema, backfill, and the 5-min maintenance ticker are wired into
  `main.go` (mirroring the RF/channels/node-health wiring at
  `main.go:243-249` + `main.go:609-664`). All active only when
  `analyticsSqlBackend` is true.
- Sequence: deploy → flag already on → backfill runs in background
  (in-memory fallback serves the endpoint meanwhile) → backfill
  completes (`[distance-rollup] backfill complete`) → maintenance keeps
  it fresh → rollup serves.
- Watch the deploy logs for the completion line and **no `SQLITE_BUSY`
  storm** beyond the steady-state pattern other rollups already show. A
  storm → roll back the flag (kills all rollups; document trade-off)
  and fix.

## Non-goals / follow-ups

- The remaining analytics groups (hash-sizes, subpath, topology) — each
  its own rollup later.
- Retroactive re-roll of old hours when late `resolved_path` or
  `nodes.lat/lon` updates land — same drift accepted across all
  rollups; not addressed.
- Changing the `/api/analytics/distance` response shape or the
  frontend.
- A finer time tier (e.g., 15-minute buckets) for sub-hour precision —
  current hour-bucket granularity matches the in-memory `HourBucket`.
