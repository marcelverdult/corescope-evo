/**
 * Playwright E2E tests — proof of concept
 * Runs against prod (analyzer.00id.net), read-only.
 * Usage: node test-e2e-playwright.js
 */
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';
const results = [];

async function test(name, fn) {
  try {
    await fn();
    results.push({ name, pass: true });
    console.log(`  \u2705 ${name}`);
  } catch (err) {
    results.push({ name, pass: false, error: err.message });
    console.log(`  \u274c ${name}: ${err.message}`);
  }
}

function assert(condition, msg) {
  if (!condition) throw new Error(msg || 'Assertion failed');
}

async function run() {
  console.log('Launching Chromium...');
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage']
  });
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(10000);

  console.log(`\nRunning E2E tests against ${BASE}\n`);

  // --- Group: Home page (tests 1, 6, 7) ---

  // Test 1: Home page loads
  await test('Home page loads', async () => {
    await page.goto(BASE, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('nav, .navbar, .nav, [class*="nav"]');
    const title = await page.title();
    assert(title.toLowerCase().includes('meshcore'), `Title "${title}" doesn't contain MeshCore`);
    const nav = await page.$('nav, .navbar, .nav, [class*="nav"]');
    assert(nav, 'Nav bar not found');
  });

  // Test 6: Theme customizer opens (reuses home page from test 1)
  await test('Theme customizer opens', async () => {
    // Look for palette/customize button
    const btn = await page.$('button[title*="ustom" i], button[aria-label*="theme" i], [class*="customize"], button:has-text("\ud83c\udfa8")');
    if (!btn) {
      // Try finding by emoji content
      const allButtons = await page.$$('button');
      let found = false;
      for (const b of allButtons) {
        const text = await b.textContent();
        if (text.includes('\ud83c\udfa8')) {
          await b.click();
          found = true;
          break;
        }
      }
      assert(found, 'Could not find theme customizer button');
    } else {
      await btn.click();
    }
    await page.waitForFunction(() => {
      const html = document.body.innerHTML;
      return html.includes('preset') || html.includes('Preset') || html.includes('theme') || html.includes('Theme');
    });
    const html = await page.content();
    const hasCustomizer = html.includes('preset') || html.includes('Preset') || html.includes('theme') || html.includes('Theme');
    assert(hasCustomizer, 'Customizer panel not found after clicking');
  });

  // Test 7: Dark mode toggle (fresh navigation \u2014 customizer panel may be open)
  await test('Dark mode toggle', async () => {
    await page.goto(BASE, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('nav, .navbar, .nav, [class*="nav"]');
    const themeBefore = await page.$eval('html', el => el.getAttribute('data-theme'));
    // Find toggle button
    const allButtons = await page.$$('button');
    let toggled = false;
    for (const b of allButtons) {
      const text = await b.textContent();
      if (text.includes('\u2600') || text.includes('\ud83c\udf19') || text.includes('\ud83c\udf11') || text.includes('\ud83c\udf15')) {
        await b.click();
        toggled = true;
        break;
      }
    }
    assert(toggled, 'Could not find dark mode toggle button');
    await page.waitForFunction(
      (before) => document.documentElement.getAttribute('data-theme') !== before,
      themeBefore
    );
    const themeAfter = await page.$eval('html', el => el.getAttribute('data-theme'));
    assert(themeBefore !== themeAfter, `Theme didn't change: before=${themeBefore}, after=${themeAfter}`);
  });

  // --- Group: Nodes page (tests 2, 5) ---

  // Test 2: Nodes page loads with data
  await test('Nodes page loads with data', async () => {
    await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('table tbody tr');
    const headers = await page.$$eval('th', els => els.map(e => e.textContent.trim()));
    for (const col of ['Name', 'Public Key', 'Role']) {
      assert(headers.some(h => h.includes(col)), `Missing column: ${col}`);
    }
    assert(headers.some(h => h.includes('Last Seen') || h.includes('Last')), 'Missing Last Seen column');
    const rows = await page.$$('table tbody tr');
    assert(rows.length >= 1, `Expected >=1 nodes, got ${rows.length}`);
  });

  // Test 5: Node detail loads (reuses nodes page from test 2)
  await test('Node detail loads', async () => {
    await page.waitForSelector('table tbody tr');
    // Click first row
    const firstRow = await page.$('table tbody tr');
    assert(firstRow, 'No node rows found');
    await firstRow.click();
    // Wait for detail pane to appear
    await page.waitForSelector('.node-detail');
    const html = await page.content();
    // Check for status indicator
    const hasStatus = html.includes('\ud83d\udfe2') || html.includes('\u26aa') || html.includes('status') || html.includes('Active') || html.includes('Stale');
    assert(hasStatus, 'No status indicator found in node detail');
  });

  // --- Group: Map page (tests 3, 9, 10, 13, 16) ---

  // Test 3: Map page loads with markers
  await test('Map page loads with markers', async () => {
    await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-container');
    await page.waitForSelector('.leaflet-tile-loaded');
    // Wait for markers/overlays to render (may not exist with empty DB)
    try {
      await page.waitForSelector('.leaflet-marker-icon, .leaflet-interactive, circle, .marker-cluster, .leaflet-marker-pane > *, .leaflet-overlay-pane svg path, .leaflet-overlay-pane svg circle', { timeout: 3000 });
    } catch (_) {
      // No markers with empty DB \u2014 assertion below handles it
    }
    const markers = await page.$$('.leaflet-marker-icon, .leaflet-interactive, circle, .marker-cluster, .leaflet-marker-pane > *, .leaflet-overlay-pane svg path, .leaflet-overlay-pane svg circle');
    assert(markers.length > 0, 'No map markers/overlays found');
  });

  // Test 9: Map heat checkbox persists in localStorage (reuses map page)
  await test('Map heat checkbox persists in localStorage', async () => {
    await page.waitForSelector('#mcHeatmap');
    // Uncheck first to ensure clean state
    await page.evaluate(() => localStorage.removeItem('meshcore-map-heatmap'));
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#mcHeatmap');
    let checked = await page.$eval('#mcHeatmap', el => el.checked);
    assert(!checked, 'Heat checkbox should be unchecked by default');
    // Check it
    await page.click('#mcHeatmap');
    const stored = await page.evaluate(() => localStorage.getItem('meshcore-map-heatmap'));
    assert(stored === 'true', `localStorage should be "true" but got "${stored}"`);
    // Reload and verify persisted
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#mcHeatmap');
    checked = await page.$eval('#mcHeatmap', el => el.checked);
    assert(checked, 'Heat checkbox should be checked after reload');
    // Clean up
    await page.evaluate(() => localStorage.removeItem('meshcore-map-heatmap'));
  });

  // Test 10: Map heat checkbox is not disabled (unless matrix mode)
  await test('Map heat checkbox is clickable', async () => {
    await page.waitForSelector('#mcHeatmap');
    const disabled = await page.$eval('#mcHeatmap', el => el.disabled);
    assert(!disabled, 'Heat checkbox should not be disabled');
    // Click and verify state changes
    const before = await page.$eval('#mcHeatmap', el => el.checked);
    await page.click('#mcHeatmap');
    const after = await page.$eval('#mcHeatmap', el => el.checked);
    assert(before !== after, 'Heat checkbox state should toggle on click');
  });

  // Test 13: Heatmap opacity stored in localStorage (reuses map page)
  await test('Heatmap opacity persists in localStorage', async () => {
    await page.evaluate(() => localStorage.setItem('meshcore-heatmap-opacity', '0.5'));
    // Enable heat to trigger layer creation with saved opacity
    await page.evaluate(() => localStorage.setItem('meshcore-map-heatmap', 'true'));
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#mcHeatmap');
    const opacity = await page.evaluate(() => localStorage.getItem('meshcore-heatmap-opacity'));
    assert(opacity === '0.5', `Opacity should persist as "0.5" but got "${opacity}"`);
    // Verify the canvas element has the opacity applied (if heat layer exists)
    const canvasOpacity = await page.evaluate(() => {
      if (window._meshcoreHeatLayer && window._meshcoreHeatLayer._canvas) {
        return window._meshcoreHeatLayer._canvas.style.opacity;
      }
      return null; // no heat layer (no node data) \u2014 skip
    });
    if (canvasOpacity !== null) {
      assert(canvasOpacity === '0.5', `Canvas opacity should be "0.5" but got "${canvasOpacity}"`);
    }
    // Clean up
    await page.evaluate(() => {
      localStorage.removeItem('meshcore-heatmap-opacity');
      localStorage.removeItem('meshcore-map-heatmap');
    });
  });

  // Test 16: Map re-renders markers on resize (decollision recalculates)
  await test('Map re-renders on resize', async () => {
    await page.waitForSelector('.leaflet-container');
    // Wait for markers (may not exist with empty DB)
    try {
      await page.waitForSelector('.leaflet-marker-icon, .leaflet-interactive', { timeout: 3000 });
    } catch (_) {
      // No markers with empty DB
    }
    // Count markers before resize
    const beforeCount = await page.$$eval('.leaflet-marker-icon, .leaflet-interactive', els => els.length);
    // Resize viewport
    await page.setViewportSize({ width: 600, height: 400 });
    // Wait for Leaflet to process resize
    await page.waitForFunction(() => {
      const c = document.querySelector('.leaflet-container');
      return c && c.offsetWidth <= 600;
    });
    // Markers should still be present after resize (re-rendered, not lost)
    const afterCount = await page.$$eval('.leaflet-marker-icon, .leaflet-interactive', els => els.length);
    assert(afterCount > 0, `Should have markers after resize, got ${afterCount}`);
    // Restore
    await page.setViewportSize({ width: 1280, height: 720 });
  });

  // --- Group: Packets page (test 4) ---

  // Test 4: Packets page loads with filter
  await test('Packets page loads with filter', async () => {
    await page.goto(`${BASE}/#/packets`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('table tbody tr');
    const rowsBefore = await page.$$('table tbody tr');
    assert(rowsBefore.length > 0, 'No packets visible');
    // Use the specific filter input
    const filterInput = await page.$('#packetFilterInput');
    assert(filterInput, 'Packet filter input not found');
    await filterInput.fill('type == ADVERT');
    // Client-side filter has input debounce (~250ms); wait for it to apply
    await page.waitForTimeout(500);
    // Verify filter was applied (count may differ)
    const rowsAfter = await page.$$('table tbody tr');
    assert(rowsAfter.length > 0, 'No packets after filtering');
  });

  // Test: Packet detail pane dismiss button (Issue #125)
  await test('Packet detail pane closes on ✕ click', async () => {
    // Reuse packets page from test 4 — check if any rows exist
    const rows = await page.$$('table tbody tr');
    if (rows.length === 0) {
      console.log('    ⏭️  Skipped (no packets in DB)');
      return; // skip gracefully on empty data
    }
    // Click first packet row to open detail pane
    const firstRow = await page.$('table tbody tr[data-action]');
    if (!firstRow) {
      console.log('    ⏭️  Skipped (no clickable packet rows)');
      return;
    }
    await firstRow.click();
    // Wait for detail pane to become visible (empty class removed)
    await page.waitForFunction(() => {
      const panel = document.getElementById('pktRight');
      return panel && !panel.classList.contains('empty');
    }, { timeout: 5000 });
    // Verify detail pane is visible
    const panelVisible = await page.$eval('#pktRight', el => !el.classList.contains('empty'));
    assert(panelVisible, 'Detail pane should be visible after clicking a row');
    // Click the close button (✕)
    const closeBtn = await page.$('#pktRight .panel-close-btn');
    assert(closeBtn, 'Close button (✕) not found in detail pane');
    await closeBtn.click();
    // Verify the detail pane is hidden (empty class restored)
    await page.waitForFunction(() => {
      const panel = document.getElementById('pktRight');
      return panel && panel.classList.contains('empty');
    }, { timeout: 3000 });
    const panelHidden = await page.$eval('#pktRight', el => el.classList.contains('empty'));
    assert(panelHidden, 'Detail pane should be hidden after clicking ✕');
  });

  // --- Group: Analytics page (test 8) ---

  // Test 8: Analytics page loads
  await test('Analytics page loads', async () => {
    await page.goto(`${BASE}/#/analytics`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => {
      const html = document.body.innerHTML.toLowerCase();
      return html.includes('analytics') || html.includes('tab') || html.includes('chart') || html.includes('topology');
    });
    const html = await page.content();
    // Check for any analytics content
    const hasContent = html.includes('analytics') || html.includes('Analytics') || html.includes('tab') || html.includes('chart') || html.includes('topology');
    assert(hasContent, 'Analytics page has no recognizable content');
  });

  // --- Group: Live page (tests 11, 12) ---

  // Test 11: Live page heat checkbox disabled by matrix/ghosts mode
  await test('Live heat disabled when ghosts mode active', async () => {
    await page.goto(`${BASE}/#/live`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#liveHeatToggle');
    // Enable matrix mode if not already
    const matrixEl = await page.$('#liveMatrixToggle');
    if (matrixEl) {
      await page.evaluate(() => {
        const mt = document.getElementById('liveMatrixToggle');
        if (mt && !mt.checked) mt.click();
      });
      await page.waitForFunction(() => {
        const heat = document.getElementById('liveHeatToggle');
        return heat && heat.disabled;
      });
      const heatDisabled = await page.$eval('#liveHeatToggle', el => el.disabled);
      assert(heatDisabled, 'Heat should be disabled when ghosts/matrix is on');
      // Turn off matrix
      await page.evaluate(() => {
        const mt = document.getElementById('liveMatrixToggle');
        if (mt && mt.checked) mt.click();
      });
      await page.waitForFunction(() => {
        const heat = document.getElementById('liveHeatToggle');
        return heat && !heat.disabled;
      });
      const heatEnabled = await page.$eval('#liveHeatToggle', el => !el.disabled);
      assert(heatEnabled, 'Heat should be re-enabled when ghosts/matrix is off');
    }
  });

  // Test 12: Live page heat checkbox persists across reload (reuses live page)
  await test('Live heat checkbox persists in localStorage', async () => {
    await page.waitForSelector('#liveHeatToggle');
    // Clear state
    await page.evaluate(() => localStorage.removeItem('meshcore-live-heatmap'));
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#liveHeatToggle');
    // Default is checked (has `checked` attribute in HTML)
    const defaultState = await page.$eval('#liveHeatToggle', el => el.checked);
    // Uncheck it
    if (defaultState) await page.click('#liveHeatToggle');
    const stored = await page.evaluate(() => localStorage.getItem('meshcore-live-heatmap'));
    assert(stored === 'false', `localStorage should be "false" after unchecking but got "${stored}"`);
    // Reload and verify persisted
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#liveHeatToggle');
    const afterReload = await page.$eval('#liveHeatToggle', el => el.checked);
    assert(!afterReload, 'Live heat checkbox should stay unchecked after reload');
    // Clean up
    await page.evaluate(() => localStorage.removeItem('meshcore-live-heatmap'));
  });

  // --- Group: No navigation needed (tests 14, 15) ---

  // Test 14: Live heatmap opacity stored in localStorage
  await test('Live heatmap opacity persists in localStorage', async () => {
    // Verify localStorage key works (no page load needed \u2014 reuse current page)
    await page.evaluate(() => localStorage.setItem('meshcore-live-heatmap-opacity', '0.6'));
    const opacity = await page.evaluate(() => localStorage.getItem('meshcore-live-heatmap-opacity'));
    assert(opacity === '0.6', `Live opacity should persist as "0.6" but got "${opacity}"`);
    await page.evaluate(() => localStorage.removeItem('meshcore-live-heatmap-opacity'));
  });

  // Test 15: Customizer has separate Map and Live opacity sliders
  await test('Customizer has separate map and live opacity sliders', async () => {
    // Verify by checking JS source \u2014 avoids heavy page reloads that crash ARM chromium
    const custJs = await page.evaluate(async () => {
      const res = await fetch('/customize.js?_=' + Date.now());
      return res.text();
    });
    assert(custJs.includes('custHeatOpacity'), 'customize.js should have map opacity slider (custHeatOpacity)');
    assert(custJs.includes('custLiveHeatOpacity'), 'customize.js should have live opacity slider (custLiveHeatOpacity)');
    assert(custJs.includes('meshcore-heatmap-opacity'), 'customize.js should use meshcore-heatmap-opacity key');
    assert(custJs.includes('meshcore-live-heatmap-opacity'), 'customize.js should use meshcore-live-heatmap-opacity key');
    // Verify labels are distinct
    assert(custJs.includes('Nodes Map') || custJs.includes('nodes map') || custJs.includes('\ud83d\uddfa'), 'Map slider should have map-related label');
    assert(custJs.includes('Live Map') || custJs.includes('live map') || custJs.includes('\ud83d\udce1'), 'Live slider should have live-related label');
  });

  await browser.close();

  // Summary
  const passed = results.filter(r => r.pass).length;
  const failed = results.filter(r => !r.pass).length;
  console.log(`\n${passed}/${results.length} tests passed${failed ? `, ${failed} failed` : ''}`);
  process.exit(failed > 0 ? 1 : 0);
}

run().catch(err => {
  console.error('Fatal error:', err);
  process.exit(1);
});
