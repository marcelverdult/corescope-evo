# RF Analytics Rollup — Design

**Date:** 2026-05-19
**Status:** Design approved, pending spec review
**Scope:** RF analytics only. Replaces the on-demand SQL backend with a pre-aggregated rollup.

## Context & motivation

The flag-gated RF analytics SQL backend (`analyticsSqlBackend`, spec `2026-05-19-rf-analytics-sql-design.md`) was implemented, merged (`8b7e0994`), deployed, and activated on analyzer.kiekr.app — where `/api/analytics/rf` returned **502 after 901 s**.

Cause: a request with no window param makes `computeAnalyticsRFSQL` query the entire `observations` table (~1.8M rows, growing ~700k/day) — an unindexed `ORDER BY snr` full sort, ~1.8M floats fetched into Go (twice), plus heavy joins. The in-memory path was fast only because `retentionHours:1` kept it tiny. Parity tests used small fixtures, so the perf cliff was never caught. The flag was rolled back to `false`.

Aggregating a growing full table on demand is inherently O(total rows) and gets slower forever. The requirement is **full-DB analytics that are fast and stay fast as data grows**. The only way to achieve that is **pre-aggregation**: a rollup table.

## Goal

Make every RF analytics query — global and region-filtered, any history depth — fast, by reading a pre-aggregated `rf_rollup` table instead of scanning raw `observations`. Keep it fast as the DB grows.

## Decisions

- **Approach C — rollup table.** A background-maintained `rf_rollup` holds per-hour pre-aggregated RF stats; the read path sums rollup rows.
- **Per-observer key** `(hour, payload_type, observer_idx)` — the only granularity that serves region-filtered analytics fast. Costs ~10k rollup rows/day (~1.5–2 GB/year with packed histograms); accepted.
- **Fixed-bin histograms** — dynamic min/max bins cannot be rolled up. SNR/RSSI/size use fixed bins. Slight change from the old dynamic bins; "statistically equivalent".
- **Approximate median** — interpolated from the summed fixed-bin histogram. The old `vals[len/2]` was already crude.
- **Approximate region `totalTransmissions`** — a per-observer rollup cannot exactly dedupe a transmission seen by N observers; `SUM(n_tx)` over-counts for region queries. No-region `totalTransmissions` is exact via a per-`(hour,payload_type)` `distinct_tx`.
- Reuses the existing `analyticsSqlBackend` flag and the `rfCache` TTL cache. The in-memory `computeAnalyticsRF` stays as fallback and parity reference.

## Section 1 — `rf_rollup` schema

Table `rf_rollup`, PK `(hour, payload_type, observer_idx)`:

- `hour TEXT` — UTC hour bucket, `'2026-05-19T07'`
- `payload_type INTEGER` — `-1` sentinel for NULL payload_type (SQLite composite-PK NULL is messy)
- `observer_idx INTEGER`
- Scalars: `n_obs, n_snr, snr_sum, snr_sumsq, snr_min, snr_max, n_rssi, rssi_sum, rssi_sumsq, rssi_min, rssi_max, pkt_n, pkt_sum, pkt_min, pkt_max, n_tx`
- Histograms — packed `int16` little-endian count arrays: `snr_bins BLOB`, `rssi_bins BLOB`, `size_bins BLOB`
- Scatter — `scatter BLOB`: ~8 sampled `(snr,rssi)` pairs for this cell

Companion table `rf_rollup_tx`, PK `(hour, payload_type)`: `distinct_tx INTEGER` — exact distinct-transmission count per hour/type for the no-region case.

**Fixed bin definitions** (compiled-in constants):
- SNR: −30…+20 dB, 1 dB width → 50 bins
- RSSI: −130…−20 dBm, 1 dB width → 110 bins
- Size: 0…256 B, 4 B width → 64 bins
- Values outside the range clamp to the first/last bin.

stddev is derived: `sqrt(SUM(sumsq)/n − (SUM(sum)/n)^2)`.

## Section 2 — Backfill + incremental maintenance

**Backfill (one-time).** On startup, when `analyticsSqlBackend` is true and `rf_rollup` is empty: roll up all existing `observations` (joined to `transmissions` for `payload_type`/`raw_hex`) by `(hour, payload_type, observer_idx)`, computing aggregates + bin counts + scatter samples, and bulk-insert. ~1.8M rows → minutes. Runs in a background goroutine (pattern: existing `backfillResolvedPathsAsync`), not blocking startup. While backfill is in progress the RF read path uses the in-memory fallback so analytics never break.

**Incremental maintenance — recompute-touched-hours.** A periodic job (~5 min, registered with the existing background-task machinery, active only when the flag is on):
- Tracks a watermark — the last rolled-up observation `id`.
- Each cycle: select the distinct `hour`s among observations with `id > watermark`. For each such hour: `DELETE` its `rf_rollup` and `rf_rollup_tx` rows, recompute that hour from raw, insert. Advance the watermark to the max observed `id`.
- The current open hour always has new data → recomputed every cycle (~30k raw rows for one hour → cheap).
- Late-arriving data for a closed hour → that hour recomputed once.

Recomputing a whole hour from raw is idempotent and simple — chosen over incremental delta-merge, which would have to merge packed histogram blobs (fiddly, error-prone).

## Section 3 — Read path

`computeAnalyticsRFSQL` is rewritten to read `rf_rollup`. Given (window, region):

- **Scalars** — SQL `SUM(n_obs)`, `SUM(snr_sum)`, `SUM(snr_sumsq)`, `MIN(snr_min)`, `MAX(snr_max)`, … over `rf_rollup WHERE hour` in window `[AND observer_idx IN (region observer set)]`. One row back, any history depth → flat-fast. Region observer set from the existing `rfRegionObserverIdxs`.
- **Per-hour / per-type** — `GROUP BY hour` → `packetsPerHour`, `signalOverTime`; `GROUP BY payload_type` → `payloadTypes`, `snrByType`. SQL collapses millions of cells to ≤8760 hour-rows / ~5 type-rows server-side.
- **Histograms** — fetch the matching rows' `snr_bins`/`rssi_bins`/`size_bins` blobs, element-sum in Go → fixed-bin histogram, emitted in the existing `{bins:[{x,w,count}],min,max}` shape.
- **Median** — interpolated from the summed SNR/RSSI histogram (the bin containing the n/2-th value).
- **Scatter** — sub-sample the matching rows' `scatter` blobs down to ≤500 `{snr,rssi}` points (existing `scatterData` shape).
- **`totalTransmissions`** — no-region: `SUM(distinct_tx)` from `rf_rollup_tx`. Region: `SUM(n_tx)` over the region's rollup rows (approximate — documented).

**Performance profile:** scalars + per-hour + per-type stay flat-fast at any depth (SQL `GROUP BY` collapses server-side). Histogram + scatter fetch per-cell blobs — a 24h query fetches ~10k small blobs (instant); a cold full-history query fetches ~1.5–2M blobs/year (~1–2 s, then `rfCache`-cached). Accepted. Truly-flat full-history histograms would need a second rollup tier (hourly → daily) — a deferred follow-up extension, not in this scope.

The response JSON shape is unchanged; the frontend is untouched.

## Section 4 — Testing & perf gate

- Unit tests: rollup-row computation (raw → aggregates + bins), backfill, incremental maintenance (recompute-touched-hours + watermark advance), read-path assembly.
- Parity tests: rollup-based `computeAnalyticsRFSQL` vs in-memory `computeAnalyticsRF` on a fixture. Tolerance — scalars exact; histograms compared against a fixed-bin recomputation of the same raw data; median within one bin width.
- Perf gate (both):
  1. Automated benchmark — synthetic DB ~1M+ observations, backfill the rollup, run `computeAnalyticsRFSQL` for a 24h window and for full history; assert under thresholds (24h < 200 ms, full-history < 2 s). Runs in CI.
  2. Manual — run against the real backup DB before the live flag is re-enabled.
- Maintenance-job test: insert new observations, run the job, assert the rollup updates and the watermark advances.

## Section 5 — Rollout

- `rf_rollup` backfill + the maintenance job are active only when `analyticsSqlBackend` is `true` (flag off → no rollup work).
- Sequence: deploy → flag on → backfill runs in background (in-memory fallback serves RF meanwhile) → backfill completes, maintenance keeps the rollup fresh → rollup serves.
- The live flag is re-enabled only after the perf gate passes (benchmark green + manual backup-DB check).

## Non-goals / follow-ups

- The other 6 analytics groups (topology/neighbor-graph, channels, distance, subpath, hash, node-health) — each needs its own rollup later; this RF rollup is the template. Not in scope.
- Two-tier rollup (hourly → daily) for truly-flat full-history histograms — deferred extension.
- Changing the analytics JSON response shape or the frontend.
- The old on-demand `computeAnalyticsRFSQL` query helpers (`rfCoreAggregates`, `rfSortedColumn`, etc.) are replaced by the rollup read path; `rfSortedColumn` and the unbounded scan paths are removed.
