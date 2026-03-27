# Kobayashi — History

## Project Context

MeshCore Analyzer is a real-time LoRa mesh packet analyzer. Node.js + Express + SQLite backend, vanilla JS SPA frontend with Leaflet maps, WebSocket live feed, MQTT ingestion. Production at v2.6.0, ~18K lines, 85%+ backend test coverage.

User: User

## Learnings

- Session started 2026-03-26. Team formed: Kobayashi (Lead), Hicks (Backend), Newt (Frontend), Bishop (Tester).
- **E2E Playwright performance audit (2026-03-26):** 16 tests, single browser/context/page (good). Key bottlenecks: (1) `waitUntil: 'networkidle'` used ~20 times — catastrophic for SPA with WebSocket + map tiles, (2) ~17s of hardcoded `waitForTimeout` sleeps, (3) redundant `page.goto()` to same routes across tests, (4) CI installs Playwright browser on every run with no caching, (5) coverage collection launches a second full browser session, (6) `sleep 5` server startup instead of health-check polling. Estimated 40-50% total runtime reduction achievable.
- **Issue triage session (2026-03-27):** Triaged 4 open issues, assigned to team:
  - **#131** (Feature: Auto-update nodes tab) → Newt (⚛️). Requires WebSocket real-time updates in nodes.js, similar to existing packets feed.
  - **#130** (Bug: Disappearing nodes on live map) → Newt (⚛️). High severity, multiple Cascadia Mesh community reports. Likely status calculation or map filter bug. Nodes visible in static list but vanishing from live map.
  - **#129** (Feature: Packet comparison between observers) → Newt (⚛️). Feature request from letsmesh analyzer. Side-by-side packet filtering for two repeaters to diagnose repeater issues.
  - **#123** (Feature: Show channel hash on decrypt failure) → Hicks (🔧). Core contributor (lincomatic) request. Decoder needs to track why decrypt failed (no key vs. corruption) and expose channel hash + reason in API response.
