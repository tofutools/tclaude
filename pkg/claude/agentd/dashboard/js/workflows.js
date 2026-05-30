// workflows.js — the Workflows tab: template list + running instances,
// a live mermaid graph with per-node status coloring, a live-vitals
// overlay on ai nodes, and the manual drive controls (instantiate,
// advance/settle a node, cancel, delete).
//
// Data source: the main /api/snapshot poll already carries
// `workflows` (instance summary rows) and `workflow_templates`
// (discoverable templates). Per-instance detail — the snapshotted
// mermaid chart, the node states and the event timeline — comes from
// GET /api/workflows/{id}. Mutations go to the dashboard's
// /api/workflows* surface (POST create, PATCH a node to advance, POST
// cancel, DELETE). All of that is Step 3/4's backend; this module is
// the front-end only.
//
// Mermaid is vendored locally (v9.4.3 UMD, self-contained) and loaded
// via a plain <script> in dashboard.html, so it lands on window.mermaid
// — there is no CDN dependency and no ES-module chunk loading.

import { $, $$, esc, shortId, relTime, statePill } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { refresh, confirmModal, toast, bindBackdropDiscard } from './refresh.js';

// ---- module state -----------------------------------------------------

// The instance whose detail pane is open (null = none selected). The
// node selected within that instance's graph (null = none).
let selectedInstanceId = null;
let selectedNodeId = null;
// Last detail payload fetched for the selected instance — rendered by
// the side panel + timeline so a 2s poll can refresh them without a
// graph re-render.
let currentDetail = null;
// detailSeq drops stale in-flight detail fetches: each fetch captures
// the current value and only applies its result if still latest.
let detailSeq = 0;
// Graph-render bookkeeping: the instance id + mermaid signature the
// currently-rendered SVG belongs to, so refreshDetail() recolors in
// place (no flicker) and only does a full mermaid re-render when the
// selected instance — or its snapshotted chart — actually changes.
let graphRenderedFor = null;
let graphRenderedSig = '';
let mermaidRenderSeq = 0;
let mermaidReady = false;

// Node status → the mermaid classDef name we attach for coloring, and
// the CSS class list we toggle for in-place recolor. Mirrors the db
// WorkflowNodeStatus* constants.
const WF_STATUS_CLASS = {
  pending: 'wfPending',
  ready: 'wfReady',
  running: 'wfRunning',
  awaiting_verify: 'wfAwaiting',
  done: 'wfDone',
  failed: 'wfFailed',
  skipped: 'wfSkipped',
};
const WF_ALL_STATUS_CLASSES = Object.values(WF_STATUS_CLASS);

// Instance status → pill class (reuses the dashboard's state-pill
// palette so workflows read like the rest of the UI).
function instStatusClass(s) {
  if (s === 'completed') return 'state-idle';
  if (s === 'running') return 'state-working';
  if (s === 'failed') return 'state-error';
  if (s === 'cancelled') return 'state-offline';
  return 'state-offline';
}
function instStatusPill(s) {
  return `<span class="state-pill ${instStatusClass(s)}" title="${esc(s)}">${esc(s || 'unknown')}</span>`;
}

// ---- tab render -------------------------------------------------------

function filterInstances(list, q) {
  if (!q) return list;
  const n = q.toLowerCase();
  return list.filter(w =>
    (w.title || '').toLowerCase().includes(n) ||
    (w.template_name || '').toLowerCase().includes(n) ||
    (w.template_ref || '').toLowerCase().includes(n) ||
    (w.status || '').toLowerCase().includes(n) ||
    (w.group_name || '').toLowerCase().includes(n));
}

function renderWorkflowsTab() {
  if (!lastSnapshot) return;
  const q = ($('#filter-workflows') && $('#filter-workflows').value) || '';
  const templates = lastSnapshot.workflow_templates || [];
  const instances = lastSnapshot.workflows || [];
  const shown = filterInstances(instances, q);

  const countEl = $('#filter-workflows-count');
  if (countEl) {
    countEl.textContent = q
      ? `${shown.length} / ${instances.length}`
      : `${instances.length} instance${instances.length === 1 ? '' : 's'}`;
  }

  const tHost = $('#wf-templates-list');
  if (tHost) tHost.innerHTML = templatesListHTML(templates);
  const iHost = $('#wf-instances-list');
  if (iHost) iHost.innerHTML = instancesListHTML(shown);

  // Drop a selection that no longer exists (deleted instance).
  if (selectedInstanceId != null && !instances.some(w => w.id === selectedInstanceId)) {
    selectedInstanceId = null;
    selectedNodeId = null;
    currentDetail = null;
    graphRenderedFor = null;
  }
  markSelectedInstanceRow();

  // Keep the open detail pane live: re-fetch on each poll (fire and
  // forget — the seq guard drops stale responses). Only while the
  // Workflows tab is actually showing, so a selected instance doesn't
  // keep polling GET /api/workflows/{id} from a backgrounded tab; the
  // next poll resumes within 2s once the user switches back.
  if (selectedInstanceId != null) {
    if (workflowsTabActive()) refreshDetail();
  } else {
    renderDetailEmpty();
  }
}

// workflowsTabActive reports whether the Workflows tab section is the
// visible one (bindTabs toggles .active on the shown <main> section).
function workflowsTabActive() {
  const s = $('#tab-workflows');
  return !!(s && s.classList.contains('active'));
}

function templatesListHTML(templates) {
  if (!templates.length) {
    return `<div class="wf-empty">No workflow templates found. Drop a <code>workflow.yaml</code> + <code>flow.mmd</code> under a project/user workflows dir, or use the shipped <code>example:</code> ones.</div>`;
  }
  return templates.map(t => {
    const broken = !!t.err;
    const warns = Array.isArray(t.warnings) ? t.warnings : [];
    const meta = [
      t.source ? `<span class="wf-tag">${esc(t.source)}</span>` : '',
      `<span class="wf-muted">${t.node_count} node${t.node_count === 1 ? '' : 's'}</span>`,
      warns.length ? `<span class="wf-warn-chip" title="${esc(warns.join('\n'))}">⚠ ${warns.length}</span>` : '',
    ].filter(Boolean).join(' ');
    const instBtn = broken
      ? `<button class="tool" disabled title="${esc(t.err)}">⚠ broken</button>`
      : `<button class="primary" data-wfact="instantiate" data-ref="${esc(t.ref)}" title="Instantiate this workflow">⎘ instantiate</button>`;
    return `<div class="wf-tpl-card${broken ? ' wf-broken' : ''}" data-ref="${esc(t.ref)}">
      <div class="wf-tpl-head">
        <span class="wf-tpl-name">${esc(t.name)}</span>
        <span class="wf-tpl-actions">${instBtn}</span>
      </div>
      ${t.description ? `<div class="wf-tpl-descr">${esc(t.description)}</div>` : ''}
      <div class="wf-tpl-meta">${meta}<span class="wf-ref">${esc(t.ref)}</span></div>
      ${broken ? `<div class="wf-tpl-err">⚠ ${esc(t.err)}</div>` : ''}
    </div>`;
  }).join('');
}

function instancesListHTML(instances) {
  if (!instances.length) {
    return `<div class="wf-empty">No workflow instances yet. Instantiate a template on the left to start one.</div>`;
  }
  return instances.map(w => {
    const total = w.total || 0;
    const done = w.done || 0;
    const failed = w.failed || 0;
    const running = w.running || 0;
    const pct = total ? Math.round((done / total) * 100) : 0;
    const grp = w.group_name
      ? `<span class="wf-tag" title="bound group">${esc(w.group_name)}</span>` : '';
    return `<div class="wf-inst-row" data-wfact="select" data-id="${w.id}" tabindex="0" role="button" title="Show this instance">
      <div class="wf-inst-main">
        <span class="wf-inst-title">${esc(w.title || ('#' + w.id))}</span>
        ${instStatusPill(w.status)}
        ${grp}
      </div>
      <div class="wf-inst-sub">
        <span class="wf-muted">${esc(w.template_name || w.template_ref || '')}</span>
        <span class="wf-progress" title="${done} done / ${running} running / ${failed} failed of ${total}">
          <span class="wf-progress-bar"><span class="wf-progress-fill" style="width:${pct}%"></span></span>
          <span class="wf-muted">${done}/${total}${failed ? ` · <span class="wf-fail">${failed}✗</span>` : ''}</span>
        </span>
      </div>
    </div>`;
  }).join('');
}

function markSelectedInstanceRow() {
  $$('#wf-instances-list .wf-inst-row').forEach(r => {
    r.classList.toggle('wf-selected-row', Number(r.dataset.id) === selectedInstanceId);
  });
}

// ---- detail pane ------------------------------------------------------

function renderDetailEmpty() {
  const host = $('#wf-detail');
  if (host) {
    // Clearing instId forces a clean scaffold rebuild if this same host
    // later shows an instance again (the placeholder blew the panes away).
    host.dataset.instId = '';
    host.innerHTML = `<div class="wf-empty wf-detail-empty">Select an instance to see its graph, node states and live agent vitals.</div>`;
  }
}

async function selectInstance(id) {
  selectedInstanceId = id;
  selectedNodeId = null;
  graphRenderedFor = null; // force a fresh graph render for the new instance
  markSelectedInstanceRow();
  const host = $('#wf-detail');
  if (host) {
    host.dataset.instId = ''; // the Loading placeholder replaces the scaffold
    host.innerHTML = `<div class="wf-empty wf-detail-empty">Loading…</div>`;
  }
  await refreshDetail();
}

// refreshDetail re-fetches the selected instance's detail and updates
// the pane. The graph is only fully re-rendered when the instance (or
// its snapshotted chart) changes; otherwise node colors are updated in
// place so the 2s poll never flickers the SVG.
async function refreshDetail() {
  const id = selectedInstanceId;
  if (id == null) return;
  const seq = ++detailSeq;
  let detail;
  try {
    const r = await fetch(`/api/workflows/${id}`, { credentials: 'same-origin' });
    if (!r.ok) {
      if (seq === detailSeq) {
        const host = $('#wf-detail');
        if (host) host.innerHTML = `<div class="wf-empty wf-detail-empty">Failed to load instance #${id}: HTTP ${r.status}</div>`;
      }
      return;
    }
    detail = await r.json();
  } catch (e) {
    return; // transient — next poll retries
  }
  if (seq !== detailSeq || selectedInstanceId !== id) return; // superseded
  currentDetail = detail;
  await renderDetail(detail);
}

async function renderDetail(detail) {
  const host = $('#wf-detail');
  if (!host) return;
  const inst = detail.instance || {};
  const nodes = detail.nodes || [];
  const sig = detail.mermaid || '';

  // Build the static scaffold once per instance/chart; thereafter we
  // patch sub-regions in place.
  const needScaffold = host.dataset.instId !== String(inst.id);
  if (needScaffold) {
    host.dataset.instId = String(inst.id);
    host.innerHTML = detailScaffoldHTML(inst);
  }
  // Header (status can change on every poll).
  const hdr = $('#wf-detail-head');
  if (hdr) hdr.innerHTML = detailHeadHTML(inst);

  // Warnings banner (feature-detected: present once Step 4 surfaces
  // detail.warnings; the template row's warnings are a fallback).
  renderWarnings(detail);

  // Graph: full render only when the *built* definition changes — i.e.
  // the chart itself or any node's status. Mermaid emits classDef CSS
  // only for the statuses present at render time, so a node advancing
  // into a not-yet-seen status must re-render rather than rely on an
  // in-place class toggle with no matching style. Steady-state polls
  // (no status change) keep the same def → no re-render, no flicker.
  const def = sig ? buildGraphDef(sig, nodes) : '';
  if (graphRenderedFor !== inst.id || graphRenderedSig !== def) {
    const ok = await renderGraph(sig, def, nodes);
    if (ok) { graphRenderedFor = inst.id; graphRenderedSig = def; }
    else { graphRenderedFor = null; graphRenderedSig = ''; } // retry next poll
  } else {
    applyNodeColors(nodes);
  }
  markSelectedNodeInGraph();

  // Side panel (selected node + vitals + actions) and the timeline.
  renderSidePanel(detail);
  renderTimeline(detail);
}

function detailScaffoldHTML(inst) {
  return `
    <div id="wf-detail-head"></div>
    <div id="wf-warnings"></div>
    <div class="wf-detail-body">
      <div class="wf-graph-wrap">
        <div id="wf-graph" class="wf-graph"><div class="wf-empty">Rendering graph…</div></div>
      </div>
      <div id="wf-side" class="wf-side"></div>
    </div>
    <details class="wf-timeline-details">
      <summary>Event timeline</summary>
      <div id="wf-timeline" class="wf-timeline"></div>
    </details>`;
}

function detailHeadHTML(inst) {
  const ended = (inst.status === 'completed' || inst.status === 'failed' || inst.status === 'cancelled');
  const cancelBtn = ended ? ''
    : `<button class="tool" data-wfact="cancel" data-id="${inst.id}" title="Cancel this instance — every non-terminal node is marked skipped">⏹ cancel</button>`;
  const delBtn = `<button class="danger" data-wfact="delete" data-id="${inst.id}" title="Delete this instance and its history">🗑 delete</button>`;
  return `
    <div class="wf-detail-title">
      <span class="wf-inst-title">${esc(inst.title || ('#' + inst.id))}</span>
      ${instStatusPill(inst.status)}
      <span class="wf-muted">${esc(inst.template_name || inst.template_ref || '')}</span>
      <span class="wf-detail-actions">${cancelBtn}${delBtn}</span>
    </div>`;
}

function renderWarnings(detail) {
  const host = $('#wf-warnings');
  if (!host) return;
  let warns = Array.isArray(detail.warnings) ? detail.warnings.slice() : [];
  // Fallback to the template row's warnings if the detail payload
  // doesn't carry them yet (feature-detect Step 4's field).
  if (!warns.length) {
    const ref = (detail.instance && detail.instance.template_ref) || '';
    const trow = ((lastSnapshot && lastSnapshot.workflow_templates) || []).find(t => t.ref === ref);
    if (trow && Array.isArray(trow.warnings)) warns = trow.warnings.slice();
  }
  if (!warns.length) { host.innerHTML = ''; return; }
  host.innerHTML = `<div class="wf-warn-banner" title="static topology analysis">
    <span class="wf-warn-ico">⚠</span>
    <div class="wf-warn-list">${warns.map(wn => `<div>${esc(wn)}</div>`).join('')}</div>
  </div>`;
}

// ---- mermaid graph ----------------------------------------------------

function ensureMermaid() {
  if (mermaidReady) return !!window.mermaid;
  if (!window.mermaid) return false;
  try {
    window.mermaid.initialize({
      startOnLoad: false,
      securityLevel: 'strict',
      theme: 'dark',
      flowchart: { useMaxWidth: true, htmlLabels: true, curve: 'basis' },
    });
  } catch (_) { /* older builds tolerate re-init */ }
  mermaidReady = true;
  return true;
}

// buildGraphDef appends classDef + class statements to the snapshotted
// mermaid so each node is colored by its current status.
function buildGraphDef(mermaidText, nodes) {
  const defs = [
    'classDef wfPending fill:#2a2a33,stroke:#555,color:#b8b8c0;',
    'classDef wfReady fill:#16324d,stroke:#4a90d9,color:#dce6f5;',
    'classDef wfRunning fill:#5a4410,stroke:#e0a82e,color:#fff;',
    'classDef wfAwaiting fill:#3a2456,stroke:#9a6fd9,color:#fff;',
    'classDef wfDone fill:#14401f,stroke:#3fae5f,color:#dff5e6;',
    'classDef wfFailed fill:#511b1b,stroke:#e05252,color:#fff;',
    'classDef wfSkipped fill:#222226,stroke:#3a3a40,color:#6a6a72;',
  ];
  const byClass = {};
  for (const n of nodes) {
    const cls = WF_STATUS_CLASS[n.status] || 'wfPending';
    (byClass[cls] = byClass[cls] || []).push(n.node_id);
  }
  const classLines = Object.entries(byClass).map(
    ([cls, ids]) => `class ${ids.map(cssId).join(',')} ${cls};`);
  return `${mermaidText}\n${defs.join('\n')}\n${classLines.join('\n')}`;
}

// cssId escapes a node id for a mermaid class statement. Mermaid ids are
// already restricted by the parser, but guard against stray spaces.
function cssId(id) { return String(id).trim(); }

// escapeRe escapes regex metacharacters so a node id can be embedded in
// a RegExp literal safely (mermaid ids are normally alnum/underscore,
// but this keeps the whole-segment match robust regardless).
function escapeRe(s) { return String(s).replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }

// renderGraph draws the mermaid SVG and returns true on success. A
// false return (no chart, mermaid not loaded, or a render throw) tells
// the caller to retry on the next poll rather than cache it as drawn.
async function renderGraph(mermaidText, def, nodes) {
  const host = $('#wf-graph');
  if (!host) return false;
  if (!mermaidText) { host.innerHTML = `<div class="wf-empty">This instance has no chart snapshot.</div>`; return true; }
  if (!ensureMermaid()) {
    host.innerHTML = `<div class="wf-empty wf-bad">mermaid failed to load — the vendored bundle did not initialise.</div>`;
    return false;
  }
  const renderId = `wf-mermaid-${++mermaidRenderSeq}`;
  try {
    const svg = await mermaidRender(renderId, def);
    if (!$('#wf-graph')) return false; // pane gone while awaiting
    host.innerHTML = svg;
  } catch (e) {
    host.innerHTML = `<div class="wf-empty wf-bad">graph render failed: ${esc((e && e.message) || String(e))}</div>`;
    return false;
  }
  applyNodeColors(nodes);
  bindGraphClicks();
  return true;
}

// mermaidRender wraps v9's render (synchronous string return, with a
// callback variant) and a possible promise return in newer builds, so
// the caller always gets the SVG string via a Promise.
function mermaidRender(id, def) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const done = (svg) => { if (!settled) { settled = true; clearTimeout(timer); resolve(svg); } };
    const fail = (e) => { if (!settled) { settled = true; clearTimeout(timer); reject(e); } };
    // Guard against a future mermaid build that neither returns a
    // value nor invokes the callback — without this the await would
    // hang forever and the graph would never settle. v9.4.3 always
    // settles synchronously, so this timer is only ever cleared.
    const timer = setTimeout(() => fail(new Error('mermaid render timed out')), 8000);
    try {
      const ret = window.mermaid.render(id, def, (svgCode) => done(svgCode));
      if (typeof ret === 'string') done(ret);
      else if (ret && typeof ret.then === 'function') ret.then(o => done(o && o.svg ? o.svg : o)).catch(fail);
      else if (ret && ret.svg) done(ret.svg);
    } catch (e) { fail(e); }
  });
}

// nodeElements maps node_id → the rendered <g class="node"> element by
// parsing mermaid's "flowchart-<id>-<n>" element ids.
function nodeElements(knownIds) {
  const host = $('#wf-graph');
  const map = {};
  if (!host) return map;
  const ids = new Set(knownIds);
  $$('.node', host).forEach(el => {
    const raw = el.id || '';
    let guess = raw.replace(/^flowchart-/, '').replace(/-\d+$/, '');
    if (!ids.has(guess)) {
      // Fall back to the longest known id that sits on whole-segment
      // boundaries in the element id (mermaid ids look like
      // "flowchart-<id>-<n>"). A plain substring test would let "build"
      // win inside "build_all"; the (^|-)id(-|$) anchors prevent that.
      let best = '';
      ids.forEach(id => {
        const re = new RegExp('(^|-)' + escapeRe(id) + '(-|$)');
        if (re.test(raw) && id.length > best.length) best = id;
      });
      if (best) guess = best;
    }
    if (guess && !map[guess]) map[guess] = el;
  });
  return map;
}

function applyNodeColors(nodes) {
  const map = nodeElements(nodes.map(n => n.node_id));
  for (const n of nodes) {
    const el = map[n.node_id];
    if (!el) continue;
    WF_ALL_STATUS_CLASSES.forEach(c => el.classList.remove(c));
    el.classList.add(WF_STATUS_CLASS[n.status] || 'wfPending');
  }
}

function bindGraphClicks() {
  const host = $('#wf-graph');
  if (!host || host.dataset.clickBound === '1') return;
  host.dataset.clickBound = '1';
  host.addEventListener('click', e => {
    const g = e.target.closest('.node');
    if (!g || !currentDetail) return;
    const ids = (currentDetail.nodes || []).map(n => n.node_id);
    const map = nodeElements(ids);
    const hit = Object.keys(map).find(id => map[id] === g);
    if (!hit) return;
    selectedNodeId = (selectedNodeId === hit) ? null : hit;
    markSelectedNodeInGraph();
    renderSidePanel(currentDetail);
  });
}

function markSelectedNodeInGraph() {
  const host = $('#wf-graph');
  if (!host) return;
  $$('.node.wf-node-selected', host).forEach(el => el.classList.remove('wf-node-selected'));
  if (!selectedNodeId || !currentDetail) return;
  const map = nodeElements((currentDetail.nodes || []).map(n => n.node_id));
  const el = map[selectedNodeId];
  if (el) el.classList.add('wf-node-selected');
}

// ---- side panel: selected node, vitals overlay, drive controls --------

function renderSidePanel(detail) {
  const host = $('#wf-side');
  if (!host) return;
  const nodes = detail.nodes || [];
  if (!selectedNodeId) {
    host.innerHTML = nodeLegendHTML(nodes);
    return;
  }
  const node = nodes.find(n => n.node_id === selectedNodeId);
  if (!node) { host.innerHTML = nodeLegendHTML(nodes); return; }

  const rows = [];
  rows.push(kv('Node', `${esc(node.label || node.node_id)} <span class="wf-ref">${esc(node.node_id)}</span>`));
  rows.push(kv('Executor', esc(node.executor_kind || '—')));
  rows.push(kv('Status', wfStatusBadge(node.status)));
  if (node.outcome) rows.push(kv('Outcome', esc(node.outcome)));
  if (node.visits) rows.push(kv('Visits', String(node.visits)));
  if (node.started_at) rows.push(kv('Started', relTime(node.started_at)));
  if (node.finished_at) rows.push(kv('Finished', relTime(node.finished_at)));
  if (node.output) rows.push(kv('Output', `<span class="wf-output">${esc(node.output)}</span>`));

  const vitals = (node.executor_kind === 'ai') ? vitalsHTML(node, detail) : '';
  const controls = driveControlsHTML(detail.instance || {}, node);

  host.innerHTML = `
    <div class="wf-side-head">
      <span class="wf-side-title">${esc(node.label || node.node_id)}</span>
      <button class="tool" data-wfact="clear-node" title="Back to legend">✕</button>
    </div>
    <div class="wf-kv">${rows.join('')}</div>
    ${vitals}
    ${controls}`;
}

function kv(k, v) {
  return `<div class="wf-kv-row"><span class="wf-kv-k">${esc(k)}</span><span class="wf-kv-v">${v}</span></div>`;
}

// wfStatusBadge renders a node-status chip using the same color classes
// as the graph nodes, so the side panel and the chart read identically.
function wfStatusBadge(s) {
  const cls = WF_STATUS_CLASS[s] || 'wfPending';
  return `<span class="wf-legend-chip ${cls}-chip">${esc(s)}</span>`;
}

function nodeLegendHTML(nodes) {
  const counts = {};
  for (const n of nodes) counts[n.status] = (counts[n.status] || 0) + 1;
  const order = ['running', 'awaiting_verify', 'ready', 'pending', 'done', 'skipped', 'failed'];
  const chips = order.filter(s => counts[s]).map(s =>
    `<span class="wf-legend-chip ${WF_STATUS_CLASS[s]}-chip">${esc(s)} ${counts[s]}</span>`).join('');
  return `<div class="wf-side-head"><span class="wf-side-title">Nodes</span></div>
    <div class="wf-legend">${chips || '<span class="wf-muted">no nodes</span>'}</div>
    <div class="wf-hint">Click a node in the graph to inspect it, drive it forward, or see the bound agent's live vitals.</div>`;
}

// vitalsHTML renders the live-vitals overlay for an ai node: it matches
// the node's runtime `assignee` (and the intended-agent hint
// `node.agent` once Step 4 surfaces it) against the bound group's
// members in the snapshot, then shows that member's online/idle/working
// state + current subject. Degrades to "no agent bound" when there is
// no group, no assignee, or no matching member — it never blocks on
// Step 4's group binding.
function vitalsHTML(node, detail) {
  const intended = node.agent || ''; // executor.Agent hint (Step 4 field)
  const assignee = node.assignee || '';
  const member = resolveBoundMember(detail.instance, assignee, intended);

  let body;
  if (member) {
    body = `
      <div class="wf-vitals-row">
        ${statePill(member.state, member.online)}
        <span class="wf-vitals-name">${esc(member.title || shortId(member.conv_id))}</span>
      </div>
      <div class="wf-vitals-sub">
        <span class="wf-muted">${member.online ? 'online' : 'offline'}</span>
        ${member.state && member.state.status_detail ? `<span class="wf-muted">· ${esc(member.state.status_detail)}</span>` : ''}
      </div>`;
  } else {
    const why = assignee
      ? `assignee “${esc(assignee)}” not found in the bound group`
      : 'no agent bound yet';
    body = `<div class="wf-vitals-empty">— ${why}</div>`;
  }
  return `<div class="wf-vitals">
    <div class="wf-vitals-head">Live agent ${intended ? `<span class="wf-muted">intended: ${esc(intended)}</span>` : ''}</div>
    ${body}
  </div>`;
}

// resolveBoundMember finds the snapshot group member backing an ai node.
// Group comes from the instance summary row (group_name); the match key
// is the runtime assignee, with the intended-agent hint as a fallback.
// Matches on conv_id or title, case-insensitive, robust to whichever
// identity Step 4 stamps into assignee.
function resolveBoundMember(inst, assignee, intended) {
  if (!inst || !lastSnapshot) return null;
  const row = (lastSnapshot.workflows || []).find(w => w.id === inst.id);
  const groupName = (row && row.group_name) || '';
  if (!groupName) return null;
  const group = (lastSnapshot.groups || []).find(g => g.name === groupName);
  if (!group) return null;
  const keys = [assignee, intended].filter(Boolean).map(s => s.toLowerCase());
  if (!keys.length) return null;
  return (group.members || []).find(m =>
    keys.includes((m.conv_id || '').toLowerCase()) ||
    keys.includes((m.title || '').toLowerCase())) || null;
}

// driveControlsHTML renders the manual-advance controls for a node: a
// "start" (mark running) for a ready node, a settle button per allowed
// outcome (the human-gate "approve" path), and a fail button. ai nodes
// also get a "start agent" hook that POSTs .../start — guarded so the
// Step 4 501 reads as a friendly note, not an error.
function driveControlsHTML(inst, node) {
  const ended = (inst.status === 'completed' || inst.status === 'failed' || inst.status === 'cancelled');
  if (ended) return `<div class="wf-controls wf-muted">instance ${esc(inst.status)} — no manual drive</div>`;

  const terminal = (node.status === 'done' || node.status === 'failed' || node.status === 'skipped');
  if (terminal) return `<div class="wf-controls wf-muted">node settled (${esc(node.status)})</div>`;

  const id = inst.id;
  const nid = node.node_id;
  const btns = [];
  if (node.status === 'ready') {
    btns.push(`<button class="tool" data-wfact="node-start" data-id="${id}" data-node="${esc(nid)}" title="Mark this node running">▶ mark running</button>`);
  }
  if (node.executor_kind === 'ai') {
    btns.push(`<button class="tool" data-wfact="node-spawn" data-id="${id}" data-node="${esc(nid)}" title="Spawn / attach the agent for this node (Step 4)">🤖 start agent</button>`);
  }
  const outcomes = (node.allowed_outcomes && node.allowed_outcomes.length)
    ? node.allowed_outcomes : ['pass'];
  const settle = outcomes.map(o =>
    `<button class="primary" data-wfact="node-settle" data-id="${id}" data-node="${esc(nid)}" data-outcome="${esc(o)}" title="Settle this node done with outcome “${esc(o)}”">✓ ${esc(o)}</button>`).join('');
  const failBtn = `<button class="danger" data-wfact="node-fail" data-id="${id}" data-node="${esc(nid)}" title="Mark this node failed">✗ fail</button>`;

  return `<div class="wf-controls">
    <div class="wf-controls-label">Drive node${node.executor_kind === 'human' ? ' · human gate' : ''}</div>
    <div class="wf-controls-btns">${btns.join('')}${settle}${failBtn}</div>
  </div>`;
}

// ---- timeline ---------------------------------------------------------

function renderTimeline(detail) {
  const host = $('#wf-timeline');
  if (!host) return;
  const events = detail.events || [];
  if (!events.length) { host.innerHTML = `<div class="wf-muted">no events yet</div>`; return; }
  // newest first
  const rows = events.slice().reverse().map(e => `
    <div class="wf-evt">
      <span class="wf-evt-time">${esc(relTime(e.at) || '')}</span>
      <span class="wf-evt-kind">${esc(e.kind)}</span>
      ${e.node_id ? `<span class="wf-ref">${esc(e.node_id)}</span>` : ''}
      ${e.message ? `<span class="wf-muted">${esc(e.message)}</span>` : ''}
    </div>`).join('');
  host.innerHTML = rows;
}

// ---- mutations --------------------------------------------------------

async function patchNode(id, nodeId, body, okMsg) {
  try {
    const r = await fetch(`/api/workflows/${id}/nodes/${encodeURIComponent(nodeId)}`, {
      method: 'PATCH', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const txt = await r.text();
    if (!r.ok) { toast(txt || `HTTP ${r.status}`, true); return; }
    if (okMsg) toast(okMsg);
    await refreshDetail();
    refresh();
  } catch (e) {
    toast((e && e.message) || String(e), true);
  }
}

async function spawnNodeAgent(id, nodeId) {
  // Step 4 owns /start; until it merges this 501s. Treat that as a
  // friendly "not yet" rather than an error.
  try {
    const r = await fetch(`/api/workflows/${id}/nodes/${encodeURIComponent(nodeId)}/start`, {
      method: 'POST', credentials: 'same-origin',
    });
    const txt = await r.text();
    if (r.status === 501) { toast('agent start/attach lands in Step 4 (group integration)'); return; }
    if (!r.ok) { toast(txt || `HTTP ${r.status}`, true); return; }
    toast(`agent started for ${nodeId}`);
    await refreshDetail();
    refresh();
  } catch (e) {
    toast((e && e.message) || String(e), true);
  }
}

async function cancelInstance(id) {
  const ok = await confirmModal({
    title: 'Cancel workflow?',
    body: 'Cancel this instance? Every node that has not settled is marked skipped and the instance becomes cancelled. History is kept.',
    okLabel: 'Cancel workflow',
  });
  if (!ok) return;
  try {
    const r = await fetch(`/api/workflows/${id}/cancel`, { method: 'POST', credentials: 'same-origin' });
    if (!r.ok) { toast((await r.text()) || `HTTP ${r.status}`, true); return; }
    toast('workflow cancelled');
    await refreshDetail();
    refresh();
  } catch (e) {
    toast((e && e.message) || String(e), true);
  }
}

async function deleteInstance(id) {
  const ok = await confirmModal({
    title: 'Delete workflow?',
    body: 'Delete this instance and its entire node + event history? This cannot be undone. (The template is untouched.)',
    okLabel: 'Delete instance',
  });
  if (!ok) return;
  try {
    const r = await fetch(`/api/workflows/${id}`, { method: 'DELETE', credentials: 'same-origin' });
    if (!r.ok && r.status !== 204) { toast((await r.text()) || `HTTP ${r.status}`, true); return; }
    toast('workflow deleted');
    if (selectedInstanceId === id) {
      selectedInstanceId = null;
      selectedNodeId = null;
      currentDetail = null;
    }
    refresh();
  } catch (e) {
    toast((e && e.message) || String(e), true);
  }
}

// ---- instantiate modal ------------------------------------------------

function openInstantiateModal(presetRef) {
  const templates = (lastSnapshot && lastSnapshot.workflow_templates) || [];
  const usable = templates.filter(t => !t.err);
  if (!usable.length) { toast('no instantiable workflow templates found', true); return; }
  const sel = $('#wf-instantiate-template');
  sel.innerHTML = usable.map(t =>
    `<option value="${esc(t.ref)}">${esc(t.name)} — ${esc(t.ref)}</option>`).join('');
  if (presetRef && usable.some(t => t.ref === presetRef)) sel.value = presetRef;
  $('#wf-instantiate-title').value = '';
  $('#wf-instantiate-params').value = '';
  $('#wf-instantiate-error').textContent = '';
  $('#wf-instantiate-modal').classList.add('show');
  setTimeout(() => $('#wf-instantiate-title').focus(), 0);
}

function closeInstantiateModal() { $('#wf-instantiate-modal').classList.remove('show'); }

// parseParams reads the params textarea: one `key=value` per line.
// Empty → {}. Values are kept as strings (the engine interpolates them
// later); a leading/trailing space is trimmed off the key.
function parseParams(text) {
  const out = {};
  for (const raw of (text || '').split('\n')) {
    const line = raw.trim();
    if (!line) continue;
    const eq = line.indexOf('=');
    if (eq < 0) { out[line] = ''; continue; }
    out[line.slice(0, eq).trim()] = line.slice(eq + 1);
  }
  return out;
}

async function submitInstantiate() {
  const ref = $('#wf-instantiate-template').value;
  const title = $('#wf-instantiate-title').value.trim();
  const errEl = $('#wf-instantiate-error');
  errEl.textContent = '';
  if (!ref) { errEl.textContent = 'pick a template'; return; }
  const payload = {
    template_ref: ref,
    title: title,
    params: parseParams($('#wf-instantiate-params').value),
  };
  const btn = $('#wf-instantiate-submit');
  btn.disabled = true;
  btn.textContent = 'Instantiating…';
  try {
    const r = await fetch('/api/workflows', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const txt = await r.text();
    if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
    let resp = {};
    try { resp = JSON.parse(txt); } catch (_) {}
    closeInstantiateModal();
    toast(`workflow instantiated${resp.id ? ` (#${resp.id})` : ''}`);
    await refresh();
    if (resp.id) selectInstance(resp.id);
  } catch (e) {
    errEl.textContent = (e && e.message) || String(e);
  } finally {
    btn.disabled = false;
    btn.textContent = 'Instantiate';
  }
}

// ---- wiring -----------------------------------------------------------

function bindWorkflowsUI() {
  // Template-pane + instance-pane delegated actions. data-wfact keeps
  // these off the global row-action bus.
  const tab = $('#tab-workflows');
  if (tab) {
    tab.addEventListener('click', e => {
      const btn = e.target.closest('[data-wfact]');
      if (!btn) return;
      const act = btn.dataset.wfact;
      const id = btn.dataset.id != null ? Number(btn.dataset.id) : null;
      const node = btn.dataset.node;
      switch (act) {
        case 'instantiate': openInstantiateModal(btn.dataset.ref); break;
        case 'select': selectInstance(Number(btn.dataset.id)); break;
        case 'cancel': cancelInstance(id); break;
        case 'delete': deleteInstance(id); break;
        case 'clear-node': selectedNodeId = null; markSelectedNodeInGraph(); if (currentDetail) renderSidePanel(currentDetail); break;
        case 'node-start': patchNode(id, node, { status: 'running' }, `${node}: running`); break;
        case 'node-settle': patchNode(id, node, { status: 'done', outcome: btn.dataset.outcome }, `${node}: ${btn.dataset.outcome}`); break;
        case 'node-fail': patchNode(id, node, { status: 'failed' }, `${node}: failed`); break;
        case 'node-spawn': spawnNodeAgent(id, node); break;
        default: break;
      }
    });
    // Keyboard select on instance rows (they're role=button).
    tab.addEventListener('keydown', e => {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      const row = e.target.closest('.wf-inst-row');
      if (!row) return;
      e.preventDefault();
      selectInstance(Number(row.dataset.id));
    });
  }

  // New-instance button in the filter bar.
  const newBtn = $('#wf-new-open');
  if (newBtn) newBtn.addEventListener('click', () => openInstantiateModal(null));

  // Instantiate modal.
  $('#wf-instantiate-cancel').addEventListener('click', closeInstantiateModal);
  $('#wf-instantiate-submit').addEventListener('click', submitInstantiate);
  bindBackdropDiscard('wf-instantiate-modal', closeInstantiateModal);
}

export { renderWorkflowsTab, bindWorkflowsUI };
