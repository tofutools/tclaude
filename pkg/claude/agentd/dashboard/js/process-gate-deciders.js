// process-gate-deciders.js -- each decider of an agreement gate on its own
// row (tk_44d02f32, mock item 5 variant A). A decider has its own ask, its
// own "if nobody" (the performer fallback: spawn one with a profile) and its
// own "not" (differentFrom: not the agent who did another step). The gate's
// Asks is the text the deciders share; a decider whose text differs has its
// own ask. Pure: no DOM, no fetch.

import { firstSpawn } from './process-quickstart.js';
import { expandParamShorthand } from './process-step-list.js';
import { addDecider, deciderWho, gateDeciders } from './process-mini-edit.js';
import { keepLifetime, withLifetime } from './process-spawn-lifetime.js';

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

// gateAsk is the text most of a gate's deciders are asked: the gate's Asks.
// On a tie it is `prefer` when that is one of them (the editor passes the
// Asks it showed last, so giving one decider its own ask never swaps which
// text is the shared one), else the first.
export function gateAsk(node, prefer = null) {
  const counts = new Map();
  for (const decider of node?.deciders || []) {
    const text = textOf(decider);
    counts.set(text, (counts.get(text) || 0) + 1);
  }
  let best = '';
  let most = 0;
  for (const [text, count] of counts) if (count > most) { best = text; most = count; }
  if (prefer !== null && prefer !== best && counts.get(prefer) === most) return prefer;
  return best;
}

// deciderOwnAsk is decider index's own ask, or '' when it is asked the
// gate's Asks.
export function deciderOwnAsk(node, index, shared = gateAsk(node)) {
  const text = textOf(node?.deciders?.[index]);
  return text === shared ? '' : text;
}

function edit(view, id, change) {
  const next = clone(view);
  const node = next.template.nodes[id];
  if (!node?.deciders?.length) return { view, id };
  change(node);
  return { view: next, id };
}

// setGateAsk changes the gate's Asks (`shared`, the one shown): every
// decider without its own ask. The text is stored with {{x}} expanded.
export function setGateAsk(view, id, text, shared = null) {
  return edit(view, id, (node) => {
    const old = shared ?? gateAsk(node);
    for (const decider of node.deciders) if (textOf(decider) === old) withText(decider, expandParamShorthand(text));
  });
}

// setDeciderAsk gives decider index its own ask; blank asks it the gate's
// (`shared`, the one shown).
export function setDeciderAsk(view, id, index, text, shared = null) {
  return edit(view, id, (node) => {
    const decider = node.deciders[index];
    if (!decider) return;
    withText(decider, text.trim() ? expandParamShorthand(text) : shared ?? gateAsk(node));
  });
}

// setDeciderFallback sets what decider index does when no member can take
// its vote; null follows the process's fallback.
export function setDeciderFallback(view, id, index, fallback) {
  return edit(view, id, (node) => {
    const decider = node.deciders[index];
    if (decider?.kind !== 'agent') return;
    const before = decider.fallback;
    delete decider.fallback;
    if (fallback || decider.spawn === 'allowed') delete decider.spawn;
    if (fallback) decider.fallback = keepLifetime(before, fallback);
  });
}

// setDeciderLifetime sets the lifetime of decider index's spawn fallback, as
// picked (the decider default clears it).
export function setDeciderLifetime(view, id, index, lifetime) {
  return edit(view, id, (node) => {
    const decider = node.deciders[index];
    if (decider?.fallback?.action !== 'spawn') return;
    decider.fallback = withLifetime(decider.fallback, lifetime, { decider: true });
  });
}

// setDeciderNot keeps decider index off the agents who did steps.
// A decider naming one agent takes no "not": the validator refuses both.
export function setDeciderNot(view, id, index, steps) {
  return edit(view, id, (node) => {
    const decider = node.deciders[index];
    if (decider?.kind !== 'agent' || decider.assignee) return;
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

// addReviewer is a gate's + another decider: a new agent decider kept off
// the reviewed step's agent, and so is the first decider when this makes the
// plain decision a gate (it was its own performer, picked before there was a
// gate to review for) and names nobody yet or anyone: a lead or a role was
// picked on purpose, and kept off a step that lead or role did it could
// never vote.
export function addReviewer(view, id) {
  const index = gateDeciders(view.template.nodes[id]).length;
  const added = addDecider(view, id, { mode: 'nobody' });
  const was = view.template.nodes[id]?.performer;
  const open = index === 1 && was?.kind === 'agent' && !was.lead && !was.role && !was.profile;
  const first = open ? withReviewerDefault(added.view, id, 0).view : added.view;
  return withReviewerDefault(first, id, index);
}

// spawns says whether decider spawns when no member can vote: its own
// fallback, else the legacy spawn field, else the process's; a spawn with no
// profile to use (none named, no group default) waits.
function spawns(decider, processFallback, defaultSpawnProfile) {
  if (decider.fallback?.action) return decider.fallback.action === 'spawn' && !!(decider.fallback.profile || defaultSpawnProfile);
  if (decider.spawn === 'allowed') return !!decider.profile;
  return processFallback?.action === 'spawn' && !!(processFallback.profile || defaultSpawnProfile);
}

// deciderSummary is what a closed decider row says is set besides who:
// "own ask · not implement · if nobody: spawn sonnet-dev".
export function deciderSummary(view, id, index, shared = null) {
  const node = view.template.nodes[id];
  const decider = node?.deciders?.[index];
  if (!decider) return '';
  const name = (step) => view.template.nodes[step]?.name || step;
  const parts = [];
  if (deciderOwnAsk(node, index, shared ?? gateAsk(node))) parts.push('own ask');
  if (decider.kind === 'agent') {
    if (decider.differentFrom?.length) parts.push(`not ${decider.differentFrom.map(name).join(', ')}`);
    if (decider.sameAs) parts.push(`same as ${name(decider.sameAs)}`);
    const action = decider.fallback?.action || (decider.spawn === 'allowed' ? 'spawn' : '');
    // A new agent's spawn is its Who, shown in the row already.
    const newAgent = deciderWho(decider).mode === 'spawn';
    if (action === 'spawn') {
      if (!newAgent) parts.push(decider.fallback?.profile ? `if nobody: spawn ${decider.fallback.profile}` : 'if nobody: spawn');
    } else if (action) parts.push(`if nobody: ${action}`);
  }
  return parts.join(' · ');
}

// eligible is the names of the roster members who could cast decider's vote;
// null when it names nobody yet. A "not" step's performer is out when the
// editor can tell who that is: its named agent, or the one member with its
// role.
function eligible(view, decider, roster) {
  let names;
  const role = decider.role || (!decider.assignee && !decider.anyone && !decider.lead ? decider.profile : '');
  if (decider.assignee) names = roster.filter((m) => m.name === decider.assignee);
  else if (role) names = roster.filter((m) => m.role === role);
  else if (decider.anyone) names = roster;
  else if (decider.lead) {
    const leads = roster.filter((m) => m.role === 'lead');
    names = leads.length ? leads : roster.slice(0, 1);
  } else return null;
  const out = new Set();
  for (const step of decider.differentFrom || []) {
    const performer = view.template.nodes[step]?.performer;
    if (performer?.kind !== 'agent') continue;
    if (performer.assignee) out.add(performer.assignee);
    const stepRole = performer.role || (!performer.assignee && !performer.anyone && !performer.lead ? performer.profile : '');
    const holders = stepRole ? roster.filter((m) => m.role === stepRole) : [];
    if (holders.length === 1) out.add(holders[0].name);
  }
  return names.map((m) => m.name).filter((name) => !out.has(name));
}

// stuckGate says when a gate would wait forever: no agent votes twice at one
// gate, so its agent deciders need as many members who can vote, and one
// that cannot be given a member does not spawn. Deciders that do not spawn
// are matched first. Returns {deciders, members, noun, index} for the vote
// left waiting, or null.
export function stuckGate(view, id, roster = [], processFallback = null, defaultSpawnProfile = '') {
  const deciders = view.template.nodes[id]?.deciders || [];
  const agents = deciders.map((decider, index) => ({ decider, index, names: decider.kind === 'agent' ? eligible(view, decider, roster) : null }))
    .filter((entry) => entry.names);
  if (agents.length < 2) return null;
  const held = new Map(); // member name → the entry holding it
  const place = (entry, seen) => {
    for (const name of entry.names) {
      if (seen.has(name)) continue;
      seen.add(name);
      const holder = held.get(name);
      if (!holder || place(holder, seen)) { held.set(name, entry); return true; }
    }
    return false;
  };
  const waiting = agents.filter((entry) => !spawns(entry.decider, processFallback, defaultSpawnProfile));
  const unplaced = waiting.filter((entry) => !place(entry, new Set()));
  if (!unplaced.length) return null;
  const pool = new Set(agents.flatMap((entry) => entry.names));
  const roles = new Set(agents.map((entry) => entry.decider.role || ''));
  const role = roles.size === 1 ? [...roles][0] : '';
  const noun = role ? (pool.size === 1 ? role : `${role}s`) : (pool.size === 1 ? 'member who can vote' : 'members who can vote');
  return { deciders: agents.length, members: pool.size, noun, index: unplaced[0].index };
}

// spawnIfNobody is the fallback the stuck-gate warning's link sets.
export function spawnIfNobody({ defaultSpawnProfile = '', profiles = [] } = {}) {
  return defaultSpawnProfile || profiles.some(Boolean) ? firstSpawn({ defaultSpawnProfile, profiles }) : null;
}
