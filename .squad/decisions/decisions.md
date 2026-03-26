# Squad Decisions Log

## Decision: E2E Playwright Performance Improvements

**Author:** Kobayashi (Lead)  
**Date:** 2026-03-26  
**Status:** Proposed — awaiting user sign-off before implementation

### Context

Playwright E2E tests (16 tests in `test-e2e-playwright.js`) are slow in CI. Analysis identified ~40-50% potential runtime reduction across test code and CI pipeline.

### Recommendations (prioritized)

#### HIGH impact (30%+ improvement)

1. **Replace `waitUntil: 'networkidle'` with `'domcontentloaded'` + targeted waits** — used ~20 times; `networkidle` is worst-case for SPAs with persistent WebSocket connections and Leaflet tile loading. Each navigation pays a 500ms+ penalty waiting for zero in-flight requests.

2. **Eliminate redundant navigations** — group tests by route; tests 2+5 both go to `/#/nodes`, 6+7 both go to home, 9+10 both go to `/#/map`, 11+12 both go to `/#/live`. Navigate once, run all assertions for that route.

3. **Cache Playwright browser install in CI** — `npx playwright install chromium --with-deps` runs on every frontend-touched push. Self-hosted runner should retain the browser between runs; skip install if version matches.

#### MEDIUM impact (10-30%)

4. **Replace hardcoded `waitForTimeout` with event-driven waits** — ~17s of `waitForTimeout` scattered across tests. Replace with `waitForSelector`, `waitForFunction`, or `page.waitForResponse`.

5. **Merge coverage collection into the E2E run** — `collect-frontend-coverage.js` launches a second full browser to exercise the app again. Instead, extract `window.__coverage__` at the end of the E2E test run itself.

6. **Replace `sleep 5` server startup with health-check polling** — `curl --retry` or a loop checking `/api/stats` would start tests as soon as the server is ready (~1-2s savings).

#### LOW impact (<10% but good practice)

7. **Block unnecessary resources for non-visual tests** — use `page.route()` to abort map tile fetches, font loads, etc. for tests that only check DOM structure.

8. **Reduce default timeout from 15s to 10s** — current `page.setDefaultTimeout(15000)` is generous; 10s is sufficient for local CI.

### Implementation notes

- Items 1-2 are test-file-only changes (Bishop/Newt scope)
- Items 3, 5-6 are CI pipeline changes (Hicks scope)
- No architectural changes needed; all are incremental
- All existing test assertions remain identical — only wait strategies change
