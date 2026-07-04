// logs.js — the Logs tab: a read-only viewer over tclaude's own log file
// (~/.tclaude/output.log, now JSON lines). Each row is one log record —
// time, level, message, and any structured fields slog attached.
//
// Search, level filtering, time-range filtering and pagination all happen
// SERVER-SIDE (see agentd/dashboard_logs.go) so the tab stays responsive
// no matter how large the log grows — the client only ever holds the page
// in view, like the Audit and Messages tabs. Fetched on tab activation,
// the ⟳ refresh button, any filter/page change, and — when "stream" is
// ticked — a 2s tail-follow poll (default off). Never on the 2s snapshot
// tick.

import { $, esc, relTime } from './helpers.js';
import { morphInto } from './morph.js';

// View state. page/pageSize + the filters are sent to the server; total +
// totalUnfiltered come back with each fetch and drive the pager + count.
const logs = {
  page: 1,
  pageSize: 100,
  total: 0,
  totalUnfiltered: 0,
  rows: [],
};
const PAGE_SIZES = [50, 100, 250, 500];

// Stream (tail-follow) poll interval. Kept coarse — a log rarely needs
// sub-2s freshness and this matches the snapshot cadence.
const STREAM_MS = 2000;
let streamTimer = null;

// Monotonic guard: a slow response must never repaint over a newer one
// (rapid typing / filter flips / stream ticks can land out of order).
let loadSeq = 0;
// Debounce the search box so a few keystrokes settle into one fetch.
let searchTimer = null;

// levelKey maps a raw level string to a whitelisted class token. Anything
// outside the known slog levels (empty, or a tampered/foreign value)
// collapses to "raw" — so the token is always safe to interpolate into a
// class name and can never inject markup from log content.
function levelKey(level) {
  return { debug: 'debug', info: 'info', warn: 'warn', error: 'error' }[(level || '').toLowerCase()] || 'raw';
}

// levelPill colourises a log level. Unknown / empty (raw, non-JSON lines)
// render as a neutral "raw" chip so they stay visible and distinct.
function levelPill(level) {
  const key = levelKey(level);
  if (key === 'raw') return `<span class="log-level log-raw" title="not a structured log line">raw</span>`;
  return `<span class="log-level log-${key}">${key}</span>`;
}

// fmtAbsTime renders a record's timestamp as a stable local
// "YYYY-MM-DD HH:MM:SS.mmm" stamp. It anchors the When column so it never
// reads stale — the table only re-renders on activation / filter / page /
// a stream tick, so a bare "5m ago" would freeze between fetches.
function fmtAbsTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const p = (n, w = 2) => String(n).padStart(w, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} `
    + `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

// whenCellHTML renders the When column: the stable absolute stamp plus a
// dimmed "(5m ago)" relative hint for quick scanning.
function whenCellHTML(e) {
  const rel = relTime(e.time);
  return `<span class="last-hook" title="${esc(e.time || '')}">${esc(fmtAbsTime(e.time))}</span>`
    + (rel ? ` <span class="muted">(${esc(rel)})</span>` : '');
}

// fieldsText flattens a record's structured fields into a compact
// "key=value key=value" string for the inline detail.
function fieldsText(fields) {
  if (!fields) return '';
  return Object.entries(fields)
    .map(([k, v]) => `${k}=${v !== null && typeof v === 'object' ? JSON.stringify(v) : v}`)
    .join('  ');
}

function renderLogs() {
  const rows = logs.rows;
  // Count chip: matched / all when a filter narrows the set.
  const filtered = logs.total !== logs.totalUnfiltered;
  $('#filter-logs-count').textContent = logs.totalUnfiltered === 0 ? ''
    : filtered ? `${logs.total} / ${logs.totalUnfiltered}`
    : `${logs.total} line${logs.total === 1 ? '' : 's'}`;

  if (!rows.length) {
    morphInto($('#logs-list'), logs.totalUnfiltered
      ? '<div class="empty">No log lines match the filter.</div>'
      : '<div class="empty">No log lines yet. tclaude writes its daemon + CLI log to <code>~/.tclaude/output.log</code>.</div>');
    renderPager();
    return;
  }

  // Morph rather than swap so a selection in the copy-heavy log table survives
  // a stream tail-follow tick. Rows are matched POSITIONALLY (no data-key): a
  // log record carries no stable per-line id, and a content hash (time+msg) is
  // unsafe — a burst can emit byte-identical lines at the same millisecond, and
  // duplicate keys corrupt the reconciler. This is a paged table, so positional
  // matching is acceptable (JOH-339); an idle tick with no new line still hits
  // the isEqualNode fast path and preserves the selection intact.
  morphInto($('#logs-list'), `
    <table class="logs-table">
      <thead><tr><th>When</th><th>Level</th><th>Message</th></tr></thead>
      <tbody>
        ${rows.map(e => {
          const ft = fieldsText(e.fields);
          return `
          <tr class="log-row log-row-${levelKey(e.level)}">
            <td class="logs-nowrap">${whenCellHTML(e)}</td>
            <td class="logs-nowrap">${levelPill(e.level)}</td>
            <td class="logs-msg-cell">
              <span class="logs-msg">${esc(e.msg || '')}</span>
              ${ft ? ` <span class="logs-fields muted" title="${esc(ft)}">${esc(ft)}</span>` : ''}
            </td>
          </tr>`;
        }).join('')}
      </tbody>
    </table>`);
  renderPager();
}

function pageCount() {
  return Math.max(1, Math.ceil(logs.total / logs.pageSize));
}

function renderPager() {
  const bar = $('#logs-pager');
  if (!bar) return;
  if (!logs.totalUnfiltered) { bar.hidden = true; bar.innerHTML = ''; return; }
  bar.hidden = false;
  const pages = pageCount();
  const page = Math.min(logs.page, pages);
  let nav = '';
  if (pages > 1) {
    const atStart = page <= 1;
    const atEnd = page >= pages;
    nav = `
      <button data-act="logs-page-first" title="First page (newest)"${atStart ? ' disabled' : ''}>«</button>
      <button data-act="logs-page-prev" title="Previous page"${atStart ? ' disabled' : ''}>‹</button>
      <span class="audit-pager-pos">Page ${page} / ${pages}</span>
      <button data-act="logs-page-next" title="Next page (older)"${atEnd ? ' disabled' : ''}>›</button>
      <button data-act="logs-page-last" title="Last page (oldest)"${atEnd ? ' disabled' : ''}>»</button>`;
  }
  const sizeOpts = PAGE_SIZES.map(sz =>
    `<option value="${sz}"${sz === logs.pageSize ? ' selected' : ''}>${sz}</option>`).join('');
  bar.innerHTML = `${nav}<span class="grow"></span>`
    + `<label class="audit-pager-size" title="Rows per page"><select id="logs-page-size">${sizeOpts}</select> / page</label>`;
}

// fmtInt renders a line count with locale thousands separators
// ("12,345") — exact, not the abbreviated "12k" a token meter would use,
// because the operator is trying to reason about exactly which lines the
// tab is presenting.
function fmtInt(n) {
  return (Number(n) || 0).toLocaleString();
}

// renderStatus fills the muted status strip on the Logs filter bar with a
// plain statement of WHAT is being shown: which file(s) were read and how
// many lines from each (log rotation splits history across output.log,
// .1, .2, …, so this is otherwise invisible), a click-to-include hint when
// rotated siblings exist but the toggle is off, and the byte-cap warning.
function renderStatus(data) {
  const el = $('#logs-status');
  if (!el) return;
  const sources = data.sources || [];
  const bits = [];

  if (sources.length) {
    const active = sources.find(s => !s.rotated);
    const rotated = sources.filter(s => s.rotated);
    const totalLines = sources.reduce((n, s) => n + (s.lines || 0), 0);
    const anchor = active ? active.path : (data.path || sources[0].path);

    // Per-file breakdown for the hover tooltip: "output.log — 1,234 lines".
    const detail = `Reading ${sources.length} file${sources.length === 1 ? '' : 's'}:\n`
      + sources.map(s => `  ${s.name} — ${fmtInt(s.lines)} line${s.lines === 1 ? '' : 's'}`).join('\n');

    let label = esc(anchor);
    if (rotated.length) {
      label += ` <span class="muted">+ ${rotated.length} rotated file${rotated.length === 1 ? '' : 's'}</span>`;
    }
    label += ` <span class="muted">· ${fmtInt(totalLines)} line${totalLines === 1 ? '' : 's'}</span>`;
    bits.push(`<span class="logs-sources" title="${esc(detail)}">${label}</span>`);
  } else if (data.path) {
    bits.push(esc(data.path));
  }

  // Rotated siblings exist on disk but weren't scanned this request —
  // surface them and let a click tick the "rotated" toggle to include them.
  const avail = data.rotated_available || 0;
  if (avail && !data.include_rotated) {
    bits.push(`<a href="#" class="logs-rotated-hint" title="Also read the ${avail} rotated log file${avail === 1 ? '' : 's'} (output.log.1 … .${avail}) for older history">+ ${avail} rotated file${avail === 1 ? '' : 's'} available</a>`);
  }

  if (data.truncated) bits.push('<span class="logs-warn" title="Only the newest slice of the log was read; older lines were skipped. Narrow the time range or enable rotation to keep the file bounded.">newest slice only</span>');
  el.innerHTML = bits.join(' · ');
}

async function loadLogs() {
  const seq = ++loadSeq;
  const params = new URLSearchParams({
    page: String(logs.page),
    page_size: String(logs.pageSize),
  });
  const q = ($('#filter-logs')?.value || '').trim();
  if (q) params.set('q', q);
  const level = $('#logs-level')?.value;
  if (level) params.set('level', level);
  // The "since" preset is a duration in ms; convert to an absolute
  // lower-bound so a slow request still filters against a stable instant.
  const rangeMs = Number($('#logs-range')?.value || 0);
  if (rangeMs > 0) params.set('from', String(Date.now() - rangeMs));
  if ($('#logs-rotated')?.checked) params.set('include_rotated', '1');
  if ($('#logs-hide-raw')?.checked) params.set('hide_raw', '1');

  try {
    const r = await fetch('/api/logs?' + params.toString(), { credentials: 'same-origin' });
    if (!r.ok) throw new Error(await r.text() || r.status);
    const data = await r.json();
    if (seq !== loadSeq) return; // superseded
    logs.rows = data.entries || [];
    logs.total = data.total || 0;
    logs.totalUnfiltered = data.total_unfiltered || 0;
    // Trust the server's clamped page (a stale page past the last one
    // comes back pulled to the last page).
    if (typeof data.page === 'number') logs.page = data.page;
    if (typeof data.page_size === 'number') logs.pageSize = data.page_size;
    renderStatus(data);
    renderLogs();
  } catch (e) {
    if (seq !== loadSeq) return;
    logs.rows = [];
    $('#filter-logs-count').textContent = '';
    $('#logs-status').textContent = '';
    $('#logs-pager').hidden = true;
    morphInto($('#logs-list'),
      `<div class="empty">Failed to load logs: ${esc(e.message || e)}</div>`);
  }
}

// reloadFromFirstPage resets to page 1 — used whenever a filter changes
// the result set (a page-2 view of the old set is meaningless).
function reloadFromFirstPage() {
  logs.page = 1;
  loadLogs();
}

function logsTabActive() {
  return $('#tab-logs').classList.contains('active');
}

function startStreaming() {
  if (streamTimer) return;
  // Jump to the newest page so the tail is what follows.
  logs.page = 1;
  streamTimer = setInterval(() => {
    // Cheap guard: keep the timer but skip fetches while the tab is hidden.
    if (logsTabActive()) loadLogs();
  }, STREAM_MS);
  loadLogs();
}

function stopStreaming() {
  if (streamTimer) { clearInterval(streamTimer); streamTimer = null; }
}

// bindLogsTab wires the tab: load on activation; server-side search /
// level / time / rotated filters; a manual refresh; the pager; and the
// opt-in stream (tail-follow) poll.
function bindLogsTab() {
  $('nav button[data-tab="logs"]').addEventListener('click', loadLogs);

  const filter = $('#filter-logs');
  if (filter) {
    filter.addEventListener('input', () => {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(reloadFromFirstPage, 300);
    });
  }
  const clear = $('#filter-logs-clear');
  if (clear) {
    clear.addEventListener('click', () => {
      if (filter) filter.value = '';
      reloadFromFirstPage();
    });
  }
  ['#logs-level', '#logs-range', '#logs-rotated', '#logs-hide-raw'].forEach(sel => {
    const el = $(sel);
    if (el) el.addEventListener('change', reloadFromFirstPage);
  });

  const refresh = $('#logs-refresh');
  if (refresh) refresh.addEventListener('click', loadLogs);

  // The status strip's "+ N rotated files available" hint ticks the
  // rotated toggle (delegated — renderStatus rewrites this node each load).
  const status = $('#logs-status');
  if (status) {
    status.addEventListener('click', e => {
      const hint = e.target.closest('.logs-rotated-hint');
      if (!hint) return;
      e.preventDefault();
      const cb = $('#logs-rotated');
      if (cb && !cb.checked) {
        cb.checked = true;
        reloadFromFirstPage();
      }
    });
  }

  const stream = $('#logs-stream');
  if (stream) {
    stream.addEventListener('change', () => {
      if (stream.checked) startStreaming(); else stopStreaming();
    });
  }

  // Pager (delegated — the table is re-rendered each load).
  $('#logs-pager').addEventListener('click', e => {
    const btn = e.target.closest('button[data-act]');
    if (!btn || btn.disabled) return;
    const pages = pageCount();
    switch (btn.dataset.act) {
      case 'logs-page-first': logs.page = 1; break;
      case 'logs-page-prev': logs.page = Math.max(1, logs.page - 1); break;
      case 'logs-page-next': logs.page = Math.min(pages, logs.page + 1); break;
      case 'logs-page-last': logs.page = pages; break;
      default: return;
    }
    loadLogs();
  });
  $('#logs-pager').addEventListener('change', e => {
    if (e.target.id !== 'logs-page-size') return;
    logs.pageSize = Number(e.target.value) || 100;
    reloadFromFirstPage();
  });
}

export { bindLogsTab };
