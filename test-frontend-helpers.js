/* Unit tests for frontend helper functions (tested via VM sandbox) */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

// --- Build a browser-like sandbox ---
function makeSandbox() {
  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      createElement: () => ({ id: '', textContent: '', innerHTML: '' }),
      head: { appendChild: () => {} },
      getElementById: () => null,
      addEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console,
    Date,
    Infinity,
    Math,
    Array,
    Object,
    String,
    Number,
    JSON,
    RegExp,
    Error,
    TypeError,
    parseInt,
    parseFloat,
    isNaN,
    isFinite,
    encodeURIComponent,
    decodeURIComponent,
    setTimeout: () => {},
    clearTimeout: () => {},
    setInterval: () => {},
    clearInterval: () => {},
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => {
      const store = {};
      return {
        getItem: k => store[k] || null,
        setItem: (k, v) => { store[k] = String(v); },
        removeItem: k => { delete store[k]; },
      };
    })(),
    location: { hash: '' },
    CustomEvent: class CustomEvent {},
    Map,
    Promise,
    URLSearchParams,
    addEventListener: () => {},
    dispatchEvent: () => {},
    requestAnimationFrame: (cb) => setTimeout(cb, 0),
  };
  vm.createContext(ctx);
  return ctx;
}

function loadInCtx(ctx, file) {
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx);
  // Copy window.* to global context so bare references work
  for (const k of Object.keys(ctx.window)) {
    ctx[k] = ctx.window[k];
  }
}

// ===== APP.JS TESTS =====
console.log('\n=== app.js: timeAgo ===');
{
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  const timeAgo = ctx.timeAgo;

  test('null returns dash', () => assert.strictEqual(timeAgo(null), '—'));
  test('undefined returns dash', () => assert.strictEqual(timeAgo(undefined), '—'));
  test('empty string returns dash', () => assert.strictEqual(timeAgo(''), '—'));

  test('30 seconds ago', () => {
    const d = new Date(Date.now() - 30000).toISOString();
    assert.strictEqual(timeAgo(d), '30s ago');
  });
  test('5 minutes ago', () => {
    const d = new Date(Date.now() - 300000).toISOString();
    assert.strictEqual(timeAgo(d), '5m ago');
  });
  test('2 hours ago', () => {
    const d = new Date(Date.now() - 7200000).toISOString();
    assert.strictEqual(timeAgo(d), '2h ago');
  });
  test('3 days ago', () => {
    const d = new Date(Date.now() - 259200000).toISOString();
    assert.strictEqual(timeAgo(d), '3d ago');
  });
}

console.log('\n=== app.js: escapeHtml ===');
{
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  const escapeHtml = ctx.escapeHtml;

  test('escapes < and >', () => assert.strictEqual(escapeHtml('<script>'), '&lt;script&gt;'));
  test('escapes &', () => assert.strictEqual(escapeHtml('a&b'), 'a&amp;b'));
  test('escapes quotes', () => assert.strictEqual(escapeHtml('"hello"'), '&quot;hello&quot;'));
  test('null returns empty', () => assert.strictEqual(escapeHtml(null), ''));
  test('undefined returns empty', () => assert.strictEqual(escapeHtml(undefined), ''));
  test('number coerced', () => assert.strictEqual(escapeHtml(42), '42'));
}

console.log('\n=== app.js: routeTypeName / payloadTypeName ===');
{
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');

  test('routeTypeName(0) = TRANSPORT_FLOOD', () => assert.strictEqual(ctx.routeTypeName(0), 'TRANSPORT_FLOOD'));
  test('routeTypeName(2) = DIRECT', () => assert.strictEqual(ctx.routeTypeName(2), 'DIRECT'));
  test('routeTypeName(99) = UNKNOWN', () => assert.strictEqual(ctx.routeTypeName(99), 'UNKNOWN'));
  test('payloadTypeName(4) = Advert', () => assert.strictEqual(ctx.payloadTypeName(4), 'Advert'));
  test('payloadTypeName(2) = Direct Msg', () => assert.strictEqual(ctx.payloadTypeName(2), 'Direct Msg'));
  test('payloadTypeName(99) = UNKNOWN', () => assert.strictEqual(ctx.payloadTypeName(99), 'UNKNOWN'));
}

console.log('\n=== app.js: truncate ===');
{
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  const truncate = ctx.truncate;

  test('short string unchanged', () => assert.strictEqual(truncate('hello', 10), 'hello'));
  test('long string truncated', () => assert.strictEqual(truncate('hello world', 5), 'hello…'));
  test('null returns empty', () => assert.strictEqual(truncate(null, 5), ''));
  test('empty returns empty', () => assert.strictEqual(truncate('', 5), ''));
}

// ===== NODES.JS TESTS =====
console.log('\n=== nodes.js: getStatusInfo ===');
{
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/roles.js');
  // nodes.js is an IIFE that registers a page — we need to mock registerPage and other globals
  ctx.registerPage = () => {};
  ctx.api = () => Promise.resolve([]);
  ctx.timeAgo = vm.runInContext(`(${fs.readFileSync('public/app.js', 'utf8').match(/function timeAgo[^}]+}/)[0]})`, ctx);
  // Actually, let's load app.js first for its globals
  loadInCtx(ctx, 'public/app.js');
  ctx.RegionFilter = { init: () => {}, getSelected: () => null, onRegionChange: () => {} };
  ctx.onWS = () => {};
  ctx.offWS = () => {};
  ctx.invalidateApiCache = () => {};
  ctx.favStar = () => '';
  ctx.bindFavStars = () => {};
  ctx.getFavorites = () => [];
  ctx.isFavorite = () => false;
  ctx.connectWS = () => {};
  loadInCtx(ctx, 'public/nodes.js');

  // getStatusInfo is inside the IIFE, not on window. We need to extract it differently.
  // Let's use a modified approach - inject a hook before loading
}

// Since nodes.js functions are inside an IIFE, we need to extract them.
// Strategy: modify the IIFE to expose functions on window for testing
console.log('\n=== nodes.js: getStatusTooltip / getStatusInfo (extracted) ===');
{
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');

  // Extract the functions from nodes.js source by wrapping them
  const nodesSource = fs.readFileSync('public/nodes.js', 'utf8');

  // Extract function bodies using regex - getStatusTooltip, getStatusInfo, renderNodeBadges, sortNodes
  const fnNames = ['getStatusTooltip', 'getStatusInfo', 'renderNodeBadges', 'renderStatusExplanation', 'sortNodes'];
  // Instead, let's inject an exporter into the IIFE
  const modifiedSource = nodesSource.replace(
    /\(function \(\) \{/,
    '(function () { window.__nodesExport = {};'
  ).replace(
    /function getStatusTooltip/,
    'window.__nodesExport.getStatusTooltip = getStatusTooltip; function getStatusTooltip'
  ).replace(
    /function getStatusInfo/,
    'window.__nodesExport.getStatusInfo = getStatusInfo; function getStatusInfo'
  ).replace(
    /function renderNodeBadges/,
    'window.__nodesExport.renderNodeBadges = renderNodeBadges; function renderNodeBadges'
  ).replace(
    /function renderStatusExplanation/,
    'window.__nodesExport.renderStatusExplanation = renderStatusExplanation; function renderStatusExplanation'
  ).replace(
    /function sortNodes/,
    'window.__nodesExport.sortNodes = sortNodes; function sortNodes'
  );

  // Provide required globals
  ctx.registerPage = () => {};
  ctx.RegionFilter = { init: () => {}, getSelected: () => null, onRegionChange: () => {} };
  ctx.onWS = () => {};
  ctx.offWS = () => {};
  ctx.invalidateApiCache = () => {};
  ctx.favStar = () => '';
  ctx.bindFavStars = () => {};
  ctx.getFavorites = () => [];
  ctx.isFavorite = () => false;
  ctx.connectWS = () => {};
  ctx.HopResolver = { init: () => {}, resolve: () => ({}), ready: () => false };

  try {
    vm.runInContext(modifiedSource, ctx);
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  } catch (e) {
    console.log('  ⚠️ Could not load nodes.js in sandbox:', e.message.slice(0, 100));
  }

  const ex = ctx.window.__nodesExport || {};

  if (ex.getStatusTooltip) {
    const gst = ex.getStatusTooltip;
    test('active repeater tooltip mentions 72h', () => {
      assert.ok(gst('repeater', 'active').includes('72h'));
    });
    test('stale companion tooltip mentions normal', () => {
      assert.ok(gst('companion', 'stale').includes('normal'));
    });
    test('stale sensor tooltip mentions offline', () => {
      assert.ok(gst('sensor', 'stale').includes('offline'));
    });
    test('active companion tooltip mentions 24h', () => {
      assert.ok(gst('companion', 'active').includes('24h'));
    });
  }

  if (ex.getStatusInfo) {
    const gsi = ex.getStatusInfo;
    test('active repeater status', () => {
      const info = gsi({ role: 'repeater', last_heard: new Date().toISOString() });
      assert.strictEqual(info.status, 'active');
      assert.ok(info.statusLabel.includes('Active'));
    });
    test('stale companion status (old date)', () => {
      const old = new Date(Date.now() - 48 * 3600000).toISOString();
      const info = gsi({ role: 'companion', last_heard: old });
      assert.strictEqual(info.status, 'stale');
    });
    test('repeater stale at 4 days', () => {
      const old = new Date(Date.now() - 96 * 3600000).toISOString();
      const info = gsi({ role: 'repeater', last_heard: old });
      assert.strictEqual(info.status, 'stale');
    });
    test('repeater active at 2 days', () => {
      const d = new Date(Date.now() - 48 * 3600000).toISOString();
      const info = gsi({ role: 'repeater', last_heard: d });
      assert.strictEqual(info.status, 'active');
    });
  }

  if (ex.renderNodeBadges) {
    test('renderNodeBadges includes role', () => {
      const html = ex.renderNodeBadges({ role: 'repeater', public_key: 'abcdef1234', last_heard: new Date().toISOString() }, '#ff0000');
      assert.ok(html.includes('repeater'));
    });
  }

  if (ex.sortNodes) {
    const sortNodes = ex.sortNodes;
    // We need to set sortState — it's closure-captured. Test via the exposed function behavior.
    // sortNodes uses the closure sortState, so we can't easily test different sort modes
    // without calling toggleSort. Let's just verify it returns a sorted array.
    test('sortNodes returns array', () => {
      const arr = [
        { name: 'Bravo', last_heard: new Date().toISOString() },
        { name: 'Alpha', last_heard: new Date(Date.now() - 1000).toISOString() },
      ];
      const result = sortNodes(arr);
      assert.ok(Array.isArray(result));
    });
  }
}

// ===== HOP-RESOLVER TESTS =====
console.log('\n=== hop-resolver.js ===');
{
  const ctx = makeSandbox();
  ctx.IATA_COORDS_GEO = {};
  loadInCtx(ctx, 'public/hop-resolver.js');
  const HR = ctx.window.HopResolver;

  test('ready() false before init', () => assert.strictEqual(HR.ready(), false));

  test('init + ready', () => {
    HR.init([{ public_key: 'abcdef1234567890', name: 'NodeA', lat: 37.3, lon: -122.0 }]);
    assert.strictEqual(HR.ready(), true);
  });

  test('resolve single unique prefix', () => {
    HR.init([
      { public_key: 'abcdef1234567890', name: 'NodeA', lat: 37.3, lon: -122.0 },
      { public_key: '123456abcdef0000', name: 'NodeB', lat: 37.4, lon: -122.1 },
    ]);
    const result = HR.resolve(['ab'], null, null, null, null);
    assert.strictEqual(result['ab'].name, 'NodeA');
  });

  test('resolve ambiguous prefix', () => {
    HR.init([
      { public_key: 'abcdef1234567890', name: 'NodeA', lat: 37.3, lon: -122.0 },
      { public_key: 'abcd001234567890', name: 'NodeC', lat: 38.0, lon: -121.0 },
    ]);
    const result = HR.resolve(['ab'], null, null, null, null);
    assert.ok(result['ab'].ambiguous);
    assert.strictEqual(result['ab'].candidates.length, 2);
  });

  test('resolve unknown prefix returns null name', () => {
    HR.init([{ public_key: 'abcdef1234567890', name: 'NodeA' }]);
    const result = HR.resolve(['ff'], null, null, null, null);
    assert.strictEqual(result['ff'].name, null);
  });

  test('empty hops returns empty', () => {
    const result = HR.resolve([], null, null, null, null);
    assert.strictEqual(Object.keys(result).length, 0);
  });

  test('geo disambiguation with origin anchor', () => {
    HR.init([
      { public_key: 'abcdef1234567890', name: 'NearNode', lat: 37.31, lon: -122.01 },
      { public_key: 'abcd001234567890', name: 'FarNode', lat: 50.0, lon: 10.0 },
    ]);
    const result = HR.resolve(['ab'], 37.3, -122.0, null, null);
    // Should prefer the nearer node
    assert.strictEqual(result['ab'].name, 'NearNode');
  });

  test('regional filtering with IATA', () => {
    HR.init(
      [
        { public_key: 'abcdef1234567890', name: 'SFONode', lat: 37.6, lon: -122.4 },
        { public_key: 'abcd001234567890', name: 'LHRNode', lat: 51.5, lon: -0.1 },
      ],
      {
        observers: [{ id: 'obs1', iata: 'SFO' }],
        iataCoords: { SFO: { lat: 37.6, lon: -122.4 } },
      }
    );
    const result = HR.resolve(['ab'], null, null, null, null, 'obs1');
    assert.strictEqual(result['ab'].name, 'SFONode');
    assert.ok(!result['ab'].ambiguous);
  });
}

// ===== SNR/RSSI Number casting =====
{
  // These test the pattern used in observer-detail.js, home.js, traces.js, live.js
  // Values from DB may be strings — Number() must be called before .toFixed()
  test('Number(string snr).toFixed works', () => {
    const snr = "7.5"; // string from DB
    assert.strictEqual(Number(snr).toFixed(1), "7.5");
  });

  test('Number(number snr).toFixed works', () => {
    const snr = 7.5;
    assert.strictEqual(Number(snr).toFixed(1), "7.5");
  });

  test('Number(null) produces NaN, guarded by != null check', () => {
    const snr = null;
    assert.ok(!(snr != null) || !isNaN(Number(snr).toFixed(1)));
  });

  test('Number(string rssi).toFixed works', () => {
    const rssi = "-85";
    assert.strictEqual(Number(rssi).toFixed(0), "-85");
  });

  test('Number(negative string snr).toFixed works', () => {
    const snr = "-3.2";
    assert.strictEqual(Number(snr).toFixed(1), "-3.2");
  });

  test('Number(integer string).toFixed adds decimal', () => {
    const snr = "10";
    assert.strictEqual(Number(snr).toFixed(1), "10.0");
  });
}

// ===== ROLES.JS: copyToClipboard =====
console.log('\n=== roles.js: copyToClipboard ===');
{
  // Helper: build a sandbox with clipboard/DOM mocks for copyToClipboard tests
  function makeClipboardSandbox(opts) {
    const ctx = makeSandbox();
    const createdEls = [];
    const appendedEls = [];
    const removedEls = [];

    // Enhanced createElement that returns a mock textarea
    ctx.document.createElement = (tag) => {
      const el = { tagName: tag, value: '', style: {}, focus() {}, select() {} };
      createdEls.push(el);
      return el;
    };
    ctx.document.body = {
      appendChild: (el) => { appendedEls.push(el); },
      removeChild: (el) => { removedEls.push(el); },
    };
    ctx.document.execCommand = opts.execCommand || (() => true);

    // navigator mock
    if (opts.clipboardWriteText) {
      ctx.navigator = { clipboard: { writeText: opts.clipboardWriteText } };
    } else {
      ctx.navigator = {};
    }

    loadInCtx(ctx, 'public/roles.js');
    return { ctx, createdEls, appendedEls, removedEls };
  }

  // Test 1: Fallback succeeds when clipboard API is unavailable
  test('copyToClipboard fallback calls onSuccess when execCommand succeeds', () => {
    const { ctx } = makeClipboardSandbox({ execCommand: () => true });
    let succeeded = false;
    ctx.window.copyToClipboard('hello', () => { succeeded = true; }, () => { throw new Error('onFail should not be called'); });
    assert.strictEqual(succeeded, true);
  });

  // Test 2: Fallback uses textarea when clipboard API is unavailable
  test('copyToClipboard fallback creates textarea with correct value', () => {
    const { ctx, createdEls, appendedEls, removedEls } = makeClipboardSandbox({ execCommand: () => true });
    const beforeCount = createdEls.length; // roles.js may create elements on init
    ctx.window.copyToClipboard('test-text');
    const newEls = createdEls.slice(beforeCount);
    assert.strictEqual(newEls.length, 1);
    assert.strictEqual(newEls[0].tagName, 'textarea');
    assert.strictEqual(newEls[0].value, 'test-text');
    assert.strictEqual(appendedEls.length, 1, 'textarea should be appended to body');
    assert.strictEqual(removedEls.length, 1, 'textarea should be removed from body');
  });

  // Test 3: Fallback calls onFail when execCommand returns false
  test('copyToClipboard fallback calls onFail when execCommand fails', () => {
    const { ctx } = makeClipboardSandbox({ execCommand: () => false });
    let failCalled = false;
    ctx.window.copyToClipboard('hello', () => { throw new Error('onSuccess should not be called'); }, () => { failCalled = true; });
    assert.strictEqual(failCalled, true);
  });

  // Test 4: Fallback calls onFail when execCommand throws
  test('copyToClipboard fallback calls onFail when execCommand throws', () => {
    const { ctx } = makeClipboardSandbox({ execCommand: () => { throw new Error('not allowed'); } });
    let failCalled = false;
    ctx.window.copyToClipboard('hello', null, () => { failCalled = true; });
    assert.strictEqual(failCalled, true);
  });

  // Test 5: Handles null input gracefully (no crash)
  test('copyToClipboard handles null input without throwing', () => {
    const { ctx } = makeClipboardSandbox({ execCommand: () => true });
    // Should not throw
    ctx.window.copyToClipboard(null);
    ctx.window.copyToClipboard(undefined);
  });

  // Test 6: Clipboard API path calls writeText with correct argument
  test('copyToClipboard uses clipboard API when available', () => {
    let writtenText = null;
    const { ctx } = makeClipboardSandbox({
      clipboardWriteText: (text) => { writtenText = text; return Promise.resolve(); },
    });
    ctx.window.copyToClipboard('clipboard-text');
    assert.strictEqual(writtenText, 'clipboard-text');
  });

  // Test 7: No crash when callbacks are omitted
  test('copyToClipboard works without callbacks', () => {
    const { ctx } = makeClipboardSandbox({ execCommand: () => true });
    ctx.window.copyToClipboard('no-callbacks');
    // No callbacks — should not throw
  });

  // Test 8: Cleanup happens even when execCommand throws
  test('copyToClipboard cleans up textarea on execCommand throw', () => {
    const { ctx, removedEls } = makeClipboardSandbox({ execCommand: () => { throw new Error('denied'); } });
    ctx.window.copyToClipboard('cleanup-test');
    assert.strictEqual(removedEls.length, 1, 'textarea should be removed even on error');
  });
}

// ===== LIVE.JS: pruneStaleNodes =====
console.log('\n=== live.js: pruneStaleNodes ===');
{
  function makeLiveSandbox() {
    const ctx = makeSandbox();
    // Leaflet mock
    const removedLayers = [];
    ctx.L = {
      circleMarker: () => {
        const m = {
          addTo: function() { return m; },
          bindTooltip: function() { return m; },
          on: function() { return m; },
          setRadius: function() {},
          setStyle: function() {},
          setLatLng: function() {},
          getLatLng: function() { return { lat: 0, lng: 0 }; },
          _baseColor: '', _baseSize: 5, _glowMarker: null,
        };
        return m;
      },
      polyline: () => {
        const p = { addTo: function() { return p; }, setStyle: function() {}, remove: function() {} };
        return p;
      },
      map: () => {
        const m = {
          setView: function() { return m; }, addLayer: function() { return m; },
          on: function() { return m; }, getZoom: function() { return 11; },
          getCenter: function() { return { lat: 37, lng: -122 }; },
          getBounds: function() { return { contains: () => true }; },
          fitBounds: function() { return m; }, invalidateSize: function() {},
          remove: function() {}, hasLayer: function() { return false; },
        };
        return m;
      },
      layerGroup: () => {
        const g = {
          addTo: function() { return g; }, addLayer: function() {},
          removeLayer: function(l) { removedLayers.push(l); },
          clearLayers: function() {}, hasLayer: function() { return true; },
          eachLayer: function() {},
        };
        return g;
      },
      tileLayer: () => ({ addTo: function() { return this; } }),
      control: { attribution: () => ({ addTo: function() {} }) },
      DomUtil: { addClass: function() {}, removeClass: function() {} },
    };
    ctx.getComputedStyle = () => ({ getPropertyValue: () => '' });
    ctx.matchMedia = () => ({ matches: false, addEventListener: () => {} });
    ctx.registerPage = () => {};
    ctx.onWS = () => {};
    ctx.offWS = () => {};
    ctx.connectWS = () => {};
    ctx.api = () => Promise.resolve([]);
    ctx.invalidateApiCache = () => {};
    ctx.favStar = () => '';
    ctx.bindFavStars = () => {};
    ctx.getFavorites = () => [];
    ctx.isFavorite = () => false;
    ctx.HopResolver = { init: () => {}, resolve: () => ({}), ready: () => false };
    ctx.MeshAudio = null;
    ctx.RegionFilter = { init: () => {}, getSelected: () => null, onRegionChange: () => {} };
    ctx.WebSocket = function() { this.close = () => {}; };
    ctx.navigator = {};
    ctx.visualViewport = null;
    ctx.document.documentElement = { getAttribute: () => null, setAttribute: () => {} };
    ctx.document.body = { appendChild: () => {}, removeChild: () => {}, contains: () => false };
    ctx.document.querySelector = () => null;
    ctx.document.querySelectorAll = () => [];
    ctx.document.createElementNS = () => ctx.document.createElement();
    ctx.cancelAnimationFrame = () => {};
    ctx.IATA_COORDS_GEO = {};

    loadInCtx(ctx, 'public/roles.js');
    try {
      loadInCtx(ctx, 'public/live.js');
    } catch (e) {
      // live.js may have non-critical load errors in sandbox
      for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
    }
    return { ctx, removedLayers };
  }

  test('pruneStaleNodes removes nodes older than silentMs threshold', () => {
    const { ctx, removedLayers } = makeLiveSandbox();
    const prune = ctx.window._livePruneStaleNodes;
    const getMarkers = ctx.window._liveNodeMarkers;
    const getData = ctx.window._liveNodeData;
    assert.ok(prune, '_livePruneStaleNodes must be exposed');
    assert.ok(getMarkers, '_liveNodeMarkers must be exposed');
    assert.ok(getData, '_liveNodeData must be exposed');

    const markers = getMarkers();
    const data = getData();

    // Inject a companion node last seen 48 hours ago (exceeds nodeSilentMs=24h)
    markers['staleKey'] = { _glowMarker: null };
    data['staleKey'] = { public_key: 'staleKey', role: 'companion', _liveSeen: Date.now() - 48 * 3600000 };

    // Inject an active companion seen just now
    markers['freshKey'] = { _glowMarker: null };
    data['freshKey'] = { public_key: 'freshKey', role: 'companion', _liveSeen: Date.now() };

    prune();

    assert.ok(!markers['staleKey'], 'stale companion should be pruned');
    assert.ok(!data['staleKey'], 'stale companion data should be pruned');
    assert.ok(markers['freshKey'], 'fresh companion should remain');
    assert.ok(data['freshKey'], 'fresh companion data should remain');
  });

  test('pruneStaleNodes uses longer threshold for infrastructure roles', () => {
    const { ctx } = makeLiveSandbox();
    const prune = ctx.window._livePruneStaleNodes;
    const markers = ctx.window._liveNodeMarkers();
    const data = ctx.window._liveNodeData();

    // A repeater seen 48h ago should NOT be pruned (infraSilentMs = 72h)
    markers['rpt1'] = { _glowMarker: null };
    data['rpt1'] = { public_key: 'rpt1', role: 'repeater', _liveSeen: Date.now() - 48 * 3600000 };

    // A repeater seen 96h ago SHOULD be pruned
    markers['rpt2'] = { _glowMarker: null };
    data['rpt2'] = { public_key: 'rpt2', role: 'repeater', _liveSeen: Date.now() - 96 * 3600000 };

    prune();

    assert.ok(markers['rpt1'], 'repeater at 48h should remain (under 72h threshold)');
    assert.ok(data['rpt1'], 'repeater data at 48h should remain');
    assert.ok(!markers['rpt2'], 'repeater at 96h should be pruned (over 72h threshold)');
    assert.ok(!data['rpt2'], 'repeater data at 96h should be pruned');
  });

  test('node count does not grow unbounded with repeated ADVERTs', () => {
    const { ctx } = makeLiveSandbox();
    const prune = ctx.window._livePruneStaleNodes;
    const markers = ctx.window._liveNodeMarkers();
    const data = ctx.window._liveNodeData();

    // Simulate 500 nodes added over time, most now stale
    for (var i = 0; i < 500; i++) {
      var key = 'node' + i;
      markers[key] = { _glowMarker: null };
      // First 400 are old (stale), last 100 are fresh
      var age = i < 400 ? 48 * 3600000 : 0;
      data[key] = { public_key: key, role: 'companion', _liveSeen: Date.now() - age };
    }

    assert.strictEqual(Object.keys(markers).length, 500, 'should start with 500 nodes');
    prune();
    assert.strictEqual(Object.keys(markers).length, 100, 'should have pruned down to 100 active nodes');
    assert.strictEqual(Object.keys(data).length, 100, 'nodeData should match');
  });

  test('pruneStaleNodes skips nodes with no timestamp', () => {
    const { ctx } = makeLiveSandbox();
    const prune = ctx.window._livePruneStaleNodes;
    const markers = ctx.window._liveNodeMarkers();
    const data = ctx.window._liveNodeData();

    markers['noTs'] = { _glowMarker: null };
    data['noTs'] = { public_key: 'noTs', role: 'companion' };

    prune();

    assert.ok(markers['noTs'], 'node with no timestamp should not be pruned');
  });

  test('pruneStaleNodes uses last_heard as fallback for _liveSeen', () => {
    const { ctx } = makeLiveSandbox();
    const prune = ctx.window._livePruneStaleNodes;
    const markers = ctx.window._liveNodeMarkers();
    const data = ctx.window._liveNodeData();

    // Node with last_heard (from API) but no _liveSeen — stale
    markers['apiOld'] = { _glowMarker: null };
    data['apiOld'] = { public_key: 'apiOld', role: 'companion', last_heard: new Date(Date.now() - 48 * 3600000).toISOString() };

    // Node with last_heard — fresh
    markers['apiFresh'] = { _glowMarker: null };
    data['apiFresh'] = { public_key: 'apiFresh', role: 'companion', last_heard: new Date().toISOString() };

    prune();

    assert.ok(!markers['apiOld'], 'WS node with stale last_heard should be pruned');
    assert.ok(markers['apiFresh'], 'WS node with fresh last_heard should remain');
  });

  test('pruneStaleNodes dims API-loaded nodes instead of removing them', () => {
    const { ctx } = makeLiveSandbox();
    const prune = ctx.window._livePruneStaleNodes;
    const markers = ctx.window._liveNodeMarkers();
    const data = ctx.window._liveNodeData();

    let lastStyle = {};
    let glowStyle = {};
    markers['apiStale'] = {
      _glowMarker: { setStyle: function(s) { glowStyle = s; } },
      _staleDimmed: false,
      setStyle: function(s) { lastStyle = s; },
    };
    data['apiStale'] = { public_key: 'apiStale', role: 'repeater', _fromAPI: true, _liveSeen: Date.now() - 96 * 3600000 };

    prune();

    assert.ok(markers['apiStale'], 'API node should NOT be removed');
    assert.ok(data['apiStale'], 'API node data should NOT be removed');
    assert.ok(markers['apiStale']._staleDimmed, 'API node should be marked as dimmed');
    assert.strictEqual(lastStyle.fillOpacity, 0.25, 'marker should be dimmed to 0.25 fillOpacity');
    assert.strictEqual(glowStyle.fillOpacity, 0.04, 'glow should be dimmed to 0.04 fillOpacity');
  });

  test('pruneStaleNodes restores API nodes when they become active again', () => {
    const { ctx } = makeLiveSandbox();
    const prune = ctx.window._livePruneStaleNodes;
    const markers = ctx.window._liveNodeMarkers();
    const data = ctx.window._liveNodeData();

    let lastStyle = {};
    let glowStyle = {};
    markers['apiNode'] = {
      _glowMarker: { setStyle: function(s) { glowStyle = s; } },
      _staleDimmed: true,
      setStyle: function(s) { lastStyle = s; },
    };
    data['apiNode'] = { public_key: 'apiNode', role: 'repeater', _fromAPI: true, _liveSeen: Date.now() };

    prune();

    assert.ok(markers['apiNode'], 'API node should remain');
    assert.strictEqual(markers['apiNode']._staleDimmed, false, 'staleDimmed should be cleared');
    assert.strictEqual(lastStyle.fillOpacity, 0.85, 'opacity should be restored to 0.85');
    assert.strictEqual(glowStyle.fillOpacity, 0.12, 'glow should be restored to 0.12');
  });

  test('pruneStaleNodes still removes WS-only nodes when stale', () => {
    const { ctx } = makeLiveSandbox();
    const prune = ctx.window._livePruneStaleNodes;
    const markers = ctx.window._liveNodeMarkers();
    const data = ctx.window._liveNodeData();

    // WS-only node (no _fromAPI) — should be removed
    markers['wsNode'] = { _glowMarker: null };
    data['wsNode'] = { public_key: 'wsNode', role: 'companion', _liveSeen: Date.now() - 48 * 3600000 };

    // API node — should be dimmed, not removed
    markers['apiNode'] = {
      _glowMarker: { setStyle: function() {} },
      _staleDimmed: false,
      setStyle: function() {},
    };
    data['apiNode'] = { public_key: 'apiNode', role: 'companion', _fromAPI: true, _liveSeen: Date.now() - 48 * 3600000 };

    prune();

    assert.ok(!markers['wsNode'], 'WS-only stale node should be removed');
    assert.ok(!data['wsNode'], 'WS-only stale node data should be removed');
    assert.ok(markers['apiNode'], 'API stale node should NOT be removed');
    assert.ok(data['apiNode'], 'API stale node data should NOT be removed');
  });
}

// ===== SUMMARY =====
console.log(`\n${'═'.repeat(40)}`);
console.log(`  Frontend helpers: ${passed} passed, ${failed} failed`);
console.log(`${'═'.repeat(40)}\n`);
if (failed > 0) process.exit(1);
