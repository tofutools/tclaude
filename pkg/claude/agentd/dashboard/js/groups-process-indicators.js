// groups-process-indicators.js -- process state inside the Groups tab's
// Agents focus (mock G1): `N running / N needs you / N planned` chips on each
// group header, and a `run › node` badge with a step strip on each member row
// for the process node that agent performs. Both read the process context
// read model (process-context.js) and click through to the Processes focus.
//
// Large fleets: one context fetch per snapshot poll, and one agent→runs index
// per context object (cached), so each row is a Map lookup.

import { h } from 'preact';
import htm from 'htm';
import { ProcessRunBadge } from './process-kit.js';
import { groupContextEntry, processContext } from './process-context.js';
import { featureState } from './feature-state-registry.js';
import { chooseGroupsFocus } from './groups-focus.js';

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

// openGroupInProcesses switches to the Processes focus on `filter` and brings
// the group's section into view.
export function openGroupInProcesses(groupName, filter = 'running', { doc = document } = {}) {
  const groups = featureState('groups');
  if (!groups?.setFocus) return;
  groups.setProcessFilter?.(filter);
  groups.setFocusedRun?.(null);
  chooseGroupsFocus(groups, 'processes', { doc });
  // The group's row, not its body: a collapsed group still has one.
  const reveal = () => doc.querySelector(`details[data-group-key="${cssString(groupName)}"]`)?.scrollIntoView?.({ block: 'start', behavior: 'smooth' });
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(() => requestAnimationFrame(reveal));
  else reveal();
}

// openRunInProcesses opens one run in the Processes lens's single-run focus.
export function openRunInProcesses(groupName, runId, { doc = document } = {}) {
  const groups = featureState('groups');
  if (!groups?.setFocus) return;
  if (groupName && runId && groups.setFocusedRun) groups.setFocusedRun({ group: groupName, runId });
  chooseGroupsFocus(groups, 'processes', { doc });
}

function cssString(value) {
  return String(value).replace(/["\\]/g, '\\$&');
}

function enabled(snapshot, source) {
  return snapshot?.processes_enabled === true && source.status.value === 'ready';
}

export function GroupProcessChips({ group, snapshot, source = processContext, wizard = false }) {
  if (!enabled(snapshot, source)) return null;
  const counts = groupProcessCounts(groupContextEntry(source.context.value, group));
  if (!counts.running && !counts.needsYou && !counts.planned) {
    return html`<span class="gpi-none">${wizard ? 'no rites' : 'no processes'}</span>`;
  }
  const chip = (count, state, filter, plain, arcane, glyph) => count > 0 && html`<button type="button"
      class=${`pk-chip pk-state-${state} gpi-chip`} data-group-processes=${filter === 'running' && state === 'waiting' ? 'needs-you' : filter}
      title=${`Show ${group.name}'s ${plain} in the Processes focus`}
      onClick=${(event) => { event.preventDefault(); event.stopPropagation(); openGroupInProcesses(group.name, filter); }}
    ><span aria-hidden="true">${glyph}</span> ${count} ${wizard ? arcane : plain}</button>`;
  return html`<span class="gpi-chips">
    ${chip(counts.running, 'running', 'running', 'running', 'unfolding', '●')}
    ${chip(counts.needsYou, 'waiting', 'running', 'needs you', 'awaits you', '●')}
    ${chip(counts.planned, 'planned', 'planned', 'planned', 'foretold', '◷')}
  </span>`;
}

export function MemberProcessCell({ member, group, snapshot, source = processContext }) {
  if (!enabled(snapshot, source)) return html`<span class="muted">—</span>`;
  const entries = agentRunIndex(source.context.value).get(member.agent_id) || [];
  if (!entries.length) return html`<span class="muted">—</span>`;
  return html`<span class="gpi-badges">${entries.map((entry) => html`<${ProcessRunBadge}
    key=${`${entry.runId}:${entry.nodeId}`} run=${entry.run} nodeId=${entry.nodeId}
    onOpen=${() => openRunInProcesses(entry.run.scope?.group || group?.name || '', entry.runId)} />`)}</span>`;
}
