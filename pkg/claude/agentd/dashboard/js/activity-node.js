// activity-node.js — whose daemon Activity's Logs and Performance show in a
// fused view (tcl-4h1t8f). They are one daemon's own (its log, its poll
// timings), so they switch between the fused nodes rather than merge. A peer
// serves them only under unrestricted trust; a refusal says so.
import { h } from 'preact';
import { signal } from '@preact/signals';
import htm from 'htm';
import { nodeFetch } from './node-api.js';
import { fleetState } from './fleet-state.js';
import { tickedNodes } from './fleet-scope.js';

const html = htm.bind(h);

// activityNode is the chosen node: '' is this node, else a peer's ID.
export const activityNode = signal('');

// activityNodes are the nodes the switch offers (this node first), or []
// outside a fused view (always this node's page).
export function activityNodes(fleet = fleetState, remote = globalThis.__tclaudeRemoteNode) {
  const value = fleet.fleet.value;
  const fused = fleet.fused.value;
  return value && fused && !remote ? tickedNodes(value, fused) : [];
}

// activityTarget is the chosen peer while it is still ticked, else null.
export function activityTarget(nodes = activityNodes(), id = activityNode.value) {
  return (id && nodes.find((n) => n.id === id && !n.local)) || null;
}

// activityFetch returns the fetch that reaches the chosen node: fetchImpl for
// this node, the peer's routes for a peer. A refused peer read is reworded.
export function activityFetch(fetchImpl, { target = activityTarget(), fetchFor = nodeFetch } = {}) {
  if (!target) return fetchImpl;
  const peerFetch = fetchFor(target.id);
  return async (input, init) => {
    const response = await peerFetch(input, init);
    if (response.status !== 403) return response;
    const why = `${target.name} does not share its daemon log or timings with you (needs unrestricted trust from ${target.name})`;
    return { ok: false, status: 403, text: async () => why, json: async () => ({ error: why }) };
  };
}

// ActivityNodeSwitch is the switch in Logs' and Performance's filter bars.
export function ActivityNodeSwitch({ nodes = activityNodes(), value = activityNode.value, onChange }) {
  if (nodes.length < 2) return null;
  const shown = activityTarget(nodes, value) ? value : '';
  return html`<label class="filter-toggle" title="Whose daemon to show"><span>node</span><select class="activity-node" value=${shown}
    onChange=${(event) => { activityNode.value = event.currentTarget.value; onChange?.(); }}>
    ${nodes.map((n) => html`<option key=${n.id} value=${n.local ? '' : n.id}>${n.local ? `⌂ ${n.name}` : n.name}</option>`)}
  </select></label>`;
}
