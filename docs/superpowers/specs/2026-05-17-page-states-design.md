# Page-state standardization — design

Date: 2026-05-17
ROADMAP item: P1 "Inconsistent page states — loading/empty/error UI is ad-hoc; standardize."

## Problem

Loading, empty, and error UI is implemented ad-hoc across ~15 page modules. A
codebase audit found:

- ~14 modules render a loading state, ~9 an empty state, ~14 an error state —
  all inline, copy-pasted markup with inconsistent classes, padding, and colors.
- `.spinner` is used as a class in `nodes.js`, `analytics.js`, and `live.js` but
  is **not defined in any CSS file** — invisible dead markup.
- `map.js` and `live.js` handle some fetch failures with `console.error` only —
  the user sees nothing when a request fails.
- `perf.js` shows a bare hardcoded `Loading...` string.
- Several pages have no empty state at all (home, packets, map, observers,
  perf, compare).
- A `.empty-state` CSS component exists (style.css) but only `live.js` uses it.

This is the kind of inconsistency the project is moving away from. The fix is a
single, well-built, accessible, mobile-friendly page-state component that every
page uses.

## Scope

In scope: build the component, convert all 15 page modules, fix the real bugs
found en route (dead `.spinner`, console-only errors, bare `Loading...`,
missing empty states).

Out of scope: the larger frontend rearchitecture (build step, module system,
component framework, design system) — that remains a separate P3 initiative.
This component is plain markup + CSS + a11y and survives a future
rearchitecture untouched.

## Architecture

Two new files, both single-purpose:

- `public/page-state.js` — a classic-script IIFE (~150 lines) that explicitly
  assigns `window.PageState`. It does NOT rely on a top-level `function`
  declaration becoming a global (a known scoping hazard in this classic-script
  codebase). String-builder helpers plus one container renderer for the error
  state (which needs a bound retry handler).
- `public/page-state.css` — all page-state styling. Topical CSS file, matching
  the existing split (style.css / home.css / live.css / bottom-nav.css).

Both are loaded in `index.html` with `defer`, ordered before `app.js` and the
page modules so `window.PageState` exists when any page module runs. `defer`
preserves document order.

## API

String builders — for embedding in larger `innerHTML` assembly, which is the
universal idiom in this codebase:

```
PageState.loading(message)            → HTML string: GPU spinner + message
PageState.empty({icon, title, hint})  → HTML string: icon + title + optional hint
PageState.skeleton({rows, cols})      → HTML string: shimmer skeleton (table/card)
PageState.errorText(message)          → HTML string: static error, no retry
PageState.row(colspan, innerHTML)     → "<tr><td colspan=…>…</td></tr>" wrapper
```

Container renderer — the error state needs a bound JS handler for retry, which a
returned string cannot carry:

```
PageState.error(container, err, onRetry)
  Renders the error state into `container` (a DOM element).
  `err` may be an Error or a string; the message is read from err.message.
  If `onRetry` is a function, a Retry <button> is rendered and wired to it.
  If `onRetry` is omitted, no button is shown.
```

Rationale: loading/empty/skeleton are non-interactive, so cheap immutable
strings fit the `el.innerHTML = …` idiom. Error is the one interactive state, so
it gets a renderer. Two shapes, each justified — not redundant.

## States

- **Loading** — a GPU-friendly CSS spinner (`@keyframes` rotation via
  `transform`) plus a message. Table and card-grid pages use `skeleton()`
  instead — a shimmer placeholder matching the content shape, for perceived
  performance. Small panels and detail views use the spinner. Skeletons are NOT
  used for tiny panels (a skeleton for a small box is noise).
- **Empty** — a large, low-opacity icon, a title, and an optional hint line.
- **Error** — red accent (`--status-red`), the error message, and a Retry
  button. No page currently offers retry; adding it is the main UX upgrade.

## Accessibility

- Loading and empty containers: `role="status"` + `aria-live="polite"`.
- Error container: `role="alert"`.
- The spinner is decorative: `aria-hidden="true"`. State is conveyed by text.
- Retry is a real `<button>` — focusable, keyboard-activatable, labelled.
- `@media (prefers-reduced-motion: reduce)` disables spinner rotation and
  skeleton shimmer (static fallback).

## Mobile

- Container padding is fluid: `clamp(24px, 6vw, 60px)`.
- No fixed widths; text blocks use `max-width` and center.
- Retry button has a touch target of at least 44x44 px.

## CSS classes

Prefixed `ps-` to avoid collisions with the existing ad-hoc classes (`.ch-empty`,
`.trace-empty`, `.alab-empty`, `.empty-state`, the dead `.spinner`):

- `.ps` — container (flex column, centered, fluid padding, min-height).
- `.ps-spinner` — animated spinner.
- `.ps-icon` — empty-state icon.
- `.ps-title`, `.ps-hint` — text.
- `.ps-error` — error variant modifier.
- `.ps-retry` — retry button.
- `.ps-skeleton`, `.ps-skeleton-row`, `.ps-skeleton-cell` — skeleton shimmer.

## Conversion

All 15 page modules: home, packets, map, live, nodes, channels, observers,
analytics, perf, audio-lab, mc-keygen, node-analytics, observer-detail, traces,
compare. Inline loading/empty/error markup is replaced with `PageState` calls.

The dead `.spinner` markup is replaced with `PageState.loading(...)`. The
console-only error paths in `map.js` and `live.js` are replaced with visible
error states. The old ad-hoc empty classes are removed as their pages convert;
the unused `.empty-state` CSS in style.css is removed once `live.js` converts.

## Testing

- Pure frontend change — no Go code touched.
- `node --check` on `page-state.js` and every converted module.
- `node --check` does NOT catch classic-script scoping bugs, so every converted
  page is browser-tested on a local preview server: trigger loading (network
  throttle), empty (filter to no results), and error (block the API endpoint),
  and confirm the rendered state plus a clean console.
- Retry button: confirm clicking it re-runs the fetch.
- Verify `prefers-reduced-motion` stops the animations.

## Delivery

Incremental commits:

1. `page-state.js` + `page-state.css` + `index.html` wiring.
2. Page conversions in batches (grouped by area), each batch browser-tested
   before commit.
3. Remove the dead `.spinner` / `.empty-state` / ad-hoc empty CSS once no page
   references them.
4. Deploy to Coolify and verify the live site after the final batch.
