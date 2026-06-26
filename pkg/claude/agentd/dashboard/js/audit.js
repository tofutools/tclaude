// audit.js — the Audit tab: the trail of daemon-proxied tclaude commands
// (JOH-268). Each row is one command — WHO ran WHAT against WHICH target
// — in the operator's symbolic form: actor | verb | target | detail.
// Denied / errored attempts are shown too, distinguished by a status
// pill, so the trail answers "who tried what", not only "what landed".
//
// Search, sort and pagination all happen SERVER-SIDE (see
// agentd/dashboard_audit.go + db.ListAuditLog) so the tab stays
// responsive no matter how large the trail grows — the client only ever
// holds the page in view, like the Messages tab. Fetched on tab
// activation + any filter/sort/page change + a slow re-poll while
// visible; never on the 2s snapshot tick.

import { $, esc, relTime, shortId } from './helpers.js';

// View state. page/pageSize/sort/dir are sent to the server; total +
// totalUnfiltered come back with each fetch and drive the pager + count.
const audit = {
  page: 1,
  pageSize: 100,
  sort: 'time',
  dir: 'desc',
  total: 0,
  totalUnfiltered: 0,
  rows: [],
};
const PAGE_SIZES = [50, 100, 250, 500];

let lastFetchedAt = 0;
// Monotonic guard: a slow response must never repaint over a newer one
// (rapid typing / sort flips / page nav can land out of order).
let loadSeq = 0;
const REPOLL_MS = 30_000;
// Debounce the search box so a few keystrokes settle into one fetch.
let searchTimer = null;

// The sortable columns, in render order. A column without a `sort` key
// (Detail) is not server-sortable. The keys match db.AuditSort* tokens.
const COLUMNS = [
  { label: 'When', sort: 'time' },
  { label: 'Actor', sort: 'actor' },
  { label: 'Action', sort: 'verb' },
  { label: 'Target', sort: 'target' },
  { label: 'Detail' },
  { label: 'Outcome', sort: 'status' },
];

// verbClass buckets a verb for colouring — destructive, elevation, or
// the everyday create/rest.
function verbClass(verb) {
  const v = verb || '';
  if (/(^|\.)(delete|retire|remove|stop|deny|revoke|prune|wipe|shutdown)(\.|$)/.test(v)) return 'audit-verb danger';
  if (/^(permissions|sudo|owner|approval|remote-access)/.test(v)) return 'audit-verb elevate';
  if (/^(spawn|clone|reincarnate|group\.create|member\.add|template\.instantiate|power\.on)/.test(v)) return 'audit-verb create';
  return 'audit-verb';
}

// statusPill colourises the HTTP status: 2xx success (green), 401/403
// denied (amber), other 4xx rejected (amber), 5xx error (red).
function statusPill(status) {
  const s = Number(status) || 0;
  if (s >= 200 && s < 300) return `<span class="state-pill state-working" title="${s}">ok</span>`;
  if (s === 401 || s === 403) return `<span class="state-pill state-awaiting" title="${s} — permission denied">denied</span>`;
  if (s >= 400 && s < 500) return `<span class="state-pill state-awaiting" title="${s}">rejected</span>`;
  if (s >= 500) return `<span class="state-pill state-offline" title="${s} — error">err</span>`;
  return `<span class="state-pill" title="${s}">${s || '—'}</span>`;
}

// fmtAbsTime renders an audit row's recorded time as a stable local
// "YYYY-MM-DD HH:MM:SS" stamp. It anchors the When column so it never
// reads stale — the table only re-renders on activation / filter / sort /
// page / a slow re-poll, so a bare "5m ago" would freeze between fetches.
function fmtAbsTime(iso) {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso || '—';
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} `
    + `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

// whenCellHTML renders the When column: the stable absolute stamp plus a
// dimmed "(5m ago)" relative hint for quick scanning. The relative part
// may drift slightly between re-polls, but the absolute stamp beside it
// is always correct, so the staleness never misleads.
function whenCellHTML(e) {
  const rel = relTime(e.at);
  return `<span class="last-hook" title="${esc(e.at)}">${esc(fmtAbsTime(e.at))}</span>`
    + (rel ? ` <span class="muted">(${esc(rel)})</span>` : '');
}

function actorCell(e) {
  if (e.actor_kind === 'human') {
    return `<span class="audit-actor human" title="the human operator">operator</span>`;
  }
  if (e.actor_kind === 'agent') {
    return `<span class="rowname">${esc(e.actor_label || '(agent)')}</span>`
      + (e.actor_conv ? ` <span class="id">${esc(shortId(e.actor_conv))}</span>` : '');
  }
  return `<span class="muted" title="caller identity could not be resolved">${esc(e.actor_label || 'unknown')}</span>`;
}

function targetCell(e) {
  const bits = [];
  if (e.group_name) bits.push(`<span class="tag">${esc(e.group_name)}</span>`);
  if (e.target_label) bits.push(`<span class="rowname">${esc(e.target_label)}</span>`);
  return bits.length ? bits.join(' ') : '<span class="muted">—</span>';
}

// actorTitle / targetTitle build the plain-text full value for the cell's
// hover tooltip — the Actor / Target columns truncate with an ellipsis when
// narrow (see .audit-trunc), so the untruncated text lives on the title.
function actorTitle(e) {
  if (e.actor_kind === 'human') return 'the human operator';
  if (e.actor_kind === 'agent') {
    return (e.actor_label || '(agent)') + (e.actor_conv ? ' ' + shortId(e.actor_conv) : '');
  }
  return e.actor_label || 'unknown';
}
function targetTitle(e) {
  return [e.group_name, e.target_label].filter(Boolean).join(' ') || '—';
}

// headerHTML renders the sortable column header row with the active
// sort's direction arrow.
function headerHTML() {
  return '<tr>' + COLUMNS.map(c => {
    if (!c.sort) return `<th>${esc(c.label)}</th>`;
    const active = audit.sort === c.sort;
    const arrow = active ? (audit.dir === 'asc' ? ' ▲' : ' ▼') : '';
    return `<th class="audit-sort${active ? ' active' : ''}" data-sort="${c.sort}" title="Sort by ${esc(c.label)}">${esc(c.label)}${arrow}</th>`;
  }).join('') + '</tr>';
}

function renderAudit() {
  const rows = audit.rows;
  // Count chip: matched / all when a filter narrows the set.
  const filtered = audit.total !== audit.totalUnfiltered;
  $('#filter-audit-count').textContent = audit.totalUnfiltered === 0 ? ''
    : filtered ? `${audit.total} / ${audit.totalUnfiltered}`
    : `${audit.total} event${audit.total === 1 ? '' : 's'}`;

  if (!rows.length) {
    $('#audit-list').innerHTML = audit.totalUnfiltered
      ? '<div class="empty">No events match the filter.</div>'
      : '<div class="empty">No commands recorded yet. Audit rows are written as agents and the operator run tclaude commands (spawn, message, lifecycle, permissions…).</div>';
    renderPager();
    return;
  }

  $('#audit-list').innerHTML = `
    <table class="audit-table">
      <thead>${headerHTML()}</thead>
      <tbody>
        ${rows.map(e => `
          <tr>
            <td class="audit-nowrap">${whenCellHTML(e)}</td>
            <td class="audit-trunc" title="${esc(actorTitle(e))}">${actorCell(e)}</td>
            <td class="audit-trunc" title="${esc(e.verb || '')}"><span class="${verbClass(e.verb)}">${esc(e.verb)}</span>${e.source === 'dashboard' ? ' <span class="id" title="run from the dashboard">⊞</span>' : ''}</td>
            <td class="audit-trunc" title="${esc(targetTitle(e))}">${targetCell(e)}</td>
            <td class="audit-detail"><span class="muted" title="${esc(e.detail || '')}">${esc(e.detail || '')}</span></td>
            <td class="audit-nowrap">${statusPill(e.status)}</td>
          </tr>`).join('')}
      </tbody>
    </table>`;
  renderPager();
}

function pageCount() {
  return Math.max(1, Math.ceil(audit.total / audit.pageSize));
}

function renderPager() {
  const bar = $('#audit-pager');
  if (!bar) return;
  if (!audit.totalUnfiltered) { bar.hidden = true; bar.innerHTML = ''; return; }
  bar.hidden = false;
  const pages = pageCount();
  const page = Math.min(audit.page, pages);
  let nav = '';
  if (pages > 1) {
    const atStart = page <= 1;
    const atEnd = page >= pages;
    nav = `
      <button data-act="audit-page-first" title="First page"${atStart ? ' disabled' : ''}>«</button>
      <button data-act="audit-page-prev" title="Previous page"${atStart ? ' disabled' : ''}>‹</button>
      <span class="audit-pager-pos">Page ${page} / ${pages}</span>
      <button data-act="audit-page-next" title="Next page"${atEnd ? ' disabled' : ''}>›</button>
      <button data-act="audit-page-last" title="Last page"${atEnd ? ' disabled' : ''}>»</button>`;
  }
  const sizeOpts = PAGE_SIZES.map(sz =>
    `<option value="${sz}"${sz === audit.pageSize ? ' selected' : ''}>${sz}</option>`).join('');
  bar.innerHTML = `${nav}<span class="grow"></span>`
    + `<label class="audit-pager-size" title="Rows per page"><select id="audit-page-size">${sizeOpts}</select> / page</label>`;
}

function renderRetention(data) {
  const el = $('#audit-retention');
  if (!el) return;
  el.textContent = data.pruning_on
    ? `keeping ${data.retention_days} day${data.retention_days === 1 ? '' : 's'}`
    : 'kept forever';
}

async function loadAudit() {
  const seq = ++loadSeq;
  lastFetchedAt = Date.now();
  const params = new URLSearchParams({
    page: String(audit.page),
    page_size: String(audit.pageSize),
    sort: audit.sort,
    dir: audit.dir,
  });
  const q = ($('#filter-audit')?.value || '').trim();
  if (q) params.set('q', q);
  const outcome = $('#audit-outcome')?.value;
  if (outcome) params.set('outcome', outcome);
  const source = $('#audit-source')?.value;
  if (source) params.set('source', source);

  try {
    const r = await fetch('/api/audit?' + params.toString(), { credentials: 'same-origin' });
    if (!r.ok) throw new Error(await r.text() || r.status);
    const data = await r.json();
    if (seq !== loadSeq) return; // superseded
    audit.rows = data.entries || [];
    audit.total = data.total || 0;
    audit.totalUnfiltered = data.total_unfiltered || 0;
    // Trust the server's clamped page (a stale page past the last one
    // comes back pulled to the last page).
    if (typeof data.page === 'number') audit.page = data.page;
    if (typeof data.page_size === 'number') audit.pageSize = data.page_size;
    if (data.sort) audit.sort = data.sort;
    if (data.dir) audit.dir = data.dir;
    renderRetention(data);
    renderAudit();
  } catch (e) {
    if (seq !== loadSeq) return;
    audit.rows = [];
    $('#filter-audit-count').textContent = '';
    $('#audit-retention').textContent = '';
    $('#audit-pager').hidden = true;
    $('#audit-list').innerHTML =
      `<div class="empty">Failed to load audit log: ${esc(e.message || e)}</div>`;
  }
}

// reloadFromFirstPage resets to page 1 — used whenever a filter or sort
// changes the result set (a page-2 view of the old set is meaningless).
function reloadFromFirstPage() {
  audit.page = 1;
  loadAudit();
}

function auditTabActive() {
  return $('#tab-audit').classList.contains('active');
}

// bindAuditTab wires the tab: load on activation; server-side search /
// outcome / source filters; sortable headers; pager; slow re-poll.
function bindAuditTab() {
  $('nav button[data-tab="audit"]').addEventListener('click', loadAudit);

  const filter = $('#filter-audit');
  if (filter) {
    filter.addEventListener('input', () => {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(reloadFromFirstPage, 300);
    });
  }
  const clear = $('#filter-audit-clear');
  if (clear) {
    clear.addEventListener('click', () => {
      if (filter) filter.value = '';
      reloadFromFirstPage();
    });
  }
  ['#audit-outcome', '#audit-source'].forEach(sel => {
    const el = $(sel);
    if (el) el.addEventListener('change', reloadFromFirstPage);
  });

  // Sortable headers (delegated — the table is re-rendered each load).
  $('#audit-list').addEventListener('click', e => {
    const th = e.target.closest('th.audit-sort');
    if (!th) return;
    const key = th.dataset.sort;
    if (audit.sort === key) {
      audit.dir = audit.dir === 'asc' ? 'desc' : 'asc';
    } else {
      audit.sort = key;
      // Text columns read better A→Z; time/status default newest/highest.
      audit.dir = (key === 'actor' || key === 'verb' || key === 'target') ? 'asc' : 'desc';
    }
    reloadFromFirstPage();
  });

  // Pager (delegated).
  $('#audit-pager').addEventListener('click', e => {
    const btn = e.target.closest('button[data-act]');
    if (!btn || btn.disabled) return;
    const pages = pageCount();
    switch (btn.dataset.act) {
      case 'audit-page-first': audit.page = 1; break;
      case 'audit-page-prev': audit.page = Math.max(1, audit.page - 1); break;
      case 'audit-page-next': audit.page = Math.min(pages, audit.page + 1); break;
      case 'audit-page-last': audit.page = pages; break;
      default: return;
    }
    loadAudit();
  });
  $('#audit-pager').addEventListener('change', e => {
    if (e.target.id !== 'audit-page-size') return;
    audit.pageSize = Number(e.target.value) || 100;
    reloadFromFirstPage();
  });

  document.addEventListener('tclaude:snapshot', () => {
    if (auditTabActive() && Date.now() - lastFetchedAt > REPOLL_MS) loadAudit();
  });
}

export { bindAuditTab };
