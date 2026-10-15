// process-mini-edit.js -- the inline mini editor's graph operations (mock M1,
// tcl-jqmrj5): pure functions over a process edit view ({template, edges,
// layout}) that insert, branch, run alongside, duplicate, re-kind and delete
// one step, plus the read-only kit projection the mini graph draws. No DOM
// and no fetch: the card (groups-mini-editor.js) applies each result to its
// ProcessEditModel as one undoable edit, and jstest/process-mini-edit.test.mjs
// exercises this file directly.
//
// A step here is what the step list edits: a task or decision done by an
// agent, by you (a decision, as the runtime requires) or by a command.
// Parallel, wait and subprocess nodes are drawn but edited in the full
// editor.

import { setStepWho } from './process-step-list.js';
import { LOOP_DEFAULT_ROUNDS, edgeGoesBack, loopBound, loopableEdge, setLoopBound } from './process-loops.js';

// PASS_LABELS mirrors model.PassOutcomeLabels: the edge a task's pass
// verdict follows.
const PASS_LABELS = ['pass', 'done', 'success', 'next'];
// A new step's continuing outcome: a task passes on, a "you" decision
// continues.
const CONTINUE = { agent: 'next', command: 'next', you: 'continue' };

export const MINI_KINDS = Object.freeze(['agent', 'you', 'command']);

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

function copyView(view) {
  const out = clone({ template: view.template || {}, edges: view.edges || [], layout: view.layout || {} });
  if (!out.template.nodes) out.template.nodes = {};
  if (!out.layout.nodes) out.layout.nodes = {};
  return out;
}

// miniKind is how the mini editor treats a node: start, end, one of the
// three step kinds, or 'advanced' (the full editor's).
export function miniKind(node) {
  const type = node?.type || 'task';
  if (type === 'start' || type === 'end') return type;
  if (type !== 'task' && type !== 'decision') return 'advanced';
  const kind = node.performer?.kind;
  if (kind === 'human') return 'you';
  if (kind === 'program') return 'command';
  return 'agent';
}

export function isStep(node) {
  return MINI_KINDS.includes(miniKind(node));
}

function outgoing(view, id) {
  return view.edges.filter((edge) => edge.from === id);
}

function incoming(view, id) {
  return view.edges.filter((edge) => edge.to === id && edge.from !== '');
}

// mainEdge is the edge a step continues along: a task's pass edge, a
// decision's first answer, a start's only edge.
export function mainEdge(view, id) {
  const out = outgoing(view, id);
  if (!out.length) return null;
  const node = view.template.nodes[id];
  if (node?.type === 'decision') {
    const first = (node.performer?.choices || []).find((choice) => out.some((edge) => edge.outcome === choice));
    if (first) return out.find((edge) => edge.outcome === first);
    return out[0];
  }
  return PASS_LABELS.map((label) => out.find((edge) => edge.outcome === label)).find(Boolean) || out[0];
}

function uniqueID(view, base) {
  const stem = String(base || 'step').toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[^a-z0-9]+/, '').replace(/-+$/, '') || 'step';
  let id = stem;
  for (let n = 2; view.template.nodes[id]; n += 1) id = `${stem}-${n}`;
  return id;
}

function uniqueOutcome(view, id, base) {
  const taken = new Set(outgoing(view, id).map((edge) => edge.outcome));
  let outcome = base;
  for (let n = 2; taken.has(outcome); n += 1) outcome = `${base}-${n}`;
  return outcome;
}

// newStepNode is a fresh step of `kind`.
export function newStepNode(kind, name = '') {
  const label = name || { agent: 'New step', you: 'Decide', command: 'Command' }[kind] || 'New step';
  if (kind === 'you') return { type: 'decision', name: label, performer: { kind: 'human', ask: '', choices: [CONTINUE.you] } };
  if (kind === 'command') return { type: 'task', name: label, performer: { kind: 'program', run: '' } };
  return { type: 'task', name: label, performer: { kind: 'agent', prompt: '' } };
}

function addNode(view, node, base) {
  const id = uniqueID(view, base || node.name);
  view.template.nodes[id] = node;
  return id;
}

// dropStaleJoins clears a join on any node left with fewer than two inbound
// edges, which the server rejects.
function dropStaleJoins(view) {
  for (const [id, node] of Object.entries(view.template.nodes)) {
    if (node.join && incoming(view, id).length < 2) delete node.join;
  }
}

// canInsertAfter / canInsertBefore / canBranch / canRunAlongside say why an
// operation is not offered on a node ('' when it is).
export function canInsertAfter(view, id) {
  const node = view.template.nodes[id];
  if (!node) return 'no such step';
  if (node.type === 'end') return 'nothing runs after an end';
  return '';
}

export function canInsertBefore(view, id) {
  const node = view.template.nodes[id];
  if (!node) return 'no such step';
  if (node.type === 'start') return 'nothing runs before the start';
  return '';
}

export function canBranch(view, id) {
  const node = view.template.nodes[id];
  if (!isStep(node)) return 'only a step branches';
  // The engine routes a task one way only; branching is a decision's.
  if (node.type === 'task') return 'a task continues one way; make it a "you" step or use a decision to branch';
  return '';
}

export function canRunAlongside(view, id) {
  const node = view.template.nodes[id];
  if (!isStep(node)) return 'only a step runs alongside another';
  if (view.template.start === id || view.edges.some((edge) => edge.from === '' && edge.to === id)) return 'the first step has no step before it to fan out from';
  if (incoming(view, id).length !== 1) return 'it has several steps leading to it';
  const main = mainEdge(view, id);
  if (!main) return 'it has no next step to join at';
  // The fork must close where it opened: one way out of the step, into a
  // node nothing else enters.
  if (outgoing(view, id).length !== 1) return 'its other outcomes would leave the parallel branch';
  if (incoming(view, main.to).length !== 1) return 'the next step is already where several paths meet';
  return '';
}

// syncLoops keeps each decision's loop bounds in step with its edges after
// an edit, as the step list's syncStepLoops does: a bound whose outcome no
// longer goes back is dropped, and an outcome that now goes back gets the
// default bound, so a draft never holds an unbounded loop with no rounds
// control to fix it.
export function syncLoops(view) {
  const { nodes } = view.template;
  for (const [id, node] of Object.entries(nodes)) {
    if (node.type !== 'decision') {
      delete node.loops;
      continue;
    }
    for (const outcome of Object.keys(node.loops || {})) {
      if (!loopableEdge(nodes, view.edges, id, outcome)) setLoopBound(node, outcome, null);
    }
    for (const edge of outgoing(view, id)) {
      if (!loopBound(node, edge.outcome) && loopableEdge(nodes, view.edges, id, edge.outcome)) {
        setLoopBound(node, edge.outcome, { maxIterations: LOOP_DEFAULT_ROUNDS });
      }
    }
  }
  return view;
}

// insertAfter puts a new step on `id`'s main path, before what came next.
export function insertAfter(view, id, kind) {
  const why = canInsertAfter(view, id);
  if (why) throw new Error(why);
  const next = copyView(view);
  const added = addNode(next, newStepNode(kind));
  const main = mainEdge(next, id);
  if (main) {
    const target = main.to;
    main.to = added;
    next.edges.push({ from: added, outcome: CONTINUE[kind], to: target });
  } else {
    next.edges.push({ from: id, outcome: next.template.nodes[id].type === 'decision' ? uniqueOutcome(next, id, 'next') : 'next', to: added });
  }
  return { view: syncLoops(next), id: added };
}

// insertBefore puts a new step in front of `id`: whatever led to it leads to
// the new step, which continues to it.
export function insertBefore(view, id, kind) {
  const why = canInsertBefore(view, id);
  if (why) throw new Error(why);
  const next = copyView(view);
  const node = newStepNode(kind);
  // A join belongs where the paths meet, which is now the new step.
  if (next.template.nodes[id].join) node.join = next.template.nodes[id].join;
  const added = addNode(next, node);
  for (const edge of next.edges) if (edge.to === id) edge.to = added;
  if (next.template.start === id) next.template.start = added;
  next.edges.push({ from: added, outcome: CONTINUE[kind], to: id });
  dropStaleJoins(next);
  return { view: syncLoops(next), id: added };
}

// branch adds an outcome to a decision that leads to a new step, which then
// rejoins its main path: a "you" step gains an answer, an agent decision an
// outcome.
export function branch(view, id, kind) {
  const why = canBranch(view, id);
  if (why) throw new Error(why);
  const next = copyView(view);
  const node = next.template.nodes[id];
  const main = mainEdge(next, id);
  let outcome;
  if (node.performer?.kind === 'human') {
    outcome = uniqueOutcome(next, id, 'option');
    node.performer.choices = [...(node.performer.choices || []), outcome];
  } else {
    outcome = uniqueOutcome(next, id, 'other');
  }
  const added = addNode(next, newStepNode(kind));
  next.edges.push({ from: id, outcome, to: added });
  if (main) next.edges.push({ from: added, outcome: CONTINUE[kind], to: main.to });
  return { view: syncLoops(next), id: added, outcome };
}

// runAlongside runs a new step in parallel with `id`: the edge into `id`
// goes to a new parallel node that fans out to both, and both join at what
// came after `id`.
export function runAlongside(view, id, kind) {
  const why = canRunAlongside(view, id);
  if (why) throw new Error(why);
  const next = copyView(view);
  const [into] = incoming(next, id);
  const main = mainEdge(next, id);
  const added = addNode(next, newStepNode(kind));
  const fork = addNode(next, { type: 'parallel', name: 'Together' }, 'together');
  into.to = fork;
  next.edges.push({ from: fork, outcome: id, to: id }, { from: fork, outcome: added, to: added });
  next.edges.push({ from: added, outcome: CONTINUE[kind], to: main.to });
  next.template.nodes[main.to].join = 'all';
  return { view: syncLoops(next), id: added, fork };
}

// duplicate puts a copy of `id` right after it: the copy takes over `id`'s
// outgoing edges and `id` continues to the copy.
export function duplicate(view, id) {
  const node = view.template.nodes[id];
  if (!isStep(node)) throw new Error('only a step can be duplicated here');
  const next = copyView(view);
  const copy = clone(node);
  copy.name = `${node.name || id} copy`;
  delete copy.join;
  const added = addNode(next, copy, `${id}-copy`);
  const main = mainEdge(next, id);
  for (const edge of outgoing(next, id)) next.edges.push({ from: added, outcome: edge.outcome, to: edge.to });
  if (main) {
    main.to = added;
  } else {
    next.edges.push({ from: id, outcome: CONTINUE[miniKind(node)] || 'next', to: added });
  }
  dropStaleJoins(next);
  return { view: syncLoops(next), id: added };
}

// changeKind turns a step into an agent, "you" or command step, keeping its
// name and text. A "you" step is a decision whose answers are its outcomes;
// leaving a decision keeps only its main path.
export function changeKind(view, id, kind) {
  const node = view.template.nodes[id];
  if (!isStep(node)) throw new Error('only a step can change kind here');
  const from = miniKind(node);
  if (from === kind) return { view, id };
  const next = copyView(view);
  const who = kind === 'you' ? { mode: 'you' } : kind === 'command' ? { mode: 'command' } : { mode: 'nobody' };
  const step = setStepWho({ id, node: next.template.nodes[id] }, who);
  const changed = step.node;
  const main = mainEdge(next, id);
  if (kind === 'you') {
    changed.type = 'decision';
    if (main) main.outcome = CONTINUE.you;
    const answers = outgoing(next, id).map((edge) => edge.outcome);
    changed.performer.choices = answers.length ? [CONTINUE.you, ...answers.filter((a) => a !== CONTINUE.you)] : [CONTINUE.you];
  }
  next.template.nodes[id] = changed;
  let orphans = [];
  if (from === 'you') {
    // A task continues one way: only the main answer's path stays, and the
    // steps only the other answers led to go.
    const before = reachable(next);
    changed.type = 'task';
    next.edges = next.edges.filter((edge) => edge.from !== id || edge === main);
    if (main) main.outcome = 'next';
    const after = reachable(next);
    orphans = [...before].filter((nodeID) => !after.has(nodeID));
    for (const orphan of orphans) removeNode(next, orphan);
  }
  dropStaleJoins(next);
  return { view: syncLoops(next), id, orphans };
}

// entryOf is where a run starts: the start pseudo edge's target, else
// template.start.
function entryOf(view) {
  return view.edges.find((edge) => edge.from === '')?.to || view.template.start || '';
}

function reachable(view) {
  const seen = new Set();
  const queue = [entryOf(view)].filter(Boolean);
  while (queue.length) {
    const id = queue.shift();
    if (seen.has(id) || !view.template.nodes[id]) continue;
    seen.add(id);
    for (const edge of outgoing(view, id)) queue.push(edge.to);
  }
  return seen;
}

function removeNode(view, id) {
  delete view.template.nodes[id];
  delete view.layout.nodes[id];
  view.edges = view.edges.filter((edge) => edge.from !== id && edge.to !== id);
}

// canDelete says why a node cannot be deleted ('' when it can): the start,
// or an end that steps still lead to.
export function canDelete(view, id) {
  const node = view.template.nodes[id];
  if (!node) return 'no such step';
  if (node.type === 'start') return 'the start cannot be deleted';
  if (node.type === 'end' && incoming(view, id).length) return 'steps still end here; point them elsewhere first';
  return '';
}

// deleteStep removes a step; what led to it goes where it continued, and a
// start that pointed at it moves on too. Steps only it led to (a decision's
// branches) go with it: `orphans` names them, so the menu can say so first.
export function deleteStep(view, id) {
  const why = canDelete(view, id);
  if (why) throw new Error(why);
  const node = view.template.nodes[id];
  const next = copyView(view);
  const before = reachable(next);
  const main = mainEdge(next, id);
  const target = main && main.to !== id ? main.to : '';
  next.edges = next.edges.filter((edge) => edge.from !== id);
  if (target) {
    for (const edge of next.edges) if (edge.to === id) edge.to = target;
  }
  removeNode(next, id);
  if (next.template.start === id) next.template.start = target;
  // A join moves on with the paths that met here.
  if (node.join && target && next.template.nodes[target] && !next.template.nodes[target].join) next.template.nodes[target].join = node.join;
  // A parallel fan-out names its branches by outcome; two branches landing on
  // one step collapse to one.
  const seen = new Set();
  next.edges = next.edges.filter((edge) => {
    if (next.template.nodes[edge.from]?.type !== 'parallel') return true;
    const key = `${edge.from}\u0000${edge.to}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  const after = reachable(next);
  const orphans = [...before].filter((nodeID) => nodeID !== id && !after.has(nodeID));
  for (const orphan of orphans) removeNode(next, orphan);
  dropStaleJoins(next);
  return { view: syncLoops(next), id: target, orphans };
}

// deleteOrphans names the steps deleting `id` would also remove.
export function deleteOrphans(view, id) {
  try {
    return deleteStep(view, id).orphans.filter((orphan) => isStep(view.template.nodes[orphan]))
      .map((orphan) => view.template.nodes[orphan]?.name || orphan);
  } catch {
    return [];
  }
}

// setOutcomeTarget points one of a step's outcomes at another node.
export function setOutcomeTarget(view, id, outcome, to) {
  const next = copyView(view);
  const edge = next.edges.find((e) => e.from === id && e.outcome === outcome);
  if (!edge) throw new Error(`no outcome ${outcome}`);
  edge.to = to;
  dropStaleJoins(next);
  return { view: syncLoops(next), id };
}

// addOutcome gives a decision one more outcome, leading to `to`.
export function addOutcome(view, id, to) {
  const node = view.template.nodes[id];
  if (node?.type !== 'decision') throw new Error('only a decision has more outcomes');
  const next = copyView(view);
  const decision = next.template.nodes[id];
  const outcome = uniqueOutcome(next, id, decision.performer?.kind === 'human' ? 'option' : 'other');
  if (decision.performer?.kind === 'human') decision.performer.choices = [...(decision.performer.choices || []), outcome];
  next.edges.push({ from: id, outcome, to });
  return { view: syncLoops(next), id, outcome };
}

// removeOutcome drops one outcome of a decision (never its last).
export function removeOutcome(view, id, outcome) {
  const next = copyView(view);
  const decision = next.template.nodes[id];
  if (outgoing(next, id).length < 2) throw new Error('a decision keeps at least one outcome');
  next.edges = next.edges.filter((edge) => !(edge.from === id && edge.outcome === outcome));
  if (decision.performer?.kind === 'human') decision.performer.choices = (decision.performer.choices || []).filter((c) => c !== outcome);
  dropStaleJoins(next);
  return { view: syncLoops(next), id };
}

// renameOutcome renames one outcome of a decision (an answer, for "you").
export function renameOutcome(view, id, outcome, name) {
  const value = String(name || '').trim();
  if (!value || value === outcome) return { view, id };
  if (outgoing(view, id).some((edge) => edge.outcome === value)) throw new Error(`${value} is already an outcome`);
  const next = copyView(view);
  const edge = next.edges.find((e) => e.from === id && e.outcome === outcome);
  if (edge) edge.outcome = value;
  const decision = next.template.nodes[id];
  const bound = loopBound(decision, outcome);
  if (bound) {
    setLoopBound(decision, outcome, null);
    setLoopBound(decision, value, bound);
  }
  if (decision.performer?.kind === 'human') decision.performer.choices = (decision.performer.choices || []).map((c) => (c === outcome ? value : c));
  return { view: syncLoops(next), id };
}

// updateStep applies a step-list edit (setStepName, setStepText, setStepWho)
// to one node.
export function updateStep(view, id, edit) {
  const next = copyView(view);
  next.template.nodes[id] = edit({ id, node: next.template.nodes[id] }).node;
  return { view: syncLoops(next), id };
}

// outcomesOf lists a step's outcomes with their targets, main first.
export function outcomesOf(view, id) {
  const main = mainEdge(view, id);
  const out = outgoing(view, id);
  return [...(main ? [main] : []), ...out.filter((edge) => edge !== main)].map((edge) => ({ outcome: edge.outcome, to: edge.to, main: edge === main }));
}

// targetsFor lists the nodes an outcome of `id` may lead to: every node but
// the start and the step itself. Going back to earlier work is a decision's
// bounded loop, so a task is offered only targets ahead of it (a backward
// target it already has stays), and a decision's backward targets say so
// (back: true).
export function targetsFor(view, id, outcome = '') {
  const decision = view.template.nodes[id]?.type === 'decision';
  const current = view.edges.find((edge) => edge.from === id && edge.outcome === outcome);
  return Object.entries(view.template.nodes)
    .filter(([nodeID, node]) => nodeID !== id && node.type !== 'start')
    .map(([nodeID, node]) => {
      const edges = current ? view.edges.map((edge) => (edge === current ? { ...edge, to: nodeID } : edge)) : view.edges;
      const back = !!current && edgeGoesBack(view.template.nodes, edges, id, outcome);
      return { id: nodeID, back, label: node.type === 'end' ? `end${node.name ? ` (${node.name})` : ''}` : node.name || nodeID };
    })
    .filter((target) => decision || !target.back || target.id === current?.to);
}

// setLoopRounds sets how many rounds a decision's backward outcome may run.
export function setLoopRounds(view, id, outcome, rounds) {
  const next = copyView(view);
  const node = next.template.nodes[id];
  const bound = loopBound(node, outcome);
  if (!bound) throw new Error(`${outcome} does not go back`);
  setLoopBound(node, outcome, { ...bound, maxIterations: rounds });
  return { view: next, id };
}

// draftProjection is the kit run projection of a draft: every node planned,
// a step's sub line saying who does it, and an edge labelled by its outcome
// unless it is a plain pass.
export function draftProjection(view, { whoOf = () => '', name = '' } = {}) {
  const nodes = Object.entries(view.template?.nodes || {}).map(([id, node]) => {
    const kind = miniKind(node);
    const projected = { id, label: node.name || (node.type === 'end' ? 'end' : id), state: 'planned', sub: isStep(node) ? whoOf(id, node) : '' };
    if (node.type === 'task' && node.performer) projected.performer = { kind: node.performer.kind };
    else projected.kind = node.type === 'task' ? 'agent' : node.type;
    if (kind === 'advanced') projected.sub = 'full editor';
    return projected;
  });
  const edges = (view.edges || []).filter((edge) => edge.from !== '').map((edge) => {
    const projected = {
      from: edge.from, to: edge.to, label: PASS_LABELS.includes(edge.outcome) || view.template.nodes[edge.from]?.type === 'parallel' ? '' : edge.outcome,
    };
    // A bounded loop (node.loops[outcome]) draws as a back edge with its bound.
    const max = Number(view.template.nodes[edge.from]?.loops?.[edge.outcome]?.maxIterations) || 0;
    return max > 0 ? { ...projected, label: projected.label || edge.outcome, back: true, maxIterations: max } : projected;
  });
  return { id: 'draft', name: name || view.template?.name || 'draft', status: 'planned', nodes, edges };
}

// changeSummary names what a draft changed against the stored view: steps
// added and removed, and the steps whose definition or outcomes changed.
export function changeSummary(before, after) {
  const was = before.template?.nodes || {};
  const now = after.template?.nodes || {};
  const name = (nodes, id) => nodes[id]?.name || id;
  const added = Object.keys(now).filter((id) => !was[id]).map((id) => `added “${name(now, id)}”`);
  const removed = Object.keys(was).filter((id) => !now[id]).map((id) => `removed “${name(was, id)}”`);
  const edgesOf = (view, id) => JSON.stringify((view.edges || []).filter((e) => e.from === id).map((e) => [e.outcome, e.to]).sort());
  const changed = Object.keys(now).filter((id) => was[id] && (JSON.stringify(was[id]) !== JSON.stringify(now[id]) || edgesOf(before, id) !== edgesOf(after, id)))
    .map((id) => `${name(now, id)} changed`);
  return [...added, ...removed, ...changed];
}
