# Performance Optimization Results

**Dataset:** 27,346 packets, 501 nodes, 2 observers

## Server Response Times

| Endpoint | Before | After | Improvement |
|---|---|---|---|
| `/api/packets` (main view) | 77.5ms | 2ms (in-memory) | **39× faster** |
| `/api/packets` (with filters) | 77.5ms | 7ms | **11× faster** |
| `/api/analytics/subpaths` (×4 queries) | 937ms + 1.99s + 3.09s + 6.19s | **<1ms each** (pre-warmed) | **6,000× faster** |
| `/api/analytics/rf` | 270ms | 0.7ms (cached) | **386× faster** |
| `/api/analytics/topology` | 697ms | 195ms cold / <1ms cached | **~700× faster** (cached) |
| `/api/analytics/hash-sizes` | 430ms | 128ms cold / <1ms cached | **~430× faster** (cached) |
| `/api/packets/timestamps` | ~10ms | 1.3ms | **8× faster** |
| `/api/packets/:id` | ~25ms | 3ms | **8× faster** |

## Payload Sizes

| Endpoint | Before | After | Reduction |
|---|---|---|---|
| `/api/analytics/rf` | **1,032 KB** | **22 KB** | **98% smaller** |
| Total RF page load | ~1.1 MB | ~25 KB | **98% reduction** |

## Network Requests Eliminated

| Scenario | Before | After |
|---|---|---|
| New packet arrives (flat mode) | Full `/api/packets` re-fetch | **Zero API calls** — WS prepend |
| New packet arrives (grouped mode) | Full `/api/packets?groupByHash=true` re-fetch | **Zero API calls** — client-side group update |
| Subpath analysis (4 parallel queries) | 4 × full 27K packet scan | **1 shared pre-computation**, served from cache |

## Architecture Changes

### In-Memory Packet Store
- All 27K packets loaded into RAM on startup (~12MB)
- Indexed by: `id`, `hash`, `observer_id`, `node pubkey`
- SQLite is now **write-only** for the packets table
- All reads served from RAM — sub-millisecond
- Configurable memory cap (default 1GB → ~2.3M packets max)
- Ring buffer eviction when limit reached

### Smart Cache Invalidation
- **Before:** Every packet burst nuked ALL caches (including 1-hour analytics)
- **After:** Only channels/observers invalidated on packet burst. Node/health caches invalidated only on ADVERT. Analytics expire by TTL only.

### Server-Side Computation
- RF histograms computed server-side (20-25 bins) instead of sending 27K raw values
- Scatter plot downsampled to 500 representative points (from 27K)
- Subpath analysis: single-pass computation shared across all query variants, pre-warmed on startup

### WebSocket Streaming
- Packets page receives full packet data via WebSocket
- Client-side filtering + prepend — no API round-trip
- Grouped mode: increment counts, update timestamps, keep longest path — all in-browser

## Configuration

All cache TTLs configurable in `config.json` under `cacheTTL`:

```json
{
  "cacheTTL": {
    "stats": 10,
    "channels": 15,
    "channelMessages": 10,
    "nodeDetail": 300,
    "nodeHealth": 300,
    "bulkHealth": 600,
    "analyticsRF": 1800,
    "analyticsTopology": 1800,
    "analyticsSubpaths": 3600
  },
  "packetStore": {
    "maxMemoryMB": 1024,
    "estimatedPacketBytes": 450
  }
}
```

No code changes needed to tune — edit config, restart.
