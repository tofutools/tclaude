// process-terminal-overlay.js -- the Terminals process overlay (mocks T1, T2).
//
// T1: each terminal pane gets a small top-right cluster with one icon per
// process run its agent takes part in (the kit's ProcessRunIcon: the ring is
// the run state, the badge is this agent's step i/n), collapsible so it never
// sits on terminal content that matters. The tab strip repeats the icons in
// miniature. T2: clicking an icon unfolds a popover anchored top-right that
// shows the run's mini-graph with this agent's node lit ("you are here"), what
// feeds it, what waits downstream, the node's contract, and run actions. The
// terminal stays usable underneath: nothing here takes focus or traps keys.
//
// Data comes from GET /v1/process/context (the read model), polled by ONE
// shared poller however many panes subscribe, and only while the snapshot
// says features.processes is on.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ProcessMiniGraph, ProcessRunIcon, ProcessScopeChip, ProcessStateChip,
  normalizeNodeState, runIconView, runName,
} from './process-kit.js';

const html = htm.bind(h);

export const PROCESS_CONTEXT_PATH = '/v1/process/context';
export const PROCESS_CONTEXT_POLL_MS = 5000;

// ── Shared poller ────────────────────────────────────────────────────────

const poller = {
  listeners: new Set(),
  latest: null,
  timer: null,
  inflight: false,
  fetch: (...args) => globalThis.fetch(...args),
  interval: PROCESS_CONTEXT_POLL_MS,
};

async function pollProcessContext() {
  if (poller.inflight) return;
  poller.inflight = true;
  let next = null;
  try {
    const response = await poller.fetch(PROCESS_CONTEXT_PATH, { headers: { Accept: 'application/json' } });
    next = response.ok ? await response.json() : null;
  } catch {
    next = null;
  } finally {
    poller.inflight = false;
  }
  poller.latest = next;
  for (const listener of poller.listeners) listener(next);
}

function subscribeProcessContext(listener) {
  poller.listeners.add(listener);
  if (poller.latest) listener(poller.latest);
  if (!poller.timer) {
    void pollProcessContext();
    poller.timer = setInterval(pollProcessContext, poller.interval);
  }
  return () => {
    poller.listeners.delete(listener);
    if (!poller.listeners.size && poller.timer) {
      clearInterval(poller.timer);
      poller.timer = null;
    }
  };
}

// configureProcessContextPoller swaps the transport and cadence (tests) and
// returns a restore function. It also drops any cached context.
export function configureProcessContextPoller({ fetch = null, interval = null } = {}) {
  const prior = { fetch: poller.fetch, interval: poller.interval };
  if (fetch) poller.fetch = fetch;
  if (interval) poller.interval = interval;
  poller.latest = null;
  return () => {
    Object.assign(poller, prior);
    poller.latest = null;
  };
}

export function refreshProcessContext() {
  return pollProcessContext();
}

export function useProcessContext(enabled) {
  const [context, setContext] = useState(enabled ? poller.latest : null);
  useEffect(() => (enabled ? subscribeProcessContext(setContext) : undefined), [enabled]);
  return enabled ? context : null;
}

// ── Pure helpers ─────────────────────────────────────────────────────────

// processRunsForAgent lists the runs an agent takes part in: the runs bound
// to it (the read model's per-agent entries) plus any run where one of its
// nodes names the agent as performer. Order follows the read model.
export function processRunsForAgent(context, agentId) {
  if (!context || !agentId) return [];
  const runs = context.runs || {};
  const ids = [];
  for (const entry of context.agents || []) {
    if (entry.agentId !== agentId) continue;
    for (const step of entry.runs || []) ids.push(step.runId);
  }
  for (const [id, run] of Object.entries(runs)) {
    const performs = (node) => node.performer?.ref === agentId
      || (node.performers || []).some((performer) => performer?.id === agentId);
    if ((run.nodes || []).some(performs)) ids.push(id);
  }
  return [...new Set(ids)].map((id) => runs[id]).filter(Boolean);
}

// nodeNeighbourhood is what the popover's "around it" column says about a
// node: the nodes feeding it, its siblings (other targets of those feeders),
// and the nodes it leads to, each with its edge label.
export function nodeNeighbourhood(run, nodeId) {
  const nodes = new Map((run?.nodes || []).map((node) => [node.id, node]));
  const edges = run?.edges || [];
  const upstream = edges.filter((edge) => edge.to === nodeId && !edge.back);
  const downstream = edges.filter((edge) => edge.from === nodeId && !edge.back);
  const feeders = new Set(upstream.map((edge) => edge.from));
  const siblings = [...new Set(edges
    .filter((edge) => feeders.has(edge.from) && edge.to !== nodeId && !edge.back)
    .map((edge) => edge.to))];
  const describe = (id, label = '') => ({ id, label, node: nodes.get(id) || { id, label: id } });
  return {
    upstream: upstream.map((edge) => describe(edge.from, edge.label)),
    siblings: siblings.map((id) => describe(id)),
    downstream: downstream.map((edge) => describe(edge.to, edge.label)),
    // A lone unlabelled `next` is plain sequencing, not an outcome to pick.
    outcomes: downstream.map((edge) => edge.label).filter((label) => label && label !== 'next'),
  };
}

export function formatRunAge(startedAt, now = Date.now()) {
  const started = Date.parse(startedAt || '');
  if (!Number.isFinite(started)) return '';
  const minutes = Math.max(0, Math.floor((now - started) / 60000));
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${String(minutes % 60).padStart(2, '0')}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

function ownerAgentID(run) {
  const owner = String(run?.scope?.owner || '');
  return owner.startsWith('agent:') ? owner.slice('agent:'.length) : '';
}

// ── Components ───────────────────────────────────────────────────────────

// ProcessTabIcons repeats a pane's run icons in miniature on its tab.
export function ProcessTabIcons({ agentId, enabled }) {
  const runs = processRunsForAgent(useProcessContext(enabled && !!agentId), agentId);
  if (!runs.length) return null;
  return html`<span class="pk-tab-icons" data-process-tab-icons>${runs.map((run) => html`
    <${ProcessRunIcon} key=${run.runId} run=${run} performerId=${agentId} small=${true} />`)}</span>`;
}

// ProcessPaneOverlay is the T1 cluster plus, when an icon is open, the T2
// popover. actions: { onComposeMessage(seed), openGroups(run), decide(run,
// nodeId, verdict) } — each optional; a missing action disables its button.
export function ProcessPaneOverlay({ agentId, enabled, actions = {} }) {
  const runs = processRunsForAgent(useProcessContext(enabled && !!agentId), agentId);
  const [collapsed, setCollapsed] = useState(false);
  const [openID, setOpenID] = useState('');
  const open = collapsed ? null : runs.find((run) => run.runId === openID) || null;
  useEffect(() => {
    if (!open) return undefined;
    const onKey = (event) => { if (event.key === 'Escape') setOpenID(''); };
    globalThis.addEventListener?.('keydown', onKey);
    return () => globalThis.removeEventListener?.('keydown', onKey);
  }, [open]);
  if (!runs.length) return null;
  const toggleTitle = collapsed ? `Show ${runs.length} process icon${runs.length === 1 ? '' : 's'}` : 'Hide process icons';
  return html`
    <div class=${`pk-pane-overlay${collapsed ? ' collapsed' : ''}${open ? ' open' : ''}`} data-process-overlay=${agentId}>
      <div class="pk-pane-overlay-bar" role="toolbar" aria-label="Processes this agent takes part in">
        ${collapsed
          ? html`<span class="pk-pane-overlay-count" aria-hidden="true">◈ ${runs.length}</span>`
          : runs.map((run) => html`<${ProcessRunIcon} key=${run.runId} run=${run} performerId=${agentId}
              active=${openID === run.runId} onOpen=${() => setOpenID(openID === run.runId ? '' : run.runId)} />`)}
        <button type="button" class="pk-pane-overlay-toggle" aria-expanded=${collapsed ? 'false' : 'true'}
          title=${toggleTitle} aria-label=${toggleTitle}
          onClick=${() => { setCollapsed(!collapsed); setOpenID(''); }}>${collapsed ? '◂' : '▾'}</button>
      </div>
      ${open ? html`<${ProcessRunPopover} run=${open} agentId=${agentId} actions=${actions} onClose=${() => setOpenID('')} />` : null}
    </div>
  `;
}

function NeighbourList({ items, prefix }) {
  if (!items.length) return null;
  return items.map((item) => html`<div key=${`${prefix}:${item.id}:${item.label}`} class="pk-around-row">
    <span class="pk-around-arrow" aria-hidden="true">${prefix}</span>
    <b>${item.node.label || item.id}</b>
    ${item.label && item.label !== 'next' ? html`<span class="pk-around-label">${item.label}</span>` : null}
    <span class=${`pk-around-state pk-state-${normalizeNodeState(item.node.state)}`}>${normalizeNodeState(item.node.state)}</span>
  </div>`);
}

// ProcessRunPopover is mock T2: the run seen from this agent. It is a div,
// built from divs: dashboard.css styles bare section/header/footer as page
// chrome (a section is a hidden tab, the footer is pinned to the viewport).
export function ProcessRunPopover({ run, agentId, actions = {}, onClose }) {
  const view = runIconView(run, agentId);
  const node = view.node;
  const around = node ? nodeNeighbourhood(run, node.id) : { upstream: [], siblings: [], downstream: [], outcomes: [] };
  const age = formatRunAge(run.startedAt);
  const owner = ownerAgentID(run);
  // The kit normalizes the read model's "awaiting" decision state to waiting.
  const awaitingDecision = node && node.kind === 'decision' && node.state === 'waiting';
  return html`
    <div class="pk-run-popover" role="dialog" aria-label=${`${runName(run)} from this agent`} data-process-popover=${run.runId}>
      <div class="pk-run-popover-head">
        <${ProcessRunIcon} run=${run} performerId=${agentId} small=${true} />
        <b class="pk-run-popover-name">${runName(run)}</b>
        <${ProcessScopeChip} scope=${run.scope} />
        <${ProcessStateChip} state=${view.state} label=${`${run.status || view.state}${age ? ` ${age}` : ''}`} />
        <button type="button" class="pk-run-popover-close" title="Close" aria-label="Close" onClick=${onClose}>×</button>
      </div>
      <div class="pk-run-popover-graph"><${ProcessMiniGraph} run=${run} performerId=${agentId} compact=${true} /></div>
      <div class="pk-run-popover-body">
        <div class="pk-run-popover-node">
          ${node ? html`
            <div class="pk-run-popover-kicker">This agent's node${view.badge ? ` · ${view.badge.replace('/', ' of ')}` : ''}</div>
            <div class="pk-run-popover-title">${node.label || node.id}</div>
            ${node.description ? html`<p class="pk-run-popover-desc">${node.description}</p>` : null}
            ${around.outcomes.length ? html`<div class="pk-run-popover-outcomes"><b>Outcomes:</b>
              ${around.outcomes.map((outcome) => html`<span key=${outcome} class="pk-chip">${outcome}</span>`)}</div>` : null}
          ` : html`<div class="pk-run-popover-kicker">No step of this run is current for this agent.</div>`}
        </div>
        <div class="pk-run-popover-around">
          <div class="pk-run-popover-kicker">Around it</div>
          <${NeighbourList} items=${around.upstream} prefix="↑" />
          <${NeighbourList} items=${around.siblings} prefix="⇉" />
          <${NeighbourList} items=${around.downstream} prefix="↓" />
        </div>
      </div>
      <div class="pk-run-popover-actions">
        <button type="button" class="mux-btn" disabled=${!actions.openGroups} onClick=${() => actions.openGroups?.(run)}>↗ open in Groups</button>
        <button type="button" class="mux-btn" disabled=${!owner || !actions.onComposeMessage}
          title=${owner ? `Message the run owner (${owner})` : 'The run owner is the human'}
          onClick=${() => actions.onComposeMessage?.({ agent: owner, label: owner })}>✉ message owner</button>
        <button type="button" class="mux-btn" disabled=${true}
          title="Reassigning a node arrives with performer dispatch">↻ reassign node</button>
        ${awaitingDecision && actions.decide
          ? around.outcomes.map((verdict) => html`<button key=${verdict} type="button" class="mux-btn pk-run-popover-verdict"
              title=${`Decide ${verdict} as the human`} onClick=${() => actions.decide(run, node.id, verdict)}>✓ ${verdict} (as human)</button>`)
          : html`<button type="button" class="mux-btn" disabled=${true}
              title="Marking an outcome as the human applies to a node waiting on a decision">✓ mark outcome (as human)</button>`}
      </div>
    </div>
  `;
}

// processOverlayActions is the production action set: open the Groups tab,
// compose through the operator message dialog, and decide through the run
// API, refreshing the context afterwards.
export function processOverlayActions({ onComposeMessage = null, fetch = (...args) => globalThis.fetch(...args) } = {}) {
  return {
    onComposeMessage,
    openGroups: () => globalThis.document?.querySelector('nav [data-tab="groups"]')?.click(),
    decide: async (run, nodeId, verdict) => {
      await fetch(`/v1/process/runs/${encodeURIComponent(run.runId)}/decide`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ nodeId, verdict }),
      });
      await refreshProcessContext();
    },
  };
}
