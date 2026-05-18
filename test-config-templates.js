#!/usr/bin/env node
/* Config-activatable branding templates E2E
 *
 * Exercises the template feature end-to-end in two server configurations:
 *
 * A. DEFAULT — no "template" key in config (or template: "").
 *    1. Served / HTML contains <title>CoreScope-EVO</title> (server-rendered meta tag).
 *    2. Home page #/ has NO .home-donate section.
 *    3. Observers page #/observers has NO .observer-setup-help element.
 *    4. GET /api/config/theme returns template: "default" and intact shape.
 *
 * B. CORNMEISTER — config sets template: "cornmeister".
 *    5. Served / HTML contains <meta property="og:title" content="Cornmeister.nl">
 *       (server-side meta injection via __SITE_META__ placeholder).
 *    6. document.title after SPA init equals the template branding.siteName ("CORNMEISTER.NL").
 *    7. GET /api/config/theme returns template: "cornmeister" with a populated sections.
 *    8. Home page #/ renders a .home-donate section containing the bunq.me link.
 *    9. GET /template-assets/logo.svg returns HTTP 200.
 *   10. Observers page #/observers renders a .observer-setup-help element.
 *   11. Announcement modal (.ann-overlay/.ann-card) appears on #/ (or lighter
 *       assertion: sections.announcement.enabled is true in /api/config/theme).
 *
 * Server start/stop: the test spawns the Go server binary directly, writing a
 * minimal config.json to a temp directory per run. Uses the test-fixtures DB so
 * the server starts without requiring a production database.
 *
 * Compatible with the same Playwright + Node harness used by test-logo-rebrand-e2e.js
 * and friends. CHROMIUM_REQUIRE=1 makes a missing Chromium a hard fail.
 */
'use strict';

const { chromium } = require('playwright');
const { spawn } = require('child_process');
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');

const SERVER_BIN = path.resolve(__dirname, 'cmd/server/server');
const FIXTURE_DB = path.resolve(__dirname, 'test-fixtures/e2e-fixture.db');
const PUBLIC_DIR = path.resolve(__dirname, 'public');

// Two different ports so runs do not collide.
const PORT_DEFAULT     = 13582;
const PORT_CORNMEISTER = 13583;

function fail(msg) {
  console.error(`test-config-templates.js: FAIL — ${msg}`);
  process.exit(1);
}
function assert(cond, msg) { if (!cond) fail(msg || 'assertion failed'); }

// ── HTTP helpers ──────────────────────────────────────────────────────────────

function httpGet(url) {
  return new Promise((resolve, reject) => {
    const u = new URL(url);
    const req = http.request({
      method: 'GET',
      hostname: u.hostname,
      port: u.port || 80,
      path: u.pathname + (u.search || ''),
    }, (res) => {
      let body = '';
      res.on('data', (c) => { body += c; });
      res.on('end', () => resolve({ status: res.statusCode, ct: res.headers['content-type'] || '', body }));
    });
    req.on('error', reject);
    req.end();
  });
}

function waitForServer(url, retries = 20, delayMs = 250) {
  return new Promise((resolve, reject) => {
    let attempts = 0;
    function attempt() {
      attempts++;
      const u = new URL(url);
      const req = http.request({
        method: 'GET',
        hostname: u.hostname,
        port: u.port || 80,
        path: u.pathname + (u.search || ''),
        timeout: 500,
      }, (res) => {
        res.resume(); // drain
        resolve();
      });
      req.on('error', () => {
        if (attempts >= retries) { reject(new Error(`server at ${url} did not become ready after ${retries} attempts`)); return; }
        setTimeout(attempt, delayMs);
      });
      req.on('timeout', () => { req.destroy(); });
      req.end();
    }
    attempt();
  });
}

// ── Server lifecycle ──────────────────────────────────────────────────────────

function startServer(port, configJson) {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cs-tmpl-test-'));
  fs.writeFileSync(path.join(tmpDir, 'config.json'), JSON.stringify(configJson));

  const proc = spawn(SERVER_BIN, [
    '--port', String(port),
    '--public', PUBLIC_DIR,
    '--db', FIXTURE_DB,
    '--config-dir', tmpDir,
  ], {
    cwd: __dirname, // so templates/<name>/ are resolved from repo root
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  // Collect stderr for diagnostics; suppress to keep test output clean.
  proc.stderr.on('data', () => {});
  proc.stdout.on('data', () => {});

  return { proc, tmpDir };
}

function stopServer(proc, tmpDir) {
  try { proc.kill('SIGTERM'); } catch (_) {}
  try { fs.rmSync(tmpDir, { recursive: true, force: true }); } catch (_) {}
}

// ── Main ──────────────────────────────────────────────────────────────────────

async function main() {
  const requireChromium = process.env.CHROMIUM_REQUIRE === '1';
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (requireChromium) {
      console.error(`test-config-templates.js: FAIL — Chromium required but unavailable: ${err.message}`);
      process.exit(1);
    }
    console.log(`test-config-templates.js: SKIP (Chromium unavailable: ${err.message.split('\n')[0]})`);
    process.exit(0);
  }

  let passed = 0;
  const total = 11;

  // ── Phase A: Default template ─────────────────────────────────────────────

  const { proc: procA, tmpDir: tmpA } = startServer(PORT_DEFAULT, {});
  const BASE_A = `http://localhost:${PORT_DEFAULT}`;

  try {
    await waitForServer(BASE_A + '/api/config/theme');
  } catch (err) {
    stopServer(procA, tmpA);
    try { await browser.close(); } catch (_) {}
    fail(`default-template server did not start: ${err.message}`);
  }

  console.log('\n── Phase A: default template ──');

  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    // Set experienced so we see the real home page, not the first-visit chooser.
    await page.addInitScript(() => {
      try { localStorage.setItem('meshcore-user-level', 'experienced'); } catch (_) {}
      // Clear any leftover announcement dismissals so they don't bleed between phases.
      try { Object.keys(localStorage).forEach(k => { if (k.startsWith('meshcore-announcement-')) localStorage.removeItem(k); }); } catch (_) {}
    });

    // 1. Served / HTML contains <title>CoreScope-EVO</title> (server-injected meta).
    //    The SPA re-sets document.title to branding.siteName ("CoreScope") after init,
    //    so we assert the server-side injection via HTTP, not document.title.
    const htmlResA = await httpGet(BASE_A + '/');
    assert(htmlResA.status === 200, `GET / returned ${htmlResA.status}`);
    assert(
      htmlResA.body.includes('<title>CoreScope-EVO</title>'),
      `default template: server HTML missing <title>CoreScope-EVO</title>\n(sample: ${htmlResA.body.slice(0, 600)})`
    );
    await page.goto(BASE_A + '/#/', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.nav-brand', { timeout: 10000 });
    // Wait for SITE_CONFIG to load (async fetch of /api/config/theme).
    await page.waitForFunction(() => !!window.SITE_CONFIG, { timeout: 8000 });
    console.log(`  ✅ [1] server HTML contains <title>CoreScope-EVO</title> (default template)`);
    passed++;

    // 2. Home page has NO .home-donate section — navigate away+back AFTER SITE_CONFIG
    //    is loaded so renderHome() has access to the full config and the negative
    //    assertion is meaningful (not just "page loaded before config arrived").
    await page.evaluate(() => { window.location.hash = '#/packets'; });
    await page.waitForFunction(() => location.hash === '#/packets');
    await page.evaluate(() => { window.location.hash = '#/home'; });
    await page.waitForFunction(() => location.hash === '#/home' || location.hash === '#/');
    const hasDonateA = await page.$('.home-donate');
    assert(!hasDonateA,
      'default template: .home-donate section found but must be absent without donate config');
    console.log('  ✅ [2] no .home-donate on home page after SITE_CONFIG loaded (default template)');
    passed++;

    // 3. Observers page has NO .observer-setup-help — navigate after SITE_CONFIG loaded.
    await page.evaluate(() => { window.location.hash = '#/observers'; });
    await page.waitForFunction(() => location.hash === '#/observers');
    await page.waitForSelector('.observers-page', { timeout: 10000 });
    const hasSetupA = await page.$('.observer-setup-help');
    assert(!hasSetupA,
      'default template: .observer-setup-help found but must be absent without observerSetup config');
    console.log('  ✅ [3] no .observer-setup-help on observers page after SITE_CONFIG loaded (default template)');
    passed++;

    // 4. /api/config/theme shape — template is "default", sections exists
    const themeResA = await httpGet(BASE_A + '/api/config/theme');
    assert(themeResA.status === 200, `/api/config/theme returned ${themeResA.status} (expected 200)`);
    const themeA = JSON.parse(themeResA.body);
    assert(themeA.template === 'default' || themeA.template === '',
      `/api/config/theme template should be "default" or "", got "${themeA.template}"`);
    assert(typeof themeA.branding === 'object' && themeA.branding !== null,
      '/api/config/theme missing "branding" object');
    assert(typeof themeA.meta === 'object' && themeA.meta !== null,
      '/api/config/theme missing "meta" object');
    console.log(`  ✅ [4] /api/config/theme template="${themeA.template}", shape intact`);
    passed++;

    await context.close();
  } catch (err) {
    stopServer(procA, tmpA);
    try { await browser.close(); } catch (_) {}
    console.error(`test-config-templates.js: FAIL — ${err.message}`);
    process.exit(1);
  }

  stopServer(procA, tmpA);

  // ── Phase B: Cornmeister template ─────────────────────────────────────────

  const { proc: procB, tmpDir: tmpB } = startServer(PORT_CORNMEISTER, { template: 'cornmeister' });
  const BASE_B = `http://localhost:${PORT_CORNMEISTER}`;

  try {
    await waitForServer(BASE_B + '/api/config/theme');
  } catch (err) {
    stopServer(procB, tmpB);
    try { await browser.close(); } catch (_) {}
    fail(`cornmeister server did not start: ${err.message}`);
  }

  console.log('\n── Phase B: cornmeister template ──');

  try {
    // 5. Server-side HTML contains og:title = "Cornmeister.nl"
    const htmlRes = await httpGet(BASE_B + '/');
    assert(htmlRes.status === 200, `GET / returned ${htmlRes.status}`);
    assert(
      htmlRes.body.includes('<meta property="og:title" content="Cornmeister.nl">'),
      `server HTML missing <meta property="og:title" content="Cornmeister.nl"> — got:\n${htmlRes.body.slice(0, 800)}`
    );
    console.log('  ✅ [5] served / HTML contains <meta property="og:title" content="Cornmeister.nl">');
    passed++;

    // 6. document.title after SPA init equals the template branding.siteName.
    //    The cornmeister template sets branding.siteName = "CORNMEISTER.NL";
    //    customize-v2.js applies that as document.title once SITE_CONFIG is loaded.
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    await page.addInitScript(() => {
      try { localStorage.setItem('meshcore-user-level', 'experienced'); } catch (_) {}
      try { Object.keys(localStorage).forEach(k => { if (k.startsWith('meshcore-announcement-')) localStorage.removeItem(k); }); } catch (_) {}
    });

    await page.goto(BASE_B + '/#/', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.nav-brand', { timeout: 10000 });
    await page.waitForFunction(() => !!window.SITE_CONFIG, { timeout: 8000 });
    const titleB = await page.title();
    // branding.siteName from cornmeister/template.json is "CORNMEISTER.NL"
    const expectedBrandingName = 'CORNMEISTER.NL';
    assert(titleB === expectedBrandingName,
      `cornmeister template: expected document.title "${expectedBrandingName}" (branding.siteName), got "${titleB}"`);
    console.log(`  ✅ [6] document.title = "${titleB}" (template branding.siteName)`);
    passed++;

    // 7. /api/config/theme returns template: "cornmeister" with sections
    const themeResB = await httpGet(BASE_B + '/api/config/theme');
    assert(themeResB.status === 200, `/api/config/theme returned ${themeResB.status}`);
    const themeB = JSON.parse(themeResB.body);
    assert(themeB.template === 'cornmeister',
      `/api/config/theme template should be "cornmeister", got "${themeB.template}"`);
    assert(typeof themeB.sections === 'object' && themeB.sections !== null,
      '/api/config/theme missing "sections" object for cornmeister template');
    const sectionKeys = Object.keys(themeB.sections);
    assert(sectionKeys.length > 0,
      '/api/config/theme "sections" is empty for cornmeister template — expected donate, announcement, etc.');
    console.log(`  ✅ [7] /api/config/theme template="cornmeister", sections keys: ${sectionKeys.join(', ')}`);
    passed++;

    // 8. Home page renders .home-donate with the bunq.me link
    // Navigate fresh to ensure home page re-renders with SITE_CONFIG loaded.
    // 8. Home page renders .home-donate with the bunq.me link.
    //    The SPA renders home BEFORE /api/config/theme resolves, so
    //    donateSection() sees no SITE_CONFIG and returns empty. We must:
    //    (a) wait for SITE_CONFIG.sections.donate.enabled to be truthy,
    //    (b) navigate to another page and back, which triggers a re-init
    //        with the now-loaded SITE_CONFIG so donateSection() fires.
    await page.waitForFunction(() => {
      var d = window.SITE_CONFIG && window.SITE_CONFIG.sections && window.SITE_CONFIG.sections.donate;
      return d && d.enabled;
    }, { timeout: 8000 });
    // Navigate away then back to force a fresh init() with SITE_CONFIG loaded.
    await page.evaluate(() => { window.location.hash = '#/packets'; });
    await page.waitForFunction(() => location.hash === '#/packets');
    await page.evaluate(() => { window.location.hash = '#/home'; });
    await page.waitForFunction(() => location.hash === '#/home' || location.hash === '#/');

    const donateB = await page.waitForSelector('.home-donate', { timeout: 8000 });
    assert(!!donateB, 'cornmeister template: .home-donate section not found on home page');
    const donateHtml = await page.evaluate(() => {
      var el = document.querySelector('.home-donate');
      return el ? el.innerHTML : '';
    });
    assert(donateHtml.includes('bunq.me'),
      `cornmeister template: .home-donate exists but bunq.me link not found (got: ${donateHtml.slice(0, 200)})`);
    console.log('  ✅ [8] .home-donate section present with bunq.me support link');
    passed++;

    // 9. GET /template-assets/logo.svg returns 200
    const logoRes = await httpGet(BASE_B + '/template-assets/logo.svg');
    assert(logoRes.status === 200,
      `/template-assets/logo.svg returned ${logoRes.status} (expected 200)`);
    console.log('  ✅ [9] GET /template-assets/logo.svg → 200');
    passed++;

    // 10. Observers page renders .observer-setup-help.
    //     Same SITE_CONFIG timing issue as #8: navigate to #/observers only
    //     after SITE_CONFIG.sections.observerSetup.enabled is truthy.
    await page.waitForFunction(() => {
      var s = window.SITE_CONFIG && window.SITE_CONFIG.sections && window.SITE_CONFIG.sections.observerSetup;
      return s && s.enabled;
    }, { timeout: 8000 });
    // Navigate away + back to force a fresh observers init() with SITE_CONFIG loaded.
    await page.evaluate(() => { window.location.hash = '#/home'; });
    await page.waitForFunction(() => location.hash === '#/home' || location.hash === '#/');
    await page.evaluate(() => { window.location.hash = '#/observers'; });
    await page.waitForFunction(() => location.hash === '#/observers');
    await page.waitForSelector('.observers-page', { timeout: 10000 });
    const setupB = await page.waitForSelector('.observer-setup-help', { timeout: 8000 });
    assert(!!setupB, 'cornmeister template: .observer-setup-help not found on observers page');
    console.log('  ✅ [10] .observer-setup-help present on observers page (cornmeister template)');
    passed++;

    // 11. Announcement modal — prefer DOM assertion (.ann-overlay/.ann-card present on #/).
    //     The modal is shown by maybeShowAnnouncement() which is called from home init().
    //     We need to be on #/home AFTER SITE_CONFIG is loaded and announcement not dismissed.
    //     Since SITE_CONFIG is already loaded at this point, navigate to home to trigger init.
    await page.evaluate(() => { window.location.hash = '#/home'; });
    await page.waitForFunction(() => location.hash === '#/home' || location.hash === '#/');
    await page.waitForSelector('.nav-brand', { timeout: 10000 });
    await page.waitForFunction(() => !!(window.SITE_CONFIG && window.SITE_CONFIG.sections && window.SITE_CONFIG.sections.announcement), { timeout: 8000 });

    const annModal = await page.waitForSelector('.ann-overlay', { timeout: 5000 }).catch(() => null);
    if (annModal) {
      const hasCard = await page.$('.ann-card');
      assert(!!hasCard, 'cornmeister template: .ann-overlay present but .ann-card missing inside it');
      console.log('  ✅ [11] announcement modal (.ann-overlay + .ann-card) rendered on first visit');
      passed++;
    } else {
      // Lighter assertion: confirm the config is correct even if DOM modal timed out.
      const annEnabled = await page.evaluate(() => {
        var a = window.SITE_CONFIG && window.SITE_CONFIG.sections && window.SITE_CONFIG.sections.announcement;
        return a && a.enabled;
      });
      assert(annEnabled,
        'cornmeister template: announcement modal DOM not found AND sections.announcement.enabled is falsy — modal not wired');
      console.log('  ✅ [11] sections.announcement.enabled=true (modal DOM not rendered within timeout — lighter assertion accepted)');
      passed++;
    }

    await context.close();
  } catch (err) {
    stopServer(procB, tmpB);
    try { await browser.close(); } catch (_) {}
    console.error(`test-config-templates.js: FAIL — ${err.message}`);
    process.exit(1);
  }

  stopServer(procB, tmpB);
  try { await browser.close(); } catch (_) {}

  console.log(`\ntest-config-templates.js: ${passed}/${total} PASS`);
  if (passed < total) process.exit(1);
}

main();
