/* === CoreScope — observer-detail.js === */
'use strict';
(function () {
  // PAYLOAD_LABELS / CHART_COLORS are shared — see chart-constants.js (loaded first).
  const PAYLOAD_LABELS = window.PAYLOAD_LABELS;
  const CHART_COLORS = window.CHART_COLORS;

  let charts = [];
  let currentDays = 7;
  let currentId = null;
  let boundApp = null;
  let appKeydownHandler = null;

  function destroyCharts() {
    charts.forEach(c => { try { c.destroy(); } catch {} });
    charts = [];
  }

  function chartDefaults() {
    const style = getComputedStyle(document.documentElement);
    Chart.defaults.color = style.getPropertyValue('--text-muted').trim() || '#6b7280';
    Chart.defaults.borderColor = style.getPropertyValue('--border').trim() || '#e2e5ea';
  }

  function formatDuration(secs) {
    if (!secs) return '—';
    const d = Math.floor(secs / 86400);
    const h = Math.floor((secs % 86400) / 3600);
    const m = Math.floor((secs % 3600) / 60);
    if (d > 0) return d + 'd ' + h + 'h';
    if (h > 0) return h + 'h ' + m + 'm';
    return m + 'm';
  }

  function init(app, routeParam) {
    currentId = routeParam;
    if (!currentId) {
      app.innerHTML = PageState.empty({ title: 'No observer ID specified' });
      return;
    }

    app.innerHTML = `
      <div class="observer-detail-page" style="padding:16px">
        <div class="page-header" style="display:flex;align-items:center;gap:12px;margin-bottom:16px">
          <a href="#/observers" class="btn-icon" title="Back to Observers" aria-label="Back">←</a>
          <h2 style="margin:0" id="obsTitle">Observer Detail</h2>
          <div style="margin-left:auto;display:flex;gap:8px">
            <select id="obsDaysSelect" class="time-range-select" aria-label="Time range">
              <option value="1">24 Hours</option>
              <option value="3">3 Days</option>
              <option value="7" selected>7 Days</option>
              <option value="30">30 Days</option>
            </select>
          </div>
        </div>
        <div id="obsDetailContent">${PageState.loading('Loading observer…')}</div>
      </div>`;

    document.getElementById('obsDaysSelect').addEventListener('change', function (e) {
      currentDays = parseInt(e.target.value);
      loadDetail();
    });

    // #209 — Keyboard accessibility for recent packet rows. Bound ONCE here
    // via delegation on the persistent #app element; previously renderRecentPackets()
    // re-added a listener to #obsRecentPackets on every loadDetail() (leak fix).
    if (boundApp && appKeydownHandler) boundApp.removeEventListener('keydown', appKeydownHandler);
    appKeydownHandler = function (e) {
      var row = e.target.closest('tr[data-action="navigate"]');
      if (!row) return;
      if (e.key !== 'Enter' && e.key !== ' ') return;
      e.preventDefault();
      location.hash = row.dataset.value;
    };
    boundApp = app;
    app.addEventListener('keydown', appKeydownHandler);

    loadDetail();
  }

  function destroy() {
    destroyCharts();
    if (boundApp && appKeydownHandler) boundApp.removeEventListener('keydown', appKeydownHandler);
    boundApp = null;
    appKeydownHandler = null;
    currentId = null;
  }

  async function loadDetail() {
    try {
      destroyCharts();
      chartDefaults();
      // Telemetry metrics endpoint takes a `since` timestamp + resolution
      // rather than the analytics `days` param. Coarsen resolution as the
      // window grows so long ranges return a manageable number of points.
      const since = new Date(Date.now() - currentDays * 86400000).toISOString();
      const metricsRes = currentDays <= 1 ? '5m' : currentDays <= 7 ? '1h' : '1d';
      const [obs, analytics, obsSkewArr, metrics] = await Promise.all([
        api('/observers/' + encodeURIComponent(currentId)),
        api('/observers/' + encodeURIComponent(currentId) + '/analytics?days=' + currentDays),
        api('/observers/clock-skew', { ttl: 30000 }).catch(function() { return []; }),
        api('/observers/' + encodeURIComponent(currentId) + '/metrics?since=' + encodeURIComponent(since) + '&resolution=' + metricsRes).catch(function() { return null; }),
      ]);
      // Find this observer's calibration data.
      var obsSkew = null;
      (Array.isArray(obsSkewArr) ? obsSkewArr : []).forEach(function(s) {
        if (s && s.observerID === currentId) obsSkew = s;
      });
      renderDetail(obs, analytics, obsSkew, metrics);
    } catch (e) {
      PageState.error(document.getElementById('obsDetailContent'), e, loadDetail);
    }
  }

  function renderDetail(obs, analytics, obsSkew, metrics) {
    const el = document.getElementById('obsDetailContent');
    if (!el) return;

    // Pre-compute telemetry data sets so chart cards can be conditionally
    // rendered in the template — observers without metrics (new, or fields
    // not yet reported) show a clean page with no blank card boxes.
    var mSamples = metrics && metrics.metrics ? metrics.metrics : [];
    var uptimePoints     = mSamples.filter(function(m) { return m.uptime_secs != null; });
    var batteryPoints    = mSamples.filter(function(m) { return m.battery_mv != null; });
    var noiseFloorPoints = mSamples.filter(function(m) { return m.noise_floor != null; });
    var rssiPoints       = (analytics.rssiTimeline && analytics.rssiTimeline.length > 0) ? analytics.rssiTimeline : [];
    var airtimePoints    = mSamples.filter(function(m) { return m.tx_airtime_pct != null || m.rx_airtime_pct != null; });
    var recvErrorPoints  = mSamples.filter(function(m) { return m.recv_errors != null; });
    var queueLenPoints   = mSamples.filter(function(m) { return m.queue_len != null; });

    const title = document.getElementById('obsTitle');
    if (title) title.textContent = obs.name || obs.id.substring(0, 16) + '…';

    // Parse radio string
    let radioHtml = '—';
    if (obs.radio) {
      const rp = obs.radio.split(',');
      radioHtml = rp[0] + ' MHz · SF' + (rp[2] || '?') + ' · BW' + (rp[1] || '?') + ' · CR' + (rp[3] || '?');
    }

    // Health status
    const ago = obs.last_seen ? Date.now() - new Date(obs.last_seen).getTime() : Infinity;
    const statusCls = ago < 600000 ? 'health-green' : ago < HEALTH_THRESHOLDS.nodeDegradedMs ? 'health-yellow' : 'health-red';
    const statusLabel = ago < 600000 ? 'Online' : ago < HEALTH_THRESHOLDS.nodeDegradedMs ? 'Stale' : 'Offline';

    el.innerHTML = `
      <div class="obs-info-grid" style="display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:12px;margin-bottom:20px">
        <div class="stat-card">
          <div class="stat-label">Status</div>
          <div class="stat-value"><span class="health-dot ${statusCls}">●</span> ${statusLabel}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Region</div>
          <div class="stat-value">${obs.iata ? '<span class="badge-region">' + obs.iata + '</span>' : '—'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Model</div>
          <div class="stat-value">${obs.model || '—'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Firmware</div>
          <div class="stat-value" style="font-size:0.8em;word-break:break-all">${obs.firmware || '—'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Client</div>
          <div class="stat-value" style="font-size:0.8em;word-break:break-all">${obs.client_version || '—'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Radio</div>
          <div class="stat-value" style="font-size:0.85em">${radioHtml}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Battery</div>
          <div class="stat-value">${obs.battery_mv ? obs.battery_mv + ' mV' : '—'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Uptime</div>
          <div class="stat-value">${formatDuration(obs.uptime_secs)}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Noise Floor</div>
          <div class="stat-value">${obs.noise_floor != null ? obs.noise_floor + ' dBm' : '—'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Total Packets</div>
          <div class="stat-value">${(obs.packet_count || 0).toLocaleString()}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Packets/Hour</div>
          <div class="stat-value">${(obs.packetsLastHour || 0).toLocaleString()}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">First Seen</div>
          <div class="stat-value" style="font-size:0.85em">${obs.first_seen ? new Date(obs.first_seen).toLocaleDateString() : '—'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Last Status Update</div>
          <div class="stat-value" style="font-size:0.85em">${obs.last_seen ? timeAgo(obs.last_seen) + '<br><span style="font-size:0.8em;color:var(--text-muted)">' + new Date(obs.last_seen).toLocaleString() + '</span>' : '—'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Last Packet Observation</div>
          <div class="stat-value" style="font-size:0.85em">${obs.last_packet_at ? timeAgo(obs.last_packet_at) + '<br><span style="font-size:0.8em;color:var(--text-muted)">' + new Date(obs.last_packet_at).toLocaleString() + '</span>' : '<span style="color:var(--text-muted)">never</span>'}</div>
        </div>
      </div>
      <div class="mono" style="font-size:0.75em;color:var(--text-muted);margin-bottom:20px;word-break:break-all">
        ID: ${obs.id}
      </div>
      ${obsSkew && obsSkew.samples > 0 ? `
      <div class="node-full-card skew-detail-section" style="margin-bottom:20px;padding:12px">
        <h4 style="margin:0 0 6px">⏰ Clock Offset</h4>
        <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
          <span style="font-size:18px;font-weight:700;font-family:var(--mono)">${formatSkew(obsSkew.offsetSec)}</span>
          ${renderSkewBadge(observerSkewSeverity(obsSkew.offsetSec), obsSkew.offsetSec)}
          <span class="text-muted" style="font-size:12px">${obsSkew.samples} sample${obsSkew.samples !== 1 ? 's' : ''}</span>
        </div>
        <div style="font-size:12px;color:var(--text-muted);margin-top:8px;max-width:600px">
          <strong>How this is computed:</strong> when this observer and another observer see the same packet, we compare their receive timestamps. The median deviation across all multi-observer packets is this observer's offset.
        </div>
      </div>` : ''}
      <div class="obs-charts" style="display:grid;grid-template-columns:repeat(auto-fit,minmax(400px,1fr));gap:16px">
        <div class="chart-card" style="padding:12px">
          <h3 style="margin:0 0 8px;font-size:0.95em">Packets Over Time</h3>
          <canvas id="obsTimeChart" role="img" aria-label="Packets over time chart"></canvas>
        </div>
        <div class="chart-card" style="padding:12px">
          <h3 style="margin:0 0 8px;font-size:0.95em">Packet Types</h3>
          <div style="max-width:280px;margin:0 auto"><canvas id="obsTypeChart" role="img" aria-label="Packet types chart"></canvas></div>
        </div>
        <div class="chart-card" style="padding:12px">
          <h3 style="margin:0 0 8px;font-size:0.95em">Unique Nodes Heard</h3>
          <canvas id="obsNodesChart" role="img" aria-label="Unique nodes heard chart"></canvas>
        </div>
        <div class="chart-card" style="padding:12px">
          <h3 style="margin:0 0 8px;font-size:0.95em">SNR Distribution</h3>
          <canvas id="obsSnrChart" role="img" aria-label="SNR distribution chart"></canvas>
        </div>
        ${uptimePoints.length > 0 ? `<div class="chart-card" style="padding:12px"><h3 style="margin:0 0 8px;font-size:0.95em">Uptime</h3><canvas id="obsUptimeChart" role="img" aria-label="Uptime chart"></canvas></div>` : ''}
        ${batteryPoints.length > 0 ? `<div class="chart-card" style="padding:12px"><h3 style="margin:0 0 8px;font-size:0.95em">Battery</h3><canvas id="obsBatteryChart" role="img" aria-label="Battery voltage chart"></canvas></div>` : ''}
        ${noiseFloorPoints.length > 0 ? `<div class="chart-card" style="padding:12px"><h3 style="margin:0 0 8px;font-size:0.95em">Noise Floor</h3><canvas id="obsNoiseFloorChart" role="img" aria-label="Noise floor chart"></canvas></div>` : ''}
        ${rssiPoints.length > 0 ? `<div class="chart-card" style="padding:12px"><h3 style="margin:0 0 8px;font-size:0.95em">RSSI (avg per period)</h3><canvas id="obsRssiChart" role="img" aria-label="RSSI chart"></canvas></div>` : ''}
        ${airtimePoints.length > 0 ? `<div class="chart-card" style="padding:12px"><h3 style="margin:0 0 8px;font-size:0.95em" id="obsAirtimeTitle">Airtime Utilization (%)</h3><canvas id="obsAirtimeChart" role="img" aria-label="Airtime utilization chart"></canvas></div>` : ''}
        ${recvErrorPoints.length > 0 ? `<div class="chart-card" style="padding:12px"><h3 style="margin:0 0 8px;font-size:0.95em">Receive Errors (per interval)</h3><canvas id="obsRecvErrorsChart" role="img" aria-label="Receive errors chart"></canvas></div>` : ''}
        ${queueLenPoints.length > 0 ? `<div class="chart-card" style="padding:12px"><h3 style="margin:0 0 8px;font-size:0.95em">TX Queue Length</h3><canvas id="obsQueueLenChart" role="img" aria-label="TX queue length chart"></canvas></div>` : ''}
      </div>
      <div style="margin-top:20px">
        <h3 style="font-size:0.95em">Recent Packets</h3>
        <div id="obsRecentPackets">${PageState.loading('Loading recent packets…')}</div>
      </div>`;

    // Render charts
    if (analytics.timeline && analytics.timeline.length > 0) {
      renderTimelineChart(analytics.timeline);
    }
    if (analytics.packetTypes) {
      renderTypeChart(analytics.packetTypes);
    }
    if (analytics.nodesTimeline && analytics.nodesTimeline.length > 0) {
      renderNodesChart(analytics.nodesTimeline);
    }
    if (analytics.snrDistribution && analytics.snrDistribution.length > 0) {
      renderSnrChart(analytics.snrDistribution);
    }
    if (uptimePoints.length > 0) {
      renderUptimeChart(uptimePoints);
    }
    if (batteryPoints.length > 0) {
      renderBatteryChart(batteryPoints);
    }
    if (noiseFloorPoints.length > 0) {
      renderNoiseFloorChart(noiseFloorPoints);
    }
    if (rssiPoints.length > 0) {
      renderRssiChart(rssiPoints);
    }
    if (airtimePoints.length > 0) {
      renderAirtimeChart(airtimePoints);
    }
    if (recvErrorPoints.length > 0) {
      renderRecvErrorsChart(recvErrorPoints);
    }
    if (queueLenPoints.length > 0) {
      renderQueueLenChart(queueLenPoints);
    }
    if (analytics.recentPackets) {
      renderRecentPackets(analytics.recentPackets);
    }
  }

  function renderTimelineChart(timeline) {
    const ctx = document.getElementById('obsTimeChart');
    if (!ctx) return;
    const c = new Chart(ctx, {
      type: 'bar',
      data: {
        labels: timeline.map(t => t.label),
        datasets: [{
          label: 'Packets',
          data: timeline.map(t => t.count),
          backgroundColor: CHART_COLORS[0] + '80',
          borderColor: CHART_COLORS[0],
          borderWidth: 1,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { beginAtZero: true, ticks: { precision: 0 } }
        }
      }
    });
    charts.push(c);
  }

  function renderTypeChart(types) {
    const ctx = document.getElementById('obsTypeChart');
    if (!ctx) return;
    const labels = Object.keys(types).map(k => PAYLOAD_LABELS[k] || 'Type ' + k);
    const values = Object.values(types);
    const c = new Chart(ctx, {
      type: 'doughnut',
      data: {
        labels: labels,
        datasets: [{ data: values, backgroundColor: CHART_COLORS.slice(0, labels.length) }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { position: 'bottom', labels: { boxWidth: 12 } } }
      }
    });
    charts.push(c);
  }

  function renderNodesChart(timeline) {
    const ctx = document.getElementById('obsNodesChart');
    if (!ctx) return;
    const c = new Chart(ctx, {
      type: 'line',
      data: {
        labels: timeline.map(t => t.label),
        datasets: [{
          label: 'Unique Nodes',
          data: timeline.map(t => t.count),
          borderColor: CHART_COLORS[2],
          backgroundColor: CHART_COLORS[2] + '20',
          fill: true, tension: 0.3, pointRadius: 2,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { beginAtZero: true, ticks: { precision: 0 } }
        }
      }
    });
    charts.push(c);
  }

  function renderSnrChart(distribution) {
    const ctx = document.getElementById('obsSnrChart');
    if (!ctx) return;
    const c = new Chart(ctx, {
      type: 'bar',
      data: {
        labels: distribution.map(d => d.range),
        datasets: [{
          label: 'Packets',
          data: distribution.map(d => d.count),
          backgroundColor: CHART_COLORS[3] + '80',
          borderColor: CHART_COLORS[3],
          borderWidth: 1,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: false } },
        scales: {
          x: { title: { display: true, text: 'SNR (dB)' } },
          y: { beginAtZero: true, ticks: { precision: 0 } }
        }
      }
    });
    charts.push(c);
  }

  function metricLabels(samples) {
    return samples.map(function(s) {
      return new Date(s.timestamp).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
    });
  }

  function renderUptimeChart(samples) {
    const ctx = document.getElementById('obsUptimeChart');
    if (!ctx) return;
    // For each reboot, push a 0 at the same timestamp first so the line drops
    // vertically to the baseline before rising again — clean sharkfin shape.
    const labels = [];
    const data = [];
    samples.forEach(function(s) {
      const lbl = new Date(s.timestamp).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
      if (s.is_reboot_sample || s.is_reboot) {
        labels.push(lbl);
        data.push(0);
      }
      labels.push(lbl);
      data.push(s.uptime_secs);
    });
    const c = new Chart(ctx, {
      type: 'line',
      data: {
        labels: labels,
        datasets: [{
          label: 'Uptime',
          data: data,
          borderColor: CHART_COLORS[2],
          backgroundColor: CHART_COLORS[2] + '20',
          fill: true, tension: 0, pointRadius: 0, spanGaps: true,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: {
          legend: { display: false },
          tooltip: { callbacks: { label: function(ctx) { return formatDuration(ctx.raw); } } }
        },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { beginAtZero: true, ticks: { callback: function(v) { return formatDuration(v); } } }
        }
      }
    });
    charts.push(c);
  }

  function renderBatteryChart(samples) {
    const ctx = document.getElementById('obsBatteryChart');
    if (!ctx) return;
    const c = new Chart(ctx, {
      type: 'line',
      data: {
        labels: metricLabels(samples),
        datasets: [{
          label: 'Battery (mV)',
          data: samples.map(function(s) { return s.battery_mv; }),
          borderColor: CHART_COLORS[3],
          backgroundColor: CHART_COLORS[3] + '20',
          fill: true, tension: 0.3, pointRadius: 2,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { ticks: { callback: function(v) { return v + ' mV'; } } }
        }
      }
    });
    charts.push(c);
  }

  function renderNoiseFloorChart(samples) {
    const ctx = document.getElementById('obsNoiseFloorChart');
    if (!ctx) return;
    const c = new Chart(ctx, {
      type: 'line',
      data: {
        labels: metricLabels(samples),
        datasets: [{
          label: 'Noise Floor (dBm)',
          data: samples.map(function(s) { return s.noise_floor; }),
          borderColor: CHART_COLORS[4],
          backgroundColor: CHART_COLORS[4] + '20',
          fill: true, tension: 0.3, pointRadius: 2,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { ticks: { callback: function(v) { return v + ' dBm'; } } }
        }
      }
    });
    charts.push(c);
  }

  function renderRssiChart(timeline) {
    const ctx = document.getElementById('obsRssiChart');
    if (!ctx) return;
    const c = new Chart(ctx, {
      type: 'line',
      data: {
        labels: timeline.map(function(t) { return t.label; }),
        datasets: [{
          label: 'Avg RSSI (dBm)',
          data: timeline.map(function(t) { return t.avg; }),
          borderColor: CHART_COLORS[5],
          backgroundColor: CHART_COLORS[5] + '20',
          fill: true, tension: 0.3, pointRadius: 2,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { ticks: { callback: function(v) { return v + ' dBm'; } } }
        }
      }
    });
    charts.push(c);
  }

  function renderAirtimeChart(samples) {
    const ctx = document.getElementById('obsAirtimeChart');
    if (!ctx) return;
    const txVals = samples.map(function(s) { return s.tx_airtime_pct; });
    const rxVals = samples.map(function(s) { return s.rx_airtime_pct; });
    const avg = function(vals) {
      const v = vals.filter(function(x) { return x != null; });
      return v.length ? Math.round(v.reduce(function(a, b) { return a + b; }, 0) / v.length * 100) / 100 : null;
    };
    const txAvg = avg(txVals);
    const rxAvg = avg(rxVals);
    const titleEl = document.getElementById('obsAirtimeTitle');
    if (titleEl) {
      var suffix = '';
      if (txAvg != null || rxAvg != null) {
        suffix = ' — ' + (txAvg != null ? '(TX: ' + txAvg + '%)' : '') +
          (txAvg != null && rxAvg != null ? ' - ' : '') +
          (rxAvg != null ? '(RX: ' + rxAvg + '%)' : '');
      }
      titleEl.textContent = 'Airtime Utilization (%)' + suffix;
    }
    const c = new Chart(ctx, {
      type: 'line',
      data: {
        labels: metricLabels(samples),
        datasets: [
          {
            label: 'TX Airtime %',
            data: txVals,
            borderColor: CHART_COLORS[0],
            backgroundColor: CHART_COLORS[0] + '20',
            fill: false, tension: 0.3, pointRadius: 2, spanGaps: true,
          },
          {
            label: 'RX Airtime %',
            data: rxVals,
            borderColor: CHART_COLORS[1],
            backgroundColor: CHART_COLORS[1] + '20',
            fill: false, tension: 0.3, pointRadius: 2, spanGaps: true,
          },
        ]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: true, position: 'top', labels: { boxWidth: 12 } } },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { beginAtZero: true, ticks: { callback: function(v) { return v + '%'; } } }
        }
      }
    });
    charts.push(c);
  }

  function renderRecvErrorsChart(samples) {
    const ctx = document.getElementById('obsRecvErrorsChart');
    if (!ctx) return;
    const c = new Chart(ctx, {
      type: 'bar',
      data: {
        labels: metricLabels(samples),
        datasets: [{
          label: 'Errors',
          data: samples.map(function(s) { return s.recv_errors; }),
          backgroundColor: CHART_COLORS[1] + '80',
          borderColor: CHART_COLORS[1],
          borderWidth: 1,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { beginAtZero: true, ticks: { precision: 0 } }
        }
      }
    });
    charts.push(c);
  }

  function renderQueueLenChart(samples) {
    const ctx = document.getElementById('obsQueueLenChart');
    if (!ctx) return;
    const c = new Chart(ctx, {
      type: 'bar',
      data: {
        labels: metricLabels(samples),
        datasets: [{
          label: 'Queue Length',
          data: samples.map(function(s) { return s.queue_len; }),
          backgroundColor: CHART_COLORS[6] + '80',
          borderColor: CHART_COLORS[6],
          borderWidth: 1,
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: true,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { maxRotation: 45, autoSkip: true, maxTicksLimit: 12 } },
          y: { beginAtZero: true, ticks: { precision: 0 } }
        }
      }
    });
    charts.push(c);
  }

  function renderRecentPackets(packets) {
    const el = document.getElementById('obsRecentPackets');
    if (!el || !packets.length) { if (el) el.innerHTML = PageState.empty({ title: 'No recent packets' }); return; }
    el.innerHTML = `<table class="data-table" style="font-size:0.85em">
      <thead><tr><th scope="col">Time</th><th scope="col">Type</th><th scope="col">Hash</th><th scope="col">SNR</th><th scope="col">RSSI</th><th scope="col">Hops</th></tr></thead>
      <tbody>${packets.map(p => {
        const decoded = typeof p.decoded_json === 'string' ? JSON.parse(p.decoded_json) : (p.decoded_json || {});
        const hops = typeof p.path_json === 'string' ? JSON.parse(p.path_json) : (p.path_json || []);
        const typeName = PAYLOAD_LABELS[p.payload_type] || 'Type ' + p.payload_type;
        return `<tr style="cursor:pointer" tabindex="0" role="row" data-action="navigate" data-value="#/packets/${p.hash || p.id}" onclick="location.hash='#/packets/${p.hash || p.id}'">
          <td>${timeAgo(p.timestamp)}</td>
          <td>${typeName}</td>
          <td class="mono" style="font-size:0.85em">${(p.hash || '').substring(0, 10)}</td>
          <td>${p.snr != null ? Number(p.snr).toFixed(1) : '—'}</td>
          <td>${p.rssi != null ? p.rssi : '—'}</td>
          <td>${hops.length}</td>
        </tr>`;
      }).join('')}</tbody>
    </table>`;
    // Keyboard accessibility is handled by the delegated #app keydown listener
    // bound once in init() — no per-render listener here (leak fix).
  }

  registerPage('observer-detail', { init, destroy });
})();
