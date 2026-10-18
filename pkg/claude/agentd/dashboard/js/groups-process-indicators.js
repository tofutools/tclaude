// groups-process-indicators.js -- process state inside the Groups tab: an
// animated running / needs-you / planned flow-graph row on each group header
// (the process twin of the member bots in group-activity.js, shown in both
// perspectives), and a `run › node` badge with a step strip on each member
// row for the process node that agent performs (Agents focus, mock G1). Both
// read the process context read model (process-context.js) and click
// through to the runs (the Processes focus, or Both's Processes section).
//
// Large fleets: one context fetch per snapshot poll, and one agent→runs index
// per context object (cached), so each row is a Map lookup.

import { h } from 'preact';
import htm from 'htm';
import { ProcessRunBadge } from './process-kit.js';
import { groupContextEntry, processContext, withRunKind } from './process-context.js';
import { featureState } from './feature-state-registry.js';
import { focusRun, groupsRunLocation, revealRunsLens, showRunsLens } from './groups-focus.js';
import { setFusedSectionOpen } from './groups-fused-sections.js';
import { UNGROUPED_VKEY } from './virtual-groups.js';
import { useProcessScope } from './groups-process-scope.js';

const html = htm.bind(h);

const agentIndexCache = new WeakMap();

// agentRunIndex maps agent id → [{ run, nodeId, state }] for one context.
export function agentRunIndex(context) {
  if (!context || typeof context !== 'object') return new Map();
  const cached = agentIndexCache.get(context);
  if (cached) return cached;
  const runs = context.runs || {};
  const index = new Map();
  for (const agent of context.agents || []) {
    if (!agent?.agentId) continue;
    const entries = (agent.runs || [])
      .filter((entry) => runs[entry.runId])
      .map((entry) => ({ ...entry, run: { ...runs[entry.runId], runId: runs[entry.runId].runId ?? entry.runId } }));
    if (entries.length) index.set(agent.agentId, entries);
  }
  agentIndexCache.set(context, index);
  return index;
}

export function groupProcessCounts(entry) {
  if (!entry) return { running: 0, needsYou: 0, planned: 0 };
  return {
    running: Number.isFinite(entry.activeCount) ? entry.activeCount : (entry.active || []).length,
    needsYou: Number.isFinite(entry.needsYouCount) ? entry.needsYouCount : (entry.needsYou || []).length,
    planned: (entry.planned || []).length,
  };
}

// openGroupInProcesses shows the group's runs with the `filter` kind ticked
// and brings them into view: in Both, the group's Processes section, opened;
// otherwise the group's row in the Processes focus. The group's row is opened
// too (the open state is shared between lenses, so a group folded in one
// would hide its runs in another).
// `rowKey` is the group's row when the list names it apart from the run
// context (the fused view's group@node).
export function openGroupInProcesses(groupName, filter = 'running', { doc = document, rowKey = groupName } = {}) {
  const groups = featureState('groups');
  if (!groups?.setFocus) return;
  // Tick the clicked kind without unticking the others.
  groups.setProcessFilter?.(withRunKind(groups.processFilter?.value, filter));
  if (groups.focusedRun) focusRun(groups, null, { doc });
  const fused = revealRunsLens(groups, { doc });
  if (fused) {
    setFusedSectionOpen(groupName, 'processes', true);
    groups.rerender?.();
  }
  const reveal = () => {
    const row = doc.querySelector(`details[data-group-key="${cssString(rowKey)}"]`);
    if (!row) return;
    row.open = true;
    const target = fused ? row.querySelector(`[data-fused-processes="${cssString(groupName)}"]`) || row : row;
    target.scrollIntoView?.({ block: 'start', behavior: 'smooth' });
  };
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(() => requestAnimationFrame(reveal));
  else reveal();
}

// openRunInProcesses opens one run in the single-run focus, which Both and
// Processes share.
export function openRunInProcesses(groupName, runId, { doc = document } = {}) {
  const groups = featureState('groups');
  if (!groups?.setFocus) return;
  if (!groupName || !runId || !groups.focusedRun) { revealRunsLens(groups, { doc }); return; }
  showRunsLens(groups);
  // Already open (behind the Agents lens): the lens change alone is the step.
  if (!focusRun(groups, { group: groupName, runId }, { doc })) {
    doc.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: groupsRunLocation(groups.focusedRun.value) } }));
  }
}

function cssString(value) {
  return String(value).replace(/["\\]/g, '\\$&');
}

export function enabled(snapshot, source) {
  return snapshot?.processes_enabled === true && source.status.value === 'ready';
}

// GRAPH_STATES is the process twin of group-activity.js's bot row: one
// animated flow-graph glyph per distinct state present, loudest first, each
// carrying a count when more than one run shares it. Running moves a packet
// node to node, needs-you pulses an amber gate node, planned is a quiet
// dashed outline (dashboard.css → .gpi-graph).
const GRAPH_STATES = [
  { key: 'needsYou', state: 'waiting', filter: 'running', data: 'needs-you', plain: 'needs you', arcane: 'awaits you' },
  { key: 'running', state: 'running', filter: 'running', data: 'running', plain: 'running', arcane: 'unfolding' },
  { key: 'planned', state: 'planned', filter: 'planned', data: 'planned', plain: 'planned', arcane: 'foretold' },
];

function FlowGlyph() {
  return html`<svg class="gpi-flow" viewBox="0 0 22 16" aria-hidden="true">
    <polyline class="gpi-edge" points="3,10 11,5 19,10" />
    <circle class="gpi-node" cx="3" cy="10" r="2.4" />
    <circle class="gpi-node gpi-gate" cx="11" cy="5" r="2.4" />
    <circle class="gpi-node" cx="19" cy="10" r="2.4" />
    <circle class="gpi-packet" cx="3" cy="10" r="1.6" />
  </svg>`;
}

// ProcessGraphRow renders counts as the glyph row; nothing when no run is
// present. With onOpen each glyph is a button showing the group's runs on
// its filter (without toggling the enclosing <details>).
export function ProcessGraphRow({ counts, wizard = false, groupName = '', onOpen = null }) {
  const present = GRAPH_STATES.filter((spec) => counts[spec.key] > 0);
  if (!present.length) return null;
  return html`<span class=${`gpi-graphs level-${present[0].state}`}>
    ${present.map((spec) => {
      const n = counts[spec.key];
      const label = `${n} ${wizard ? spec.arcane : spec.plain}`;
      const body = html`<${FlowGlyph} />${n > 1 ? html`<span class="gpi-count">${n}</span>` : null}`;
      const className = `gpi-graph gpi-state-${spec.state}`;
      if (!onOpen) return html`<span key=${spec.data} class=${className} data-group-processes=${spec.data} role="img" title=${label} aria-label=${label}>${body}</span>`;
      return html`<button key=${spec.data} type="button" class=${className} data-group-processes=${spec.data}
        title=${`${label} — show ${groupName}'s processes`} aria-label=${label}
        onClick=${(event) => { event.preventDefault(); event.stopPropagation(); onOpen(spec.filter); }}
      >${body}</button>`;
    })}
  </span>`;
}

// GroupProcessGraphs is a group header's process indicator, shown beside the
// member bots in both perspectives; a quiet group shows nothing.
export function GroupProcessGraphs({ group, snapshot, source = processContext, wizard = false }) {
  const own = useProcessScope()(group);
  if (!own || !enabled(snapshot, source)) return null;
  const counts = groupProcessCounts(groupContextEntry(source.context.value, own));
  return html`<${ProcessGraphRow} counts=${counts} wizard=${wizard} groupName=${group.name}
    onOpen=${(filter) => openGroupInProcesses(own.name, filter, { rowKey: group.name })} />`;
}

export function MemberProcessCell({ member, group, snapshot, source = processContext }) {
  if (!enabled(snapshot, source)) return html`<span class="muted">—</span>`;
  const entries = agentRunIndex(source.context.value).get(member.agent_id) || [];
  if (!entries.length) return html`<span class="muted">—</span>`;
  return html`<span class="gpi-badges">${entries.map((entry) => html`<${ProcessRunBadge}
    key=${`${entry.runId}:${entry.nodeId}`} run=${entry.run} nodeId=${entry.nodeId}
    onOpen=${() => openRunInProcesses(entry.run.scope?.group || group?.name || UNGROUPED_VKEY, entry.runId)} />`)}</span>`;
}
