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
// "Merge" and "Amending allowed" are rule decisions, edited with the same
// rows as inline (process-rule-cases.js), and CI goes on when it fails
// through the same "If it fails" row (process-failure-row.js).

import { ProcessEditModel, blankEditView } from '../dashboard/js/process-edit-model.js';
import {
  addGateDecider, defaultPerformer, setDecisionKind, setNodeFailure, setPerformerField, setPerformerKind, setRule,
} from '../dashboard/js/process-node-form.js';
import { ruleOperands, withAnd, withRow } from '../dashboard/js/process-rules.js';
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

  // failure is the Retry block's "If it fails" row.
  failure(id, mode) {
    this.op(`if-fails ${id}`, () => this.model.updateNode(id, (draft) => setNodeFailure(draft, { mode })));
  }

  // rule is a decision's Performer kind set to rule: one case on the step
  // just before it, picking its first answer, else its next.
  rule(id) {
    this.op(`kind ${id} rule`, () => this.model.updateNode(id, (draft) => setDecisionKind(this.model, id, draft, draft.performer ||= defaultPerformer('human'), 'rule')));
  }

  // ruleRow and ruleAnd are the rule rows' pickers and "+ and".
  ruleRow(id, index, row, change) {
    this.editRule(`rule ${id}.${index}.${row}`, id, (rule, operands) => withRow(rule, operands, index, row, change));
  }

  ruleAnd(id, index) {
    this.editRule(`rule ${id}.${index} + and`, id, (rule, operands) => withAnd(rule, operands, index));
  }

  editRule(label, id, edit) {
    const operands = ruleOperands(this.model, id);
    this.op(label, () => this.model.updateNode(id, (draft) => setRule(draft.performer, edit(draft.performer, operands))));
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
  // A new task comes with an agent doing it.
  const id = e.node(from, 'task', name);
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
function shipIt(e, from, outcome, { ci = false } = {}) {
  const ready = agentStep(e, from, 'Ready the PR', ci ? 'Mark the PR ready for review and notify the operator with a short summary.'
    : 'Once CI on the PR passes (have it fixed until it does), mark the PR ready for review and notify the operator with a short summary.', LEAD);
  e.label(from, 'pass', outcome);
  let before = ready;
  if (ci) {
    before = e.node(ready, 'task', 'CI');
    e.kind(before, 'program');
    e.field(before, 'run', 'gh');
    e.field(before, 'args', ['pr', 'checks', '--watch']);
    e.failure(before, 'go-on');
  }
  const merge = e.node(before, 'decision', 'Merge');
  const mergePR = agentStep(e, merge, 'Merge the PR', ci ? 'Merge the PR now that CI passed.' : 'Merge the PR.', LEAD);
  e.label(merge, 'pass', 'merge');
  const end = e.node(mergePR, 'end');
  e.connect(merge, 'leave', end);
  // With ci: CI succeeded (the seed) and auto_merge → merge. Without: auto_merge alone.
  e.rule(merge);
  if (ci) {
    e.ruleAnd(merge, 0);
    e.ruleRow(merge, 0, 1, { operand: 'params.auto_merge' });
  } else e.ruleRow(merge, 0, 0, { operand: 'params.auto_merge' });
  return { ready, end };
}

export function fullSimple() {
  const e = new FullEditor('Simple implementation');
  // A rule reads only declared params: declare them first.
  e.params({
    idea: { type: 'string', required: true, description: 'The idea to build' },
    auto_merge: { type: 'boolean', default: false, description: 'Merge on green CI without asking' },
  });
  const decided = discussAndDecide(e);
  const provision = agentStep(e, decided, 'Provision an implementor', 'Bring in an implementor for the idea and brief it.', LEAD);
  e.label(decided, 'pass', 'decided');
  const build = agentStep(e, provision, 'Build the feature', 'Build the agreed idea on a branch and open a draft PR. Keep at it until CI on the PR passes.', { role: 'implementor' });
  const review = agentStep(e, build, 'Cold review', 'Review the PR cold, as someone who did not build it. List what you find.', { anyone: true, differentFrom: [build] });
  const decide = agentDecision(e, review, 'Iterate or move on', 'Does the review warrant another round of changes, or does it move ahead?', LEAD);
  e.connect(decide, 'iterate', build);
  const gaveUp = giveUp(e, decide);
  e.loop(decide, 'iterate', { maxIterations: 3, onExhausted: 'give-up' });
  shipIt(e, decide, 'move-on');
  void gaveUp;
  return e;
}

export function fullAdvanced() {
  const e = new FullEditor('Advanced implementation');
  // A rule reads only declared params: declare them first.
  e.params({
    idea: { type: 'string', required: true, description: 'The idea to build' },
    allow_amend: { type: 'boolean', default: false, description: 'The lead may make minor changes to the criteria and design' },
    auto_merge: { type: 'boolean', default: false, description: 'Merge on green CI without asking' },
  });
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
  const { ready } = shipIt(e, conform, 'meets', { ci: true });
  // Off the criteria: amend them if the operator allowed it, else rework.
  const allowed = e.node(conform, 'decision', 'Amending allowed');
  e.label(conform, 'pass', 'misses');
  const amend = agentDecision(e, allowed, 'Amend the criteria',
    'The operator allows minor changes to the criteria and design. Is the gap minor? Then answer amend; otherwise rework.', LEAD);
  e.label(allowed, 'pass', 'allowed');
  e.connect(allowed, 'rework', implement);
  e.connect(allowed, 'give-up', gaveUp);
  // allow_amend is true → allowed, else rework (made a rule before rework
  // is bounded, else's first pick is rework, as inline).
  e.rule(allowed);
  e.ruleRow(allowed, 0, 0, { operand: 'params.allow_amend' });
  e.loop(allowed, 'rework', { maxIterations: 3, onExhausted: 'give-up' });
  const amended = agentStep(e, amend, 'Amend criteria and design', 'Make the minor changes to the criteria and the design, and note them for the operator.', LEAD);
  e.label(amend, 'pass', 'amend');
  e.connect(amended, 'pass', ready);
  e.connect(amend, 'rework', implement);
  e.connect(amend, 'give-up', gaveUp);
  e.loop(amend, 'rework', { maxIterations: 3, onExhausted: 'give-up' });
  return e;
}

// canonical is a view with node ids replaced by their names' slugs (an
// unnamed end is "end"), so two editors' processes compare as the same
// process whatever ids each minted. Task pass labels compare as one.
export function canonical(view) {
  const { nodes } = view.template;
  const ids = {};
  for (const [id, node] of Object.entries(nodes)) ids[id] = node.name ? stepSlug(node.name) : node.type;
  if (new Set(Object.values(ids)).size !== Object.keys(ids).length) throw new Error(`two nodes share a name: ${JSON.stringify(ids)}`);
  const PASS = new Set(['pass', 'next', 'done', 'success', 'continue']);
  const performer = (p) => {
    if (!p) return undefined;
    const out = { ...p };
    // A rule names steps by id in its conditions.
    if (out.cases) out.cases = out.cases.map((c) => ({ ...c, when: c.when.replace(/\bsteps\.([A-Za-z0-9_-]+)\./g, (all, id) => `steps.${ids[id] || id}.`) }));
    if (out.sameAs) out.sameAs = ids[out.sameAs];
    if (out.differentFrom) out.differentFrom = out.differentFrom.map((id) => ids[id]);
    return out;
  };
  const out = {};
  for (const [id, node] of Object.entries(nodes)) {
    const copy = { type: node.type, name: node.name, performer: performer(node.performer), deciders: node.deciders?.map(performer), agree: node.agree, loops: node.loops, mayFail: node.mayFail };
    if (node.type === 'end') copy.result = node.result || 'success';
    out[ids[id]] = JSON.parse(JSON.stringify(copy));
  }
  const edges = view.edges.filter((edge) => edge.from).map((edge) => {
    const plain = nodes[edge.from]?.type === 'task' || nodes[edge.from]?.type === 'start';
    return `${ids[edge.from]} -${plain && PASS.has(edge.outcome) ? 'pass' : edge.outcome}-> ${ids[edge.to]}`;
  }).sort();
  return { nodes: out, edges, params: view.template.params };
}
