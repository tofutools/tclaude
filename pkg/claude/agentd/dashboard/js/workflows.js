// workflows.js — the "Workflows" tab: a read-only view of Claude Code's
// builtin workflow runs and saved templates, served by /api/workflows over the
// ccworkflows data layer.
//
// Master/detail: the list (saved templates + runs) is always shown; clicking a
// run fetches /api/workflows/{runId} and renders its phase → agent fan-out tree
// plus the script, below the list. v1 is poll-on-demand: the tab loads when
// activated and re-loads on the ↻ button (live tailing is a later slice).

import { $, esc, shortId, relTime, shortCwd } from './helpers.js';

let listData = null;       // last /api/workflows payload
let selectedRunId = null;  // currently-drilled run
let detailData = null;     // last /api/workflows/{runId} payload
let filterText = '';

// renderWorkflowsTab (re)loads the list and paints. Safe to call repeatedly.
export async function renderWorkflowsTab() {
  try {
    const res = await fetch('/api/workflows', { headers: { Accept: 'application/json' } });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    listData = await res.json();
  } catch (e) {
    const el = $('#workflows-list');
    if (el) el.innerHTML = `<div class="wf-empty">Failed to load workflows: ${esc(e.message || String(e))}</div>`;
    return;
  }
  paintList();
  // Keep an open drill-down honest across reloads. A vanished run is cleared;
  // an in-flight run is live-re-synced (this is what drives live progress under
  // the auto-poll); a finished run is left stable so the user can inspect it
  // (its script panel / scroll position survive the poll).
  if (selectedRunId) {
    const sel = (listData.runs || []).find((r) => r.runId === selectedRunId);
    if (!sel) {
      selectedRunId = null;
      detailData = null;
      const det = $('#workflows-detail');
      if (det) det.innerHTML = '';
    } else if (sel.status === 'running') {
      loadDetail(selectedRunId, { silent: true });
    }
  }
}

// workflowsTabActive reports whether the Workflows tab is the visible one — the
// gate for auto-polling (no background fetches when the tab/page is hidden).
function workflowsTabActive() {
  const sec = $('#tab-workflows');
  return !!sec && sec.classList.contains('active') && document.visibilityState === 'visible';
}

function fmtTimeMs(ms) {
  if (!ms || ms <= 0) return '—';
  return relTime(new Date(ms).toISOString());
}

function fmtDurationMs(ms) {
  if (!ms || ms <= 0) return '—';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  const m = Math.floor(ms / 60000);
  const s = Math.floor((ms % 60000) / 1000);
  return `${m}m${String(s).padStart(2, '0')}s`;
}

function statusPill(status) {
  const s = String(status || 'unknown');
  return `<span class="wf-pill wf-status-${esc(s)}">${esc(s)}</span>`;
}

function matchesFilter(run) {
  if (!filterText) return true;
  const hay = [
    run.runId, run.workflowName, run.status, run.sessionId,
    run.convTitle, run.convCwd, run.gitBranch,
  ].join(' ').toLowerCase();
  return hay.includes(filterText);
}

function paintList() {
  const el = $('#workflows-list');
  if (!el || !listData) return;
  const saved = listData.saved || [];
  const runs = (listData.runs || []).filter(matchesFilter);

  let html = '';

  html += `<div class="wf-section-title">Saved templates <span class="wf-count">${saved.length}</span></div>`;
  if (saved.length === 0) {
    html += `<div class="wf-empty">No saved templates (~/.claude/workflows/saved is empty or absent).</div>`;
  } else {
    html += '<table class="wf-table"><thead><tr><th>Name</th><th>Scope</th><th>Phases</th><th>Description</th></tr></thead><tbody>';
    for (const s of saved) {
      const phases = (s.meta && s.meta.phases) ? s.meta.phases.length : 0;
      const desc = (s.meta && s.meta.description) || '';
      html += `<tr><td class="wf-mono">${esc(s.name)}</td><td>${esc(s.scope || '')}</td><td>${phases}</td><td>${esc(desc)}</td></tr>`;
    }
    html += '</tbody></table>';
  }

  html += `<div class="wf-section-title">Runs <span class="wf-count">${runs.length}</span></div>`;
  if (runs.length === 0) {
    html += `<div class="wf-empty">No workflow runs found${filterText ? ' matching the filter' : ''}.</div>`;
  } else {
    html += '<table class="wf-table"><thead><tr><th>Status</th><th>Run</th><th>Workflow</th><th>Agents</th><th>Started</th><th>Launched by</th></tr></thead><tbody>';
    for (const r of runs) {
      const sel = r.runId === selectedRunId ? ' class="wf-row-selected"' : '';
      const launched = r.convTitle
        ? `${esc(r.convTitle)}${r.convCwd ? ' <span class="wf-dim">· ' + esc(shortCwd(r.convCwd)) + '</span>' : ''}`
        : `<span class="wf-dim">session ${esc(shortId(r.sessionId || ''))}</span>`;
      html += `<tr data-wf-run="${esc(r.runId)}"${sel}>` +
        `<td>${statusPill(r.status)}</td>` +
        `<td class="wf-mono">${esc(r.runId)}</td>` +
        `<td>${esc(r.workflowName || '—')}</td>` +
        `<td>${r.agentCount > 0 ? r.agentCount : '—'}</td>` +
        `<td>${fmtTimeMs(r.startTimeMs)}</td>` +
        `<td>${launched}</td>` +
        '</tr>';
    }
    html += '</tbody></table>';
  }

  el.innerHTML = html;
}

// loadDetail fetches and renders one run's drill-down. `silent` skips the
// "Loading…" placeholder + detail-clear, for a background re-sync on refresh.
// A response is only applied if its runId is still the selected one, so a slow
// response for run A can never clobber a newer click on run B.
async function loadDetail(runId, { silent = false } = {}) {
  selectedRunId = runId;
  const el = $('#workflows-detail');
  if (!silent) {
    detailData = null;
    if (el) el.innerHTML = '<div class="wf-empty">Loading run…</div>';
  }
  let payload;
  try {
    const res = await fetch('/api/workflows/' + encodeURIComponent(runId), { headers: { Accept: 'application/json' } });
    if (!res.ok) {
      let msg = 'HTTP ' + res.status;
      try { msg = (await res.json()).error || msg; } catch { /* keep status */ }
      throw new Error(msg);
    }
    payload = await res.json();
  } catch (e) {
    if (runId !== selectedRunId) return; // a newer selection superseded us
    if (el) el.innerHTML = `<div class="wf-empty">Failed to load run ${esc(runId)}: ${esc(e.message || String(e))}</div>`;
    return;
  }
  if (runId !== selectedRunId) return; // stale response — newer selection won
  detailData = payload;
  paintList(); // refresh row highlight
  paintDetail();
}

function paintDetail() {
  const el = $('#workflows-detail');
  if (!el || !detailData) return;
  const d = detailData;

  const phases = d.phases || [];
  const agents = d.agents || [];
  const byPhase = new Map();
  for (const a of agents) {
    // Absent phaseIndex (omitempty drops 0) → -1, a sentinel that never matches
    // a real 1-based phase, so unphased agents fall through to "Unassigned".
    const k = a.phaseIndex ?? -1;
    if (!byPhase.has(k)) byPhase.set(k, []);
    byPhase.get(k).push(a);
  }

  let html = '';
  html += '<div class="wf-detail-head">';
  html += `<button class="wf-close" data-wf-close title="Close run">✕</button>`;
  html += `<span class="wf-mono wf-detail-id">${esc(d.runId)}</span> ${statusPill(d.status)}`;
  if (d.workflowName) html += ` <span class="wf-detail-name">${esc(d.workflowName)}</span>`;
  html += '</div>';

  const join = d.join || {};
  const meta = [
    join.convTitle ? `launched by ${esc(join.convTitle)}` : (join.sessionId ? `session ${esc(shortId(join.sessionId))}` : ''),
    join.gitBranch ? `branch ${esc(join.gitBranch)}` : '',
    `started ${fmtTimeMs(d.startTimeMs)}`,
    `duration ${fmtDurationMs(d.durationMs)}`,
    d.totalTokens ? `${d.totalTokens.toLocaleString()} tokens` : '',
    `source ${esc(d.source || '')}`,
  ].filter(Boolean).join(' · ');
  html += `<div class="wf-detail-meta">${meta}</div>`;
  if (d.summary) html += `<div class="wf-detail-summary">${esc(d.summary)}</div>`;

  if (d.source === 'journal') {
    html += '<div class="wf-note">In-flight run reconstructed from the live journal; labels are best-effort (marked ~) where the script fans out dynamically.</div>';
  }

  // Phase → agent tree.
  html += '<div class="wf-tree">';
  const renderAgents = (list) => {
    let out = '';
    for (const a of (list || [])) {
      const conf = a.labelConfident === false ? '<span class="wf-approx" title="best-effort label (dynamic fan-out)">~</span> ' : '';
      const label = a.label || shortId(a.id || '');
      out += `<div class="wf-agent">` +
        `<span class="wf-pill wf-state-${esc(a.state || '')}">${esc(a.state || '')}</span> ` +
        `${conf}<span class="wf-agent-label">${esc(label)}</span>` +
        `<span class="wf-dim">${a.model ? ' · ' + esc(a.model) : ''}` +
        `${a.tokens ? ' · ' + a.tokens.toLocaleString() + ' tok' : ''}` +
        `${a.toolCalls ? ' · ' + a.toolCalls + ' tools' : ''}` +
        `${a.lastTool ? ' · ' + esc(a.lastTool) : ''}</span>` +
        '</div>';
    }
    return out;
  };
  for (const p of phases) {
    html += `<div class="wf-phase">` +
      `<div class="wf-phase-head"><span class="wf-pill wf-state-${esc(p.status || '')}">${esc(p.status || '—')}</span> ` +
      `<span class="wf-phase-title">Phase ${p.index}: ${esc(p.title)}</span>` +
      `${p.detail ? '<span class="wf-dim"> — ' + esc(p.detail) + '</span>' : ''}</div>`;
    html += renderAgents(byPhase.get(p.index));
    byPhase.delete(p.index);
    html += '</div>';
  }
  // Agents not mapped to a known phase.
  const orphans = [];
  for (const list of byPhase.values()) orphans.push(...list);
  if (orphans.length) {
    html += `<div class="wf-phase"><div class="wf-phase-head"><span class="wf-phase-title">Unassigned agents</span></div>`;
    html += renderAgents(orphans);
    html += '</div>';
  }
  html += '</div>';

  // Script (collapsed).
  if (d.script) {
    html += `<details class="wf-script"><summary>Script</summary><pre>${esc(d.script)}</pre></details>`;
  }

  el.innerHTML = html;
}

// bindWorkflowsTab installs the tab's delegated listeners once. Called at init.
export function bindWorkflowsTab() {
  // Load (and re-load) when the Workflows nav tab is clicked.
  const tabBtn = document.querySelector('nav button[data-tab="workflows"]');
  if (tabBtn) tabBtn.addEventListener('click', () => renderWorkflowsTab());

  // Live progress: poll while the tab is the active, visible one. Matches the
  // dashboard's ~2s snapshot cadence but stays decoupled (its own timer that
  // no-ops when hidden) — the journal is the only live signal, so this is a
  // poll, and an in-flight open run re-syncs each tick (see renderWorkflowsTab).
  // `polling` skips a tick while the previous one is still in flight so slow
  // fetches (e.g. a large machine-wide scan) can't stack up into a herd.
  const POLL_MS = 2000;
  let polling = false;
  setInterval(() => {
    if (polling || !workflowsTabActive()) return;
    polling = true;
    renderWorkflowsTab().finally(() => { polling = false; });
  }, POLL_MS);

  const section = $('#tab-workflows');
  if (!section) return;

  // Run-row click → drill in; close button → clear the detail.
  section.addEventListener('click', (e) => {
    if (e.target.closest('[data-wf-close]')) {
      selectedRunId = null;
      detailData = null;
      const det = $('#workflows-detail');
      if (det) det.innerHTML = '';
      paintList();
      return;
    }
    const row = e.target.closest('[data-wf-run]');
    if (row) loadDetail(row.getAttribute('data-wf-run'));
  });

  // Manual refresh.
  const refreshBtn = $('#workflows-refresh');
  if (refreshBtn) refreshBtn.addEventListener('click', () => renderWorkflowsTab());

  // Filter.
  const input = $('#filter-workflows');
  const clear = $('#filter-workflows-clear');
  const key = 'tclaude.dash.filter.workflows';
  if (input) {
    input.value = localStorage.getItem(key) || '';
    filterText = input.value.trim().toLowerCase();
    input.addEventListener('input', () => {
      const v = input.value;
      if (v) localStorage.setItem(key, v); else localStorage.removeItem(key);
      filterText = v.trim().toLowerCase();
      paintList();
    });
  }
  if (clear) clear.addEventListener('click', () => {
    if (input) { input.value = ''; localStorage.removeItem(key); filterText = ''; paintList(); input.focus(); }
  });
}
