// audit.js — the Audit tab: the trail of daemon-proxied tclaude commands
// (JOH-268). Each row is one command — WHO ran WHAT against WHICH target
// — in the operator's symbolic form: actor | verb | target | detail.
// Denied / errored attempts are shown too, distinguished by a status
// pill, so the trail answers "who tried what", not only "what landed".
//
// Data comes from GET /api/audit (see agentd/dashboard_audit.go), fetched
// on tab activation + a slow re-poll while visible — the append-only log
// doesn't move on the 2s snapshot cadence. The outcome/source filters are
// applied server-side; the text box filters the fetched rows client-side.

import { $, $$, esc, relTime, shortId } from './helpers.js';

// Last fetched rows (server-filtered by outcome/source); the text box
// narrows these client-side without a refetch.
let auditRows = [];
let lastFetchedAt = 0;
// Monotonic guard: a slow response must never repaint over a newer one.
let loadSeq = 0;

const REPOLL_MS = 30_000;

// Verbs group into families for colouring — destructive (retire/delete/
// stop), elevation (permissions/sudo), and the everyday rest. Keyed on a
// prefix/word match so e.g. "permissions.grant" and "group.delete" land
// in the right bucket.
function verbClass(verb) {
  const v = verb || '';
  if (/(^|\.)(delete|retire|remove|stop|deny|revoke)(\.|$)/.test(v)) return 'audit-verb danger';
  if (/^(permissions|sudo|owner)/.test(v)) return 'audit-verb elevate';
  if (/^(spawn|clone|reincarnate|group\.create|member\.add)/.test(v)) return 'audit-verb create';
  return 'audit-verb';
}

// statusPill colourises the HTTP status: 2xx success (green), 4xx denied/
// client (amber), 5xx error (red). The label reads "ok" / "denied" /
// "err" with the code on hover.
function statusPill(status) {
  const s = Number(status) || 0;
  if (s >= 200 && s < 400) {
    return `<span class="state-pill state-working" title="${s}">ok</span>`;
  }
  if (s === 401 || s === 403) {
    return `<span class="state-pill state-awaiting" title="${s} — permission denied">denied</span>`;
  }
  if (s >= 400 && s < 500) {
    return `<span class="state-pill state-awaiting" title="${s}">rejected</span>`;
  }
  return `<span class="state-pill state-offline" title="${s} — error">err</span>`;
}

// actorCell renders WHO ran the command. The human operator is a single
// chip; an agent shows its title + short conv-id.
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

// targetCell renders WHAT the command acted on: a group chip, a target
// agent, or both (e.g. a message into a group). Empty when the verb has
// no distinct target (a group create names the group via group_name).
function targetCell(e) {
  const bits = [];
  if (e.group_name) bits.push(`<span class="tag">${esc(e.group_name)}</span>`);
  if (e.target_label) {
    bits.push(`<span class="rowname">${esc(e.target_label)}</span>`);
  }
  return bits.length ? bits.join(' ') : '<span class="muted">—</span>';
}

function rowMatches(e, needle) {
  if (!needle) return true;
  const hay = [e.actor_label, e.actor_conv, e.verb, e.target_label,
    e.target_conv, e.group_name, e.detail, e.source]
    .filter(Boolean).join(' ').toLowerCase();
  return hay.includes(needle);
}

function renderAudit() {
  const q = ($('#filter-audit').value || '').trim().toLowerCase();
  const rows = auditRows.filter(e => rowMatches(e, q));
  const total = auditRows.length;
  $('#filter-audit-count').textContent = q
    ? `${rows.length} / ${total}` : `${total} event${total === 1 ? '' : 's'}`;

  if (!rows.length) {
    $('#audit-list').innerHTML = total
      ? '<div class="empty">No events match the filter.</div>'
      : '<div class="empty">No commands recorded yet. Audit rows are written as agents and the operator run tclaude commands (spawn, message, lifecycle, permissions…).</div>';
    return;
  }

  $('#audit-list').innerHTML = `
    <table class="audit-table">
      <thead><tr>
        <th>When</th><th>Actor</th><th>Action</th><th>Target</th><th>Detail</th><th>Outcome</th>
      </tr></thead>
      <tbody>
        ${rows.map(e => `
          <tr>
            <td><span class="last-hook" title="${esc(e.at)}">${esc(relTime(e.at) || '—')}</span></td>
            <td>${actorCell(e)}</td>
            <td><span class="${verbClass(e.verb)}">${esc(e.verb)}</span>${e.source === 'dashboard' ? ' <span class="id" title="run from the dashboard">⊞</span>' : ''}</td>
            <td>${targetCell(e)}</td>
            <td><span class="muted" title="${esc(e.detail || '')}">${esc(e.detail || '')}</span></td>
            <td>${statusPill(e.status)}</td>
          </tr>`).join('')}
      </tbody>
    </table>`;
}

async function loadAudit() {
  const seq = ++loadSeq;
  lastFetchedAt = Date.now();
  const outcome = $('#audit-outcome') ? $('#audit-outcome').value : '';
  const source = $('#audit-source') ? $('#audit-source').value : '';
  const params = new URLSearchParams();
  if (outcome) params.set('outcome', outcome);
  if (source) params.set('source', source);
  const qs = params.toString();
  try {
    const r = await fetch('/api/audit' + (qs ? '?' + qs : ''), { credentials: 'same-origin' });
    if (!r.ok) throw new Error(await r.text() || r.status);
    const data = await r.json();
    if (seq !== loadSeq) return; // superseded
    auditRows = data.entries || [];
    renderRetention(data);
    renderAudit();
  } catch (e) {
    if (seq !== loadSeq) return;
    auditRows = [];
    $('#filter-audit-count').textContent = '';
    $('#audit-retention').textContent = '';
    $('#audit-list').innerHTML =
      `<div class="empty">Failed to load audit log: ${esc(e.message || e)}</div>`;
  }
}

function renderRetention(data) {
  const el = $('#audit-retention');
  if (!el) return;
  el.textContent = data.pruning_on
    ? `keeping ${data.retention_days} day${data.retention_days === 1 ? '' : 's'}`
    : 'kept forever';
}

function auditTabActive() {
  return $('#tab-audit').classList.contains('active');
}

// bindAuditTab wires the tab: load on activation, refetch on a
// server-side filter change, client-side text filter, slow re-poll off
// the snapshot tick while visible.
function bindAuditTab() {
  $('nav button[data-tab="audit"]').addEventListener('click', loadAudit);

  const filter = $('#filter-audit');
  if (filter) {
    filter.addEventListener('input', renderAudit);
  }
  const clear = $('#filter-audit-clear');
  if (clear) {
    clear.addEventListener('click', () => { filter.value = ''; renderAudit(); });
  }
  // The outcome / source selects narrow at the server, so changing them
  // refetches rather than filtering the already-fetched rows.
  ['#audit-outcome', '#audit-source'].forEach(sel => {
    const el = $(sel);
    if (el) el.addEventListener('change', loadAudit);
  });

  document.addEventListener('tclaude:snapshot', () => {
    if (auditTabActive() && Date.now() - lastFetchedAt > REPOLL_MS) loadAudit();
  });
}

export { bindAuditTab };
