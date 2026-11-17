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
import { LOOP_DEFAULT_ROUNDS, edgeGoesBack, exhaustionChoices, loopBound, loopableEdge, setLoopBound } from './process-loops.js';

// PASS_LABELS mirrors model.PassOutcomeLabels: the edge a task's pass
// verdict follows.
const PASS_LABELS = ['pass', 'done', 'success', 'next'];
// A new step's continuing outcome: a task passes on, a "you" decision
// continues.
const CONTINUE = { agent: 'next', command: 'next', you: 'continue' };
// DECISION_KINDS are the kinds that insert a decision with named answers:
// 'decide' is decided by an agent (pick who), 'decide-you' by you. Both
// start with yes and no, each going on to what came next; the answers are
// renamed, re-aimed and added to in the inspector.
export const DECISION_KINDS = Object.freeze(['decide', 'decide-you']);
const DECISION_OUTCOMES = Object.freeze(['yes', 'no']);
// REVIEW is branch's kind for "this agent step decides" (toAgentDecision).
export const REVIEW = 'review';

// continuesOn is the outcomes a new step of kind leaves by, all to the
// same next node.
function continuesOn(kind) {
  return DECISION_KINDS.includes(kind) ? [...DECISION_OUTCOMES] : [CONTINUE[kind]];
}

function linkOn(view, from, kind, to) {
  for (const outcome of continuesOn(kind)) view.edges.push({ from, outcome, to });
}

// A gate is an agreement gate: a decision several deciders take together.
export const MINI_KINDS = Object.freeze(['agent', 'you', 'command', 'gate']);

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
  if (type === 'decision' && node.deciders?.length) return 'gate';
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

// before is the distinct nodes with an edge into id: a decision whose
// several answers lead to id is one step before it, not several.
function before(view, id) {
  return [...new Set(incoming(view, id).map((edge) => edge.from))];
}

// mainEdge is the edge a step continues along: a task's pass edge, a
// decision's first answer that does not loop back, a start's only edge.
export function mainEdge(view, id) {
  const out = outgoing(view, id);
  if (!out.length) return null;
  const node = view.template.nodes[id];
  if (node?.type === 'decision') {
    const forward = out.filter((edge) => !loopBound(node, edge.outcome));
    const pool = forward.length ? forward : out;
    const first = (node.performer?.choices || []).find((choice) => pool.some((edge) => edge.outcome === choice));
    if (first) return pool.find((edge) => edge.outcome === first);
    return pool[0];
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
  const label = name || { agent: 'New step', you: 'Decide', command: 'Command', decide: 'Decision', 'decide-you': 'Decision' }[kind] || 'New step';
  if (kind === 'decide') return { type: 'decision', name: label, performer: { kind: 'agent', prompt: '' } };
  if (kind === 'decide-you') return { type: 'decision', name: label, performer: { kind: 'human', ask: '', choices: [...DECISION_OUTCOMES] } };
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
  // The engine routes a task one way only; branching is a decision's. A
  // task branches through a decision put after it (branchKinds), or, when
  // canDecide allows, by its agent deciding (toAgentDecision).
  return '';
}

// branchKinds are the kinds ⑂ offers on id: on a decision, a new step of
// any kind on a new outcome; on a task, a decision after it (an agent's or
// yours), plus REVIEW when its own agent can decide.
export function branchKinds(view, id) {
  const node = view.template.nodes[id];
  if (node?.type !== 'task') return null;
  return [...(canDecide(view, id) ? [] : [REVIEW]), ...DECISION_KINDS];
}

// canDecide says why an agent step cannot become an agent decision, or ''.
// A command reports no verdict; outputs are a task's; the "changes" outcome
// goes back to the one step before it, so there must be one.
export function canDecide(view, id) {
  const node = view.template.nodes[id];
  if (node?.type !== 'task') return 'only a task becomes a decision';
  if (node.performer?.kind !== 'agent') return 'a command continues one way; add a "you" step after it to branch';
  if (node.captures?.length) return 'a step with outputs continues one way; clear its outputs to let its agent decide';
  // Plan, checks, review and retry are a task's; a decision would drop them.
  if (['plan', 'checks', 'review', 'retry'].some((key) => node[key] !== undefined)) return 'its plan, checks, review or retry are a task\'s; change it in the full editor';
  if (outgoing(view, id).length > 1) return 'it already has more than one way out; change it in the full editor';
  const into = before(view, id);
  if (into.length !== 1 || !isStep(view.template.nodes[into[0]])) return 'it needs exactly one step before it to send changes back to';
  if (overlapsALoop(view, [into[0], id])) return 'going back from it would cross another loop; the server refuses loops that overlap';
  return '';
}

// overlapsALoop says whether a loop whose body is `body` would overlap an
// existing loop without one containing the other, which the server refuses
// (loop_overlap). An existing loop's body is what lies on a forward path from
// its target to the decision that goes back.
function overlapsALoop(view, body) {
  const { nodes } = view.template;
  const forward = view.edges.filter((edge) => edge.from !== '' && !loopBound(nodes[edge.from], edge.outcome));
  const reach = (start, step) => {
    const seen = new Set([start]);
    const stack = [start];
    while (stack.length) {
      const at = stack.pop();
      for (const next of step(at)) if (!seen.has(next)) { seen.add(next); stack.push(next); }
    }
    return seen;
  };
  const mine = new Set(body);
  for (const edge of view.edges) {
    if (!edge.from || !loopBound(nodes[edge.from], edge.outcome)) continue;
    const ahead = reach(edge.to, (at) => forward.filter((e) => e.from === at).map((e) => e.to));
    const behind = reach(edge.from, (at) => forward.filter((e) => e.to === at).map((e) => e.from));
    const theirs = [...ahead].filter((n) => behind.has(n));
    const shared = theirs.filter((n) => mine.has(n)).length;
    if (shared && shared !== theirs.length && shared !== mine.size) return true;
  }
  return false;
}

// toAgentDecision turns an agent step into an agent decision, the review
// shape: its agent reports "approved" (on to what came next) or "changes"
// (back to the step before it, at most LOOP_DEFAULT_ROUNDS rounds, then the
// run fails). The agent, its prompt and its fallback stay.
export function toAgentDecision(view, id) {
  const why = canDecide(view, id);
  if (why) throw new Error(why);
  const next = copyView(view);
  const node = next.template.nodes[id];
  const main = mainEdge(next, id);
  const previous = before(next, id)[0];
  node.type = 'decision';
  let after = main?.to;
  next.edges = next.edges.filter((edge) => edge.from !== id);
  if (!after) {
    after = Object.keys(next.template.nodes).find((nodeID) => next.template.nodes[nodeID]?.type === 'end' && !isFailedEnd(next.template.nodes[nodeID]))
      || addNode(next, { type: 'end', name: 'Done', result: 'success' }, 'done');
  }
  next.edges.push({ from: id, outcome: 'approved', to: after }, { from: id, outcome: 'changes', to: previous });
  syncLoops(next);
  defaultAgentExhaustion(next, id);
  return { view: next, id };
}

export function canRunAlongside(view, id) {
  const node = view.template.nodes[id];
  if (!isStep(node)) return 'only a step runs alongside another';
  if (view.template.start === id || view.edges.some((edge) => edge.from === '' && edge.to === id)) return 'the first step has no step before it to fan out from';
  if (before(view, id).length !== 1) return 'it has several steps leading to it';
  const main = mainEdge(view, id);
  if (!main) return 'it has no next step to join at';
  // The fork must close where it opened: one way out of the step, into a
  // node nothing else enters.
  if (outgoing(view, id).length !== 1) return 'its other outcomes would leave the parallel branch';
  if (incoming(view, main.to).length !== 1 && !widensFork(view, id)) return 'the next step is already where several paths meet';
  return '';
}

// widensFork is the parallel fork a one-step branch id hangs off, when
// running another step alongside it adds a branch to that same fork and
// join (three reviewers side by side, not a fork inside a fork); else ''.
function widensFork(view, id) {
  const [from] = before(view, id);
  if (view.template.nodes[from]?.type !== 'parallel') return '';
  const main = mainEdge(view, id);
  if (!main || !view.template.nodes[main.to]?.join) return '';
  // Every branch of the fork must arrive at that join, or the new one would
  // close a scope the others leave open.
  const branches = outgoing(view, from);
  return branches.every((edge) => incoming(view, main.to).some((into) => into.from === edge.to)) ? from : '';
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
    const outcomes = outgoing(view, id).map((edge) => edge.outcome);
    for (const outcome of outcomes) {
      if (!loopBound(node, outcome) && loopableEdge(nodes, view.edges, id, outcome)) {
        setLoopBound(node, outcome, { maxIterations: LOOP_DEFAULT_ROUNDS });
      }
    }
    // A used-up loop may only follow another outcome that does not loop.
    for (const outcome of Object.keys(node.loops || {})) {
      const bound = loopBound(node, outcome);
      if (bound.onExhausted && !exhaustionChoices(node, outcomes, outcome).includes(bound.onExhausted)) {
        setLoopBound(node, outcome, { maxIterations: bound.maxIterations });
      }
    }
    if (node.deciders?.length) syncGate(view, id, node, outcomes);
  }
  return view;
}

// defaultAgentExhaustion gives an agent decision's loops a way out as a
// gate's get one: past the last round its send-back is refused, and an agent
// asked again can only pass the work on. Applied only where that decision is
// edited (its Who, a new loop on it), never as a side effect of another edit,
// so a saved template the operator did not touch keeps its behaviour.
function defaultAgentExhaustion(view, id) {
  const node = view.template.nodes[id];
  if (node?.type !== 'decision' || node.deciders?.length || node.performer?.kind !== 'agent') return;
  const outcomes = outgoing(view, id).map((edge) => edge.outcome);
  for (const outcome of Object.keys(node.loops || {})) {
    const bound = loopBound(node, outcome);
    if (bound.onExhausted) continue;
    const way = safeExhaustion(view, id, node, outcomes, outcome, { refuse: true });
    if (way) setLoopBound(node, outcome, { ...bound, onExhausted: way });
  }
}

// passOutcome is the outcome that passes the work on: a gate's agree, else
// the decision's main way on.
function passOutcome(view, id, node) {
  return node.agree || mainEdge(view, id)?.outcome || '';
}

// decidedAlone says whether nobody but an agent decides: a gate, or a plain
// decision an agent decides. Its used-up loop cannot be left to a person.
function decidedAlone(node) {
  return !!node.deciders?.length || node.performer?.kind === 'agent';
}

// syncGate keeps a gate valid after its edges change: what all must choose
// never goes back, and every loop has its way out up front, since nobody is
// there to pick one when the rounds run out.
function syncGate(view, id, node, outcomes) {
  if (!outcomes.includes(node.agree) || loopBound(node, node.agree)) {
    node.agree = agreeChoice(view, id, node);
    rehomeFromAgree(view, id, node);
  }
  for (const outcome of Object.keys(node.loops || {})) {
    const bound = loopBound(node, outcome);
    if (bound.onExhausted) continue;
    setLoopBound(node, outcome, { ...bound, onExhausted: safeExhaustion(view, id, node, outcomes, outcome) });
  }
}

// GIVE_UP is the outcome a used-up gate loop takes when nothing else is
// there: it ends the run as failed. Never the agree outcome: a review the
// deciders kept sending back must not go on as if they had approved it.
export const GIVE_UP = 'give-up';

// FAIL_RUN is the picker's value for "fail the run" when no outcome goes to a
// failed end yet: not a possible outcome name, so it never collides with one.
export const FAIL_RUN = '\u0000fail';

// safeExhaustion is where a gate's used-up loop goes by default: another of
// its outcomes that neither loops nor is the agree outcome, else a give-up
// outcome to a failed end (added here, on the view being built).
// Inside a parallel branch a new end would leave the open fork, which the
// server refuses: there a gate keeps its old default (the first other way,
// which validation warns about when it is agree), and a refusable decision
// gets none (refuse: true).
function safeExhaustion(view, id, node, outcomes, outcome, { refuse = false } = {}) {
  const passing = passOutcome(view, id, node);
  const ways = exhaustionChoices(node, outcomes, outcome);
  const way = ways.find((o) => o !== passing && !APPROVING.test(o));
  if (way) return way;
  const failing = failingOutcome(view, id);
  if (failing) return failing;
  if (!inParallelBranch(view, id)) return addGiveUp(view, id);
  return refuse ? '' : (ways.find((o) => o !== node.agree) || ways[0] || '');
}

// failingOutcome is an outcome of id that goes to a failed end, or ''.
function failingOutcome(view, id) {
  return outgoing(view, id).find((edge) => isFailedEnd(view.template.nodes[edge.to]))?.outcome || '';
}

// inParallelBranch says whether id sits between a parallel fork and its join:
// walking back along forward edges, a fork met before its join opens a scope.
export function inParallelBranch(view, id) {
  const { nodes } = view.template;
  const forward = view.edges.filter((edge) => edge.from !== '' && !loopBound(nodes[edge.from], edge.outcome));
  const seen = new Set();
  const stack = [[id, 0]];
  while (stack.length) {
    const [at, depth] = stack.pop();
    const key = `${at}:${depth}`;
    if (seen.has(key)) continue;
    seen.add(key);
    for (const edge of forward.filter((e) => e.to === at)) {
      const from = nodes[edge.from];
      let d = depth;
      if (from?.type === 'parallel') {
        if (d === 0) return true;
        d -= 1;
      }
      if (from?.join) d += 1;
      stack.push([edge.from, d]);
    }
  }
  return false;
}

// addGiveUp gives decision id an outcome to a failed end (the draft's own
// failed end when it has one) and returns its name.
function addGiveUp(view, id) {
  const existing = failingOutcome(view, id);
  if (existing) return existing;
  let end = Object.keys(view.template.nodes).find((nodeID) => isFailedEnd(view.template.nodes[nodeID]));
  if (!end) end = addNode(view, { type: 'end', name: 'Gave up', result: 'failed' }, 'gave-up');
  const outcome = uniqueOutcome(view, id, GIVE_UP);
  view.edges.push({ from: id, outcome, to: end });
  const node = view.template.nodes[id];
  if (node.performer?.kind === 'human') node.performer.choices = [...(node.performer.choices || []), outcome];
  return outcome;
}

function isFailedEnd(node) {
  return node?.type === 'end' && /^fail/i.test(String(node.result || '').trim());
}

// exhaustionOptions are where a used-up loop on outcome may go: every other
// outcome that does not loop, the agree outcome last and only when it is
// already chosen (it skips the review), and FAIL_RUN ("fail the run") when no
// outcome goes to a failed end yet and one can be added (not inside a
// parallel branch). A person deciding may also refuse ('': asked again); a
// gate or a lone agent may not.
export function exhaustionOptions(view, id, outcome) {
  const node = view.template.nodes[id];
  const outcomes = outgoing(view, id).map((edge) => edge.outcome);
  const current = loopBound(node, outcome)?.onExhausted || '';
  const gate = decidedAlone(node);
  const passing = passOutcome(view, id, node);
  const passes = (o) => o === passing || APPROVING.test(o);
  const ways = exhaustionChoices(node, outcomes, outcome).filter((o) => !gate || !passes(o) || o === current);
  const options = gate ? [] : [{ value: '', label: 'ask again' }];
  for (const way of ways) {
    const to = view.edges.find((edge) => edge.from === id && edge.outcome === way)?.to;
    const failed = isFailedEnd(view.template.nodes[to]);
    options.push({ value: way, label: failed ? 'fail the run' : way, skipsReview: gate && passes(way) });
  }
  if (!options.some((o) => o.label === 'fail the run') && !inParallelBranch(view, id)) options.push({ value: FAIL_RUN, label: 'fail the run', adds: true });
  return options;
}

// setExhaustion sets where outcome's used-up loop goes: an outcome, '' to
// refuse (a person deciding only), or FAIL_RUN for a failed end (an outcome
// to one is added if the decision has none yet).
export function setExhaustion(view, id, outcome, value) {
  const next = copyView(view);
  const node = next.template.nodes[id];
  const bound = loopBound(node, outcome);
  if (!bound) throw new Error(`${outcome} does not go back`);
  const outcomes = outgoing(next, id).map((edge) => edge.outcome);
  let way = value;
  if (way === FAIL_RUN) way = addGiveUp(next, id);
  else if (way && !outcomes.includes(way)) throw new Error(`${way} is not an outcome of this step`);
  if (!way && decidedAlone(node)) throw new Error('nobody but its deciders decides here: it needs somewhere to go once its rounds run out');
  setLoopBound(node, outcome, way ? { ...bound, onExhausted: way } : { maxIterations: bound.maxIterations });
  return { view: next, id };
}

// setDecisionWho changes who decides a decision and keeps it a decision: its
// outcomes, loops and their bounds stay. A person answers with the outcomes;
// an agent reports one of them as its verdict.
export function setDecisionWho(view, id, who) {
  const node = view.template.nodes[id];
  if (node?.type !== 'decision' || node.deciders?.length) throw new Error('only a plain decision changes who decides');
  if (who.mode === 'command') throw new Error('a command does not decide');
  const step = setStepWho({ id, node }, who);
  const changed = step.node;
  if (changed.performer.kind === 'human') changed.performer.choices = outgoing(view, id).map((edge) => edge.outcome);
  else {
    delete changed.performer.choices;
    delete changed.performer.choiceOutcomes;
  }
  const next = copyView(view);
  next.template.nodes[id] = changed;
  syncLoops(next);
  defaultAgentExhaustion(next, id);
  return { view: next, id };
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
    linkOn(next, added, kind, target);
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
  linkOn(next, added, kind, id);
  dropStaleJoins(next);
  return { view: syncLoops(next), id: added };
}

// branch adds an outcome to a decision that leads to a new step, which then
// rejoins its main path: a "you" step gains an answer, an agent decision an
// outcome. On a task it puts a decision of `kind` after it (the task keeps
// its outputs and the rest), or with REVIEW makes its agent decide
// (toAgentDecision).
export function branch(view, id, kind) {
  const why = canBranch(view, id);
  if (why) throw new Error(why);
  if (view.template.nodes[id].type === 'task') {
    if (kind === REVIEW) return toAgentDecision(view, id);
    if (!DECISION_KINDS.includes(kind)) throw new Error('a step branches through a decision after it');
    return insertAfter(view, id, kind);
  }
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
  if (main) linkOn(next, added, kind, main.to);
  return { view: syncLoops(next), id: added, outcome };
}

// runAlongside runs a new step in parallel with `id`: the edge into `id`
// goes to a new parallel node that fans out to both, and both join at what
// came after `id`. On a one-step branch of a fork already there, the new step
// is one more branch of that fork, joining where the others do.
export function runAlongside(view, id, kind) {
  const why = canRunAlongside(view, id);
  if (why) throw new Error(why);
  const next = copyView(view);
  const main = mainEdge(next, id);
  const added = addNode(next, newStepNode(kind));
  const widened = incoming(next, main.to).length > 1 ? widensFork(next, id) : '';
  if (widened) {
    next.edges.push({ from: widened, outcome: added, to: added });
    linkOn(next, added, kind, main.to);
    return { view: syncLoops(next), id: added, fork: widened };
  }
  const fork = addNode(next, { type: 'parallel', name: 'Together' }, 'together');
  // Every edge in comes from the one step before it (a decision's answers
  // may be several): they all lead to the fork now.
  for (const into of incoming(next, id)) into.to = fork;
  next.edges.push({ from: fork, outcome: id, to: id }, { from: fork, outcome: added, to: added });
  linkOn(next, added, kind, main.to);
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
  // Output names are unique to one step.
  delete copy.captures;
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
  if (from === 'gate' || kind === 'gate') throw new Error('a gate gains and loses deciders, not a kind');
  if (from === kind) return { view, id };
  const next = copyView(view);
  const who = kind === 'you' ? { mode: 'you' } : kind === 'command' ? { mode: 'command' } : { mode: 'nobody' };
  const step = setStepWho({ id, node: next.template.nodes[id] }, who);
  const changed = step.node;
  const main = mainEdge(next, id);
  if (kind === 'you' && node.type === 'decision') {
    // An agent's decision becomes yours with the same answers.
    changed.performer.choices = outgoing(next, id).map((edge) => edge.outcome);
  } else if (kind === 'you') {
    changed.type = 'decision';
    if (main) main.outcome = CONTINUE.you;
    const answers = outgoing(next, id).map((edge) => edge.outcome);
    changed.performer.choices = answers.length ? [CONTINUE.you, ...answers.filter((a) => a !== CONTINUE.you)] : [CONTINUE.you];
  }
  next.template.nodes[id] = changed;
  // Only a task reports outputs.
  if (changed.type !== 'task') delete changed.captures;
  // A retry runs only on agent and command tasks, and only an agent can be
  // handed its own failure back: what the new kind cannot run goes.
  if (changed.type !== 'task') delete changed.retry;
  else if (kind === 'command' && changed.retry?.onFail && changed.retry.onFail !== 'fresh-attempt') delete changed.retry.onFail;
  let orphans = [];
  if (node.type === 'decision' && kind !== 'you') {
    // A task continues one way: only the main answer's path stays, and the
    // steps only the other answers led to go.
    const before = reachable(next);
    changed.type = 'task';
    // Only a decision bounds a loop; its back answers go with the others.
    delete changed.loops;
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
  syncLoops(next);
  // A loop just made on an agent decision gets its safe way out.
  defaultAgentExhaustion(next, id);
  return { view: next, id };
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

// canRemoveOutcome says why an outcome cannot go, or '': a decision keeps at
// least one outcome, and one that does not loop back, so a used-up loop has
// a way on.
export function canRemoveOutcome(view, id, outcome) {
  const out = outgoing(view, id);
  if (out.length < 2) return 'a decision keeps at least one outcome';
  if (view.template.nodes[id]?.deciders?.length && out.length < 3) return 'a gate needs an outcome besides the one all must choose';
  const node = view.template.nodes[id];
  // A gate's used-up loop must go somewhere: change that first.
  const exhausting = node?.deciders?.length && Object.entries(node.loops || {}).find(([, bound]) => bound.onExhausted === outcome);
  if (exhausting) return `it is where ${exhausting[0]} goes once it runs out of rounds; choose another there first`;
  if (!loopBound(node, outcome) && !out.some((edge) => edge.outcome !== outcome && !loopBound(node, edge.outcome))) {
    return 'its only outcome that does not loop back';
  }
  return '';
}

// removeOutcome drops one outcome of a decision (never its last, nor its
// last way out of a loop).
export function removeOutcome(view, id, outcome) {
  const why = canRemoveOutcome(view, id, outcome);
  if (why) throw new Error(why);
  const next = copyView(view);
  const decision = next.template.nodes[id];
  next.edges = next.edges.filter((edge) => !(edge.from === id && edge.outcome === outcome));
  if (decision.performer?.kind === 'human') decision.performer.choices = (decision.performer.choices || []).filter((c) => c !== outcome);
  if (decision.agree === outcome) {
    decision.agree = agreeChoice(next, id, decision);
    rehomeFromAgree(next, id, decision);
  }
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
  for (const [looped, other] of Object.entries(decision.loops || {})) {
    if (other.onExhausted === outcome) decision.loops[looped] = { ...other, onExhausted: value };
  }
  if (decision.performer?.kind === 'human') decision.performer.choices = (decision.performer.choices || []).map((c) => (c === outcome ? value : c));
  if (decision.agree === outcome) decision.agree = value;
  return { view: syncLoops(next), id };
}

// PARAM_REF is a {{params.x}} reference, as the server reads one.
const PARAM_REF = /\{\{\s*params\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;

// declareParams declares every parameter the steps reference and the process
// does not declare yet, as a required string: typing {{allow_amend}} in a
// step's text is how a param is added inline, and its type and default are
// then set in Params (setParams). Every inline edit lands through it.
export function declareParams(view) {
  const found = [];
  for (const node of Object.values(view.template?.nodes || {})) {
    for (const performer of [node?.performer, ...(node?.deciders || [])]) {
      for (const text of [performer?.prompt, performer?.ask, performer?.run, ...(performer?.args || [])]) {
        if (typeof text !== 'string') continue;
        for (const match of text.matchAll(PARAM_REF)) if (!found.includes(match[1])) found.push(match[1]);
      }
    }
  }
  const missing = found.filter((name) => !view.template.params?.[name]);
  if (!missing.length) return view;
  const next = copyView(view);
  next.template.params = { ...(next.template.params || {}) };
  for (const name of missing) next.template.params[name] = { type: 'string', required: true };
  return next;
}

// setParams replaces the process's parameters (the Params dialog's Apply).
export function setParams(view, params) {
  const next = copyView(view);
  next.template.params = clone(params || {});
  if (!Object.keys(next.template.params).length) delete next.template.params;
  return { view: next };
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

// loopBodies are the bounded loops of a view, each as the nodes its rounds
// run again: those on a path from the loop's target to its decision.
function loopBodies(view) {
  const { nodes } = view.template;
  const plain = view.edges.filter((edge) => edge.from && !loopBound(nodes[edge.from], edge.outcome));
  const walk = (start, step) => {
    const seen = new Set([start]);
    const stack = [start];
    while (stack.length) {
      for (const next of step(stack.pop())) if (!seen.has(next)) { seen.add(next); stack.push(next); }
    }
    return seen;
  };
  return view.edges.filter((edge) => edge.from && loopBound(nodes[edge.from], edge.outcome)).map((edge) => {
    const ahead = walk(edge.to, (id) => plain.filter((e) => e.from === id).map((e) => e.to));
    const behind = walk(edge.from, (id) => plain.filter((e) => e.to === id).map((e) => e.from));
    return new Set([...ahead].filter((id) => behind.has(id)));
  });
}

// targetsFor lists the nodes an outcome of `id` may lead to: every node but
// the start and the step itself. Going back to earlier work is a decision's
// bounded loop (back: true), offered while the decision keeps another way
// on. A task is offered only targets ahead of it, and inside a loop only
// targets in that loop, since only a decision leaves one. A target the
// outcome already has always stays.
export function targetsFor(view, id, outcome = '') {
  const node = view.template.nodes[id];
  const decision = node?.type === 'decision';
  const current = view.edges.find((edge) => edge.from === id && edge.outcome === outcome);
  const otherWayOn = outgoing(view, id).some((edge) => edge.outcome !== outcome && !loopBound(node, edge.outcome));
  const within = decision ? [] : loopBodies(view).filter((body) => body.has(id));
  return Object.entries(view.template.nodes)
    .filter(([nodeID, target]) => nodeID !== id && target.type !== 'start')
    .map(([nodeID, target]) => {
      const edges = current ? view.edges.map((edge) => (edge === current ? { ...edge, to: nodeID } : edge)) : view.edges;
      const back = !!current && edgeGoesBack(view.template.nodes, edges, id, outcome);
      return { id: nodeID, back, label: target.type === 'end' ? `end${target.name ? ` (${target.name})` : ''}` : target.name || nodeID };
    })
    .filter((target) => target.id === current?.to
      || (target.back ? decision && otherWayOn : within.every((body) => body.has(target.id))));
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
// noteOf marks a decision's label: its node draws no sub line. A gate's who
// ("2 deciders · all approve") is its caption, drawn over the diamond.
export function draftProjection(view, { whoOf = () => '', noteOf = () => '', name = '' } = {}) {
  const nodes = Object.entries(view.template?.nodes || {}).map(([id, node]) => {
    const kind = miniKind(node);
    const projected = { id, label: node.name || (node.type === 'end' ? 'end' : id), state: 'planned', sub: isStep(node) ? whoOf(id, node) : '' };
    if (node.type === 'task' && node.performer) projected.performer = { kind: node.performer.kind };
    else projected.kind = node.type === 'task' ? 'agent' : node.type;
    if (kind === 'advanced') projected.sub = 'full editor';
    if (node.type === 'decision' && node.deciders?.length) projected.caption = whoOf(id, node);
    // A step that retries if it fails wears ↻N (its "If it fails").
    const retries = node.type === 'task' && node.retry ? (Number(node.retry.maxAttempts) || 0) - 1 : 0;
    if (retries > 0) projected.retries = retries;
    const note = node.type === 'decision' ? noteOf(id, node) : '';
    if (note) projected.label = `${projected.label} ${note}`;
    return projected;
  });
  const edges = (view.edges || []).filter((edge) => edge.from !== '').map((edge) => {
    const projected = {
      from: edge.from, to: edge.to, label: PASS_LABELS.includes(edge.outcome) || view.template.nodes[edge.from]?.type === 'parallel' ? '' : edge.outcome,
    };
    // A bounded loop (node.loops[outcome]) draws as a back edge with its bound.
    const bound = view.template.nodes[edge.from]?.loops?.[edge.outcome];
    const max = Number(bound?.maxIterations) || 0;
    if (!max) return projected;
    const after = bound.onExhausted && view.edges.find((e) => e.from === edge.from && e.outcome === bound.onExhausted)?.to;
    const exhausted = after ? (isFailedEnd(view.template.nodes[after]) ? 'fail' : bound.onExhausted) : '';
    return { ...projected, label: projected.label || edge.outcome, back: true, maxIterations: max, ...(exhausted ? { exhausted } : {}) };
  });
  return { id: 'draft', name: name || view.template?.name || 'draft', status: 'planned', nodes, edges: mergeParallel(edges) };
}

// mergeParallel draws a decision's answers that go to the same step as one
// edge labelled with all of them ("yes / no"): drawn apart they would share
// one path, and their labels would print over each other.
function mergeParallel(edges) {
  const out = [];
  const seen = new Map();
  for (const edge of edges) {
    if (edge.back) { out.push(edge); continue; }
    const key = `${edge.from}\u0000${edge.to}`;
    const first = seen.get(key);
    if (!first) { seen.set(key, edge); out.push(edge); continue; }
    first.label = [first.label, edge.label].filter(Boolean).join(' / ');
  }
  return out;
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

// ---- agreement gates ----------------------------------------------------------
//
// A decision with deciders is an agreement gate: each decider votes one of its
// outcomes, and `agree` is the one they must all choose. A decision with one
// decider is a plain decision, so adding a second decider makes the gate and
// removing down to one makes a plain decision again.

export const MAX_DECIDERS = 8;
const APPROVING = /^(approved?|accept(ed)?|yes|ok|pass(ed)?|lgtm|agree[ds]?)$/i;

// deciderOf is the performer a new decider starts as: an agent (a role, a
// member, anyone, the lead) or a person, asked what the gate's first decider
// is asked.
function deciderOf(who, text) {
  if (who.mode === 'you') return { kind: 'human', ask: text || 'Do you agree?' };
  const decider = { kind: 'agent', prompt: text || 'Decide this step.' };
  if (who.mode === 'agent') decider.assignee = who.agent;
  else if (who.mode === 'role') decider.role = who.role;
  else if (who.mode === 'anyone') decider.anyone = true;
  else if (who.mode === 'lead') decider.lead = true;
  return decider;
}

// deciderWho reads a decider as a Who.
export function deciderWho(decider) {
  if (!decider || decider.kind === 'human') return { mode: 'you' };
  if (decider.assignee) return { mode: 'agent', agent: decider.assignee };
  if (decider.role) return { mode: 'role', role: decider.role };
  if (decider.anyone) return { mode: 'anyone' };
  if (decider.lead) return { mode: 'lead' };
  return { mode: 'nobody' };
}

function deciderText(decider) {
  return decider?.kind === 'human' ? decider.ask || decider.prompt || '' : decider?.prompt || '';
}

// gateDeciders is a decision's deciders: its gate's, or its one performer.
export function gateDeciders(node) {
  if (node?.deciders?.length) return node.deciders;
  return node?.performer ? [node.performer] : [];
}

// canAddDecider says why a step cannot take another decider, or ''.
export function canAddDecider(view, id) {
  const node = view.template.nodes[id];
  if (node?.type !== 'decision') return 'only a decision is decided by several';
  if (node.performer?.kind === 'program') return 'a command decides alone';
  if (gateDeciders(node).length >= MAX_DECIDERS) return `a gate has at most ${MAX_DECIDERS} deciders`;
  if (outcomesOf(view, id).length < 2) return 'a gate needs an outcome besides the one all must choose';
  return '';
}

// agreeChoice is the outcome a new gate asks all deciders to choose: an
// approving one if there is one, else its first that does not go back.
function agreeChoice(view, id, node) {
  const outcomes = outcomesOf(view, id).map((o) => o.outcome);
  const forward = outcomes.filter((o) => !node.loops?.[o]);
  return forward.find((o) => APPROVING.test(o)) || forward[0] || outcomes[0] || '';
}

// addDecider gives decision id another decider; a plain decision becomes a
// gate. A person's answer choices do not carry over: a gate's votes are its
// outcomes.
export function addDecider(view, id, who) {
  const why = canAddDecider(view, id);
  if (why) throw new Error(why);
  const next = copyView(view);
  const node = next.template.nodes[id];
  if (!node.deciders?.length) {
    const first = { ...node.performer };
    delete first.choices;
    delete first.choiceOutcomes;
    node.deciders = [first];
    delete node.performer;
    node.agree = agreeChoice(next, id, node);
    // Nobody is there to pick another outcome when a gate's rounds run out.
    for (const [outcome, bound] of Object.entries(node.loops || {})) {
      if (!bound.onExhausted) {
        node.loops[outcome] = { ...bound, onExhausted: safeExhaustion(next, id, node, outcomesOf(next, id).map((o) => o.outcome), outcome) };
      }
    }
  }
  node.deciders.push(deciderOf(who, deciderText(node.deciders[0])));
  return { view: next, id };
}

// removeDecider drops decider `index`; with one left the gate is a plain
// decision again, and a person deciding alone answers with the outcomes.
export function removeDecider(view, id, index) {
  const next = copyView(view);
  const node = next.template.nodes[id];
  if (!node.deciders?.[index]) return { view, id };
  node.deciders.splice(index, 1);
  if (node.deciders.length === 1) {
    node.performer = node.deciders[0];
    delete node.deciders;
    delete node.agree;
    if (node.performer.kind === 'human') node.performer.choices = outcomesOf(next, id).map((o) => o.outcome);
  }
  return { view: next, id };
}

// setDecider makes decider `index` who, keeping what it is asked.
export function setDecider(view, id, index, who) {
  const next = copyView(view);
  const node = next.template.nodes[id];
  const old = node.deciders?.[index];
  if (!old) return { view, id };
  const decider = deciderOf(who, deciderText(old));
  if (decider.kind === 'agent' && old.kind === 'agent') {
    for (const key of ['sameAs', 'differentFrom', 'fallback', 'model', 'effort', 'profile']) if (old[key] !== undefined && who.mode !== 'agent') decider[key] = old[key];
  }
  node.deciders[index] = decider;
  return { view: next, id };
}

// setGateText asks every decider the same: the inline inspector's one text.
export function setGateText(view, id, text) {
  const next = copyView(view);
  for (const decider of next.template.nodes[id].deciders || []) {
    if (decider.kind === 'human') decider.ask = text;
    else decider.prompt = text;
  }
  return { view: next, id };
}

// setAgree sets the outcome all deciders must choose.
export function setAgree(view, id, outcome) {
  const next = copyView(view);
  const node = next.template.nodes[id];
  node.agree = outcome;
  rehomeFromAgree(next, id, node);
  return { view: next, id };
}

// rehomeFromAgree moves a used-up loop off a gate's agree outcome after agree
// changed: it would pass work the deciders kept sending back.
function rehomeFromAgree(view, id, node) {
  const outcomes = outgoing(view, id).map((edge) => edge.outcome);
  for (const [looped, bound] of Object.entries(node.loops || {})) {
    if (bound.onExhausted === node.agree) setLoopBound(node, looped, { ...bound, onExhausted: safeExhaustion(view, id, node, outcomes, looped) });
  }
}
