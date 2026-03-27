# Newt — History

## Project Context

MeshCore Analyzer is a real-time LoRa mesh packet analyzer with a vanilla JS SPA frontend. 22 frontend modules, Leaflet maps, WebSocket live feed, VCR playback, Canvas animations, theme customizer with CSS variables. No build step, no framework. ES5/6 for broad browser support.

User: User

## Learnings

- Session started 2026-03-26. Team formed: Kobayashi (Lead), Hicks (Backend), Newt (Frontend), Bishop (Tester).
- **Issue #127 fix:** Firefox clipboard API fails silently when `navigator.clipboard.writeText()` is called outside a secure context or without proper user gesture handling. Added `window.copyToClipboard()` shared helper to `roles.js` that tries Clipboard API first, falls back to hidden textarea + `document.execCommand('copy')`. Updated all 3 clipboard call sites: `nodes.js` (Copy URL — the reported bug), `packets.js` (Copy Link — had ugly `prompt()` fallback), `customize.js` (Copy to Clipboard — already worked but now uses shared helper). Cache busters bumped. All tests pass (47 frontend, 62 packet-filter).
- **Issue #125 fix:** Added dismiss/close button (✕) to the packet detail pane on desktop. Extracted `closeDetailPanel()` shared helper and `PANEL_CLOSE_HTML` constant — DRY: Escape handler and click handler both call it. Close button uses event delegation on `#pktRight`, styled with CSS variables (`--text-muted`, `--text`, `--surface-1`) matching the mobile `.mobile-sheet-close` pattern. Hidden when panel is in `.empty` state. Clicking a different row still re-opens with new data. Files changed: `public/packets.js`, `public/style.css`. Cache busters NOT bumped (another agent editing index.html).
- **Issue #122 fix:** Node tooltip (line 45) and node detail panel (line 120) in `channels.js` used `last_seen` alone for "Last seen" display. Changed both to `last_heard || last_seen` per AGENTS.md pitfall. Pattern: always prefer `last_heard || last_seen` for any time-ago display. **Server note for Hicks:** `/api/nodes/search` and `/api/nodes/:pubkey` endpoints don't return `last_heard` — only the bulk `/api/nodes` list endpoint computes it from the in-memory packet store. These endpoints need the same `last_heard` enrichment for the frontend fix to fully take effect. Also, `/api/analytics/channels` has a separate bug: `lastActivity` is overwritten unconditionally (no `>=` check) so it shows the oldest packet's timestamp, not the newest.
