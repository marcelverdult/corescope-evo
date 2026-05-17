# Page-state Standardization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace ad-hoc loading/empty/error UI across all 15 page modules with one shared, accessible, mobile-friendly `PageState` component.

**Architecture:** A new classic-script module `public/page-state.js` exposes `window.PageState` with string-builder helpers (loading/empty/skeleton/errorText/row) plus one container renderer (`error`, which needs a bound retry handler). Styling lives in `public/page-state.css`. Both load before `app.js` and the page modules. Each page module is converted to call `PageState` instead of hand-rolled markup.

**Tech Stack:** Vanilla JS (classic scripts, no build step), CSS, vm-sandbox node tests (`test-*.js`), browser testing on a local preview server.

**Spec:** `docs/superpowers/specs/2026-05-17-page-states-design.md`

---

## Conventions for every task

- Work ONLY inside the worktree `/Users/verdi/Projects/corescope/corescope-evo/.claude/worktrees/sleepy-meninsky-1361e6`. Never edit the main repo path outside `.claude/worktrees/`.
- After editing any `.js` file: run `node --check <file>` — must print nothing (exit 0).
- `node --check` does NOT catch classic-script scoping bugs. Browser-test every converted page (Task 14 covers the harness; per-page tasks list what to trigger).
- Commit after each task with the message shown.

---

## Task 1: Build the PageState module

**Files:**
- Create: `public/page-state.js`
- Create: `public/page-state.css`
- Modify: `public/index.html` (add two `<link>`/`<script>` tags)

- [ ] **Step 1: Create `public/page-state.js`**

```js
/* PageState — shared loading / empty / error / skeleton UI.
   Classic script: window.PageState is assigned explicitly inside the IIFE so
   it does not depend on a top-level function declaration leaking to window. */
(function () {
  'use strict';

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  // Loading: spinner + message. Returns an HTML string.
  function loading(message) {
    return '<div class="ps" role="status" aria-live="polite">' +
      '<div class="ps-spinner" aria-hidden="true"></div>' +
      '<div class="ps-title">' + esc(message || 'Loading…') + '</div>' +
      '</div>';
  }

  // Empty: icon + title + optional hint. Returns an HTML string.
  function empty(opts) {
    opts = opts || {};
    var icon = opts.icon ? '<div class="ps-icon" aria-hidden="true">' + esc(opts.icon) + '</div>' : '';
    var hint = opts.hint ? '<div class="ps-hint">' + esc(opts.hint) + '</div>' : '';
    return '<div class="ps" role="status" aria-live="polite">' +
      icon +
      '<div class="ps-title">' + esc(opts.title || 'Nothing here yet') + '</div>' +
      hint +
      '</div>';
  }

  // Skeleton shimmer. opts.table=true emits <tr>/<td> rows for <tbody>;
  // otherwise emits <div> rows. opts.rows (default 5), opts.cols (default 1).
  function skeleton(opts) {
    opts = opts || {};
    var rows = Math.max(1, opts.rows || 5);
    var cols = Math.max(1, opts.cols || 1);
    var i, c, out;
    if (opts.table) {
      out = '';
      for (i = 0; i < rows; i++) {
        out += '<tr class="ps-skeleton-row" aria-hidden="true">';
        for (c = 0; c < cols; c++) out += '<td><div class="ps-skeleton-cell"></div></td>';
        out += '</tr>';
      }
      return out;
    }
    out = '<div class="ps-skeleton" role="status" aria-live="polite" aria-label="Loading">';
    for (i = 0; i < rows; i++) {
      out += '<div class="ps-skeleton-row">';
      for (c = 0; c < cols; c++) out += '<div class="ps-skeleton-cell"></div>';
      out += '</div>';
    }
    return out + '</div>';
  }

  // Static error string (no retry) — for table cells and embedded fragments.
  function errorText(message) {
    return '<div class="ps ps-error" role="alert">' +
      '<div class="ps-icon" aria-hidden="true">⚠</div>' +
      '<div class="ps-title">' + esc(message || 'Something went wrong') + '</div>' +
      '</div>';
  }

  // Wrap an HTML string in a single full-width table row.
  function row(colspan, innerHTML) {
    return '<tr><td colspan="' + (parseInt(colspan, 10) || 1) + '">' +
      (innerHTML || '') + '</td></tr>';
  }

  // Render an error state into a container element. If onRetry is a function,
  // a Retry button is rendered and wired to it.
  function error(container, err, onRetry) {
    if (!container) return;
    var message = (err && err.message) ? err.message
      : (typeof err === 'string' ? err : 'Something went wrong');
    var hasRetry = typeof onRetry === 'function';
    container.innerHTML = '<div class="ps ps-error" role="alert">' +
      '<div class="ps-icon" aria-hidden="true">⚠</div>' +
      '<div class="ps-title">' + esc(message) + '</div>' +
      (hasRetry ? '<button type="button" class="ps-retry">Retry</button>' : '') +
      '</div>';
    if (hasRetry) {
      var btn = container.querySelector('.ps-retry');
      if (btn) btn.addEventListener('click', function () { onRetry(); });
    }
  }

  window.PageState = {
    loading: loading,
    empty: empty,
    skeleton: skeleton,
    errorText: errorText,
    error: error,
    row: row
  };
})();
```

- [ ] **Step 2: Verify the module parses**

Run: `node --check public/page-state.js`
Expected: no output, exit 0.

- [ ] **Step 3: Create `public/page-state.css`**

```css
/* PageState — shared loading / empty / error / skeleton UI. */

.ps {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 12px;
  padding: clamp(24px, 6vw, 60px) 20px;
  min-height: 160px;
  text-align: center;
  color: var(--text-muted);
}

.ps-icon { font-size: 40px; line-height: 1; opacity: 0.55; }
.ps-title { font-size: 15px; font-weight: 500; color: var(--text); max-width: 42ch; }
.ps-hint { font-size: 13px; color: var(--text-muted); max-width: 48ch; }

.ps-error .ps-icon { color: var(--status-red); opacity: 0.9; }

.ps-spinner {
  width: 28px; height: 28px;
  border: 3px solid var(--border);
  border-top-color: var(--accent);
  border-radius: 50%;
  animation: ps-spin 0.8s linear infinite;
}
@keyframes ps-spin { to { transform: rotate(360deg); } }

.ps-retry {
  margin-top: 4px;
  min-height: 44px;
  padding: 8px 20px;
  font-size: 14px; font-weight: 600;
  color: #fff; background: var(--accent);
  border: none; border-radius: 6px;
  cursor: pointer;
}
.ps-retry:hover { background: var(--accent-hover); }
.ps-retry:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }

/* Skeleton shimmer */
.ps-skeleton { padding: 12px 0; display: flex; flex-direction: column; gap: 10px; }
.ps-skeleton-row { display: flex; gap: 12px; }
.ps-skeleton-cell {
  flex: 1;
  height: 16px;
  border-radius: 4px;
  background: linear-gradient(90deg,
    var(--surface-2) 25%, var(--row-hover) 37%, var(--surface-2) 63%);
  background-size: 400% 100%;
  animation: ps-shimmer 1.4s ease infinite;
}
@keyframes ps-shimmer { from { background-position: 100% 0; } to { background-position: 0 0; } }

/* Skeleton inside a table: .ps-skeleton-row is a <tr>; cells sit in <td>. */
tr.ps-skeleton-row { display: table-row; }
tr.ps-skeleton-row td { padding: 8px 12px; }

@media (prefers-reduced-motion: reduce) {
  .ps-spinner { animation: none; }
  .ps-skeleton-cell { animation: none; }
}
```

- [ ] **Step 4: Wire both files into `public/index.html`**

In the `<head>`, immediately after the `vendor/MarkerCluster.Default.css` link and BEFORE the `__THEME_STYLE__` placeholder, add the stylesheet:

```html
  <link rel="stylesheet" href="page-state.css?v=__BUST__">
```

`page-state.js` must load before `app.js` and every page module. Find the first `<script defer src="...">` tag for an app module (search `index.html` for `app.js`) and add this line immediately before it:

```html
  <script defer src="page-state.js?v=__BUST__"></script>
```

`defer` scripts execute in document order, so placing it before `app.js` guarantees `window.PageState` exists when page modules run.

- [ ] **Step 5: Commit**

```bash
git add public/page-state.js public/page-state.css public/index.html
git commit -m "feat(ui): add shared PageState component for loading/empty/error states"
```

---

## Task 2: Unit-test the PageState string builders

**Files:**
- Create: `test-page-state.js` (repo root — matches the existing `test-*.js` convention)
- Modify: `test-all.sh` (add the new test to the curated list)

- [ ] **Step 1: Write the test file `test-page-state.js`**

```js
/* Unit tests for public/page-state.js string builders (tested via VM sandbox). */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

// Load page-state.js into a sandbox with a minimal window.
const sandbox = { window: {} };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync('public/page-state.js', 'utf8'), sandbox);
const PS = sandbox.window.PageState;

test('PageState is exported', () => {
  assert.ok(PS, 'window.PageState should be defined');
  ['loading', 'empty', 'skeleton', 'errorText', 'error', 'row'].forEach((k) => {
    assert.strictEqual(typeof PS[k], 'function', k + ' should be a function');
  });
});

test('loading() includes the message and a status role', () => {
  const html = PS.loading('Loading nodes');
  assert.ok(html.includes('Loading nodes'));
  assert.ok(html.includes('role="status"'));
  assert.ok(html.includes('ps-spinner'));
});

test('loading() defaults the message', () => {
  assert.ok(PS.loading().includes('Loading'));
});

test('empty() renders title, icon, and hint', () => {
  const html = PS.empty({ icon: '📦', title: 'No packets', hint: 'Try later' });
  assert.ok(html.includes('No packets'));
  assert.ok(html.includes('Try later'));
  assert.ok(html.includes('ps-icon'));
});

test('errorText() uses role=alert and shows the message', () => {
  const html = PS.errorText('Boom');
  assert.ok(html.includes('role="alert"'));
  assert.ok(html.includes('Boom'));
});

test('builders escape HTML to prevent injection', () => {
  const html = PS.empty({ title: '<img src=x onerror=alert(1)>' });
  assert.ok(!html.includes('<img'), 'raw tag must not appear');
  assert.ok(html.includes('&lt;img'));
});

test('skeleton() non-table emits div rows', () => {
  const html = PS.skeleton({ rows: 3, cols: 2 });
  assert.strictEqual((html.match(/ps-skeleton-row/g) || []).length, 3);
  assert.strictEqual((html.match(/ps-skeleton-cell/g) || []).length, 6);
  assert.ok(!html.includes('<tr'));
});

test('skeleton({table:true}) emits tr/td rows', () => {
  const html = PS.skeleton({ rows: 2, cols: 3, table: true });
  assert.strictEqual((html.match(/<tr/g) || []).length, 2);
  assert.strictEqual((html.match(/<td>/g) || []).length, 6);
});

test('row() wraps content in a full-width table row', () => {
  assert.strictEqual(PS.row(4, 'X'), '<tr><td colspan="4">X</td></tr>');
});

test('error() renders into a container and shows the message', () => {
  let html = '';
  const container = {
    set innerHTML(v) { html = v; },
    get innerHTML() { return html; },
    querySelector() { return null; }
  };
  PS.error(container, new Error('fetch failed'));
  assert.ok(html.includes('fetch failed'));
  assert.ok(html.includes('role="alert"'));
  assert.ok(!html.includes('ps-retry'), 'no retry button without onRetry');
});

test('error() renders a retry button and wires the handler', () => {
  let html = '', clicked = 0, clickHandler = null;
  const btn = { addEventListener(ev, fn) { if (ev === 'click') clickHandler = fn; } };
  const container = {
    set innerHTML(v) { html = v; },
    get innerHTML() { return html; },
    querySelector(sel) { return sel === '.ps-retry' ? btn : null; }
  };
  PS.error(container, 'down', () => { clicked++; });
  assert.ok(html.includes('ps-retry'));
  assert.ok(typeof clickHandler === 'function', 'click handler should be wired');
  clickHandler();
  assert.strictEqual(clicked, 1);
});

console.log(`\n  ${passed} passed, ${failed} failed`);
process.exit(failed > 0 ? 1 : 0);
```

- [ ] **Step 2: Run the test, expect it to pass**

Run: `node test-page-state.js`
Expected: all tests print `✅`, final line `12 passed, 0 failed`, exit 0.

- [ ] **Step 3: Add the test to `test-all.sh`**

In `test-all.sh`, find the line `node test-frontend-helpers.js` and add a new line immediately after it:

```sh
node test-page-state.js
```

- [ ] **Step 4: Run the curated suite**

Run: `sh test-all.sh`
Expected: the suite runs `test-page-state.js` among others; no failures.

- [ ] **Step 5: Commit**

```bash
git add test-page-state.js test-all.sh
git commit -m "test(ui): cover PageState string builders and error renderer"
```

---

## Conversion tasks (Tasks 3–13)

Each conversion task targets one or a few page modules. For every call site listed, open the file at that line, replace the hand-rolled markup with the `PageState` call shown, and keep the surrounding assignment (`el.innerHTML = …`, `return …`, `tbody.innerHTML = …`) intact.

**Substitution rules:**
- Plain loading text → `PageState.loading('<message>')`.
- Loading inside a `<td colspan=N>` table cell → `PageState.row(N, PageState.loading('<message>'))`, OR for a table body that should show skeleton rows → `PageState.skeleton({ rows: 6, cols: N, table: true })`.
- Empty text → `PageState.empty({ icon: '<emoji>', title: '<text>', hint: '<optional>' })`. Pick an icon that fits the page (📦 packets, 🛰 nodes/observers, 📡 channels, 📊 analytics, 🔇 audio); omit `icon` if none fits.
- Error text, non-interactive (table cell, inline fragment) → `PageState.errorText(e.message)` (wrap in `PageState.row(N, …)` if it is a table cell).
- Error where the page owns a container element and a re-fetch function exists → `PageState.error(containerEl, e, retryFn)` where `retryFn` re-runs the page's load function.
- A `console.error`-only failure path → ADD a visible `PageState.error(...)` into the page's main container, keeping the `console.error` too.
- Replace any `<span class="spinner">` markup with `PageState.loading('<message>')` (the `.spinner` class is dead — undefined in CSS).

**Per task:** after edits run `node --check` on each changed file, then commit with the message shown. Browser testing for all converted pages happens in Task 14.

---

### Task 3: Convert `perf.js` and `observers.js`

**Files:**
- Modify: `public/perf.js`
- Modify: `public/observers.js`

- [ ] **Step 1: `perf.js:8`** — replace `<div id="perfContent">Loading...</div>` so the inner text becomes `PageState.loading('Loading performance metrics…')`. Keep the `id="perfContent"` wrapper; set its initial content via the helper (e.g. `'<div id="perfContent">' + PageState.loading('Loading performance metrics…') + '</div>'`).
- [ ] **Step 2: `perf.js:277`** — replace `<p style="color:red">Error: ...</p>` with `PageState.error(perfContentEl, err, loadPerf)` where `perfContentEl` is the `#perfContent` element and `loadPerf` is the function that fetches/renders perf data (use the actual function name in that file).
- [ ] **Step 3: `observers.js:98`** — replace the loading `<div>` with `PageState.loading('Loading observers…')`.
- [ ] **Step 4: `observers.js:206`** — replace the error `<div>` with `PageState.error(<observers container el>, e, <observers load fn>)`.
- [ ] **Step 5: `node --check public/perf.js public/observers.js`** — expect no output.
- [ ] **Step 6: Commit**

```bash
git add public/perf.js public/observers.js
git commit -m "refactor(ui): convert perf and observers pages to PageState"
```

---

### Task 4: Convert `observer-detail.js` and `compare.js`

**Files:**
- Modify: `public/observer-detail.js`
- Modify: `public/compare.js`

- [ ] **Step 1: `observer-detail.js:56`** — loading `<div>` → `PageState.loading('Loading observer…')`.
- [ ] **Step 2: `observer-detail.js:106`** — error `<div>` → `PageState.error(<detail container el>, e, <reload fn>)`.
- [ ] **Step 3: `observer-detail.js:223`** — the nested `Loading…` inside `#obsRecentPackets` → set `#obsRecentPackets` content to `PageState.loading('Loading recent packets…')`.
- [ ] **Step 4: `compare.js:112`** — loading `<div>` → `PageState.loading('Loading observers…')`.
- [ ] **Step 5: `compare.js:184`** — error `<div>` → `PageState.error(<compare container el>, e, <load observers fn>)`.
- [ ] **Step 6: `compare.js:291`** — error `<div>` → `PageState.error(<results container el>, e, <compare run fn>)`.
- [ ] **Step 7: `node --check public/observer-detail.js public/compare.js`** — expect no output.
- [ ] **Step 8: Commit**

```bash
git add public/observer-detail.js public/compare.js
git commit -m "refactor(ui): convert observer-detail and compare pages to PageState"
```

---

### Task 5: Convert `traces.js` and `home.js`

**Files:**
- Modify: `public/traces.js`
- Modify: `public/home.js`

- [ ] **Step 1: `traces.js:68`** — empty `<div class="trace-empty">` → `PageState.empty({ icon: '📡', title: 'No observations found', hint: 'Adjust your filters and try again' })`.
- [ ] **Step 2: `traces.js:99`** — error `<div class="trace-empty">` → `PageState.errorText(e.message)` (or `PageState.error(<traces container>, e, <traces load fn>)` if a container + reload function exist).
- [ ] **Step 3: `home.js:91`** — `Loading your nodes…` → `PageState.loading('Loading your nodes…')`.
- [ ] **Step 4: `home.js:410`** — `<p ...>Loading…</p>` → `PageState.loading('Loading…')`.
- [ ] **Step 5: `home.js:506`** — `<p ...>Failed to load…</p>` → `PageState.error(<the home container el for this section>, e, <home reload fn>)`. If no reload function exists for that section, use `PageState.errorText('Failed to load — reload the page to retry.')`.
- [ ] **Step 6: `node --check public/traces.js public/home.js`** — expect no output.
- [ ] **Step 7: Commit**

```bash
git add public/traces.js public/home.js
git commit -m "refactor(ui): convert traces and home pages to PageState"
```

---

### Task 6: Convert `audio-lab.js`

**Files:**
- Modify: `public/audio-lab.js`

- [ ] **Step 1: `audio-lab.js:190`** — `<div class="alab-empty">No raw hex data…</div>` → `PageState.empty({ title: 'No raw hex data for this packet' })`.
- [ ] **Step 2: `audio-lab.js:436`** — loading `<div>` → `PageState.loading('Loading packets…')`.
- [ ] **Step 3: `audio-lab.js:463`** — `<div class="alab-empty">← Select a packet…</div>` → `PageState.empty({ icon: '🔊', title: 'Select a packet from the sidebar' })`.
- [ ] **Step 4: `audio-lab.js:550`** — error `<div>` → `PageState.error(<packets container el>, e, <load packets fn>)`.
- [ ] **Step 5:** Remove the now-unused inline `.alab-empty` CSS rule at `audio-lab.js:92` if nothing else references `alab-empty` (grep `public/` for `alab-empty` first; if other references remain, leave it).
- [ ] **Step 6: `node --check public/audio-lab.js`** — expect no output.
- [ ] **Step 7: Commit**

```bash
git add public/audio-lab.js
git commit -m "refactor(ui): convert audio-lab page to PageState"
```

---

### Task 7: Convert `node-analytics.js` and `mc-keygen.js`

**Files:**
- Modify: `public/node-analytics.js`
- Modify: `public/mc-keygen.js`

- [ ] **Step 1: `node-analytics.js:45`** — loading `<div>` → `PageState.loading('Loading analytics…')`.
- [ ] **Step 2: `node-analytics.js:137` / `:342` / `:345`** — the `#batteryEmpty` element is a static node toggled via `display`. Set its inner content once to `PageState.empty({ title: 'No battery telemetry for this node' })` and keep the existing show/hide `display` logic at `:342`/`:345`.
- [ ] **Step 3: `node-analytics.js:316`** — `empty.textContent = 'Battery data unavailable: ' + e.message` → `empty.innerHTML = PageState.errorText('Battery data unavailable: ' + e.message)`.
- [ ] **Step 4: `mc-keygen.js`** — `mc-keygen.js` already has local `showError`/`hideError` (`:838`/`:839`) that target a dedicated `errorEl`. Leave the keygen error flow as-is — it is a form-validation pattern, not a page-load state. No change to `mc-keygen.js`.
- [ ] **Step 5: `node --check public/node-analytics.js`** — expect no output.
- [ ] **Step 6: Commit**

```bash
git add public/node-analytics.js
git commit -m "refactor(ui): convert node-analytics page to PageState"
```

---

### Task 8: Convert `packets.js`

**Files:**
- Modify: `public/packets.js`

- [ ] **Step 1: `packets.js:1233`** — table error row (`<td colspan>...Failed to load packets...`) → `PageState.row(<colspan count>, PageState.errorText('Failed to load packets'))`. Preserve the existing colspan number.
- [ ] **Step 2: `packets.js:1245`** — the "table skeleton" comment marks where a loading state belongs but none is rendered. Add a real skeleton: set the table body to `PageState.skeleton({ rows: 8, cols: <column count>, table: true })` while packets load.
- [ ] **Step 3: `packets.js:2586`, `:2604`, `:2611`** — each `<div class="text-center text-muted" style="padding:40px">Loading…</div>` → `PageState.loading('Loading…')`.
- [ ] **Step 4: `packets.js:3419`** — `Loading packet…` → `PageState.loading('Loading packet…')`.
- [ ] **Step 5: `packets.js:2630`** — `<div class="text-muted">Error: …</div>` → `PageState.error(<container el>, e, <reload fn>)`.
- [ ] **Step 6: `packets.js:3437`** — `<h2>Error</h2><p>…</p>` (packet-detail error) → `PageState.error(<packet detail container el>, e, <packet detail load fn>)`.
- [ ] **Step 7: `node --check public/packets.js`** — expect no output.
- [ ] **Step 8: Commit**

```bash
git add public/packets.js
git commit -m "refactor(ui): convert packets page to PageState with skeleton loader"
```

---

### Task 9: Convert `nodes.js`

**Files:**
- Modify: `public/nodes.js`

- [ ] **Step 1: `nodes.js:1077`** — table error row → `PageState.row(6, PageState.errorText('Failed to load nodes'))` (colspan is 6 per the current markup).
- [ ] **Step 2: `nodes.js:364`, `:1315`, `:1343`** — each `<div class="text-center text-muted" style="padding:40px">Loading…</div>` → `PageState.loading('Loading…')`. For `:364` (main nodes table body) prefer a skeleton: `PageState.skeleton({ rows: 8, cols: 6, table: true })`.
- [ ] **Step 3: `nodes.js:269`, `:617`, `:1449`** — `<span class="spinner"></span> Loading neighbors…` → `PageState.loading('Loading neighbors…')`.
- [ ] **Step 4: `nodes.js:623`** — `<span class="spinner"></span> Loading debug data…` → `PageState.loading('Loading debug data…')`.
- [ ] **Step 5: `nodes.js:629`, `:1454`** — `<span class="spinner"></span> Loading paths…` → `PageState.loading('Loading paths…')`.
- [ ] **Step 6: `nodes.js:361`** — `<span class="node-full-title">Loading…</span>` is a title placeholder inside a heading, not a page state. Leave it unchanged.
- [ ] **Step 7: `nodes.js:1334`, `:1349`** — `<div class="text-muted">Error: …</div>` → `PageState.errorText(e.message)`.
- [ ] **Step 8:** The right-panel empty state (`nodes.js:405`, `:1188`, `:1215` — `panel-right empty` class + `classList.add('empty')`) is a distinct panel-collapse mechanism with its own CSS (`style.css:736`). Leave it unchanged — it is not a page-load state.
- [ ] **Step 9: `node --check public/nodes.js`** — expect no output.
- [ ] **Step 10: Commit**

```bash
git add public/nodes.js
git commit -m "refactor(ui): convert nodes page to PageState, remove dead spinner markup"
```

---

### Task 10: Convert `analytics.js`

**Files:**
- Modify: `public/analytics.js`

- [ ] **Step 1: `analytics.js:131`, `:1877`, `:1985`, `:2718`, `:3673`, `:3783`** — each loading `<div>` (`Loading analytics…`, `Loading…`, `Loading node analytics…`, `Loading prefix data…`, `Loading clock health…`, `Loading roles…`) → `PageState.loading('<same message>')`.
- [ ] **Step 2: `analytics.js:1362`, `:1383`, `:3072`, `:3241`** — loading states using `<span class="spinner">` or plain `Loading…` text → `PageState.loading('<message matching the section>')`.
- [ ] **Step 3: `analytics.js:1870`, `:1882`** — error `<div>`s → `PageState.errorText(e.message)` (these sit inside analytics sub-panels; if a panel container + reload function exist, prefer `PageState.error(panelEl, e, reloadFn)`).
- [ ] **Step 4: `analytics.js:1566`, `:1641`, `:1688`, `:3074`, `:3300`** — these are data-cell tokens (`hash-cell-empty`) and an `rf-panel-empty` panel class, NOT page-load states. Leave them unchanged.
- [ ] **Step 5: `node --check public/analytics.js`** — expect no output.
- [ ] **Step 6: Commit**

```bash
git add public/analytics.js
git commit -m "refactor(ui): convert analytics page loading/error states to PageState"
```

---

### Task 11: Convert `live.js`

**Files:**
- Modify: `public/live.js`

- [ ] **Step 1: `live.js:1931`** — loading `<div>` → `PageState.loading('Loading…')`.
- [ ] **Step 2: `live.js:1991`** — `<span class="spinner">…Loading paths…` → `PageState.loading('Loading paths…')`.
- [ ] **Step 3: `live.js:2034`** — error `<div>` → `PageState.error(<live container el>, e, <live reload fn>)`.
- [ ] **Step 4: `live.js:2061`** — `console.error('Failed to load nodes:', e)` is console-only. Keep the `console.error`, and ALSO render a visible error: `PageState.error(<nodes container el>, e, <load nodes fn>)`.
- [ ] **Step 5: `live.js:996` and `:2173`** — the `live-feed-empty` element is the live-feed placeholder ("Waiting for packets…"). Replace its content with `PageState.empty({ icon: '📡', title: 'Waiting for packets', hint: 'Live packets appear here as they arrive' })`. Keep the `live-feed-empty` wrapper class if other code toggles it; otherwise the `.ps` styling applies.
- [ ] **Step 6: `node --check public/live.js`** — expect no output.
- [ ] **Step 7: Commit**

```bash
git add public/live.js
git commit -m "refactor(ui): convert live page to PageState, fix console-only error"
```

---

### Task 12: Convert `map.js`

**Files:**
- Modify: `public/map.js`

- [ ] **Step 1: `map.js:1136`** — `<p style="font-size:12px;">Loading...</p>` → `PageState.loading('Loading map data…')`.
- [ ] **Step 2: `map.js:629`** — `console.error('Map load error:', e)` is console-only. Keep the `console.error`, and ALSO render a visible error into the map page container: `PageState.error(<map container el>, e, <map load fn>)`.
- [ ] **Step 3: `map.js:121`** — `<div id="mapPiError" class="path-inspector-error"></div>` is a path-inspector error slot. When that slot is populated on failure, set its content with `PageState.errorText(<message>)`. If it is never populated in code, leave it.
- [ ] **Step 4: `node --check public/map.js`** — expect no output.
- [ ] **Step 5: Commit**

```bash
git add public/map.js
git commit -m "refactor(ui): convert map page to PageState, fix console-only error"
```

---

### Task 13: Convert `channels.js`

**Files:**
- Modify: `public/channels.js`

`channels.js` has the most state sites and a local `showAddStatus` helper for the add-channel modal. Convert only the page-load states; leave `showAddStatus` (`:441`, `:461`, `:492`, `:499`) unchanged — it is a modal form-feedback pattern.

- [ ] **Step 1: Loading states** — `channels.js:740` (`Loading channels…`), `:1861` (`Decrypting messages…`), `:1944` / `:1962` (`Loading messages…`): replace each `<div class="ch-loading">…</div>` with `PageState.loading('<same message>')`.
- [ ] **Step 2: Empty — channel picker prompts** — `channels.js:145`, `:831`, `:1181`, `:1282` (`Choose a channel from sidebar…`): replace each `<div class="ch-empty">…</div>` with `PageState.empty({ icon: '📡', title: 'Choose a channel', hint: 'Pick a channel from the sidebar to view its messages' })`.
- [ ] **Step 3: Empty — no-data states** — `channels.js:1763` (`No channels found`), `:1885` (`No encrypted messages found`), `:2023` (`No messages in this channel yet`), `:1295` (`Key removed — add key…`), `:1924` (`Encrypted and no key configured`), `:1971`/`:2001` (`Channel not available in region`): replace each with `PageState.empty({ title: '<same text>' })` (add an `icon`/`hint` where it reads naturally).
- [ ] **Step 4: Empty — section sub-headers** — `channels.js:1787`, `:1797` use `ch-section-empty` (a compact inline note under a section header, not a full page state). Leave these unchanged.
- [ ] **Step 5: Wrong-key state** — `channels.js:1875` (`ch-empty ch-wrong-key`) → `PageState.empty({ icon: '🔑', title: 'Key does not match', hint: 'The configured key cannot decrypt this channel' })`. Preserve any `ch-wrong-key` class only if other code/CSS depends on it (grep first).
- [ ] **Step 6: Error states** — `channels.js:280` (`Failed to load`), `:1656` (`Failed to load channels`), `:1878` (`{result.error}`), `:1978` (`Failed to load messages: …`): replace each with `PageState.errorText('<message>')`.
- [ ] **Step 7: Modal error slot** — `channels.js:778` (`chPskError` modal error div) is part of the add-channel modal. Leave it unchanged.
- [ ] **Step 8: `node --check public/channels.js`** — expect no output.
- [ ] **Step 9: Commit**

```bash
git add public/channels.js
git commit -m "refactor(ui): convert channels page-load states to PageState"
```

---

## Task 14: Browser-test every converted page

**Files:** none (verification only).

Run the local server and exercise each page. The server needs an existing SQLite DB (it opens read-only).

- [ ] **Step 1: Build and prepare**

```bash
cp test-fixtures/e2e-fixture.db /tmp/cs-test.db
( cd cmd/server && go build -o /tmp/cs-server . )
```

- [ ] **Step 2: Start the server**

Create `.claude/launch.json` with a `server` config running `/tmp/cs-server -port 8799 -db /tmp/cs-test.db -public <absolute worktree>/public`, then start it via the preview tool (or run `/tmp/cs-server` directly in the background). Confirm `http://localhost:8799/` returns HTTP 200.

- [ ] **Step 3: For EACH route below, verify loading, empty, and error states**

Routes: `#/home`, `#/packets`, `#/map`, `#/live`, `#/nodes`, `#/channels`, `#/observers`, `#/analytics`, `#/perf`, `#/audio-lab`, `#/traces`, `#/compare`, plus a node-detail (`#/nodes/<pubkey>`), an observer-detail, and a node analytics view.

For each:
- **Loading:** throttle the network (or reload) and confirm the `.ps` spinner or skeleton appears — not a bare `Loading...` string, not an invisible `.spinner`.
- **Empty:** filter/search to a no-results state and confirm a `.ps` empty state with icon + title renders.
- **Error:** block the page's API endpoint (DevTools request blocking, or stop the server mid-load) and confirm a `.ps-error` state with a Retry button renders. Click Retry and confirm the fetch re-runs.
- **Console:** confirm no JS errors in the console for that page.

- [ ] **Step 4: Verify `prefers-reduced-motion`**

In DevTools rendering settings, emulate `prefers-reduced-motion: reduce`. Confirm the spinner and skeleton shimmer stop animating.

- [ ] **Step 5: Mobile check**

Resize to a 375px-wide viewport. Confirm `.ps` states stay centered, padding looks right, and the Retry button is comfortably tappable.

- [ ] **Step 6: Record results**

If any page fails, fix it (re-open the relevant conversion task), re-`node --check`, and re-test before continuing. Do not proceed with known failures.

---

## Task 15: Remove dead CSS and stale classes

**Files:**
- Modify: `public/style.css`
- Possibly modify: other CSS files if dead rules are found

- [ ] **Step 1:** Grep the codebase for each old class to confirm it is now unused:

```bash
for c in empty-state ch-loading trace-empty alab-empty; do
  echo "== $c =="; grep -rln "$c" public/ ;
done
```

- [ ] **Step 2:** For any class with NO remaining references in `public/*.js`, remove its CSS rule:
  - `.empty-state`, `.empty-state-icon`, `.empty-state-text`, `.empty-state-hint` (`style.css` ~1638–1641) — remove if `live.js` no longer references `empty-state`.
  - `.ch-empty, .ch-loading` (`style.css:1315`) — remove `ch-loading` from the selector if unused; keep `ch-empty` only if `ch-section-empty`-adjacent code still needs it (it does not after Task 13 except section headers — verify).
  - `.trace-empty` (`style.css:1496`) — remove if `traces.js` no longer references it.
  - There is no `.spinner` CSS rule to remove (it never existed) — just confirm no `class="spinner"` markup remains: `grep -rn 'class="spinner"' public/`.
- [ ] **Step 3:** Keep any class still referenced (e.g. `ch-section-empty`, `panel-right empty`, `hash-cell-empty`, `rf-panel-empty`) — those were intentionally left in place.
- [ ] **Step 4: Browser smoke-test** — reload the app, click through 3–4 pages, confirm nothing visually broke from the CSS removals.
- [ ] **Step 5: Commit**

```bash
git add public/style.css
git commit -m "chore(ui): remove dead loading/empty CSS superseded by PageState"
```

---

## Task 16: Deploy and verify the live site

**Files:** none (deployment only).

- [ ] **Step 1:** Confirm the worktree branch is fully committed (`git status` clean) and run the curated suite once more: `sh test-all.sh` — expect no failures.
- [ ] **Step 2:** Fast-forward merge the worktree branch into `main` and push:

```bash
git -C /Users/verdi/Projects/corescope/corescope-evo merge --ff-only <worktree-branch>
git -C /Users/verdi/Projects/corescope/corescope-evo push origin main
```

- [ ] **Step 3:** Deploy via Coolify:

```bash
coolify deploy uuid yngsizj96krk25x05a08u8ib --format json
```

Capture `deployment_uuid`, poll `coolify deploy get <uuid> --format json` until `status` is `finished`, then confirm `coolify app get yngsizj96krk25x05a08u8ib` reports `running:healthy`.

- [ ] **Step 4:** Verify the live site (`analyzer.kiekr.app`, Traefik basic auth `admin:H4ns3m3sh`). Navigate once with credentials in the URL to cache auth, then load `#/perf`, `#/packets`, `#/nodes`, `#/channels` and confirm `.ps` states render and the console is clean.

- [ ] **Step 5:** Update `ROADMAP.md` — remove the "Inconsistent page states" bullet from the P1 list. Commit and push:

```bash
git add ROADMAP.md
git commit -m "docs: mark page-states standardization done in ROADMAP"
```

---

## Self-review notes

- **Spec coverage:** module + CSS (Task 1), API incl. error renderer (Task 1), states/skeleton (Task 1), a11y roles + reduced-motion (Tasks 1, 14), mobile (Tasks 1, 14), all 15 modules converted (Tasks 3–13; `mc-keygen` deliberately unchanged per Task 7 Step 4 — its error flow is form validation, not a page-load state, noted in the spec's intent), bug fixes for dead `.spinner` / console-only errors / bare `Loading...` (Tasks 8, 9, 11, 12), dead-CSS cleanup (Task 15), deploy + verify (Task 16).
- **Type consistency:** the API names `loading`, `empty`, `skeleton`, `errorText`, `error`, `row` are used identically in Task 1, Task 2, and every conversion task.
- **Known soft spots resolved inline:** conversion tasks reference `<container el>` and `<load fn>` placeholders by description because the exact element/function names live in each page module and must be read at execution time — this is a read-then-substitute instruction, not an unspecified design decision. The substitution rules section defines exactly how to choose them.
