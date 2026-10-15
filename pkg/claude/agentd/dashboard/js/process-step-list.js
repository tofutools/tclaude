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
  const performer = node?.performer || {};
  return [performer.prompt, performer.ask, performer.run, ...(performer.args || [])].filter((text) => typeof text === 'string');
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
//   { mode: 'you' }                the human (a human performer)
//   { mode: 'command', run }       a program performer (run: its executable)
//   { mode: 'nobody' }             an agent performer that names no one
export function stepWho(step) {
  const performer = step?.node?.performer;
  if (!performer) return { mode: 'nobody' };
  if (performer.kind === 'human') return { mode: 'you', assignee: performer.assignee || '' };
  if (performer.kind === 'program') return { mode: 'command', run: performer.run || '' };
  if (performer.assignee) return { mode: 'agent', agent: performer.assignee };
  if (performer.role) return { mode: 'role', role: performer.role };
  if (performer.anyone) return { mode: 'anyone' };
  if (performer.profile) return { mode: 'role', role: performer.profile, legacy: true };
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
  const performer = step?.node?.performer || {};
  if (performer.kind === 'human') return performer.ask || performer.prompt || '';
  if (performer.kind === 'program') return performer.run ? joinCommand([performer.run, ...(performer.args || [])]) : '';
  return performer.prompt || '';
}

const AGENT_FIELDS = ['assignee', 'role', 'anyone', 'profile', 'spawn', 'prompt', 'model', 'effort', 'fallback'];

// setStepWho returns the step with its performer switched to who. Fields the
// new kind cannot carry are dropped; the step's text moves to the new kind's
// text field. `legacyRoles` writes a role as performer.profile (today's
// role + spawn-profile field) until the backend has performer.role.
export function setStepWho(step, who, { legacyRoles = true } = {}) {
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
    if (who.mode === 'agent') performer.assignee = who.agent;
    else if (who.mode === 'role') {
      if (legacyRoles) performer.profile = who.role;
      else performer.role = who.role;
    } else if (who.mode === 'anyone') performer.anyone = true;
    if (who.mode !== 'role' && legacyRoles && old.kind === 'agent' && old.profile && !old.assignee && !old.role && !old.anyone) {
      // A legacy profile named the role; it does not survive a switch away.
      delete performer.profile;
      delete performer.spawn;
    }
  }
  node.performer = performer;
  return { ...step, node };
}

// setStepText writes the step's instruction into its performer's text field.
// A command's text is its whole command line (run, no args).
export function setStepText(step, text) {
  const node = clone(step.node);
  const performer = node.performer || { kind: 'agent' };
  const value = expandParamShorthand(text);
  if (performer.kind === 'human') performer.ask = value;
  else if (performer.kind === 'program') programArgv(performer, value);
  else performer.prompt = value;
  node.performer = performer;
  return { ...step, node };
}

// setStepName renames a step (its node name; the id is kept).
export function setStepName(step, name) {
  return { ...step, node: { ...clone(step.node), name } };
}

// setStepChoices makes a "you" step a decision with these answers, or a plain
// task when answers is empty. Decision answers are its edges: the continuing
// answer (the step's continue, when it is still an answer) goes to the next
// step, links for removed answers go away, and new answers link to
// `following`.
export function setStepChoices(step, choices, following) {
  const answers = [...new Set((choices || []).map((c) => String(c).trim()).filter(Boolean))];
  const node = clone(step.node);
  const performer = { ...(node.performer || { kind: 'human' }) };
  if (answers.length) {
    node.type = 'decision';
    performer.choices = answers;
    delete performer.choiceOutcomes;
    const cont = answers.includes(step.cont) ? step.cont : '';
    const kept = step.links.filter((link) => answers.includes(link.outcome) && link.outcome !== cont);
    const added = answers.filter((answer) => answer !== cont && !kept.some((link) => link.outcome === answer))
      .map((outcome) => ({ outcome, to: following }));
    node.performer = performer;
    return { ...step, node, cont, links: [...kept, ...added].filter((link) => link.to) };
  }
  node.type = 'task';
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
  const fallbackName = { agent: 'Step', you: 'Approve', command: 'Command' }[kind] || 'Step';
  const id = uniqueID(name || fallbackName, listNodeIDs(list));
  const base = { id, node: { type: 'task', name: name || fallbackName, performer: { kind: 'agent' } }, cont: STEP_CONTINUE_OUTCOME, links: [] };
  const mode = who || (kind === 'you' ? { mode: 'you' } : kind === 'command' ? { mode: 'command', run: text } : { mode: 'nobody' });
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
  const retarget = (at) => {
    const after = remaining[index]?.id || list.mainEnd;
    return index > at ? after : remaining[at + 1]?.id || list.mainEnd;
  };
  const steps = remaining.map((step, at) => ({
    ...step,
    links: step.links.map((link) => (link.to === id ? { ...link, to: retarget(at) } : link)).filter((link) => link.to),
  }));
  return { ...list, steps };
}

export function moveStep(list, id, delta) {
  const index = list.steps.findIndex((step) => step.id === id);
  const target = index + delta;
  if (index < 0 || target < 0 || target >= list.steps.length) return list;
  const steps = [...list.steps];
  const [moved] = steps.splice(index, 1);
  steps.splice(target, 0, moved);
  return { ...list, steps };
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

// forwardTargets are the link targets a step may pick: the steps after it and
// the ends. Templates are acyclic, so a link never points back; a backward
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
    if (to === from + 1) down[from].push(label);
    else jumps.push({ from, to, label, back: to <= from });
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
