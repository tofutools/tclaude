// The operator's two reference implementation processes, built only through
// the inline mini editor's own operations (tk_c324d5a4): each call below is
// one thing the operator does in the card, landed the way the card lands it
// (groups-mini-editor.js: apply, renameStep, the inspector's setWho), so the
// number of calls is how many edits building the process takes. The recipes
// feed process-reference-recipes.test.mjs (the budget and the fixture) and,
// through process-reference-recipes.json, the Go tests that validate and run
// them.
//
// The two settings gates are rule decisions: "Merge" merges only when the
// run's auto_merge is on and the CI step before it succeeded (CI may fail:
// the run goes on, for the rule to see), and "Amending allowed" lets the
// lead judge a criteria gap only when the run's allow_amend is on.

import * as m from '../dashboard/js/process-mini-edit.js';
import { setStepConstraint, setStepFailure, setStepName, setStepText, setStepWho, stepListToView, blankStepList } from '../dashboard/js/process-step-list.js';
import { withAnd, withRow } from '../dashboard/js/process-rules.js';
import { addReviewer, setDeciderAsk, withReviewerDefault } from '../dashboard/js/process-gate-deciders.js';
import { followName } from '../dashboard/js/process-step-ids.js';

const OPS = { after: m.insertAfter, before: m.insertBefore, branch: m.branch, alongside: m.runAlongside };
// answer is ⑂ on one answer's row: (view, decision, outcome, kind).
const onAnswer = (view, at, kind, outcome) => m.insertOnAnswer(view, at, outcome, kind);

// offered is what the card's kind picker offers for an operation
// (groups-mini-editor.js: KIND_CHOICES, INSERT_CHOICES, branchChoices,
// afterChoices, alongsideChoices), so a recipe never uses a kind the
// operator could not pick there.
const KINDS = ['agent', 'you', 'command'];
const INSERT = [...KINDS, 'decide'];
function offered(op, view, at) {
  if (op === 'alongside') return m.reviewLoop(view, at) && !m.canAddReviewerTrack(view, at) ? [...KINDS, m.REVIEWER_TRACK] : KINDS;
  if (op === 'branch') return m.branchKinds(view, at) || INSERT;
  if (op === 'after') return m.canAddReviewerTrack(view, at) ? INSERT : [...INSERT, m.REVIEWER_TRACK];
  return INSERT;
}

// InlineEditor replays the card: each method is one operator edit.
export class InlineEditor {
  constructor(name) {
    this.view = stepListToView(blankStepList(name));
    this.ops = [];
  }

  land(label, result, { declare = true } = {}) {
    this.ops.push(label);
    // The card's apply: a {{param}} a step now uses is declared with it.
    this.view = declare ? m.declareParams(result.view) : result.view;
    return result;
  }

  // add is a handle (+ after, ⑂ branch, ⫴ alongside, insert before) and the
  // kind picked: one edit.
  add(op, at, kind, outcome = '') {
    return this.land(`${op} ${kind} at ${at}`, this.added(op, at, kind, outcome));
  }

  added(op, at, kind, outcome) {
    if (!offered(op, this.view, at).includes(kind)) throw new Error(`the card does not offer ${kind} for ${op} at ${at}`);
    return op === 'answer' ? onAnswer(this.view, at, kind, outcome) : OPS[op](this.view, at, kind);
  }

  // step adds a step and fills it in: its name (typed straight into the
  // name field a new step opens on, so one edit with adding it), what it
  // does and who does it, each one edit, as in the inspector. Returns the
  // step's id (it follows the name in a draft that has never run). On
  // 'answer', `outcome` is the decision's answer the step goes on.
  step(op, at, kind, { name, text, who, constraint, outcome = '' } = {}) {
    let id;
    if (name) {
      const added = this.added(op, at, kind, outcome);
      const named = m.updateStep(added.view, added.id, (s) => setStepName(s, name));
      id = this.land(`${op} ${kind} "${name}" at ${at}`, followName(named.view, added.id)).id;
    } else id = this.add(op, at, kind, outcome).id;
    if (text) this.text(id, text);
    if (who) this.who(id, who);
    if (constraint) this.land(`constraint ${id}`, m.updateStep(this.view, id, (s) => setStepConstraint(s, constraint)));
    return id;
  }

  name(id, name) {
    const named = m.updateStep(this.view, id, (s) => setStepName(s, name));
    return this.land(`name ${id}`, followName(named.view, id)).id;
  }

  text(id, text) {
    return this.land(`text ${id}`, m.updateStep(this.view, id, (s) => setStepText(s, text)));
  }

  who(id, who) {
    const node = this.view.template.nodes[id];
    if (node.type === 'decision') return this.land(`who ${id}`, m.setDecisionWho(this.view, id, who));
    return this.land(`who ${id}`, m.updateStep(this.view, id, (s) => setStepWho(s, who)));
  }

  rename(id, outcome, name) {
    return this.land(`rename ${id}.${outcome}`, m.renameOutcome(this.view, id, outcome, name));
  }

  aim(id, outcome, to) {
    return this.land(`aim ${id}.${outcome}`, m.setOutcomeTarget(this.view, id, outcome, to));
  }

  drop(id, outcome) {
    return this.land(`drop ${id}.${outcome}`, m.removeOutcome(this.view, id, outcome));
  }

  // decider adds a decider (+ another decider) with its own ask and who:
  // three edits, as in the gate's rows.
  decider(id, who, ask) {
    const index = m.gateDeciders(this.view.template.nodes[id]).length;
    this.land(`+ decider ${id}`, addReviewer(this.view, id));
    this.deciderWho(id, index, who);
    this.land(`ask ${id}.${index}`, setDeciderAsk(this.view, id, index, ask));
  }

  deciderWho(id, index, who) {
    const before = this.view.template.nodes[id].deciders[index];
    const changed = m.setDecider(this.view, id, index, who);
    this.land(`who ${id}.${index}`, before.kind === 'agent' ? changed : withReviewerDefault(changed.view, id, index));
  }

  // params is the Params dialog: one Apply.
  params(params) {
    return this.land('params', m.setParams(this.view, params), { declare: false });
  }

  // failure is the step's "If it fails" picker.
  failure(id, mode) {
    return this.land(`if-fails ${id}`, m.updateStep(this.view, id, (s) => setStepFailure(s, { mode })));
  }

  // rule is the decision's Who picker set to "a rule": it starts with one
  // case on the first thing it can read.
  rule(id) {
    return this.who(id, { mode: 'rule' });
  }

  // ruleRow changes one picker of a rule's condition row, and ruleAnd is a
  // case's "+ and": the edits RuleCases makes (process-rules.js).
  ruleRow(id, index, row, change) {
    return this.setRule(`rule ${id}.${index}.${row}`, id, (rule, operands) => withRow(rule, operands, index, row, change));
  }

  ruleAnd(id, index) {
    return this.setRule(`rule ${id}.${index} + and`, id, (rule, operands) => withAnd(rule, operands, index));
  }

  setRule(label, id, edit) {
    const { cases, else: otherwise } = edit(this.view.template.nodes[id].performer, m.ruleOperands(this.view, id));
    return this.land(label, m.setRule(this.view, id, { cases, else: otherwise }));
  }
}

const LEAD = { mode: 'lead' };
const ANYONE = { mode: 'anyone' };
const IMPLEMENTOR = { mode: 'role', role: 'implementor' };

// discussAndDecide is how both processes open: the operator and the lead
// discuss the idea, and the operator says when the high-level idea is
// decided (not yet: back to discussing).
function discussAndDecide(e) {
  const discuss = e.step('after', 'start', 'agent', {
    name: 'Discuss the idea', who: LEAD,
    text: 'Discuss {{idea}} with the operator until the high-level idea is clear. Write it down.',
  });
  const decided = e.step('branch', discuss, 'decide-you', { name: 'Idea decided', text: 'Is the high-level idea decided?' });
  e.rename(decided, 'yes', 'decided');
  e.rename(decided, 'no', 'not-yet');
  e.aim(decided, 'not-yet', discuss);
  return decided;
}

// shipIt is how both processes close: the lead readies the PR and tells the
// operator, CI runs (it may fail: the run goes on, for the rule to see), and
// a rule merges only when the run allows it and CI passed. The params must
// be declared first: a rule reads only declared params.
function shipIt(e, at) {
  const ready = e.step('after', at, 'agent', {
    name: 'Ready the PR', who: LEAD,
    text: 'Mark the PR ready for review and notify the operator with a short summary.',
  });
  const ci = e.step('after', ready, 'command', { name: 'CI', text: 'gh pr checks --watch' });
  e.failure(ci, 'go-on');
  const merge = e.step('after', ci, 'decide', { name: 'Merge' });
  e.rename(merge, 'yes', 'merge');
  e.rename(merge, 'no', 'leave');
  // A new rule starts on the step just before it: CI succeeded.
  e.rule(merge);
  e.ruleAnd(merge, 0);
  e.ruleRow(merge, 0, 1, { operand: 'params.auto_merge' });
  e.step('after', merge, 'agent', { name: 'Merge the PR', who: LEAD, text: 'Merge the PR now that CI passed.' });
  return merge;
}

// simple is example 2: one implementor, one cold reviewer, and the lead
// decides whether the review needs another round.
export function simple() {
  const e = new InlineEditor('Simple implementation');
  const decided = discussAndDecide(e);
  const provision = e.step('after', decided, 'agent', {
    name: 'Provision an implementor', who: LEAD, text: 'Bring in an implementor for the idea and brief it.',
  });
  const build = e.step('after', provision, 'agent', {
    name: 'Build the feature', who: IMPLEMENTOR, text: 'Build the agreed idea on a branch and open a draft PR.',
  });
  const review = e.step('after', build, 'agent', {
    name: 'Cold review', who: ANYONE, constraint: { mode: 'different', steps: [build] },
    text: 'Review the PR cold, as someone who did not build it. List what you find.',
  });
  const decide = e.step('after', review, 'decide', {
    name: 'Iterate or move on', who: LEAD, text: 'Does the review warrant another round of changes, or does it move ahead?',
  });
  e.rename(decide, 'yes', 'move-on');
  e.rename(decide, 'no', 'iterate');
  e.aim(decide, 'iterate', build);
  e.params({
    idea: { type: 'string', required: true, description: 'The idea to build' },
    auto_merge: { type: 'boolean', default: false, description: 'Merge on green CI without asking' },
  });
  shipIt(e, decide);
  return e;
}

// advanced is example 1: acceptance criteria, four reviewers who must all
// approve, the lead's minors and criteria decisions, and the settings gates.
// The four reviewers vote at once in one gate: one asking for changes sends
// the work back with every reviewer's reasons.
export function advanced() {
  return advancedWith('gate');
}

// advancedTracks is example 1 with a reviewer track per reviewer: each
// reviews, and sends the work back for its own fix, at its own pace; the
// tracks join once every reviewer approved.
export function advancedTracks() {
  return advancedWith('tracks');
}

const REVIEWS = [
  ['Cold review', 'Cold review of the PR, as someone who did not build it.'],
  ['Security review', 'Security review of the PR: injection, secrets, permissions.'],
  ['Product review', 'Product review of the PR: is the feature usable as the idea meant it?'],
  ['Engineering review', 'Engineering review of the PR: structure, tests, maintainability.'],
];
const VERDICT = 'Approve it or ask for changes, with your reasons.';

function advancedWith(reviews) {
  const e = new InlineEditor(reviews === 'gate' ? 'Advanced implementation' : 'Advanced implementation, reviewer tracks');
  e.params({
    idea: { type: 'string', required: true, description: 'The idea to build' },
    allow_amend: { type: 'boolean', default: false, description: 'The lead may make minor changes to the criteria and design' },
    auto_merge: { type: 'boolean', default: false, description: 'Merge on green CI without asking' },
  });
  const decided = discussAndDecide(e);
  const criteria = e.step('after', decided, 'agent', {
    name: 'Define acceptance criteria', who: LEAD, text: 'Write the acceptance criteria for the decided idea.',
  });
  const provision = e.step('after', criteria, 'agent', {
    name: 'Provision an implementor', who: LEAD, text: 'Bring in an implementor and brief it with the idea and the criteria.',
  });
  const implement = e.step('after', provision, 'agent', {
    name: 'Implement', who: IMPLEMENTOR, text: 'Implement the idea on a branch to meet the criteria, and open a draft PR.',
  });
  let reviewed = implement;
  if (reviews === 'gate') {
    // Four reviewers vote at once; all must approve.
    const review = e.step('after', implement, 'decide', { name: 'Review' });
    e.rename(review, 'yes', 'approved');
    e.rename(review, 'no', 'changes');
    e.aim(review, 'changes', implement);
    for (const [, focus] of REVIEWS.slice(1)) e.decider(review, ANYONE, `${focus} ${VERDICT}`);
    // The first decider is the decision's own performer: give it who and focus.
    e.deciderWho(review, 0, ANYONE);
    e.land(`ask ${review}.0`, setDeciderAsk(e.view, review, 0, `${REVIEWS[0][1]} ${VERDICT}`));
    reviewed = review;
  }
  // The lead checks the result against the criteria; off the criteria, it
  // may amend them only if the operator allowed it, else back it goes.
  const conform = e.step('after', reviewed, 'decide', {
    name: 'Meets the criteria', who: LEAD, text: 'Does the implementation, as reviewed, meet the acceptance criteria?',
  });
  e.rename(conform, 'yes', 'meets');
  shipIt(e, conform);
  const ready = nodeNamed(e.view, 'Ready the PR');
  e.rename(conform, 'no', 'misses');
  // Missing the criteria still ends in a ready PR, once amended.
  e.aim(conform, 'misses', ready);
  const allowed = e.step('answer', conform, 'decide', { outcome: 'misses', name: 'Amending allowed' });
  e.rename(allowed, 'yes', 'allowed');
  e.rename(allowed, 'no', 'rework');
  e.rule(allowed);
  e.ruleRow(allowed, 0, 0, { operand: 'params.allow_amend' });
  e.aim(allowed, 'rework', implement);
  const amendGate = e.step('answer', allowed, 'decide', {
    outcome: 'allowed', name: 'Amend the criteria', who: LEAD,
    text: 'The operator allows minor changes to the criteria and design. Is the gap minor? Then answer amend; otherwise rework.',
  });
  e.rename(amendGate, 'yes', 'amend');
  e.rename(amendGate, 'no', 'rework');
  e.aim(amendGate, 'rework', implement);
  e.step('after', amendGate, 'agent', {
    name: 'Amend criteria and design', who: LEAD, text: 'Make the minor changes to the criteria and the design, and note them for the operator.',
  });
  // Once all approve, the lead fixes the minors or just proceeds.
  const minors = e.step('before', conform, 'decide', {
    name: 'Fix minors or proceed', who: LEAD, text: 'The reviewers approved. Are their minor findings worth fixing first?',
  });
  e.rename(minors, 'yes', 'proceed');
  e.rename(minors, 'no', 'fix-minors');
  e.step('answer', minors, 'agent', {
    outcome: 'fix-minors', name: 'Fix the minors', who: IMPLEMENTOR, constraint: { mode: 'same', step: implement },
    text: 'Fix the minor review findings on the same branch.',
  });
  if (reviews === 'tracks') {
    // A reviewer track after the implementation, then three alongside it.
    let track = '';
    for (const [name, focus] of REVIEWS) {
      const added = track ? e.add('alongside', track, m.REVIEWER_TRACK) : e.add('after', implement, m.REVIEWER_TRACK);
      track = e.name(added.id, name);
      e.text(track, `${focus} ${VERDICT}`);
      // Its Who (anyone free, the group having no reviewer role) and "not
      // the implementor" are the new track's defaults: no edits.
    }
  }
  return e;
}

export function nodeNamed(view, name) {
  return Object.entries(view.template.nodes).find(([, node]) => node.name === name)?.[0] || '';
}
