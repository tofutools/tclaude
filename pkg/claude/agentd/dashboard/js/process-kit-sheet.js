// process-kit-sheet.js -- the S1 reference sheet for the process UI kit: every
// node kind, run/node state, scope chip, run badge, overlay icon and two
// mini-graphs, rendered from sample run projections. It is not routed in the
// dashboard; the kit's jstests mount it so both themes of the shared
// vocabulary stay reviewable in one place.

import { h, render } from 'preact';
import htm from 'htm';
import {
  PROCESS_NODE_KINDS,
  ProcessMiniGraph,
  ProcessNodeChip,
  ProcessNodeGlyph,
  ProcessRunBadge,
  ProcessRunIcon,
  ProcessScopeChip,
  ProcessStateChip,
} from './process-kit.js';

const html = htm.bind(h);

const L = { id: 'lead', label: 'lead' };
const C = { id: 'checkout-ui', label: 'checkout-ui' };
const S = { id: 'search-indexer', label: 'search-indexer' };
const W = { id: 'docs-writer', label: 'docs-writer' };
const R = { id: 'reviewer', label: 'reviewer' };
const HUMAN = { id: 'human', label: 'you', kind: 'human' };

export const SAMPLE_GROUP_RUN = Object.freeze({
  id: 'run-release-24',
  name: 'release-2.4',
  status: 'running',
  scope: { kind: 'group', name: 'webshop' },
  nodes: [
    { id: 'start', kind: 'start', state: 'done' },
    { id: 'plan', label: 'Plan sprint', kind: 'agent', state: 'done', sub: 'lead', performers: [L] },
    { id: 'branches', label: 'Cut branches', kind: 'command', state: 'done', sub: 'git · 4s' },
    { id: 'fanout', label: 'Fan out', kind: 'parallel', state: 'done', sub: '2 parallel' },
    { id: 'checkout', label: 'Implement checkout', kind: 'agent', state: 'running', sub: 'working · 38m', performers: [C] },
    { id: 'search', label: 'Implement search', kind: 'agent', state: 'done', sub: 'done · 21m', performers: [S] },
    { id: 'review', label: 'Review gate', kind: 'human', state: 'waiting', sub: 'payments-fix PR #1469', performers: [HUMAN] },
    { id: 'qa', label: 'QA suite', kind: 'subprocess', state: 'planned', sub: 'subprocess · 5 nodes' },
    { id: 'ship', label: 'Ship', kind: 'command', state: 'planned', sub: 'deploy.sh' },
    { id: 'end', kind: 'end', state: 'planned' },
  ],
  edges: [
    { from: 'start', to: 'plan' }, { from: 'plan', to: 'branches' }, { from: 'branches', to: 'fanout' },
    { from: 'fanout', to: 'checkout' }, { from: 'fanout', to: 'search' }, { from: 'search', to: 'review' },
    { from: 'checkout', to: 'qa' }, { from: 'review', to: 'qa', label: 'approved' },
    { from: 'qa', to: 'ship' }, { from: 'ship', to: 'end' },
  ],
});

export const SAMPLE_AGENT_RUN = Object.freeze({
  id: 'run-reindex-v2',
  name: 'reindex-v2',
  status: 'running',
  scope: { kind: 'agent', name: 'search-indexer' },
  nodes: [
    { id: 'start', kind: 'start', state: 'done' },
    { id: 'snapshot', label: 'Snapshot index', kind: 'command', state: 'done' },
    { id: 'backfill', label: 'Backfill shards', kind: 'agent', state: 'running', sub: '3/4 · ETA 25m', performers: [S] },
    { id: 'verify', label: 'Verify recall@10', kind: 'command', state: 'planned' },
    { id: 'gate', label: '≥ 0.92?', kind: 'decision', state: 'planned' },
    { id: 'wait', label: 'Quiet hours', kind: 'wait', state: 'planned' },
    { id: 'swap', label: 'Swap alias', kind: 'command', state: 'planned' },
    { id: 'end', kind: 'end', state: 'planned' },
  ],
  edges: [
    { from: 'start', to: 'snapshot' }, { from: 'snapshot', to: 'backfill' }, { from: 'backfill', to: 'verify' },
    { from: 'verify', to: 'gate' }, { from: 'gate', to: 'wait', label: 'yes' }, { from: 'wait', to: 'swap' },
    { from: 'swap', to: 'end' }, { from: 'gate', to: 'backfill', label: 'no', back: true },
  ],
});

// SAMPLE_LOOP_RUN is a bounded review loop on its second round: its body
// nodes carry {round, rounds} and the loop's back edge its maxIterations.
export const SAMPLE_LOOP_RUN = Object.freeze({
  id: 'run-docs-review',
  name: 'docs-review',
  status: 'running',
  scope: { kind: 'group', name: 'docs' },
  nodes: [
    { id: 'start', kind: 'start', state: 'done' },
    { id: 'draft', label: 'Draft the page', kind: 'agent', state: 'done', round: 2, rounds: 3, performers: [W] },
    { id: 'review', label: 'Review', kind: 'agent', state: 'running', round: 2, rounds: 3, sub: 'reviewer', performers: [R] },
    { id: 'verdict', label: 'Approved?', kind: 'decision', state: 'planned', round: 2, rounds: 3 },
    { id: 'publish', label: 'Publish', kind: 'command', state: 'planned' },
    { id: 'end', kind: 'end', state: 'planned' },
  ],
  edges: [
    { from: 'start', to: 'draft' }, { from: 'draft', to: 'review' }, { from: 'review', to: 'verdict' },
    { from: 'verdict', to: 'publish', label: 'approved' }, { from: 'publish', to: 'end' },
    { from: 'verdict', to: 'draft', label: 'changes', back: true, maxIterations: 3 },
  ],
});

// loopTemplate is the same loop before any run: its bound, no rounds.
const loopTemplate = { ...SAMPLE_LOOP_RUN, status: 'pending',
  nodes: SAMPLE_LOOP_RUN.nodes.map(({ round, rounds, ...node }) => ({ ...node, state: 'planned' })) };

const STATE_ROWS = [['done', 'done'], ['running', 'running'], ['waiting', 'blocked on human'], ['failed', 'failed'], ['planned', 'planned / not reached']];

function withRunStatus(run, status, overrides = {}) {
  return { ...run, status, nodes: run.nodes.map((node) => (overrides[node.id] ? { ...node, state: overrides[node.id] } : node)) };
}

export function ProcessKitSheet({ wizard = false }) {
  const plannedRun = withRunStatus(SAMPLE_GROUP_RUN, 'pending', { start: 'pending', plan: 'pending', branches: 'pending', fanout: 'pending', checkout: 'pending', search: 'pending', review: 'pending' });
  const failedRun = withRunStatus(SAMPLE_AGENT_RUN, 'failed', { backfill: 'failed' });
  return html`<div class="pk-sheet" data-process-kit-sheet>
    <div class="pk-sheet-box"><h3>Node kinds</h3>
      ${Object.entries(PROCESS_NODE_KINDS).map(([kind, entry]) => html`<div key=${kind} class="pk-sheet-row"><${ProcessNodeGlyph} kind=${kind} wizard=${wizard} /><span>${wizard ? entry.wizardLabel : entry.label}: <span class="pk-sheet-muted">${entry.hint}</span></span></div>`)}
    </div>
    <div class="pk-sheet-box"><h3>Run / node state</h3>
      ${STATE_ROWS.map(([state, label]) => html`<div key=${state} class="pk-sheet-row"><span class="pk-sheet-cell"><${ProcessNodeChip} node=${{ id: state, label: 'Node', kind: 'agent', state }} wizard=${wizard} /></span><${ProcessStateChip} state=${state} label=${label} /></div>`)}
    </div>
    <div class="pk-sheet-box"><h3>Scope</h3>
      <div class="pk-sheet-row"><${ProcessScopeChip} scope=${SAMPLE_GROUP_RUN.scope} wizard=${wizard} /><span class="pk-sheet-muted">performers drawn from the group; owner is a lead</span></div>
      <div class="pk-sheet-row"><${ProcessScopeChip} scope=${SAMPLE_AGENT_RUN.scope} wizard=${wizard} /><span class="pk-sheet-muted">runs inside one agent’s bounds</span></div>
      <div class="pk-sheet-row">
        <${ProcessRunIcon} run=${SAMPLE_GROUP_RUN} performerId="checkout-ui" />
        <${ProcessRunIcon} run=${SAMPLE_GROUP_RUN} performerId="human" />
        <${ProcessRunIcon} run=${plannedRun} />
        <${ProcessRunIcon} run=${SAMPLE_AGENT_RUN} performerId="search-indexer" />
        <${ProcessRunIcon} run=${failedRun} />
        <span class="pk-sheet-muted">overlay icons: running · blocked on human · planned · agent-scoped · failed</span>
      </div>
      <div class="pk-sheet-row"><${ProcessRunBadge} run=${SAMPLE_GROUP_RUN} performerId="checkout-ui" onOpen=${() => {}} /><span class="pk-sheet-muted">agent-row badge</span></div>
      <div class="pk-sheet-row"><${ProcessRunBadge} run=${SAMPLE_GROUP_RUN} performerId="human" /><${ProcessRunBadge} run=${SAMPLE_AGENT_RUN} performerId="search-indexer" /></div>
    </div>
    <div class="pk-sheet-box pk-sheet-wide"><h3>${SAMPLE_GROUP_RUN.name} <${ProcessStateChip} state="running" /> <${ProcessScopeChip} scope=${SAMPLE_GROUP_RUN.scope} wizard=${wizard} /></h3>
      <${ProcessMiniGraph} run=${SAMPLE_GROUP_RUN} performerId="checkout-ui" wizard=${wizard} />
    </div>
    <div class="pk-sheet-box pk-sheet-wide"><h3>${SAMPLE_LOOP_RUN.name} <${ProcessStateChip} state="running" /> <span class="pk-sheet-muted">bounded loop: template, then round 2 of 3</span></h3>
      <${ProcessMiniGraph} run=${loopTemplate} wizard=${wizard} />
      <${ProcessMiniGraph} run=${SAMPLE_LOOP_RUN} wizard=${wizard} />
    </div>
    <div class="pk-sheet-box pk-sheet-wide"><h3>${SAMPLE_AGENT_RUN.name} <${ProcessStateChip} state="running" /> <${ProcessScopeChip} scope=${SAMPLE_AGENT_RUN.scope} wizard=${wizard} /> <span class="pk-sheet-muted">compact</span></h3>
      <${ProcessMiniGraph} run=${SAMPLE_AGENT_RUN} compact wizard=${wizard} />
    </div>
  </div>`;
}

export function mountProcessKitSheet(host) {
  render(html`<${ProcessKitSheet} wizard=${document.body.classList.contains('wizard')} />`, host);
  return () => render(null, host);
}
