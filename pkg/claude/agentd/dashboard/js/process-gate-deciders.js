// process-gate-deciders.js -- each decider of an agreement gate on its own
// row (tk_44d02f32, mock item 5 variant A). A decider has its own ask, its
// own "if nobody" (the performer fallback: spawn one with a profile) and its
// own "not" (differentFrom: not the agent who did another step). The gate's
// Asks is the text the deciders share; a decider whose text differs has its
// own ask. Pure: no DOM, no fetch.

import { firstSpawn } from './process-quickstart.js';

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

function textOf(decider) {
  return decider?.kind === 'human' ? decider.ask || decider.prompt || '' : decider?.prompt || '';
}

function withText(decider, text) {
  if (decider.kind === 'human') decider.ask = text;
  else decider.prompt = text;
}

// gateAsk is the text most of a gate's deciders are asked (the first on a
// tie): the gate's Asks.
export function gateAsk(node) {
  const counts = new Map();
  for (const decider of node?.deciders || []) {
    const text = textOf(decider);
    counts.set(text, (counts.get(text) || 0) + 1);
  }
  let best = '';
  let most = 0;
  for (const [text, count] of counts) if (count > most) { best = text; most = count; }
  return best;
}

// deciderOwnAsk is decider index's own ask, or '' when it is asked the
// gate's Asks.
export function deciderOwnAsk(node, index) {
  const text = textOf(node?.deciders?.[index]);
  return text === gateAsk(node) ? '' : text;
}

function edit(view, id, change) {
  const next = clone(view);
  const node = next.template.nodes[id];
  if (!node?.deciders?.length) return { view, id };
  change(node);
  return { view: next, id };
}

// setGateAsk changes the gate's Asks: every decider without its own ask.
export function setGateAsk(view, id, text) {
  return edit(view, id, (node) => {
    const shared = gateAsk(node);
    for (const decider of node.deciders) if (textOf(decider) === shared) withText(decider, text);
  });
}

// setDeciderAsk gives decider index its own ask; blank asks it the gate's.
export function setDeciderAsk(view, id, index, text) {
  return edit(view, id, (node) => {
    const decider = node.deciders[index];
    if (!decider) return;
    withText(decider, text.trim() ? text : gateAsk(node));
  });
}

// setDeciderFallback sets what decider index does when no member can take
// its vote; null follows the process's fallback.
export function setDeciderFallback(view, id, index, fallback) {
  return edit(view, id, (node) => {
    const decider = node.deciders[index];
    if (decider?.kind !== 'agent') return;
    delete decider.fallback;
    if (fallback || decider.spawn === 'allowed') delete decider.spawn;
    if (fallback) decider.fallback = fallback;
  });
}

// setDeciderNot keeps decider index off the agents who did steps.
export function setDeciderNot(view, id, index, steps) {
  return edit(view, id, (node) => {
    const decider = node.deciders[index];
    if (decider?.kind !== 'agent') return;
    delete decider.sameAs;
    delete decider.differentFrom;
    const kept = [...new Set((steps || []).filter(Boolean))];
    if (kept.length) decider.differentFrom = kept;
  });
}

function agentTask(node) {
  return node?.type === 'task' && node.performer?.kind === 'agent';
}

// reviewedStep is the step whose work the gate reviews: the agent step that
// reports an output the gate's deciders read ({{ outputs.pr }}), else the
// agent step leading into the gate. '' when there is none.
export function reviewedStep(view, id) {
  const nodes = view.template?.nodes || {};
  const read = new Set();
  for (const decider of nodes[id]?.deciders || []) {
    for (const match of textOf(decider).matchAll(/outputs\.([A-Za-z0-9_-]+)/g)) read.add(match[1]);
  }
  const producer = Object.entries(nodes).find(([other, node]) => other !== id && agentTask(node)
    && (node.captures || []).some((name) => read.has(name)));
  if (producer) return producer[0];
  const before = (view.edges || []).find((edge) => edge.to === id && edge.from !== id && agentTask(nodes[edge.from]));
  return before ? before.from : '';
}

// withReviewerDefault keeps a new agent decider off the agent whose work it
// reviews, unless it already names who or whom not.
export function withReviewerDefault(view, id, index) {
  const decider = view.template.nodes[id]?.deciders?.[index];
  if (decider?.kind !== 'agent' || decider.assignee || decider.sameAs || decider.differentFrom?.length) return { view, id };
  const reviewed = reviewedStep(view, id);
  return reviewed ? setDeciderNot(view, id, index, [reviewed]) : { view, id };
}

function fallbackAction(decider, processFallback) {
  if (decider.fallback?.action) return decider.fallback.action;
  if (decider.spawn === 'allowed') return 'spawn';
  return processFallback?.action || 'wait';
}

// deciderSummary is what a closed decider row says is set besides who:
// "own ask · not implement · if nobody: spawn sonnet-dev".
export function deciderSummary(view, id, index) {
  const node = view.template.nodes[id];
  const decider = node?.deciders?.[index];
  if (!decider) return '';
  const name = (step) => view.template.nodes[step]?.name || step;
  const parts = [];
  if (deciderOwnAsk(node, index)) parts.push('own ask');
  if (decider.kind === 'agent') {
    if (decider.differentFrom?.length) parts.push(`not ${decider.differentFrom.map(name).join(', ')}`);
    if (decider.sameAs) parts.push(`same as ${name(decider.sameAs)}`);
    const action = decider.fallback?.action || (decider.spawn === 'allowed' ? 'spawn' : '');
    if (action === 'spawn') parts.push(decider.fallback?.profile ? `if nobody: spawn ${decider.fallback.profile}` : 'if nobody: spawn');
    else if (action) parts.push(`if nobody: ${action}`);
  }
  return parts.join(' · ');
}

// eligible is the names of the roster members who could cast decider's vote;
// null when it names nobody yet.
function eligible(decider, roster) {
  if (decider.assignee) return roster.filter((m) => m.name === decider.assignee).map((m) => m.name);
  const role = decider.role || (!decider.anyone && !decider.lead ? decider.profile : '');
  if (role) return roster.filter((m) => m.role === role).map((m) => m.name);
  if (decider.anyone) return roster.map((m) => m.name);
  if (decider.lead) {
    const leads = roster.filter((m) => m.role === 'lead');
    return (leads.length ? leads : roster.slice(0, 1)).map((m) => m.name);
  }
  return null;
}

// stuckGate says when a gate would wait forever: its agent deciders, taken in
// order (no agent votes twice at one gate), run out of members and one of
// them does not spawn. Returns {deciders, members, noun, index} for the
// first vote left waiting, or null.
export function stuckGate(view, id, roster = [], processFallback = null) {
  const deciders = view.template.nodes[id]?.deciders || [];
  const agents = deciders.map((decider, index) => ({ decider, index, names: decider.kind === 'agent' ? eligible(decider, roster) : null }))
    .filter((entry) => entry.names);
  if (agents.length < 2) return null;
  const pool = new Set(agents.flatMap((entry) => entry.names));
  const taken = new Set();
  for (const { decider, index, names } of agents) {
    const free = names.find((name) => !taken.has(name));
    if (free) { taken.add(free); continue; }
    if (fallbackAction(decider, processFallback) === 'spawn') continue;
    const roles = new Set(agents.map((entry) => entry.decider.role || ''));
    const role = roles.size === 1 ? [...roles][0] : '';
    const noun = role ? (pool.size === 1 ? role : `${role}s`) : (pool.size === 1 ? 'member who can vote' : 'members who can vote');
    return { deciders: agents.length, members: pool.size, noun, index };
  }
  return null;
}

// spawnIfNobody is the fallback the stuck-gate warning's link sets.
export function spawnIfNobody({ defaultSpawnProfile = '', profiles = [] } = {}) {
  return defaultSpawnProfile || profiles.some(Boolean) ? firstSpawn({ defaultSpawnProfile, profiles }) : null;
}
