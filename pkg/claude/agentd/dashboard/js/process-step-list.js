// process-step-list.js -- the quick-start step list (mock Q2, tcl-7p6bgt):
// a process template edit view ({template, edges, layout}) shown as numbered
// steps, and the step list folded back into an edit view. Pure: no DOM, no
// fetch, so jstest/process-step-list.test.mjs exercises the shipped file.
//
// The list is a lossless lens, not a second model. Each step keeps its whole
// template node, and the list edits only a node's name, type and performer
// fields. Edges are explicit: a step's "continue" is its pass edge to the step
// below it (or to the end after the last step), and every other edge is a
// "then" link to a named step or end. Converting a view to a list and back
// yields the same nodes, edges and start. A graph the list cannot show
// (parallel, wait or subprocess nodes, or a start node with several edges)
// converts to { ok: false, reason } so the caller opens the full editor.
// An agreement gate (a decision with deciders) is a step like any other: its
// deciders, its agree rule and its outcomes stay on the node
// (tk_bd629fb5; process-step-gate.js edits them).

import { LOOP_DEFAULT_ROUNDS, exhaustionChoices, loopBound, setLoopBound } from './process-loops.js';
import { keepLifetime } from './process-spawn-lifetime.js';

export const STEP_LIST_START_OUTCOME = 'start';
export const STEP_CONTINUE_OUTCOME = 'next';
// YOU_CONTINUE is a new "you" step's single answer.
export const YOU_CONTINUE = 'continue';
// PASS_LABELS mirrors model.PassOutcomeLabels: the pass edge a task follows,
// in resolution order.
const PASS_LABELS = ['pass', 'done', 'success', 'next'];
const STEP_TYPES = new Set(['task', 'decision']);
const LIST_TYPES = new Set(['start', 'end', 'task', 'decision']);
const ID_PATTERN = /^[a-z0-9][a-z0-9._-]*$/;
const PARAM_REF = /\{\{\s*params\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;
// A bare {{name}} is the step list's shorthand for {{params.name}}.
const PARAM_SHORTHAND = /\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;

function clone(value) {
  return value === undefined ? undefined : JSON.parse(JSON.stringify(value));
}

function withoutNext(node) {
  const copy = clone(node) || {};
  delete copy.next;
  return copy;
}

function passLabel(next) {
  return PASS_LABELS.find((label) => next[label] != null && next[label] !== '') || '';
}

// edgesByNode groups the view's edges by source node: { from: { outcome: to } }.
// The view's edges are the truth; node.next is ignored, as on the server.
function edgesByNode(view) {
  const out = new Map();
  for (const edge of view?.edges || []) {
    if (!edge || edge.from === '' || edge.from == null) continue;
    if (!out.has(edge.from)) out.set(edge.from, {});
    out.get(edge.from)[edge.outcome] = edge.to;
  }
  return out;
}

function startTarget(view) {
  const edge = (view?.edges || []).find((e) => e && (e.from === '' || e.from == null) && e.outcome === STEP_LIST_START_OUTCOME);
  return edge ? edge.to : (view?.template?.start || '');
}

// viewToStepList converts an edit view into a step list, or explains why it
// cannot.
export function viewToStepList(view) {
  const template = view?.template;
  if (!template || typeof template !== 'object') return { ok: false, reason: 'no template' };
  const nodes = template.nodes || {};
  for (const [id, node] of Object.entries(nodes)) {
    const type = node?.type || '';
    if (!LIST_TYPES.has(type)) return { ok: false, reason: `step ${id} is a ${type || 'typeless'} node, which only the graph editor shows` };
  }
  const next = edgesByNode(view);
  const entry = startTarget(view);
  let startNode = null;
  let first = entry;
  if (entry && nodes[entry]?.type === 'start') {
    const out = next.get(entry) || {};
    const labels = Object.keys(out);
    if (labels.length > 1) return { ok: false, reason: 'the start node branches' };
    startNode = { id: entry, node: withoutNext(nodes[entry]), outcome: labels[0] || STEP_CONTINUE_OUTCOME };
    first = labels.length ? out[labels[0]] : '';
  }
  const starts = Object.keys(nodes).filter((id) => nodes[id].type === 'start');
  if (starts.length > (startNode ? 1 : 0)) return { ok: false, reason: 'a start node is not the process start' };

  // Order: a depth-first walk from the first step that follows each task's
  // pass edge first, then the other edges by outcome; unreached steps last.
  const isStep = (id) => STEP_TYPES.has(nodes[id]?.type);
  const order = [];
  const seen = new Set();
  const visit = (start) => {
    const stack = [start];
    while (stack.length) {
      const id = stack.pop();
      if (!isStep(id) || seen.has(id)) continue;
      seen.add(id);
      order.push(id);
      const out = next.get(id) || {};
      const pass = nodes[id].type === 'task' ? passLabel(out) : '';
      const targets = [
        ...(pass ? [out[pass]] : []),
        ...Object.keys(out).filter((label) => label !== pass).sort().map((label) => out[label]),
      ];
      for (let i = targets.length - 1; i >= 0; i -= 1) stack.push(targets[i]);
    }
  };
  visit(first);
  for (const id of Object.keys(nodes).sort()) visit(id);

  const endIDs = Object.keys(nodes).filter((id) => nodes[id].type === 'end').sort();
  const lastOut = order.length ? next.get(order.at(-1)) || {} : {};
  const lastPass = order.length && nodes[order.at(-1)].type === 'task' ? lastOut[passLabel(lastOut)] : '';
  let mainEnd = '';
  if (!order.length && nodes[first]?.type === 'end') mainEnd = first;
  else if (lastPass && nodes[lastPass]?.type === 'end') mainEnd = lastPass;
  else mainEnd = endIDs.find((id) => !nodes[id].result || /^(success|succeeded|complete|completed|done|pass|passed|ok)$/i.test(nodes[id].result)) || '';

  const steps = order.map((id, index) => {
    const out = next.get(id) || {};
    const following = order[index + 1] || mainEnd;
    const pass = nodes[id].type === 'task' ? passLabel(out) : '';
    const cont = pass && following && out[pass] === following ? pass : '';
    // A decision's answers keep their authored order; other outcomes sort.
    const choices = nodes[id].performer?.choices || [];
    const rank = (label) => (choices.includes(label) ? choices.indexOf(label) : choices.length);
    const links = Object.keys(out).filter((label) => label !== cont)
      .sort((a, b) => rank(a) - rank(b) || a.localeCompare(b, 'en'))
      .map((outcome) => ({ outcome, to: out[outcome] }));
    return { id, node: withoutNext(nodes[id]), cont, links };
  });
  const ends = endIDs.map((id) => ({ id, node: withoutNext(nodes[id]) }));
  const rest = clone(template);
  delete rest.nodes;
  delete rest.start;
  return {
    ok: true,
    list: {
      template: rest, layout: clone(view.layout) || { nodes: {} },
      startNode, steps, ends, mainEnd,
      // An empty list still needs somewhere to start: its main end.
      emptyStart: !steps.length && !startNode ? first : '',
    },
  };
}

function uniqueID(base, taken) {
  let stem = String(base || 'step').toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[^a-z0-9]+/, '').replace(/-+$/, '');
  if (!stem || !ID_PATTERN.test(stem)) stem = 'step';
  stem = stem.slice(0, 48);
  let id = stem;
  for (let i = 2; taken.has(id); i += 1) id = `${stem}-${i}`;
  taken.add(id);
  return id;
}

// listNodeIDs is every node id the list holds.
export function listNodeIDs(list) {
  return new Set([
    ...(list.startNode ? [list.startNode.id] : []),
    ...list.steps.map((step) => step.id),
    ...list.ends.map((end) => end.id),
  ]);
}

// ensureMainEnd returns a list that has a main end, adding an `end` node when
// it has none.
export function ensureMainEnd(list) {
  if (list.mainEnd && list.ends.some((end) => end.id === list.mainEnd)) return list;
  const id = uniqueID('end', listNodeIDs(list));
  return { ...list, ends: [...list.ends, { id, node: { type: 'end' } }], mainEnd: id };
}

// stepListToView folds a step list back into an edit view. Params referenced
// in the steps but not declared are declared as required strings.
export function stepListToView(list) {
  // Only the last step's continue (or an empty list) runs to the main end.
  const working = !list.steps.length || list.steps[list.steps.length - 1].cont ? ensureMainEnd(list) : list;
  const { startNode, steps, ends, mainEnd } = working;
  const nodes = {};
  const edges = [];
  const first = steps[0]?.id || list.emptyStart || mainEnd;
  if (startNode) {
    nodes[startNode.id] = clone(startNode.node);
    edges.push({ from: '', outcome: STEP_LIST_START_OUTCOME, to: startNode.id });
    if (first) edges.push({ from: startNode.id, outcome: startNode.outcome || STEP_CONTINUE_OUTCOME, to: first });
  } else if (first) {
    edges.push({ from: '', outcome: STEP_LIST_START_OUTCOME, to: first });
  }
  steps.forEach((step, index) => {
    nodes[step.id] = clone(step.node);
    const following = steps[index + 1]?.id || mainEnd;
    const linked = new Set(step.links.map((link) => link.outcome));
    if (step.cont && following && !linked.has(step.cont)) edges.push({ from: step.id, outcome: step.cont, to: following });
    for (const link of step.links) edges.push({ from: step.id, outcome: link.outcome, to: link.to });
  });
  for (const end of ends) nodes[end.id] = clone(end.node);
  const template = {
    ...clone(working.template),
    start: startNode ? startNode.id : first,
    nodes,
  };
  const referenced = stepListParams(working);
  const params = { ...(template.params || {}) };
  for (const name of referenced) if (!params[name]) params[name] = { type: 'string', required: true };
  if (template.params || Object.keys(params).length) template.params = params;
  const layout = clone(working.layout) || {};
  layout.nodes = Object.fromEntries(Object.entries(layout.nodes || {}).filter(([id]) => nodes[id]));
  if (layout.edges) {
    layout.edges = Object.fromEntries(Object.entries(layout.edges)
      .filter(([from]) => nodes[from])
      .map(([from, byOutcome]) => [from, Object.fromEntries(Object.entries(byOutcome || {})
        .filter(([outcome]) => edges.some((edge) => edge.from === from && edge.outcome === outcome)))]));
  }
  return { template, edges, layout };
}

function stepTexts(node) {
  return [node?.performer || {}, ...(node?.deciders || [])]
    .flatMap((performer) => [performer?.prompt, performer?.ask, performer?.run, performer?.subject, performer?.text, ...(performer?.args || [])])
    .filter((text) => typeof text === 'string');
}

// stepListParams is every {{params.x}} the steps reference, in order of first
// use.
export function stepListParams(list) {
  const found = [];
  for (const step of list.steps || []) {
    for (const text of stepTexts(step.node)) {
      for (const match of text.matchAll(PARAM_REF)) if (!found.includes(match[1])) found.push(match[1]);
    }
  }
  return found;
}

// collapseParamShorthand shows {{params.name}} as the step list's {{name}}.
export function collapseParamShorthand(text) {
  return String(text ?? '').replace(PARAM_REF, (_, name) => `{{${name}}}`);
}

// expandParamShorthand rewrites the step list's {{name}} shorthand to the
// template's {{params.name}}; anything already dotted is left alone.
export function expandParamShorthand(text) {
  return String(text ?? '').replace(PARAM_SHORTHAND, (_, name) => `{{params.${name}}}`);
}

// ---- who performs a step ----------------------------------------------------
//
// stepWho reads a step's "Who" from its performer:
//   { mode: 'agent', agent }       a specific agent (performer.assignee)
//   { mode: 'role', role }         any member with role (performer.role, or a
//                                  legacy performer.profile, which also names
//                                  the spawn profile)
//   { mode: 'anyone' }             anyone in the run's scope (performer.anyone)
//   { mode: 'lead' }               the group's lead (performer.lead)
//   { mode: 'you' }                the human (a human performer)
//   { mode: 'command', run }       a program performer (run: its executable)
//   { mode: 'spawn', profile }     a new agent for the step: names no one
//                                  (no sameAs either), and its own fallback
//                                  spawns (profile '' is the group's default
//                                  spawn profile); with no one to match, a
//                                  group run spawns one every time
//   { mode: 'message', to }        a message (to: its recipients, model.MessageTo*)
//   { mode: 'nobody' }             an agent performer that names no one
// A message's recipients (model.MessageTo*): you, the lead, the whole
// group, a role (role:<r>) or an agent (agent:<name>).
export const MESSAGE_TO_OPERATOR = 'operator';
export const MESSAGE_TO_LEAD = 'lead';
export const MESSAGE_TO_GROUP = 'group';
export const MESSAGE_MAX_RECIPIENTS = 8;

// setStepRecipients sets a message step's recipients: each at most once,
// at most MESSAGE_MAX_RECIPIENTS.
export function setStepRecipients(step, to) {
  const node = clone(step.node);
  const list = [...new Set((to || []).map((value) => String(value || '').trim()).filter(Boolean))].slice(0, MESSAGE_MAX_RECIPIENTS);
  node.performer = { ...(node.performer || { kind: 'message' }) };
  if (list.length) node.performer.to = list;
  else delete node.performer.to;
  return { ...step, node };
}

// messageRecipientOptions are who a message can go to, for a roster: you,
// the lead, the whole group, each role the group holds (`roles`, else the
// roster's), and each member.
export function messageRecipientOptions(roster = [], roles = null) {
  const held = roles?.length ? roles.map((r) => r.role) : [...new Set(roster.map((m) => m.role).filter(Boolean))].sort();
  return [
    { value: MESSAGE_TO_OPERATOR, label: 'you (Messages)' },
    { value: MESSAGE_TO_LEAD, label: "the group's lead" },
    { value: MESSAGE_TO_GROUP, label: 'the whole group' },
    ...held.map((role) => ({ value: `role:${role}`, label: `every ${role}` })),
    ...roster.filter((m) => m.name).map((m) => ({ value: `agent:${m.name}`, label: m.name })),
  ];
}

// setStepSubject sets a message step's optional one-line subject.
export function setStepSubject(step, subject) {
  const node = clone(step.node);
  const value = expandParamShorthand(String(subject ?? '').replace(/[\r\n]+/g, ' ').trim());
  node.performer = { ...(node.performer || { kind: 'message' }) };
  if (value) node.performer.subject = value;
  else delete node.performer.subject;
  return { ...step, node };
}

// messageRecipientWords says one recipient for a person: "you", "the lead",
// "the whole group", "reviewer", "rita".
export function messageRecipientWords(to) {
  if (to === MESSAGE_TO_OPERATOR) return 'you';
  if (to === MESSAGE_TO_LEAD) return 'the lead';
  if (to === MESSAGE_TO_GROUP) return 'the whole group';
  const role = String(to || '').match(/^role:(.+)$/);
  if (role) return role[1];
  const agent = String(to || '').match(/^agent:(.+)$/);
  return agent ? agent[1] : String(to || '');
}

export function stepWho(step) {
  const performer = step?.node?.performer;
  if (!performer) return { mode: 'nobody' };
  if (performer.kind === 'human') return { mode: 'you', assignee: performer.assignee || '' };
  if (performer.kind === 'program') return { mode: 'command', run: performer.run || '' };
  if (performer.kind === 'message') return { mode: 'message', to: [...(performer.to || [])] };
  if (performer.kind === 'rule') return { mode: 'rule' };
  if (performer.assignee) return { mode: 'agent', agent: performer.assignee };
  if (performer.role) return { mode: 'role', role: performer.role };
  if (performer.anyone) return { mode: 'anyone' };
  if (performer.lead) return { mode: 'lead' };
  if (performer.profile) return { mode: 'role', role: performer.profile, legacy: true };
  // With sameAs the step goes to that step's performer once it has run, so
  // it is not a new agent: it stays "nobody" with its fallback.
  if (performer.fallback?.action === 'spawn' && !performer.sameAs) return { mode: 'spawn', profile: performer.fallback.profile || '' };
  return { mode: 'nobody' };
}

// splitCommand splits a command line into argv the way a shell would for
// plain words: whitespace separates, and '…' or "…" quote. Programs run with
// no shell, so the first word is the executable.
export function splitCommand(line) {
  const argv = [];
  let word = '';
  let quote = '';
  let started = false;
  for (const ch of String(line ?? '')) {
    if (quote) {
      if (ch === quote) quote = '';
      else word += ch;
    } else if (ch === '"' || ch === "'") {
      quote = ch;
      started = true;
    } else if (/\s/.test(ch)) {
      if (started || word) argv.push(word);
      word = '';
      started = false;
    } else {
      word += ch;
    }
  }
  if (started || word) argv.push(word);
  return argv;
}

// joinCommand is splitCommand's inverse for display.
export function joinCommand(argv) {
  return (argv || []).map((arg) => (arg === '' || /[\s'"]/.test(arg)
    ? (arg.includes("'") ? `"${arg}"` : `'${arg}'`) : arg)).join(' ');
}

function programArgv(performer, line) {
  const [run = '', ...args] = splitCommand(line);
  performer.run = run;
  if (args.length) performer.args = args;
  else delete performer.args;
}

// stepText is the step's instruction: the agent prompt, the human ask, or the
// command line.
export function stepText(step) {
  if (isGate(step)) return deciderText(step.node.deciders[0]);
  const performer = step?.node?.performer || {};
  if (performer.kind === 'human') return performer.ask || performer.prompt || '';
  if (performer.kind === 'program') return performer.run ? joinCommand([performer.run, ...(performer.args || [])]) : '';
  if (performer.kind === 'message') return performer.text || '';
  return performer.prompt || '';
}

const AGENT_FIELDS = ['assignee', 'role', 'anyone', 'lead', 'sameAs', 'differentFrom', 'profile', 'spawn', 'prompt', 'model', 'effort', 'fallback'];

// setStepWho returns the step with its performer switched to who. Fields the
// new kind cannot carry are dropped; the step's text moves to the new kind's
// text field. A role is performer.role; `legacyRoles` writes it the old way,
// as performer.profile, for a daemon without performer selectors.
export function setStepWho(step, who, { legacyRoles = false } = {}) {
  const node = clone(step.node);
  const old = node.performer || {};
  const text = stepText(step);
  const shared = Object.fromEntries(['timeout', 'contact'].filter((key) => old[key] !== undefined).map((key) => [key, old[key]]));
  let performer;
  if (who.mode === 'you') {
    performer = { kind: 'human', ...shared, ask: text };
    if (old.kind === 'human') {
      for (const key of ['choices', 'choiceOutcomes', 'assignee', 'prompt']) if (old[key] !== undefined) performer[key] = old[key];
    }
    if (!performer.ask) delete performer.ask;
  } else if (who.mode === 'message') {
    // A message has no profile, timeout or contact: nobody performs it.
    // A step becoming a message tells you until the operator picks who else.
    performer = { kind: 'message', to: [...(who.to || (old.kind === 'message' ? old.to || [] : [MESSAGE_TO_OPERATOR]))] };
    if (old.kind === 'message' && old.subject) performer.subject = old.subject;
    if (text) performer.text = text;
    if (!performer.to.length) delete performer.to;
    // A message reports no outputs.
    delete node.captures;
  } else if (who.mode === 'command') {
    performer = { kind: 'program', ...shared, run: old.kind === 'program' ? old.run || '' : '' };
    if (old.kind === 'program' && old.args) performer.args = old.args;
    if (who.run !== undefined) programArgv(performer, who.run);
    if (old.kind === 'program' && old.profile) performer.profile = old.profile;
  } else {
    performer = { kind: 'agent', ...shared };
    if (old.kind === 'agent') {
      for (const key of AGENT_FIELDS) if (old[key] !== undefined) performer[key] = old[key];
    } else if (text) {
      performer.prompt = text;
    }
    delete performer.assignee;
    delete performer.role;
    delete performer.anyone;
    delete performer.lead;
    if (who.mode === 'agent' || who.mode === 'spawn') {
      // A named agent leaves no choice for a constraint to narrow, and a new
      // one is always a different agent.
      delete performer.sameAs;
      delete performer.differentFrom;
    }
    if (who.mode === 'lead') performer.lead = true;
    if (who.mode === 'agent') performer.assignee = who.agent;
    else if (who.mode === 'role') {
      if (legacyRoles) performer.profile = who.role;
      else performer.role = who.role;
    } else if (who.mode === 'anyone') performer.anyone = true;
    const legacyRole = old.kind === 'agent' && old.profile && !old.assignee && !old.role && !old.anyone;
    if (legacyRole && (who.mode !== 'role' || !legacyRoles)) {
      // A legacy profile named the role; it does not survive a switch away.
      // Its spawn opt-in spawned that profile, which a step fallback keeps.
      if (old.spawn === 'allowed' && !old.fallback) performer.fallback = { action: 'spawn', profile: old.profile };
      delete performer.profile;
      delete performer.spawn;
    }
    if (who.mode === 'spawn') {
      // Who is the fallback: nobody matches no selector, so it always spawns.
      delete performer.profile;
      delete performer.spawn;
      performer.fallback = keepLifetime(old.fallback, who.profile ? { action: 'spawn', profile: who.profile } : { action: 'spawn' });
    }
  }
  node.performer = performer;
  return { ...step, node };
}

// ---- which agent: same as, or different from, another step ------------------
//
// stepConstraint reads the agent constraint of a step:
//   { mode: 'none' }
//   { mode: 'same', step }        performer.sameAs
//   { mode: 'different', steps }  performer.differentFrom
export function stepConstraint(step) {
  const performer = step?.node?.performer || {};
  if (performer.kind !== 'agent') return { mode: 'none' };
  if (performer.sameAs) return { mode: 'same', step: performer.sameAs };
  if (performer.differentFrom?.length) return { mode: 'different', steps: [...performer.differentFrom] };
  return { mode: 'none' };
}

// setStepConstraint returns the step with its agent constraint replaced.
export function setStepConstraint(step, constraint) {
  const node = clone(step.node);
  const performer = node.performer;
  if (!performer || performer.kind !== 'agent') return step;
  delete performer.sameAs;
  delete performer.differentFrom;
  if (constraint.mode === 'same' && constraint.step) performer.sameAs = constraint.step;
  if (constraint.mode === 'different' && constraint.steps?.length) performer.differentFrom = [...new Set(constraint.steps)];
  return { ...step, node };
}

// constraintValue / constraintFromValue map a constraint to a one-pick
// select's value and back: '', 'same:<step>', 'different:<step>[,<step>…]'.
export function constraintValue(constraint) {
  if (constraint.mode === 'same') return `same:${constraint.step}`;
  if (constraint.mode === 'different') return `different:${constraint.steps.join(',')}`;
  return '';
}

export function constraintFromValue(value) {
  if (value.startsWith('same:')) return { mode: 'same', step: value.slice(5) };
  if (value.startsWith('different:')) return { mode: 'different', steps: value.slice(10).split(',').filter(Boolean) };
  return { mode: 'none' };
}

// agentStepIDs lists the steps of a template an agent performs, other than
// `except`: the steps a constraint can name.
export function agentStepIDs(template, except = '') {
  return Object.entries(template?.nodes || {})
    .filter(([id, node]) => id !== except && (node.type === 'task' || node.type === 'decision')
      && (node.performer?.kind === 'agent' || (node.deciders || []).some((d) => d?.kind === 'agent')))
    .map(([id]) => id)
    .sort();
}

// ---- outputs ----------------------------------------------------------------

const OUTPUT_NAME = /^[a-z0-9][a-z0-9._-]*$/;

// stepOutputs is a step's declared outputs (node.captures).
export function stepOutputs(step) {
  return [...(step?.node?.captures || [])];
}

// setStepOutputs returns the step declaring the outputs named in text
// (separated by commas or spaces), lower-cased; names that are not id-shaped
// are dropped.
export function setStepOutputs(step, text) {
  const node = clone(step.node);
  const names = [...new Set(String(text ?? '').toLowerCase().split(/[\s,]+/).filter((name) => OUTPUT_NAME.test(name)))];
  if (names.length) node.captures = names;
  else delete node.captures;
  return { ...step, node };
}

// outputRef is how a prompt reads an output.
export function outputRef(name) {
  return `{{ outputs.${name} }}`;
}

// setStepText writes the step's instruction into its performer's text field.
// A command's text is its whole command line (run, no args).
export function setStepText(step, text) {
  if (isGate(step)) return step.node.deciders.reduce((next, _, index) => setDeciderText(next, index, text), step);
  const node = clone(step.node);
  const performer = node.performer || { kind: 'agent' };
  const value = expandParamShorthand(text);
  if (performer.kind === 'human') performer.ask = value;
  else if (performer.kind === 'program') programArgv(performer, value);
  else if (performer.kind === 'message') performer.text = value;
  else performer.prompt = value;
  node.performer = performer;
  return { ...step, node };
}

// ---- agreement gates ----------------------------------------------------------
//
// A gate's deciders are each asked their own text. The list shows one text
// while they are all asked the same, and each decider's otherwise.

function isGate(step) {
  return step?.node?.type === 'decision' && (step.node.deciders?.length || 0) > 0;
}

function deciderText(decider) {
  return decider?.kind === 'human' ? decider.ask || decider.prompt || '' : decider?.prompt || '';
}

// deciderTexts is each decider's text, in order ([] for a step that is no
// gate).
export function deciderTexts(step) {
  return isGate(step) ? step.node.deciders.map(deciderText) : [];
}

// setDeciderText writes one decider's text: a person's ask, an agent's
// prompt.
export function setDeciderText(step, index, text) {
  if (!isGate(step) || !step.node.deciders[index]) return step;
  const node = clone(step.node);
  const decider = node.deciders[index];
  const value = expandParamShorthand(text);
  if (decider.kind === 'human') decider.ask = value;
  else decider.prompt = value;
  return { ...step, node };
}

const APPROVING = /^(approved?|accept(ed)?|yes|ok|pass(ed)?|lgtm|agree[ds]?)$/i;

// gateAgreeChoice is the outcome a gate's deciders must agree on when its
// own has gone: an approving one, else the first that does not go back.
function gateAgreeChoice(node, answers) {
  const forward = answers.filter((answer) => !loopBound(node, answer));
  return forward.find((answer) => APPROVING.test(answer)) || forward[0] || answers[0] || '';
}

// setStepName renames a step (its node name; the id is kept).
export function setStepName(step, name) {
  return { ...step, node: { ...clone(step.node), name } };
}

// stepAnswers are a decision step's answers: a person's choices, or the
// verdicts an agent decider picks between (its outcomes, which is all an
// agent decision names), continuing one first.
export function stepAnswers(step) {
  if (step?.node?.type !== 'decision') return [];
  const performer = step.node.performer || {};
  if (performer.kind === 'human') return performer.choices || [];
  return [...new Set([...(step.cont ? [step.cont] : []), ...step.links.map((link) => link.outcome)])];
}

// AGENT_VERDICTS are a new agent decider's verdicts: approve continues,
// changes is the one to send back for another round.
export const AGENT_VERDICTS = Object.freeze(['approve', 'changes']);

// setStepChoices makes a "you" or agent step a decision with these answers,
// or a plain task when answers is empty. Decision answers are its edges: the
// continuing answer (the step's continue, when it is still an answer) goes to
// the next step, links for removed answers go away, and new answers link to
// `following`. A person's answers are also its performer's choices; an agent
// decider's are only its edges (choices are human-scoped).
export function setStepChoices(step, choices, following) {
  const answers = [...new Set((choices || []).map((c) => String(c).trim()).filter(Boolean))];
  // A gate keeps its deciders. It needs an outcome besides the one all must
  // choose, so it is never left with fewer than two.
  if (isGate(step) && answers.length < 2) return step;
  const node = clone(step.node);
  const performer = { ...(node.performer || { kind: 'human' }) };
  if (answers.length) {
    node.type = 'decision';
    if (performer.kind === 'human') performer.choices = answers;
    else delete performer.choices;
    delete performer.choiceOutcomes;
    const cont = answers.includes(step.cont) ? step.cont : '';
    const kept = step.links.filter((link) => answers.includes(link.outcome) && link.outcome !== cont);
    const added = answers.filter((answer) => answer !== cont && !kept.some((link) => link.outcome === answer))
      .map((outcome) => ({ outcome, to: following }));
    if (!isGate(step)) node.performer = performer;
    // A removed answer takes its loop bound with it, and no bound follows it.
    for (const outcome of Object.keys(node.loops || {})) {
      if (!answers.includes(outcome)) setLoopBound(node, outcome, null);
    }
    for (const [outcome, bound] of Object.entries(node.loops || {})) {
      if (bound.onExhausted && !answers.includes(bound.onExhausted)) {
        setLoopBound(node, outcome, { maxIterations: bound.maxIterations, onExhausted: '' });
      }
    }
    // A gate's agree and its loops' ways on follow its outcomes.
    return syncGateStep({ ...step, node, cont, links: [...kept, ...added].filter((link) => link.to) });
  }
  node.type = 'task';
  delete node.loops;
  delete performer.choices;
  delete performer.choiceOutcomes;
  node.performer = performer;
  // A decision's links are all answers; the task it becomes keeps only its
  // continue.
  const links = step.node.type === 'decision' ? [] : step.links.filter((link) => !step.node.performer?.choices?.includes(link.outcome));
  return { ...step, node, cont: STEP_CONTINUE_OUTCOME, links };
}

// setStepLinks replaces a step's "then" links. A link reusing the continue
// outcome takes it over.
export function setStepLinks(step, links) {
  const clean = [];
  for (const link of links || []) {
    const outcome = String(link?.outcome || '').trim();
    if (!outcome || !link.to || clean.some((l) => l.outcome === outcome)) continue;
    clean.push({ outcome, to: link.to });
  }
  const cont = clean.some((l) => l.outcome === step.cont) ? '' : step.cont;
  return { ...step, cont, links: clean };
}

// newStep builds a step of the given kind with a fresh id.
export function newStep(list, { kind = 'agent', name = '', text = '', who = null } = {}) {
  const fallbackName = { agent: 'Step', you: 'Approve', command: 'Command', message: 'Tell' }[kind] || 'Step';
  const id = uniqueID(name || fallbackName, listNodeIDs(list));
  const base = { id, node: { type: 'task', name: name || fallbackName, performer: { kind: 'agent' } }, cont: STEP_CONTINUE_OUTCOME, links: [] };
  const mode = who || (kind === 'you' ? { mode: 'you' } : kind === 'command' ? { mode: 'command', run: text }
    : kind === 'message' ? { mode: 'message', to: [MESSAGE_TO_OPERATOR] } : { mode: 'nobody' });
  let step = setStepWho(base, mode);
  if (text && mode.mode !== 'command') step = setStepText(step, text);
  // A human performs only decisions today, so a "you" step is a decision
  // whose one answer continues.
  if (mode.mode === 'you') step = setStepChoices({ ...step, cont: YOU_CONTINUE }, [YOU_CONTINUE], '');
  return step;
}

// insertStep / removeStep / moveStep return a new list. Removing a step
// retargets links that pointed at it to whatever followed it.
export function insertStep(list, step, index = list.steps.length) {
  const steps = [...list.steps];
  steps.splice(index, 0, step);
  return { ...list, steps };
}

export function removeStep(list, id) {
  const index = list.steps.findIndex((step) => step.id === id);
  if (index < 0) return list;
  const remaining = list.steps.filter((step) => step.id !== id);
  // A link to the removed step goes to whatever followed it -- unless that is
  // the linking step itself or above it, then to the linking step's own next.
  // A bounded loop back to the removed step goes back to the step that
  // followed it instead, while that is still earlier than the decision.
  const retarget = (at, link, step) => {
    const after = remaining[index]?.id || list.mainEnd;
    if (index < at && loopBound(step.node, link.outcome)) return after;
    return index > at ? after : remaining[at + 1]?.id || list.mainEnd;
  };
  const steps = remaining.map((step, at) => ({
    ...step,
    links: step.links.map((link) => (link.to === id ? { ...link, to: retarget(at, link, step) } : link)).filter((link) => link.to),
  }));
  return syncStepLoops({ ...list, steps });
}

export function moveStep(list, id, delta) {
  const index = list.steps.findIndex((step) => step.id === id);
  const target = index + delta;
  if (index < 0 || target < 0 || target >= list.steps.length) return list;
  const steps = [...list.steps];
  const [moved] = steps.splice(index, 1);
  steps.splice(target, 0, moved);
  return syncStepLoops({ ...list, steps });
}

// replaceStep swaps the step with the same id.
export function replaceStep(list, step) {
  return { ...list, steps: list.steps.map((s) => (s.id === step.id ? step : s)) };
}

// blankStepList is an empty process: a start node and an end.
export function blankStepList(name = '') {
  return {
    template: { apiVersion: 'tclaude.dev/v1alpha1', kind: 'ProcessTemplate', id: '', name, params: {} },
    layout: { nodes: {} },
    startNode: { id: 'start', node: { type: 'start' }, outcome: STEP_CONTINUE_OUTCOME },
    steps: [],
    ends: [{ id: 'end', node: { type: 'end' } }],
    mainEnd: 'end',
    emptyStart: '',
  };
}

// ---- bounded review loops (tcl-eu70x9) ---------------------------------------
//
// A decision step's answer may go back to an earlier step for another round,
// as long as it is bounded: node.loops[answer] = { maxIterations, onExhausted }.
// The list keeps that bound on the step's node, like every other node field.

// backTargets are the earlier steps a decision step's answer may go back to.
// A task never goes back, and no step goes back to itself.
export function backTargets(list, stepID) {
  const index = list.steps.findIndex((step) => step.id === stepID);
  if (index < 0 || list.steps[index].node.type !== 'decision') return [];
  return list.steps.slice(0, index).map((step) => step.id);
}

// isBackLink reports whether a step's link to a target goes back.
export function isBackLink(list, stepID, to) {
  return backTargets(list, stepID).includes(to);
}

// backTargetLabel names a back target in a "then" menu: "↺ back to 1 · name".
export function backTargetLabel(list, id) {
  return `\u21ba back to ${stepTargetLabel(list, id)}`;
}

// setStepLoop bounds (or with a null bound unbounds) one answer of a step.
export function setStepLoop(step, outcome, bound) {
  const node = clone(step.node);
  setLoopBound(node, outcome, bound);
  return syncGateStep({ ...step, node });
}

// syncGateStep keeps a gate valid after its outcomes or their loops change,
// as the graph inspector's syncGate does: what all must choose never goes
// back, and every loop has a way on once its rounds run out (an outcome that
// does not loop, another than agree if there is one), since nobody is there
// to pick one. A step that is no gate is returned as it is.
export function syncGateStep(step) {
  if (!isGate(step)) return step;
  const node = clone(step.node);
  const answers = stepAnswers({ ...step, node });
  if (!answers.includes(node.agree) || loopBound(node, node.agree)) node.agree = gateAgreeChoice(node, answers);
  for (const outcome of Object.keys(node.loops || {})) {
    const bound = loopBound(node, outcome);
    const ways = exhaustionChoices(node, answers, outcome);
    if (bound.onExhausted && ways.includes(bound.onExhausted)) continue;
    const way = ways.find((other) => other !== node.agree) || ways[0];
    if (way) setLoopBound(node, outcome, { ...bound, onExhausted: way });
  }
  return { ...step, node };
}

// setOutcomeLoop is setStepLoop addressed through the list.
export function setOutcomeLoop(list, stepID, outcome, bound) {
  const step = list.steps.find((s) => s.id === stepID);
  return step ? replaceStep(list, setStepLoop(step, outcome, bound)) : list;
}

// stepLoop is one answer's bound, or null.
export function stepLoop(step, outcome) {
  return loopBound(step.node, outcome);
}

// syncStepLoops keeps each decision's bounds in step with the order after a
// move or removal: a bound whose answer no longer goes back is dropped, and an
// answer that now goes back without one gets the default, so the list never
// holds an unbounded cycle with no rounds control to fix it.
export function syncStepLoops(list) {
  let changed = false;
  const steps = list.steps.map((step) => {
    let next = step;
    for (const outcome of Object.keys(step.node.loops || {})) {
      const to = step.links.find((link) => link.outcome === outcome)?.to;
      if (to && isBackLink(list, step.id, to)) continue;
      next = setStepLoop(next, outcome, null);
      changed = true;
    }
    for (const link of step.links) {
      if (!isBackLink(list, step.id, link.to) || loopBound(next.node, link.outcome)) continue;
      next = setStepLoop(next, link.outcome, { maxIterations: LOOP_DEFAULT_ROUNDS, onExhausted: '' });
      changed = true;
    }
    return next;
  });
  return changed ? { ...list, steps } : list;
}

// forwardTargets are the link targets a step may pick: the steps after it and
// the ends. Going back is a decision's bounded loop (backTargets); a backward
// link a template already has stays as it is.
export function forwardTargets(list, stepID) {
  const index = list.steps.findIndex((step) => step.id === stepID);
  return [...list.steps.slice(index + 1).map((step) => step.id), ...list.ends.map((end) => end.id)];
}

// stepTargetLabel names a link target for the "then" line: a step's number
// and name, or an end.
export function stepTargetLabel(list, id) {
  const index = list.steps.findIndex((step) => step.id === id);
  if (index >= 0) return `${index + 1} · ${list.steps[index].node.name || id}`;
  const end = list.ends.find((e) => e.id === id);
  if (end) {
    const result = String(end.node.result || '').toLowerCase();
    if (/^(fail|failed|failure|error)$/.test(result)) return 'stop (failed)';
    if (/^(cancel|canceled|cancelled)$/.test(result)) return 'stop (canceled)';
    return end.node.name ? `end · ${end.node.name}` : 'end';
  }
  return `${id} (missing)`;
}

// stepListFlow shapes the list as the editor's top-to-bottom preview: one row
// per start, step and end in list order, the edge to the row below as a
// labelled "down" arrow, and every other edge as a jump on a side lane.
// Jumps are packed into the fewest lanes whose row spans do not overlap, the
// shortest first, so short hops hug the steps.
export function stepListFlow(list, { whoOf = () => '' } = {}) {
  const view = stepListToView(list);
  const nodes = view.template.nodes;
  const ends = [...list.ends].sort((a, b) => (a.id === list.mainEnd ? -1 : b.id === list.mainEnd ? 1 : 0));
  const rows = [
    ...(list.startNode ? [{ id: list.startNode.id, kind: 'start', label: list.startNode.node.name || 'start' }] : []),
    ...list.steps.map((step, i) => ({ id: step.id, kind: 'step', number: i + 1, label: step.node.name || step.id, step, who: whoOf(step) })),
    ...ends.filter((end) => nodes[end.id]).map((end) => ({ id: end.id, kind: 'end', label: stepTargetLabel(list, end.id) })),
  ];
  const at = new Map(rows.map((row, i) => [row.id, i]));
  const down = rows.map(() => []);
  const jumps = [];
  for (const edge of view.edges) {
    if (!edge.from || !at.has(edge.from) || !at.has(edge.to)) continue;
    const from = at.get(edge.from);
    const to = at.get(edge.to);
    const label = PASS_LABELS.includes(edge.outcome) || edge.outcome === STEP_CONTINUE_OUTCOME ? '' : edge.outcome;
    const loop = loopBound(nodes[edge.from], edge.outcome);
    if (to === from + 1) down[from].push(label);
    else jumps.push({ from, to, label, back: to <= from, ...(loop ? { loop } : {}) });
  }
  jumps.sort((a, b) => Math.abs(a.to - a.from) - Math.abs(b.to - b.from) || a.from - b.from || a.to - b.to);
  const lanes = [];
  for (const jump of jumps) {
    const lo = Math.min(jump.from, jump.to);
    const hi = Math.max(jump.from, jump.to);
    let lane = lanes.findIndex((spans) => spans.every(([a, b]) => hi < a || lo > b));
    if (lane < 0) lane = lanes.push([]) - 1;
    lanes[lane].push([lo, hi]);
    jump.lane = lane;
  }
  return {
    rows: rows.map((row, i) => ({ ...row, down: down[i].length ? down[i].filter(Boolean).join(' / ') : null })),
    jumps, lanes: lanes.length,
  };
}

// FAILURE_RETRIES_MAX is the most retries the inline editor offers: the
// engine runs at most 100 attempts, the first included, back to back.
export const FAILURE_RETRIES_MAX = 99;

// stepFailure is what an agent or command step does if it fails, as the
// inline editor's "If it fails" row says it:
//   fail      no retry policy: the run fails (the engine's default).
//   hold      retry.maxAttempts 1: the step is held (blocked) for the
//             operator to retry, skip or cancel, without retrying itself.
//   retry     retry.maxAttempts N+1: N retries, then held the same way.
//   go-on     mayFail: the run goes on along the step's route with its
//             result failed, for a rule decision after it to branch on.
// Returns { mode, retries } (retries is 0 unless mode is retry).
export function stepFailure(step) {
  if (step?.node?.mayFail) return { mode: 'go-on', retries: 0 };
  const attempts = Number(step?.node?.retry?.maxAttempts) || 0;
  if (!step?.node?.retry) return { mode: 'fail', retries: 0 };
  if (attempts <= 1) return { mode: 'hold', retries: 0 };
  return { mode: 'retry', retries: attempts - 1 };
}

// setStepFailure returns the step with its "If it fails" set. The retry
// policy's onFail (from the full editor) is kept while it stays a retry
// policy; a backoff, which the engine does not run, is dropped. Retries are
// clamped to 1..FAILURE_RETRIES_MAX.
export function setStepFailure(step, { mode, retries = 1 } = {}) {
  const node = clone(step.node);
  if (mode === 'go-on') node.mayFail = true;
  else delete node.mayFail;
  if (mode === 'hold' || mode === 'retry') {
    const count = Math.min(FAILURE_RETRIES_MAX, Math.max(1, Math.round(Number(retries) || 1)));
    node.retry = { maxAttempts: mode === 'hold' ? 1 : count + 1, ...(node.retry?.onFail ? { onFail: node.retry.onFail } : {}) };
  } else {
    delete node.retry;
  }
  return { ...step, node };
}
