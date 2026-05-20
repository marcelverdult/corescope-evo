# Channels Analytics Rollup — Design

**Date:** 2026-05-19
**Status:** Design approved (autonomous), pending implementation
**Scope:** Channels analytics. Group 2 of the analytics-rollup migration; follows the RF rollup pattern (`.specs/2026-05-19-rf-analytics-rollup-design.md`).

## Context

`computeAnalyticsChannels` (`cmd/server/store_channels.go`, ~190 lines) iterates the in-memory `s.byPayloadType[5]` (type-5 GRP_TXT transmissions), JSON-unmarshalling each `DecodedJSON` per request. Like RF, it only sees the `retentionHours` window of in-memory data. Migrate it to a pre-aggregated rollup so channel analytics cover full history and stay fast.

Output of `computeAnalyticsChannels` (must be preserved): `activeChannels`, `decryptable`, `channels[]` (`{hash,name,messages,senders,lastActivity,encrypted}`), `topSenders[]` (top-15 `{name,count}`), `channelTimeline[]` (`{hour,channel,count}`), `msgLengths`.

## Decisions

- **Three rollup tables.** Channels has a sender dimension (arbitrary strings) that does not fit fixed columns, so it gets its own table.
- **Per-observation rollup keyed by `observer_idx`** (so region queries filter), mirroring RF — including the `-1` sentinel for NULL `observer_idx`.
- **Message count is the headline number and must be exact for the common (no-region) case.** A per-observation rollup over-counts a message seen by N observers. So a companion `channel_rollup_tx` stores the exact distinct-transmission count per `(hour, channel_hash)`. Region-filtered counts are approximate (`SUM` over the region's observer cells) — documented, same trade-off accepted for RF's `totalTransmissions`.
- **`msgLengths` becomes a fixed-bin histogram** (`{bins,min,max}`, like RF histograms) — raw per-message lengths cannot be rolled up. This is a small frontend touch in `public/analytics.js` (the channels `msgLengths` renderer).
- Gated by the existing `packetStore.analyticsSqlBackend` flag (when on, all migrated analytics groups use rollups). Backfill + maintenance run when the flag is on. In-memory `computeAnalyticsChannels` stays as fallback (while backfill runs) + parity reference.

## Section 1 — Schema

`channel_rollup`, PK `(hour, channel_hash, observer_idx)`:
- `hour TEXT` (`'2026-05-18T10'`), `channel_hash TEXT`, `observer_idx INTEGER` (`-1` = NULL observer)
- `msg_count INTEGER` — messages this observer saw in this channel-hour
- `decrypted_count INTEGER` — of those, how many were decryptable (`Text` or `Sender` present)
- `name TEXT` — best non-placeholder channel name seen in the cell, else placeholder `ch<hash>`
- `last_activity TEXT` — max `first_seen`
- `msglen_sum INTEGER, msglen_count INTEGER, msglen_min INTEGER, msglen_max INTEGER`
- `msglen_bins BLOB` — fixed-bin histogram of message text lengths

`channel_sender_rollup`, PK `(hour, channel_hash, observer_idx, sender)`:
- `msg_count INTEGER` — messages from this sender in this channel-hour-observer cell

`channel_rollup_tx`, PK `(hour, channel_hash)`:
- `distinct_tx INTEGER` — exact distinct transmission count (for exact no-region message totals)

Fixed bins for message length: 0…512 bytes, 16-byte width → 32 bins, clamp out-of-range.

`channel_rollup_meta(key,value)` — or reuse a shared meta table — holds the channels backfill watermark (`channel_rollup_last_obs_id` — keyed off `transmissions.id` since the unit is the transmission, not the observation).

## Section 2 — Backfill + maintenance

Same recompute-touched-hours pattern as RF (`rf_rollup_maintain.go`):
- `recomputeChannelRollupHour(rw, hour)` — delete the hour's rows, scan type-5 transmissions whose `first_seen` falls in the hour **using an indexed range** (`first_seen` is RFC3339 text; the hour bucket → `[hourStr+":00:00Z", nextHourStr)` string range, served by `idx_transmissions_first_seen`), join observations for `observer_idx`, unmarshal `decoded_json`, accumulate cells, write in a short transaction. Read outside the write tx; this is the lesson from the RF SQLITE_BUSY incident.
- `runChannelRollupMaintenance(rw)` — touched-hours by `transmissions.id > watermark`, recompute each, 50 ms yield between hours, advance watermark.
- `backfillChannelRollupAsync(dbPath)` — maintenance from watermark 0, background goroutine.
- A guard mutex so backfill and the 5-min ticker do not overlap.
- Wired in `main.go` next to the RF rollup wiring, under the same flag.

## Section 3 — Read path

`computeChannelsFromRollup(db, region, window)` → the result map:
- **`channels[]`** — `GROUP BY channel_hash` over `channel_rollup` (window+region filtered): `SUM(msg_count)`, `SUM(decrypted_count)`, `MAX(last_activity)`, name = pick a non-placeholder name. `messages` for no-region = exact via `channel_rollup_tx`; region = `SUM(msg_count)` (approx). `senders` = `COUNT(DISTINCT sender)` from `channel_sender_rollup` grouped by channel_hash. `encrypted` = `SUM(decrypted_count)==0`.
- **`activeChannels`** = number of channel_hash groups; **`decryptable`** = groups with `decrypted_count>0`.
- **`topSenders`** — `SELECT sender, SUM(msg_count) FROM channel_sender_rollup WHERE … GROUP BY sender ORDER BY 2 DESC LIMIT 15`.
- **`channelTimeline`** — `GROUP BY hour, name` → `{hour,channel,count}`.
- **`msgLengths`** — sum the `msglen_bins` blobs → fixed-bin histogram `{bins,min,max}`.
- Window → hour-bucket bounds; region → `observer_idx IN (…)` via `rfRegionObserverIdxs` (reused).
- The TTL `chanCache` wrapper (`GetAnalyticsChannelsWithWindow`) is unchanged; on cache miss it branches on the flag + rollup-readiness to the rollup path or the in-memory fallback.

## Section 4 — Testing

- Unit: hour recompute, backfill/maintenance + watermark, read-path assembly.
- Parity: `computeChannelsFromRollup` vs in-memory `computeAnalyticsChannels` on a fixture — `activeChannels`, `decryptable`, per-channel `messages`/`senders`, `topSenders` exact (global + region); `msgLengths` compared as fixed-bin histogram of the same data.
- Perf: synthetic ~1M type-5 rows, assert 24h < 200 ms and full-history < 2 s.

## Section 5 — Rollout

Flag-gated; backfill in background with in-memory fallback meanwhile. Deploy with the flag already on (RF rollup is live under it). The deploy is the watched checkpoint — verify channels analytics + no `SQLITE_BUSY` storm + no `[channel-rollup]` errors before declaring done.

## Non-goals

- The other analytics groups (node-health, distance, hash, subpath, topology) — separate efforts.
- Changing channel decryption / the `channelNameMatchesHash` validation logic — reused as-is in the recompute.
