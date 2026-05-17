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
