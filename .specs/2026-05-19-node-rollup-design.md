# Node-Health Rollup — Design

**Date:** 2026-05-19
**Status:** Design approved, pending spec review
**Scope:** Node-health relay/usefulness analytics. Pre-aggregates the per-node
relay-activity enrichment that makes `/api/nodes` slow.

## Context & motivation

`/api/nodes` takes ~13 s in production. The handler `handleNodes`
(`routes.go:1236`) fetches a page of nodes (default 50) from the `nodes`
table — cheap — then **enriches every node in the page** with two in-memory
scans:

- `GetRepeaterRelayInfo(pubkey, window)` — scans the `byPathHop` index for
  non-advert packets that name the node as a path hop; computes
  `LastRelayed`, `RelayCount1h`, `RelayCount24h`, `RelayActive`.
- `GetRepeaterUsefulnessScore(pubkey)` — relay-hop count for the node divided
  by the total non-advert packet count in the store. **Recomputes the
  denominator on every call.**

Both run **per node, ×50 per page**. That loop is the 13 s. The fix is the
same one used for RF and channels analytics: a background-maintained rollup
table, read instead of scanned.

## Goal

Serve `/api/nodes` relay-activity + usefulness from a pre-aggregated
`node_rollup` table. One batched query per page instead of 50 in-memory
scans. Stays flat-fast as the DB grows.

## Decisions

- **Approach — hourly path-hop rollup.** `node_rollup` holds, per hour and
  per hop key, the count of distinct non-advert transmissions whose path
  included that hop. The read path sums rollup rows over a window.
- **Key `(hour, hop_key)`.** `hop_key` is a lowercased path hop — the
  resolved full pubkey where the hop resolved, else the raw 1-byte wire
  prefix. **No `observer_idx`** — `handleNodes` never region-filters
  relay/usefulness, so the per-observer dimension the RF/channels rollups
  carry is not needed here.
- **Companion `node_rollup_total(hour, n_nonadvert)`** — total non-advert
  transmissions per hour, the usefulness-score denominator.
- **Usefulness over a trailing 7-day window.** The in-memory score divides by
  "whatever is in the store" (ill-defined for a full-DB rollup). Both
  numerator and denominator are computed over the trailing 7 days — a stable,
  meaningful "recent usefulness".
- **Per-transmission hop dedup at recompute time.** Each transmission
  contributes `+1` to each *distinct* hop key in its path. A hop is keyed by
  its resolved pubkey when resolved, else by its raw prefix — never both — so
  a read-time `SUM` over `(full pubkey, prefix)` does not double-count. This
  mirrors the in-memory dedup-by-tx-ID in `GetRepeaterRelayInfo`.
- **Bulk read.** A new bulk function takes the page's pubkeys and returns the
  whole relay/usefulness map in one query; `handleNodes` calls it once before
  its enrichment loop.
- Reuses the existing `analyticsSqlBackend` flag. The in-memory
  `GetRepeaterRelayInfo` / `GetRepeaterUsefulnessScore` stay as the fallback
  (flag off / rollup not ready) and as the parity reference.
- Naming follows the RF/channels groups: table `node_rollup`, files
  `node_rollup.go` / `node_rollup_maintain.go` / `node_rollup_read.go`, log
  prefix `[node-rollup]`. (The task brief's working name `node_hourly` is
  superseded for consistency.)

## Section 1 — `node_rollup` schema

Table `node_rollup`, PK `(hour, hop_key)`:

- `hour TEXT` — UTC hour bucket of the transmission's `first_seen`,
  `'2026-05-19T07'`.
- `hop_key TEXT` — lowercased hop: resolved full pubkey, else raw 1-byte
  wire prefix (e.g. `'ab'`).
- `relay_count INTEGER` — distinct non-advert transmissions in that hour
  whose path included this hop.
- `last_relayed TEXT` — `MAX(first_seen)` (RFC3339) among those
  transmissions.

```sql
CREATE TABLE IF NOT EXISTS node_rollup (
    hour TEXT NOT NULL,
    hop_key TEXT NOT NULL,
    relay_count INTEGER NOT NULL DEFAULT 0,
    last_relayed TEXT,
    PRIMARY KEY (hour, hop_key)
);
CREATE INDEX IF NOT EXISTS idx_node_rollup_hop  ON node_rollup(hop_key);
CREATE INDEX IF NOT EXISTS idx_node_rollup_hour ON node_rollup(hour);
```

`idx_node_rollup_hop` is load-bearing — the read path queries
`WHERE hop_key IN (...)`.

Companion `node_rollup_total`, PK `hour`:

```sql
CREATE TABLE IF NOT EXISTS node_rollup_total (
    hour TEXT PRIMARY KEY,
    n_nonadvert INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS node_rollup_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
```

**Non-advert** = `payload_type IS NULL OR payload_type != 4` (4 = ADVERT).
Self-originated adverts are not relay activity, matching
`GetRepeaterRelayInfo`'s `payloadTypeAdvert` filter.

## Section 2 — Single-hour recompute, backfill, maintenance

**Recompute one hour** (`recomputeNodeRollupHour`). Idempotent: delete then
re-insert the hour. The raw read runs **outside the write transaction** and
filters on the **indexed `transmissions.first_seen` RFC3339 range**
(mirrors `recomputeChannelRollupHour`):

1. Parse hour `'2026-05-18T10'` → `lo = '2026-05-18T10:00:00Z'`,
   `hi = '2026-05-18T11:00:00Z'`.
2. Raw read, outside any transaction:
   ```sql
   SELECT t.id, t.first_seen, o.path_json, o.resolved_path
   FROM transmissions t JOIN observations o ON o.transmission_id = t.id
   WHERE t.first_seen >= ? AND t.first_seen < ?
     AND (t.payload_type IS NULL OR t.payload_type != 4)
   ```
3. For each row, build the observation's hop keys **positionally**: for hop
   index `i`, key = `resolved_path[i]` when present and non-null, else
   `path_json[i]` (the raw wire hop). Lowercase. `resolved_path` is a JSON
   array of nullable pubkey strings (`[]*string`); `path_json` is a JSON
   array of raw hop strings. A missing/short `resolved_path` falls back to
   `path_json` entirely.
4. Accumulate per transmission: a `map[txID]map[hop_key]bool`. Each
   transmission contributes its *distinct* hop-key set (union across all its
   observations).
5. Per `hop_key`: `relay_count` = number of distinct txIDs carrying it;
   `last_relayed` = max `first_seen` among them.
6. Count `n_nonadvert` for the hour with a separate indexed query:
   ```sql
   SELECT COUNT(*) FROM transmissions
   WHERE first_seen >= ? AND first_seen < ?
     AND (payload_type IS NULL OR payload_type != 4)
   ```
   (Counts all non-advert transmissions, including any with no observation —
   matching the in-memory `byPayloadType` denominator.)
7. **Short write transaction:** `DELETE FROM node_rollup WHERE hour=?`,
   `DELETE FROM node_rollup_total WHERE hour=?`, insert the new rows,
   `COMMIT`. Scan every nullable column (`path_json`, `resolved_path`,
   `payload_type`) as `sql.Null*`.

**Maintenance** (`runNodeRollupMaintenance`, mirrors `runRFRollupMaintenance`):

- Watermark on `observations.id` (key `node_rollup_last_obs_id`). Tracking
  observations — not transmissions — catches a *new observation on an old
  transmission*, which can add path data to an already-rolled hour.
- Each cycle: touched hours =
  ```sql
  SELECT DISTINCT strftime('%Y-%m-%dT%H', t.first_seen)
  FROM observations o JOIN transmissions t ON t.id = o.transmission_id
  WHERE o.id > ?
  ```
  (`o.id > ?` uses the PK; the join is by PK — small set.) Recompute each
  touched hour, `time.Sleep(50ms)` between hours, advance the watermark to
  `MAX(observations.id)`.
- A guard mutex (`nodeRollupMaintMu`, `TryLock`) serializes the backfill
  against the ticker so a long backfill is never duplicated.

**Backfill** (`backfillNodeRollupAsync`) = maintenance from watermark 0. Runs
in a background goroutine at startup when the flag is on and the rollup is
empty; logs `[node-rollup] backfill complete in <d>`. While backfill runs,
the read path uses the in-memory fallback so `/api/nodes` never breaks.

**`nodeRollupReady`** — true once `node_rollup_meta` holds the watermark key.

**Eventual-consistency caveat.** `observations.resolved_path` is filled by an
async backfill (`backfillResolvedPathsAsync`). An hour rolled up before its
observations resolved keys those hops by raw prefix; if that hour is later
re-touched it shifts to resolved-pubkey keys. The in-memory `byPathHop` index
has the identical async behavior, so this is parity-preserving, not a
regression. Documented; not corrected by a periodic full rebuild.

## Section 3 — Read path

`computeNodeRelayFromRollup(db, pubkeys, relayWindowHours)` returns
`map[pubkey] -> RepeaterRelayInfo + usefulness`. One call per `/api/nodes`
page.

For the set of pubkeys, build the key set = each pubkey (lowercased) plus its
1-byte prefix (`pubkey[:2]`). Then:

- **Relay counts + last relayed** — one query:
  ```sql
  SELECT hop_key, hour, relay_count, last_relayed
  FROM node_rollup
  WHERE hop_key IN (<keys>) AND hour >= ?
  ```
  bounded at the trailing-7-day hour. In Go, fold each pubkey's own key and
  its prefix key together (a transmission is keyed under exactly one of the
  two per the recompute-time dedup, so summing is safe):
  - `RelayCount24h` = `SUM(relay_count)` over hours ≥ the 24h-ago bucket.
  - `RelayCount1h` = `SUM(relay_count)` over hours ≥ the 1h-ago bucket.
    Hour-bucket granularity — a small over-count vs the in-memory rolling
    cutoff. Accepted approximation (an hourly rollup cannot do sub-hour
    precision); documented.
  - `LastRelayed` = `MAX(last_relayed)`.
  - `RelayActive` = `LastRelayed` within `relayWindowHours` of now.
- **Usefulness** — numerator = `SUM(relay_count)` over the trailing 7 days
  for the node's keys; denominator = `SUM(n_nonadvert)` over the trailing
  7 days from `node_rollup_total` (one query, fetched once per page).
  `score = clamp(numerator / denominator, 0, 1)`; `0` when the denominator
  is `0`.

The prefix-key lookup can over-count when two nodes share a first byte —
this is the **same documented behavior** as the in-memory
`GetRepeaterRelayInfo` (issue #662).

**Cache wrapper.** A `PacketStore` method `GetBulkNodeRelay(pubkeys,
relayWindowHours)` branches:

```go
if s.analyticsSQLBackend && s.db != nil && nodeRollupReady(s.db.conn) {
    return computeNodeRelayFromRollup(s.db, pubkeys, relayWindowHours)
}
// fallback: per-node in-memory GetRepeaterRelayInfo + GetRepeaterUsefulnessScore
```

`handleNodes` calls `GetBulkNodeRelay` once with the page's pubkeys, then its
enrichment loop reads from the returned map instead of calling the per-node
functions. The in-memory `GetRepeaterRelayInfo` /
`GetRepeaterUsefulnessScore` are unchanged — they back the fallback and the
parity tests.

The `/api/nodes` JSON response shape is unchanged; the frontend is untouched.

## Section 4 — Testing & perf gate

- **Unit tests** — single-hour recompute (raw → hop-key counts +
  `last_relayed` + `n_nonadvert`); positional `resolved_path` ?? `path_json`
  hop-key resolution, including null entries and short/absent
  `resolved_path`; maintenance (touched-hours + watermark advance); read-path
  assembly (window math, prefix folding, usefulness clamp).
- **Parity test** — `GetBulkNodeRelay` (rollup) vs per-node in-memory
  `GetRepeaterRelayInfo` + `GetRepeaterUsefulnessScore` on a fixture.
  `RelayCount24h` / `LastRelayed` / `RelayActive` exact; `RelayCount1h`
  within one hour bucket; usefulness exact when the fixture fits inside the
  7-day window. Use an explicit `TimeWindow` covering the fixture, never
  `TimeWindow{}`.
- **Perf gate** — synthetic DB ~1M+ transmissions with multi-hop paths,
  backfill `node_rollup`, run `GetBulkNodeRelay` for 50 pubkeys; assert well
  under 1 s. Plus a manual run against the real backup DB before the live
  flag is confirmed.
- **Maintenance test** — insert new observations, run maintenance, assert
  `node_rollup` updates and the watermark advances.

**Fixture conventions:** `observations.timestamp` is epoch **seconds**
(`1779098400` = `2026-05-18T10:00:00Z`); `transmissions.first_seen` is
RFC3339 text. Recompute buckets by `first_seen`.

## Section 5 — Rollout

- `node_rollup` schema, backfill, and the 5-min maintenance ticker are wired
  into `main.go`, all active only when `analyticsSqlBackend` is `true`.
- Sequence: deploy → flag already on → backfill runs in background (in-memory
  fallback serves `/api/nodes` meanwhile) → backfill completes
  (`[node-rollup] backfill complete`) → maintenance keeps it fresh → rollup
  serves.
- Watch the deploy logs for `[node-rollup] backfill complete` and **no
  `SQLITE_BUSY` storm**. A storm → roll back the flag and fix.

## Non-goals / follow-ups

- The remaining analytics groups (distance, hash-sizes, subpath, topology) —
  each its own rollup later.
- Sub-hour-precise `RelayCount1h` — would need a finer rollup tier; the
  hour-bucket approximation is accepted.
- A periodic full rebuild to absorb late `resolved_path` resolution — the
  in-memory index has the same drift; not corrected here.
- Changing the `/api/nodes` response shape or the frontend.
