// The operator's two reference processes built in the FULL process editor
// (tk_c324d5a4, point 4), with the operations its canvas and node inspector
// perform (process-editor.js, process-node-inspector.js): a connector
// dragged to empty canvas adds a connected node, the inspector's fields
// commit one ProcessEditModel.updateNode each, connectors are relabelled,
// retargeted and bounded, and Params applies setParams. Each call is one
// operator edit. The result must be equivalent to the inline editor's
// (process-reference-recipes.mjs): the same steps, Who, text, answers,
// loops and params, ids aside (the full editor names nodes by type).
//
// The full editor cannot make a rule decision or a step that may fail (its
// performer kinds are human, agent and program, and it has no "may fail"
// field): "Merge" and "Amending allowed" are drawn with their answers, and
// the CI step as a command, and the comparison leaves those two out
// (INLINE_ONLY).

import { ProcessEditModel, blankEditView } from '../dashboard/js/process-edit-model.js';
import { addGateDecider, defaultPerformer, setPerformerField, setPerformerKind } from '../dashboard/js/process-node-form.js';
import { stepSlug } from '../dashboard/js/process-step-ids.js';

export class FullEditor {
  constructor(name) {
    this.model = new ProcessEditModel(blankEditView(name));
    this.ops = [];
  }

  op(label, fn) {
    this.ops.push(label);
    return fn();
  }

  // node drags a connector from `from` to empty canvas and picks a type,
  // then names it (the inspector's name field, focused): two edits.
  node(from, type, name) {
    const { id } = this.op(`+ ${type} from ${from}`, () => this.model.addConnectedNode(type, { connectFrom: from }));
    if (name) this.op(`name ${id}`, () => this.model.renameNode(id, name));
    return id;
  }

  // field commits one performer field of a step (or of decider `index`).
  field(id, key, value, index = -1) {
    this.op(`${key} ${id}${index >= 0 ? `.${index}` : ''}`, () => this.model.updateNode(id, (draft) => {
      const type = draft.type || 'task';
      const slot = index >= 0 ? draft.deciders[index]
        : (draft.performer ||= defaultPerformer(type === 'decision' ? 'human' : 'agent'));
      setPerformerField(slot, key, value, { choiceRouting: type !== 'decision' });
    }));
  }

  kind(id, kind, index = -1) {
    this.op(`kind ${id}`, () => this.model.updateNode(id, (draft) => {
      const slot = index >= 0 ? draft.deciders[index] : (draft.performer ||= defaultPerformer(kind));
      setPerformerKind(slot, kind);
    }));
  }

  // work is a task's "add work performer" button: an agent.
  work(id) {
    this.op(`work ${id}`, () => this.model.updateNode(id, (draft) => { draft.performer = defaultPerformer('agent'); }));
  }

  decider(id) {
    const outcomes = this.model.outgoingEdges(id).map((edge) => edge.outcome);
    this.op(`+ decider ${id}`, () => this.model.updateNode(id, (draft) => addGateDecider(draft, outcomes)));
  }

  agree(id, outcome) {
    this.op(`agree ${id}`, () => this.model.updateNode(id, (draft) => { draft.agree = outcome; }));
  }

  label(from, outcome, next) { this.op(`label ${from}.${outcome}`, () => this.model.setEdgeOutcome(from, outcome, next)); }

  // connect draws a connector between two nodes, which gets the next free
  // label, then types the one it needs over it: two edits.
  connect(from, outcome, to) {
    const free = this.model.freeOutcome(from, 'pass');
    this.op(`connect ${from} → ${to}`, () => this.model.addEdge(from, free, to));
    if (free !== outcome) this.label(from, free, outcome);
  }

  // loop bounds a connector that goes back (its rounds), then picks where a
  // used-up loop goes, when it names one: one inspector commit each.
  loop(from, outcome, bound) {
    this.op(`loop ${from}.${outcome}`, () => this.model.setEdgeLoop(from, outcome, { maxIterations: bound.maxIterations }));
    if (bound.onExhausted) this.op(`exhausted ${from}.${outcome}`, () => this.model.setEdgeLoop(from, outcome, bound));
  }

  params(params) { this.op('params', () => this.model.setParams(params)); }

  view() {
    const { template, edges, layout } = this.model.saveBody();
    return { template, edges, layout };
  }
}

// agentStep adds a task an agent does: its node and name, its performer,
// what it does, and who (one field, or two).
function agentStep(e, from, name, text, who) {
  const id = e.node(from, 'task', name);
  e.work(id);
  e.field(id, 'prompt', text);
  for (const [key, value] of Object.entries(who)) e.field(id, key, value);
  return id;
}

// agentDecision adds a decision an agent decides, with its two answers
// labelled; the second is drawn later by the caller.
function agentDecision(e, from, name, text, who) {
  const id = e.node(from, 'decision', name);
  e.kind(id, 'agent');
  e.field(id, 'prompt', text);
  for (const [key, value] of Object.entries(who)) e.field(id, key, value);
  return id;
}

const LEAD = { lead: true };

function discussAndDecide(e) {
  const discuss = agentStep(e, 'start', 'Discuss the idea', 'Discuss {{params.idea}} with the operator until the high-level idea is clear. Write it down.', LEAD);
  const decided = e.node(discuss, 'decision', 'Idea decided');
  e.field(decided, 'ask', 'Is the high-level idea decided?');
  e.field(decided, 'choices', ['decided', 'not-yet']);
  e.connect(decided, 'not-yet', discuss);
  e.loop(decided, 'not-yet', { maxIterations: 3 });
  return decided;
}

// giveUp is where an agent decision's used-up loop goes: a failed end.
function giveUp(e, from) {
  const end = e.node(from, 'end', 'Gave up');
  e.op(`result ${end}`, () => e.model.updateNode(end, (draft) => { draft.result = 'failed'; }));
  e.label(from, 'pass', 'give-up');
  return end;
}

// shipIt readies the PR after `from` (leaving it on `outcome`) and merges it
// if the run allows it; it returns the end.
function shipIt(e, from, outcome) {
  const ready = agentStep(e, from, 'Ready the PR', 'Mark the PR ready for review and notify the operator with a short summary.', LEAD);
  e.label(from, 'pass', outcome);
  const ci = e.node(ready, 'task', 'CI');
  e.kind(ci, 'program');
  e.field(ci, 'run', 'gh');
  e.field(ci, 'args', ['pr', 'checks', '--watch']);
  const merge = e.node(ci, 'decision', 'Merge');
  const mergePR = agentStep(e, merge, 'Merge the PR', 'Merge the PR now that CI passed.', LEAD);
  e.label(merge, 'pass', 'merge');
  const end = e.node(mergePR, 'end');
  e.connect(merge, 'leave', end);
  return { ready, end };
}

export function fullSimple() {
  const e = new FullEditor('Simple implementation');
  const decided = discussAndDecide(e);
  const provision = agentStep(e, decided, 'Provision an implementor', 'Bring in an implementor for the idea and brief it.', LEAD);
  e.label(decided, 'pass', 'decided');
  const build = agentStep(e, provision, 'Build the feature', 'Build the agreed idea on a branch and open a draft PR.', { role: 'implementor' });
  const review = agentStep(e, build, 'Cold review', 'Review the PR cold, as someone who did not build it. List what you find.', { anyone: true, differentFrom: [build] });
  const decide = agentDecision(e, review, 'Iterate or move on', 'Does the review warrant another round of changes, or does it move ahead?', LEAD);
  e.connect(decide, 'iterate', build);
  const gaveUp = giveUp(e, decide);
  e.loop(decide, 'iterate', { maxIterations: 3, onExhausted: 'give-up' });
  shipIt(e, decide, 'move-on');
  void gaveUp;
  e.params({
    idea: { type: 'string', required: true, description: 'The idea to build' },
    auto_merge: { type: 'boolean', default: false, description: 'Merge on green CI without asking' },
  });
  return e;
}

export function fullAdvanced() {
  const e = new FullEditor('Advanced implementation');
  const decided = discussAndDecide(e);
  const criteria = agentStep(e, decided, 'Define acceptance criteria', 'Write the acceptance criteria for the decided idea.', LEAD);
  e.label(decided, 'pass', 'decided');
  const provision = agentStep(e, criteria, 'Provision an implementor', 'Bring in an implementor and brief it with the idea and the criteria.', LEAD);
  const implement = agentStep(e, provision, 'Implement', 'Implement the idea on a branch to meet the criteria, and open a draft PR.', { role: 'implementor' });
  // The review gate: four agents who are not the implementor, all approve.
  const review = e.node(implement, 'decision', 'Review');
  e.kind(review, 'agent');
  const minors = agentDecision(e, review, 'Fix minors or proceed', 'The reviewers approved. Are their minor findings worth fixing first?', LEAD);
  e.label(review, 'pass', 'approved');
  e.connect(review, 'changes', implement);
  const gaveUp = giveUp(e, review);
  e.loop(review, 'changes', { maxIterations: 3, onExhausted: 'give-up' });
  for (let n = 0; n < 3; n += 1) e.decider(review);
  e.agree(review, 'approved');
  const focus = [
    'Cold review of the PR, as someone who did not build it. Approve it or ask for changes, with your reasons.',
    'Security review of the PR: injection, secrets, permissions. Approve it or ask for changes, with your reasons.',
    'Product review of the PR: is the feature usable as the idea meant it? Approve it or ask for changes, with your reasons.',
    'Engineering review of the PR: structure, tests, maintainability. Approve it or ask for changes, with your reasons.',
  ];
  focus.forEach((text, index) => {
    e.field(review, 'prompt', text, index);
    e.field(review, 'anyone', true, index);
    e.field(review, 'differentFrom', [implement], index);
  });
  // The lead fixes minors or proceeds, then checks the criteria.
  const fix = agentStep(e, minors, 'Fix the minors', 'Fix the minor review findings on the same branch.', { role: 'implementor', sameAs: implement });
  e.label(minors, 'pass', 'fix-minors');
  const conform = agentDecision(e, fix, 'Meets the criteria', 'Does the implementation, as reviewed, meet the acceptance criteria?', LEAD);
  e.connect(minors, 'proceed', conform);
  const { ready } = shipIt(e, conform, 'meets');
  // Off the criteria: amend them if the operator allowed it, else rework.
  const allowed = e.node(conform, 'decision', 'Amending allowed');
  e.label(conform, 'pass', 'misses');
  const amend = agentDecision(e, allowed, 'Amend the criteria',
    'The operator allows minor changes to the criteria and design. Is the gap minor? Then answer amend; otherwise rework.', LEAD);
  e.label(allowed, 'pass', 'allowed');
  e.connect(allowed, 'rework', implement);
  e.connect(allowed, 'give-up', gaveUp);
  e.loop(allowed, 'rework', { maxIterations: 3, onExhausted: 'give-up' });
  const amended = agentStep(e, amend, 'Amend criteria and design', 'Make the minor changes to the criteria and the design, and note them for the operator.', LEAD);
  e.label(amend, 'pass', 'amend');
  e.connect(amended, 'pass', ready);
  e.connect(amend, 'rework', implement);
  e.connect(amend, 'give-up', gaveUp);
  e.loop(amend, 'rework', { maxIterations: 3, onExhausted: 'give-up' });
  e.params({
    idea: { type: 'string', required: true, description: 'The idea to build' },
    allow_amend: { type: 'boolean', default: false, description: 'The lead may make minor changes to the criteria and design' },
    auto_merge: { type: 'boolean', default: false, description: 'Merge on green CI without asking' },
  });
  return e;
}

// canonical is a view with node ids replaced by their names' slugs (an
// unnamed end is "end"), so two editors' processes compare as the same
// process whatever ids each minted. Task pass labels compare as one.
// INLINE_ONLY is what only the inline editor can make: the rules deciding
// these decisions, and a step's "may fail".
export const INLINE_ONLY = Object.freeze(['merge', 'amending-allowed']);

export function canonical(view, { inlineOnly = false } = {}) {
  const { nodes } = view.template;
  const ids = {};
  for (const [id, node] of Object.entries(nodes)) ids[id] = node.name ? stepSlug(node.name) : node.type;
  if (new Set(Object.values(ids)).size !== Object.keys(ids).length) throw new Error(`two nodes share a name: ${JSON.stringify(ids)}`);
  const PASS = new Set(['pass', 'next', 'done', 'success', 'continue']);
  const performer = (p) => {
    if (!p) return undefined;
    const out = { ...p };
    if (out.sameAs) out.sameAs = ids[out.sameAs];
    if (out.differentFrom) out.differentFrom = out.differentFrom.map((id) => ids[id]);
    return out;
  };
  const out = {};
  for (const [id, node] of Object.entries(nodes)) {
    const copy = { type: node.type, name: node.name, performer: performer(node.performer), deciders: node.deciders?.map(performer), agree: node.agree, loops: node.loops, mayFail: node.mayFail };
    if (inlineOnly && INLINE_ONLY.includes(ids[id])) copy.performer = 'a rule: inline only';
    if (inlineOnly) delete copy.mayFail;
    if (node.type === 'end') copy.result = node.result || 'success';
    out[ids[id]] = JSON.parse(JSON.stringify(copy));
  }
  const edges = view.edges.filter((edge) => edge.from).map((edge) => {
    const plain = nodes[edge.from]?.type === 'task' || nodes[edge.from]?.type === 'start';
    return `${ids[edge.from]} -${plain && PASS.has(edge.outcome) ? 'pass' : edge.outcome}-> ${ids[edge.to]}`;
  }).sort();
  return { nodes: out, edges, params: view.template.params };
}
