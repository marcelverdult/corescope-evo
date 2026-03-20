# Changelog

## v2.1.1 — Multi-Broker MQTT & Observer Detail (2026-03-20)

### 🆕 New Features

- **Multi-Broker MQTT** — Connect to multiple MQTT brokers simultaneously via `mqttSources` config array. Each source gets its own connection, topics, credentials, TLS settings, and optional IATA region filter. Legacy `mqtt` config still works.
- **IATA Region Filtering** — `mqttSources[].iataFilter` restricts accepted regions per source (e.g. only accept SJC/SFO/OAK packets from a shared feed).
- **Observer Detail Pages** — Click any observer row for a full detail page with status, radio info, battery/uptime/noise floor, packet type donut chart, timeline, unique nodes chart, SNR distribution, and recent packets table.
- **Observer Status Topic Parsing** — `meshcore/<region>/<id>/status` messages populate model, firmware, client_version, radio config, battery, uptime, and noise floor. 7 new columns in the observers table with auto-migration.
- **Channel Key Auto-Derivation** — Hashtag channel keys (`#channel`) are automatically derived as `SHA256("#channelname")` first 16 bytes on startup. Only non-hashtag keys (like `public`) need manual config.
- **Map Dark/Light Mode** — Map page now uses CartoDB dark/light tiles that swap automatically with the theme toggle (same as live page).
- **Shareable URLs** — Copy Link button on packet detail, standalone packet page at `#/packet/ID`, deep links to channels and observer detail pages.
- **Multi-Node Packet Filter** — "My Nodes" toggle in packets view now uses server-side `findPacketsForNode()` to find ALL packet types (messages, acks, traces), not just ADVERTs.

### 🐛 Bug Fixes

- **Observer name resolution** — MQTT packets now pass `msg.origin` (friendly name) to both packet records and observer upserts. Previously only the status handler used it.
- **Observer analytics ordering** — Fixed `recentPackets` returning oldest instead of newest (wrong slice direction). Sorted observer analytics packets explicitly.
- **Spark bars visible** — Fixed `.data-table td { max-width: 0 }` crushing spark bar cells to zero width with inline style override.
- **My Nodes filter field names** — Fixed `pubkey` → `pubKey`, `to`/`from` → `srcPubKey`/`destPubKey`/`srcHash`/`destHash`.
- **Duplicate pin buttons** — Live page destroy now removes the nav pin button; init guards against duplicates.
- **Packets page crash** — Fixed non-async `renderTableRows` using `await` (syntax error prevented entire page from loading).
- **Node search all packet types** — Search by node name now returns messages, acks, and traces — not just ADVERTs.
- **Node packet count accuracy** — `findPacketsForNode()` is now single source of truth for all node packet lookups.
- **Health endpoint recentPackets** — Changed from `slice(-10).reverse()` to `slice(0, 20)` — 20 newest DESC instead of 10 oldest.
- **RF analytics total packets** — Added `totalAllPackets` field so frontend shows both total and signal-filtered counts.
- **Duplicate `const crypto` crash** — Removed duplicate `require('crypto')` that crashed prod for ~2 minutes.
- **PII scrubbed from git history** — Removed real names and coordinates from seed data across all commits.

### 🏗️ Infrastructure

- **Docker container deployed to Azure VM** — Live at `https://analyzer.00id.net` with automatic Let's Encrypt TLS via Caddy.
- **`deploy.sh` fixed** — Config mount (`-v config.json:/app/config.json:ro`) was missing, causing every deploy to fall back to placeholder credentials. Added `|| true` to stop/rm to prevent chain failures.
- **CI/CD via GitHub Actions** — Self-hosted runner on VM, auto-deploys on push to master.

---

## v2.0.1 — Mobile Packets (2026-03-18)

See [v2.0.1 release](https://github.com/Kpa-clawbot/meshcore-analyzer/releases/tag/v2.0.1).

## v2.0.0 — Live Trace Map & VCR Playback (2026-03-17)

See [v2.0.0 release](https://github.com/Kpa-clawbot/meshcore-analyzer/releases/tag/v2.0.0).
