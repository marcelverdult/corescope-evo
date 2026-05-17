/* === CoreScope — chart-constants.js (shared chart constants) === */
'use strict';

/**
 * Shared analytics/chart constants (issue: dedup). Previously duplicated
 * verbatim in node-analytics.js, observer-detail.js and compare.js.
 * Loaded before those modules so they can delegate to these globals.
 */
window.PAYLOAD_LABELS = {
  0: 'Request', 1: 'Response', 2: 'Direct Msg', 3: 'ACK', 4: 'Advert',
  5: 'Channel Msg', 7: 'Anon Req', 8: 'Path', 9: 'Trace', 11: 'Control'
};

window.CHART_COLORS = [
  '#4a9eff', '#ff6b6b', '#51cf66', '#fcc419', '#cc5de8',
  '#20c997', '#ff922b', '#845ef7', '#f06595', '#339af0'
];
