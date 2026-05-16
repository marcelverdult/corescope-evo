/* === CoreScope — observers.js === */
'use strict';

(function () {
  let observers = [];
  let obsSkewMap = {}; // observerID → {offsetSec, samples}
  let wsHandler = null;
  let refreshTimer = null;
  let regionChangeHandler = null;
  let sortState = { col: null, dir: 'asc' };

  var STATS_OPEN_KEY = 'meshcore-obs-stats-open';

  function loadSortState() {
    try {
      var s = localStorage.getItem('meshcore-obs-sort');
      if (s) sortState = JSON.parse(s);
    } catch (e) {}
  }

  function saveSortState() {
    try { localStorage.setItem('meshcore-obs-sort', JSON.stringify(sortState)); } catch (e) {}
  }

  function applySortState(arr) {
    if (!sortState.col) return arr;
    return arr.slice().sort(function (a, b) {
      var va, vb;
      switch (sortState.col) {
        case 'status': {
          var order = { 'health-green': 0, 'health-yellow': 1, 'health-red': 2 };
          va = order[healthStatus(a.last_seen).cls] ?? 3;
          vb = order[healthStatus(b.last_seen).cls] ?? 3;
          break;
        }
        case 'name':
          va = (a.name || a.id || '').toLowerCase();
          vb = (b.name || b.id || '').toLowerCase();
          break;
        case 'region':
          va = (a.iata || '').toLowerCase();
          vb = (b.iata || '').toLowerCase();
          break;
        case 'last_seen':
          va = a.last_seen ? new Date(a.last_seen).getTime() : 0;
          vb = b.last_seen ? new Date(b.last_seen).getTime() : 0;
          break;
        case 'last_packet':
          va = a.last_packet_at ? new Date(a.last_packet_at).getTime() : 0;
          vb = b.last_packet_at ? new Date(b.last_packet_at).getTime() : 0;
          break;
        case 'packets':
          va = a.packet_count || 0;
          vb = b.packet_count || 0;
          break;
        case 'packets_hr':
          va = a.packetsLastHour || 0;
          vb = b.packetsLastHour || 0;
          break;
        case 'uptime':
          va = a.first_seen ? Date.now() - new Date(a.first_seen).getTime() : 0;
          vb = b.first_seen ? Date.now() - new Date(b.first_seen).getTime() : 0;
          break;
        default:
          return 0;
      }
      var cmp = va < vb ? -1 : va > vb ? 1 : 0;
      return sortState.dir === 'asc' ? cmp : -cmp;
    });
  }

  function init(app) {
    loadSortState();
    app.innerHTML = `
      <div class="observers-page">
        <div class="page-header">
          <h2>Observer Status</h2>
          <a href="#/compare" class="btn-icon" title="Compare observers" aria-label="Compare observers" style="text-decoration:none">🔍</a>
          <button class="btn-icon" data-action="obs-refresh" title="Refresh" aria-label="Refresh observers">🔄</button>
        </div>
        <div class="obs-stats-panel" id="obsStatsPanel">
          <div class="obs-stats-header" data-action="toggle-stats">
            <strong>📊 Observer Statistics</strong>
            <span class="obs-stats-toggle">▶</span>
          </div>
          <div class="obs-stats-body" id="obsStatsBody" style="max-height:0px">
            <div class="obs-stats-grid" id="obsStatsGrid"></div>
          </div>
        </div>
        <div id="obsRegionFilter" class="region-filter-container"></div>
        <div id="obsContent"><div class="text-center text-muted" style="padding:40px">Loading…</div></div>
      </div>`;
    RegionFilter.init(document.getElementById('obsRegionFilter'));
    regionChangeHandler = RegionFilter.onChange(function () { render(); });
    // Restore stats panel open/close state
    try {
      if (localStorage.getItem(STATS_OPEN_KEY) === '1') {
        var body = document.getElementById('obsStatsBody');
        var tog = app.querySelector('.obs-stats-toggle');
        if (body) body.style.maxHeight = '2000px';
        if (tog) tog.textContent = '▼';
      }
    } catch (e) {}
    loadObservers();
    // Event delegation for data-action buttons
    app.addEventListener('click', function (e) {
      var th = e.target.closest('th[data-sort-col]');
      if (th) {
        var col = th.dataset.sortCol;
        sortState.dir = sortState.col === col && sortState.dir === 'asc' ? 'desc' : 'asc';
        sortState.col = col;
        saveSortState();
        render();
        return;
      }
      var btn = e.target.closest('[data-action]');
      if (btn && btn.dataset.action === 'obs-refresh') loadObservers();
      if (btn && btn.dataset.action === 'toggle-stats') {
        var statsBody = document.getElementById('obsStatsBody');
        var statsTog = btn.querySelector('.obs-stats-toggle');
        var isOpen = statsBody.style.maxHeight !== '0px';
        statsBody.style.maxHeight = isOpen ? '0px' : '2000px';
        statsTog.textContent = isOpen ? '▶' : '▼';
        try { localStorage.setItem(STATS_OPEN_KEY, isOpen ? '0' : '1'); } catch (e) {}
      }
      var row = e.target.closest('tr[data-action="navigate"]');
      if (row) {
        // #1056 AC#4: at narrow widths, open detail in slide-over instead of
        // navigating to a separate page.
        if (window.SlideOver && window.SlideOver.shouldUse()) {
          e.preventDefault();
          openObserverSlideOver(row.dataset.value);
          return;
        }
        location.hash = row.dataset.value;
      }
    });
    // #209 — Keyboard accessibility for observer rows
    app.addEventListener('keydown', function (e) {
      var row = e.target.closest('tr[data-action="navigate"]');
      if (!row) return;
      if (e.key !== 'Enter' && e.key !== ' ') return;
      e.preventDefault();
      if (window.SlideOver && window.SlideOver.shouldUse()) {
        openObserverSlideOver(row.dataset.value);
        return;
      }
      location.hash = row.dataset.value;
    });
    // Auto-refresh every 30s
    refreshTimer = setInterval(loadObservers, 30000);
    wsHandler = debouncedOnWS(function (msgs) {
      if (msgs.some(function (m) { return m.type === 'packet'; })) loadObservers();
    });
  }

  function destroy() {
    if (wsHandler) offWS(wsHandler);
    wsHandler = null;
    if (refreshTimer) clearInterval(refreshTimer);
    refreshTimer = null;
    if (regionChangeHandler) RegionFilter.offChange(regionChangeHandler);
    regionChangeHandler = null;
    observers = [];
    obsSkewMap = {};
  }

  async function loadObservers() {
    try {
      const [data, skewData] = await Promise.all([
        api('/observers', { ttl: CLIENT_TTL.observers }),
        api('/observers/clock-skew', { ttl: 30000 }).catch(function() { return []; })
      ]);
      observers = data.observers || [];
      obsSkewMap = {};
      (Array.isArray(skewData) ? skewData : []).forEach(function(s) {
        if (s && s.observerID) obsSkewMap[s.observerID] = s;
      });
      render();
    } catch (e) {
      document.getElementById('obsContent').innerHTML =
        `<div class="text-muted" role="alert" aria-live="polite" style="padding:40px">Error loading observers: ${e.message}</div>`;
    }
  }

  // NOTE: Comparing server timestamps to Date.now() can skew if client/server
  // clocks differ. We add ±30s tolerance to thresholds to reduce false positives.
  function healthStatus(lastSeen) {
    if (!lastSeen) return { cls: 'health-red', label: 'Unknown' };
    const ago = Date.now() - new Date(lastSeen).getTime();
    const tolerance = 30000; // 30s tolerance for clock skew
    if (ago < 600000 + tolerance) return { cls: 'health-green', label: 'Online' };    // < 10 min + tolerance
    if (ago < 3600000 + tolerance) return { cls: 'health-yellow', label: 'Stale' };   // < 1 hour + tolerance
    return { cls: 'health-red', label: 'Offline' };
  }

  function packetBadge(o) {
    if (!o.last_packet_at) return '<span title="No packets ever observed">📡⚠ never</span>';
    const pktAgo = Date.now() - new Date(o.last_packet_at).getTime();
    const statusAgo = o.last_seen ? Date.now() - new Date(o.last_seen).getTime() : Infinity;
    const gap = pktAgo - statusAgo;
    if (gap > 600000) {
      return `<span title="Last packet ${timeAgo(o.last_packet_at)} — status is newer by ${Math.round(gap/60000)}min. Observer may be alive but not forwarding packets.">📡⚠ ${timeAgo(o.last_packet_at)}</span>`;
    }
    return timeAgo(o.last_packet_at);
  }

  function uptimeStr(firstSeen) {
    if (!firstSeen) return '—';
    const ms = Date.now() - new Date(firstSeen).getTime();
    const d = Math.floor(ms / 86400000);
    const h = Math.floor((ms % 86400000) / 3600000);
    if (d > 0) return `${d}d ${h}h`;
    const m = Math.floor((ms % 3600000) / 60000);
    return h > 0 ? `${h}h ${m}m` : `${m}m`;
  }

  function sparkBar(count, max) {
    if (max === 0) return `<span class="text-muted">0/hr</span>`;
    const pct = Math.min(100, Math.round((count / max) * 100));
    return `<span style="display:inline-flex;align-items:center;gap:6px;white-space:nowrap"><span style="display:inline-block;width:60px;height:12px;background:var(--border);border-radius:3px;overflow:hidden;vertical-align:middle"><span style="display:block;height:100%;width:${pct}%;background:linear-gradient(90deg,#3b82f6,#60a5fa);border-radius:3px"></span></span><span style="font-size:11px">${count}/hr</span></span>`;
  }

  function renderStatsGrid(data) {
    var grid = document.getElementById('obsStatsGrid');
    if (!grid) return;

    function statBlock(title, items) {
      var rows = items.length
        ? items.map(function (item, i) {
            return `<li><span class="obs-stat-rank">${i + 1}</span><span class="obs-stat-name" title="${item.title || item.name}">${item.name}</span><span class="obs-stat-val">${item.val}</span></li>`;
          }).join('')
        : '<li><span class="text-muted" style="font-size:11px">No data</span></li>';
      return `<div class="obs-stat-block"><div class="obs-stat-block-title">${title}</div><ol class="obs-stat-list">${rows}</ol></div>`;
    }

    var byPackets = data.slice().sort(function (a, b) { return (b.packet_count || 0) - (a.packet_count || 0); }).slice(0, 5)
      .map(function (o) { return { name: o.name || o.id, val: (o.packet_count || 0).toLocaleString() }; });

    var byPktsHr = data.slice().sort(function (a, b) { return (b.packetsLastHour || 0) - (a.packetsLastHour || 0); }).slice(0, 5)
      .map(function (o) { return { name: o.name || o.id, val: (o.packetsLastHour || 0) + '/hr' }; });

    var byUptime = data.slice().sort(function (a, b) {
      var ua = a.first_seen ? Date.now() - new Date(a.first_seen).getTime() : 0;
      var ub = b.first_seen ? Date.now() - new Date(b.first_seen).getTime() : 0;
      return ub - ua;
    }).slice(0, 5).map(function (o) { return { name: o.name || o.id, val: uptimeStr(o.first_seen) }; });

    var regionMap = {};
    data.forEach(function (o) { if (o.iata) regionMap[o.iata] = (regionMap[o.iata] || 0) + 1; });
    var byRegion = Object.entries(regionMap).sort(function (a, b) { return b[1] - a[1]; }).slice(0, 5)
      .map(function (entry) { return { name: `<span class="badge-region">${entry[0]}</span>`, title: entry[0], val: entry[1] + (entry[1] === 1 ? ' observer' : ' observers') }; });

    grid.innerHTML =
      statBlock('Top 5 · Total Packets', byPackets) +
      statBlock('Top 5 · Packets / Hour', byPktsHr) +
      statBlock('Top 5 · Uptime', byUptime) +
      statBlock('Top Regions', byRegion);
  }

  function render() {
    const el = document.getElementById('obsContent');
    if (!el) return;

    // Apply region filter
    const selectedRegions = RegionFilter.getSelected();
    const filtered = selectedRegions
      ? observers.filter(o => o.iata && selectedRegions.includes(o.iata))
      : observers;

    renderStatsGrid(filtered);

    if (filtered.length === 0) {
      el.innerHTML = '<div class="text-center text-muted" style="padding:40px">No observers found.</div>';
      return;
    }

    const maxPktsHr = Math.max(1, ...filtered.map(o => o.packetsLastHour || 0));

    // Summary counts
    const online = filtered.filter(o => healthStatus(o.last_seen).cls === 'health-green').length;
    const stale = filtered.filter(o => healthStatus(o.last_seen).cls === 'health-yellow').length;
    const offline = filtered.filter(o => healthStatus(o.last_seen).cls === 'health-red').length;

    const sorted = applySortState(filtered);

    function sortTh(label, col, prio) {
      var active = sortState.col === col;
      var arrow = active ? (sortState.dir === 'asc' ? '▲' : '▼') : '⇅';
      return `<th scope="col" data-priority="${prio}" class="sortable-col${active ? ' sort-active' : ''}" data-sort-col="${col}">${label}<span class="sort-arrow">${arrow}</span></th>`;
    }

    el.innerHTML = `
      <div class="obs-summary">
        <span class="obs-stat"><span class="health-dot health-green">●</span> ${online} Online</span>
        <span class="obs-stat"><span class="health-dot health-yellow">▲</span> ${stale} Stale</span>
        <span class="obs-stat"><span class="health-dot health-red">✕</span> ${offline} Offline</span>
        <span class="obs-stat">📡 ${filtered.length} Total</span>
      </div>
      <div class="obs-table-scroll table-fluid-wrap"><table class="data-table obs-table" id="obsTable">
        <caption class="sr-only">Observer status and statistics</caption>
        <thead><tr>
          ${sortTh('Status','status',1)}${sortTh('Name','name',1)}${sortTh('Region','region',3)}${sortTh('Last Status','last_seen',2)}${sortTh('Last Packet','last_packet',2)}
          <th scope="col" data-priority="3">Packet Health</th>${sortTh('Total Packets','packets',4)}${sortTh('Packets/Hour','packets_hr',3)}<th scope="col" data-priority="4">Clock Offset</th>${sortTh('Uptime','uptime',4)}
        </tr></thead>
        <tbody>${sorted.map(o => {
          const h = healthStatus(o.last_seen);
          const shape = h.cls === 'health-green' ? '●' : h.cls === 'health-yellow' ? '▲' : '✕';
          return `<tr style="cursor:pointer" tabindex="0" role="row" data-action="navigate" data-value="#/observers/${encodeURIComponent(o.id)}" onclick="location.hash='#/observers/${encodeURIComponent(o.id)}'">
            <td><span class="health-dot ${h.cls}" title="${h.label}">${shape}</span> ${h.label}</td>
            <td class="mono">${o.name || o.id}</td>
            <td>${o.iata ? `<span class="badge-region">${o.iata}</span>` : '—'}</td>
            <td>${timeAgo(o.last_seen)}</td>
            <td>${o.last_packet_at ? timeAgo(o.last_packet_at) : '<span class="text-muted">—</span>'}</td>
            <td>${packetBadge(o)}</td>
            <td>${(o.packet_count || 0).toLocaleString()}</td>
            <td>${sparkBar(o.packetsLastHour || 0, maxPktsHr)}</td>
            <td>${(function() {
              var sk = obsSkewMap[o.id];
              if (!sk || sk.samples == null || sk.samples === 0) return '<span class="text-muted">—</span>';
              var sev = observerSkewSeverity(sk.offsetSec);
              return renderSkewBadge(sev, sk.offsetSec) + ' <span class="text-muted" title="Computed from ' + sk.samples + ' multi-observer packets. Positive = observer ahead of consensus.">(' + sk.samples + ')</span>';
            })()}</td>
            <td>${uptimeStr(o.first_seen)}</td>
          </tr>`;
        }).join('')}</tbody>
      </table></div>`;
    makeColumnsResizable('#obsTable', 'meshcore-obs-col-widths');
    // #1056: fluid columns + +N hidden pill
    if (window.TableResponsive) {
      var _obsTbl = document.getElementById('obsTable');
      if (_obsTbl) window.TableResponsive.register(_obsTbl);
    }
  }


  registerPage('observers', { init, destroy });

  // #1056 AC#4: row-detail slide-over (narrow viewports). Renders a compact
  // summary from the in-memory observer + a link to the full page.
  function openObserverSlideOver(hashHref) {
    if (!window.SlideOver) return;
    var m = String(hashHref || '').match(/#\/observers\/(.+)$/);
    if (!m) return;
    var id = decodeURIComponent(m[1]);
    var o = (observers || []).find(function (x) { return String(x.id) === id; });
    if (!o) return;
    var h = healthStatus(o.last_seen);
    var sk = obsSkewMap[o.id];
    var skewLine = (sk && sk.samples) ? renderSkewBadge(observerSkewSeverity(sk.offsetSec), sk.offsetSec) + ' (' + sk.samples + ' samples)' : '—';
    var pkts = sparkBar(o.packetsLastHour || 0, Math.max(1, o.packetsLastHour || 1));
    var content = window.SlideOver.open({ title: o.name || o.id });
    content.innerHTML =
      '<dl class="slide-over-dl" style="margin:0;display:grid;grid-template-columns:auto 1fr;gap:6px 12px;font-size:13px">' +
        '<dt>Status</dt><dd><span class="health-dot ' + h.cls + '">●</span> ' + h.label + '</dd>' +
        '<dt>Region</dt><dd>' + (o.iata ? '<span class="badge-region">' + o.iata + '</span>' : '—') + '</dd>' +
        '<dt>Last status</dt><dd>' + timeAgo(o.last_seen) + '</dd>' +
        '<dt>Last packet</dt><dd>' + (o.last_packet_at ? timeAgo(o.last_packet_at) : '—') + '</dd>' +
        '<dt>Total packets</dt><dd>' + (o.packet_count || 0).toLocaleString() + '</dd>' +
        '<dt>Packets/hr</dt><dd>' + pkts + '</dd>' +
        '<dt>Clock offset</dt><dd>' + skewLine + '</dd>' +
        '<dt>Uptime</dt><dd>' + uptimeStr(o.first_seen) + '</dd>' +
      '</dl>' +
      '<p style="margin-top:14px"><a class="btn-primary" href="' + hashHref + '">Open full detail →</a></p>';
  }
})();
