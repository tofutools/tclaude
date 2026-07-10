// process-worklist.js — the Processes tab's Worklist sub-view (TCL-297):
// the operator-facing queue of everything the process engine is waiting on
// (human waits, decisions, reviews, blocked nodes, agent obligations),
// consumed from /v1/process/worklist (TCL-295).
//
// Pure view/format logic lives in process-worklist-core.js (jstest-covered);
// this module owns fetch, render, and the action funnel. Renders go through
// morphInto with stable per-item row keys (data-key = item.id) so text
// selection, focused comment inputs, and scroll survive the 2s poll.
// Comment drafts are additionally mirrored into a JS map on input, so the
// FRESH render always carries the draft (morph's "state-backed → fresh wins"
// ownership) and an unfocused half-typed comment can't be wiped by a poll.

import { $, esc } from './helpers.js';
import { morphInto } from './morph.js';
import { dashPrefs } from './prefs.js';
import { confirmModal, toast } from './refresh.js';
import { processJSON, activateProcessSubtab, processNotice } from './processes.js';
import {
  WORKLIST_VIEWS, kindMeta, actorLabel, nudgeLine, fmtAge, fmtDue, dueBucket,
  viewItems, viewCounts, groupWaitingOn, actionableCount, isActionable,
  isDestructiveAction, buildWorklistAction,
} from './process-worklist-core.js';

const VIEW_PREF_KEY = 'tclaude.dash.worklist.view';

// Module state: the last fetched worklist (items + degraded runs), the active
// view chip, and per-item comment drafts (see the module header).
let lastWorklist = null;
let activeView = 'my-work';
const commentDrafts = new Map();
// actionInFlight serializes the action funnel: one confirm + POST at a time.
// Guards both double-submission AND concurrent use of the shared confirmModal
// singleton (concurrent confirmModal calls double its listeners — one click
// would resolve both).
let actionInFlight = false;

export function initProcessWorklist() {
  const panel = $('#process-panel-worklist');
  if (!panel) return;
  const saved = dashPrefs.getItem(VIEW_PREF_KEY);
  if (saved && WORKLIST_VIEWS.some(v => v.key === saved)) activeView = saved;
  syncViewChips();

  panel.querySelector('.process-worklist-views')?.addEventListener('click', e => {
    const chip = e.target.closest('button[data-worklist-view]');
    if (!chip) return;
    activeView = chip.dataset.worklistView;
    dashPrefs.setItem(VIEW_PREF_KEY, activeView);
    syncViewChips();
    renderFromCache();
  });
  $('#process-worklist-refresh')?.addEventListener('click', () => loadProcessWorklist());

  // Comment drafts: mirror keystrokes into the map so the next fresh render
  // re-emits them (input is delegated — rows are re-morphed every poll).
  panel.addEventListener('input', e => {
    const input = e.target.closest('input[data-worklist-comment]');
    if (!input) return;
    const id = input.dataset.worklistComment;
    if (input.value) commentDrafts.set(id, input.value);
    else commentDrafts.delete(id);
    input.classList.remove('wl-comment-missing');
  });

  panel.addEventListener('click', e => {
    const runLink = e.target.closest('button[data-worklist-run]');
    if (runLink) {
      openRunInRunsView(runLink.dataset.worklistRun);
      return;
    }
    const actionBtn = e.target.closest('button[data-worklist-action]');
    if (actionBtn) submitWorklistAction(actionBtn.dataset.worklistItem, actionBtn.dataset.worklistAction);
  });

  // Live refresh: ride the dashboard's 2s snapshot poll. The custom event
  // keeps the dependency one-way (refresh.js doesn't import this module).
  // Only while the Processes tab is on screen — the badge this feeds lives
  // inside it, so polling on other tabs would be pure waste.
  document.addEventListener('tclaude:snapshot', () => {
    const tab = $('#tab-processes');
    if (!tab || !tab.classList.contains('active')) return;
    if (document.body.classList.contains('hide-processes')) return;
    loadProcessWorklist({ quiet: true });
  });
}

// loadProcessWorklist fetches the full worklist and re-renders. quiet renders
// without touching the shared #process-notice (the poll path — the notice
// belongs to whatever sub-view the human is looking at).
export async function loadProcessWorklist({ quiet = false } = {}) {
  const mount = $('#process-worklist-list');
  if (!mount) return;
  try {
    const body = await processJSON('/v1/process/worklist');
    lastWorklist = { items: body.items || [], degradedRuns: body.degradedRuns || [] };
    renderFromCache();
    if (!quiet && worklistPanelActive()) {
      const n = actionableCount(lastWorklist.items);
      processNotice(`${n} actionable item${n === 1 ? '' : 's'}`);
    }
  } catch (error) {
    // A fetch fault must not blank a previously-rendered list (poll parity
    // with stitchListPage): keep the stale rows, surface the failure.
    if (!lastWorklist) morphInto(mount, `<p class="error">Could not load worklist: ${esc(error.message)}</p>`);
    if (!quiet) processNotice(`worklist failed: ${error.message}`);
  }
}

function worklistPanelActive() {
  const panel = $('#process-panel-worklist');
  return !!panel && panel.classList.contains('active');
}

// renderFromCache repaints everything derived from lastWorklist: the sub-nav
// badge, the per-view chip counts, the degraded strip, and (when the Worklist
// panel is the visible one) the item table itself.
function renderFromCache() {
  if (!lastWorklist) return;
  const now = Date.now();
  const { items, degradedRuns } = lastWorklist;

  const badge = $('#process-worklist-badge');
  if (badge) {
    const n = actionableCount(items);
    badge.textContent = String(n);
    badge.hidden = n === 0;
  }

  const counts = viewCounts(items, now);
  for (const v of WORKLIST_VIEWS) {
    const el = document.querySelector(`button[data-worklist-view="${v.key}"] .wl-view-count`);
    if (el) el.textContent = counts[v.key] ? String(counts[v.key]) : '';
  }

  renderDegradedStrip(degradedRuns);

  const mount = $('#process-worklist-list');
  if (mount && worklistPanelActive()) {
    morphInto(mount, renderWorklistView(items, activeView, now));
    if (!items.length && !degradedRuns.length) describeEmptyWorklist(mount);
  }
}

function syncViewChips() {
  document.querySelectorAll('button[data-worklist-view]').forEach(chip => {
    const active = chip.dataset.worklistView === activeView;
    chip.classList.toggle('active', active);
    chip.setAttribute('aria-pressed', active ? 'true' : 'false');
  });
}

// renderDegradedStrip surfaces unreadable runs — NEVER silently dropped. The
// engine derived the items above from the runs it COULD read; this names the
// ones it couldn't, so an empty view can't masquerade as "all caught up".
function renderDegradedStrip(degradedRuns) {
  const strip = $('#process-worklist-degraded');
  if (!strip) return;
  const runs = degradedRuns || [];
  strip.hidden = runs.length === 0;
  if (!runs.length) { strip.replaceChildren(); return; }
  const names = runs.map(d =>
    `<span class="wl-degraded-run" data-key="degraded-${esc(d.run)}" title="${esc(d.error || '')}">${esc(d.run)}</span>`,
  ).join(', ');
  morphInto(strip,
    `<span class="wl-degraded-glyph">⚠</span> ${runs.length} run${runs.length === 1 ? '' : 's'} could not be read `
    + `(their work items are missing from this list): ${names}`);
}

function renderWorklistView(items, view, now) {
  const rows = viewItems(items, view, now);
  if (!rows.length) {
    const total = items.filter(i => i.status === 'pending').length;
    const label = WORKLIST_VIEWS.find(v => v.key === view)?.label || view;
    const others = total ? `<p>${total} pending item${total === 1 ? '' : 's'} in other views.</p>` : '';
    return `<div class="process-placeholder"><h3>Nothing in “${esc(label)}”</h3>${others}</div>`;
  }
  const header = `<thead><tr><th>Kind</th><th>Work item</th><th>Run / node</th><th>Assignee</th><th>Age</th><th>Due</th><th>Actions</th></tr></thead>`;
  let body;
  if (view === 'waiting-on') {
    body = groupWaitingOn(rows).map(g =>
      `<tr class="wl-group-head" data-key="who-${esc(g.assignee || 'unassigned')}"><td colspan="7">Waiting on ${esc(g.label)} · ${g.items.length}</td></tr>`
      + g.items.map(i => renderItemRow(i, now)).join(''),
    ).join('');
  } else {
    body = rows.map(i => renderItemRow(i, now)).join('');
  }
  return `<table>${header}<tbody>${body}</tbody></table>`;
}

function renderItemRow(item, now) {
  const meta = kindMeta(item.kind);
  const bucket = dueBucket(item, now);
  const rowClass = ['wl-row', bucket ? `wl-${bucket}` : '', item.status !== 'pending' ? 'wl-resolved' : ''].filter(Boolean).join(' ');
  const nudge = nudgeLine(item.nudge);
  const statusPill = item.status !== 'pending'
    ? ` <span class="process-status">${esc(item.status)}</span>` : '';
  return `<tr class="${rowClass}" data-key="${esc(item.id)}">
    <td class="wl-kind"><span class="wl-glyph">${meta.glyph}</span> ${esc(meta.label)}${statusPill}</td>
    <td class="wl-main"><div class="wl-summary">${esc(item.summary || '—')}</div>${nudge ? `<div class="wl-nudge process-secondary${item.nudge && item.nudge.paused ? ' wl-paused' : ''}">${esc(nudge)}</div>` : ''}</td>
    <td class="wl-where"><button class="wl-link" data-worklist-run="${esc(item.run)}" type="button" title="Open in the Runs view (the live viewer lands with TCL-301)">${esc(item.run)}</button><div class="process-secondary"><button class="wl-link wl-link-node" data-worklist-run="${esc(item.run)}" type="button" title="Node ${esc(item.node)} — opens the run in the Runs view">${esc(item.node)}</button>${item.attempt > 1 ? ` · attempt ${item.attempt}` : ''}</div></td>
    <td class="wl-assignee">${esc(actorLabel(item.assignee))}</td>
    <td class="wl-age" title="${esc(item.createdAt || 'not recorded (TCL-303)')}">${esc(fmtAge(item.createdAt, now))}</td>
    <td class="wl-due" title="${esc(item.dueAt || 'no deadline recorded')}">${esc(fmtDue(item.dueAt, now))}</td>
    <td class="wl-actions">${renderItemActions(item)}</td>
  </tr>`;
}

function renderItemActions(item) {
  if (item.kind === 'agent-obligation') {
    return `<span class="process-secondary" title="Agent obligations are reported by the working agent through the run/node report route with a durable evidence ref — they cannot be resolved from this list.">agent reports via evidence</span>`;
  }
  if (!isActionable(item)) return '—';
  const draft = commentDrafts.get(item.id) || '';
  const buttons = (item.availableActions || []).map(a =>
    `<button class="process-action wl-action" data-worklist-action="${esc(a)}" data-worklist-item="${esc(item.id)}" type="button">${esc(a)}</button>`,
  ).join('');
  return `<input class="wl-comment" type="text" data-worklist-comment="${esc(item.id)}" placeholder="Comment (required)" value="${esc(draft)}" aria-label="Comment for ${esc(item.summary || item.id)}"><div class="wl-action-row">${buttons}</div>`;
}

// describeEmptyWorklist upgrades the bare zero-state after the fact: one extra
// runs fetch (only on the empty path — never on the 2s poll with items) tells
// apart "no runs exist yet" from "runs are flowing and nothing needs you".
async function describeEmptyWorklist(mount) {
  let text = '<h3>All caught up</h3><p>No process run is waiting on anyone.</p>';
  try {
    const body = await processJSON('/v1/process/runs');
    const runs = body.runs || [];
    if (!runs.length) {
      text = '<h3>No process runs yet</h3><p>The worklist fills as instantiated runs wait on people or hit blocks.</p>';
    } else {
      const running = runs.filter(r => r.status === 'running').length;
      text = `<h3>All caught up</h3><p>${runs.length} run${runs.length === 1 ? '' : 's'}`
        + `${running ? ` (${running} running)` : ''} — nothing is waiting on a human right now.</p>`;
    }
  } catch { /* keep the generic zero-state */ }
  // Re-check emptiness: a poll may have landed items while the runs fetch was
  // in flight.
  if (lastWorklist && lastWorklist.items.length) return;
  morphInto(mount, `<div class="process-placeholder">${text}</div>`);
}

// submitWorklistAction is the single action funnel: required comment,
// confirm step for destructive verbs, fresh idempotency key per click,
// advertised action spelling, then a plain re-fetch (no optimistic UI).
async function submitWorklistAction(itemID, action) {
  if (actionInFlight) return;
  const item = lastWorklist?.items.find(i => i.id === itemID);
  if (!item) return;
  const comment = (commentDrafts.get(itemID) || '').trim();
  if (!comment) {
    const input = document.querySelector(`input[data-worklist-comment="${CSS.escape(itemID)}"]`);
    if (input) { input.classList.add('wl-comment-missing'); input.focus(); }
    processNotice('A comment is required for every worklist action.');
    return;
  }
  actionInFlight = true;
  try {
    if (isDestructiveAction(action)) {
      const ok = await confirmModal({
        title: `${action} — are you sure?`,
        body: `“${action}” on ${item.node} (run ${item.run}) is recorded durably in the run's audit log and drives the run forward.`,
        meta: item.summary || '',
        okLabel: action,
      });
      if (!ok) return;
    }
    const request = buildWorklistAction(item, action, comment, crypto.randomUUID());
    if (!request) return;
    const response = await fetch(request.path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      body: JSON.stringify(request.body),
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
    commentDrafts.delete(itemID);
    toast(`${request.body.action} recorded for ${item.node}`);
  } catch (error) {
    toast(`worklist action failed: ${error.message}`, true);
  } finally {
    actionInFlight = false;
  }
  await loadProcessWorklist();
}

// openRunInRunsView deep-links an item's run/node toward the viewer. Until
// TCL-301 lands the target is the Runs sub-view entry: switch sub-tabs, wait
// for the row to render (the list loads async), then flash it.
async function openRunInRunsView(runID) {
  activateProcessSubtab('runs');
  const deadline = Date.now() + 3000;
  let row = null;
  while (Date.now() < deadline) {
    row = document.querySelector(`[data-process-run="${CSS.escape(runID)}"]`);
    if (row) break;
    await new Promise(resolve => setTimeout(resolve, 60));
  }
  if (!row) { processNotice(`Run ${runID} is not in the runs list.`); return; }
  row.scrollIntoView({ block: 'center', behavior: 'smooth' });
  row.classList.remove('wl-run-flash');
  // Force a reflow so re-adding the class restarts the one-shot animation.
  void row.offsetWidth;
  row.classList.add('wl-run-flash');
}
