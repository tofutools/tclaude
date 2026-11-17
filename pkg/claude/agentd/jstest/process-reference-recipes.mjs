// The operator's two reference implementation processes, built only through
// the inline mini editor's own operations (tk_c324d5a4): each call below is
// one thing the operator does in the card, landed the way the card lands it
// (groups-mini-editor.js: apply, renameStep, the inspector's setWho), so the
// number of calls is how many edits building the process takes. The recipes
// feed process-reference-recipes.test.mjs (the budget and the fixture) and,
// through process-reference-recipes.json, the Go tests that validate and run
// them.
//
// TODO(tk_c324d5a4): the two settings gates ("Amend the criteria?" and
// "Merge?") are lead decisions that quote {{params.allow_amend}} and
// {{params.auto_merge}}: the lead is trusted to obey them. Once the engine
// has rule decisions (decided by a condition on params and on a previous
// step's result, ciperf's engine task), make each one a rule decision: one
// Who change on that node, nothing else in the recipe moves.

import * as m from '../dashboard/js/process-mini-edit.js';
import { setStepConstraint, setStepName, setStepText, setStepWho, stepListToView, blankStepList } from '../dashboard/js/process-step-list.js';
import { addReviewer, setDeciderAsk, withReviewerDefault } from '../dashboard/js/process-gate-deciders.js';
import { followName } from '../dashboard/js/process-step-ids.js';

const OPS = { after: m.insertAfter, before: m.insertBefore, branch: m.branch, alongside: m.runAlongside };

// InlineEditor replays the card: each method is one operator edit.
export class InlineEditor {
  constructor(name) {
    this.view = stepListToView(blankStepList(name));
    this.ops = [];
  }

  land(label, result) {
    this.ops.push(label);
    // The card's apply: a {{param}} a step now uses is declared with it.
    this.view = m.declareParams(result.view);
    return result;
  }

  // add is a handle (+ after, ⑂ branch, ⫴ alongside, insert before) and the
  // kind picked: one edit.
  add(op, at, kind) {
    return this.land(`${op} ${kind} at ${at}`, OPS[op](this.view, at, kind));
  }

  // step adds a step and fills it in: its name, what it does and who does it,
  // each one edit, as in the inspector. Returns the step's id (it follows
  // the name in a draft that has never run).
  step(op, at, kind, { name, text, who, constraint } = {}) {
    let { id } = this.add(op, at, kind);
    if (name) id = this.name(id, name);
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
    return this.land('params', m.setParams(this.view, params));
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
  const decided = e.step('after', discuss, 'decide-you', { name: 'Idea decided', text: 'Is the high-level idea decided?' });
  e.rename(decided, 'yes', 'decided');
  e.rename(decided, 'no', 'not-yet');
  e.aim(decided, 'not-yet', discuss);
  return decided;
}

// shipIt is how both processes close: the lead readies the PR and tells the
// operator, then merges it on green CI when the run allows it.
function shipIt(e, at) {
  const ready = e.step('after', at, 'agent', {
    name: 'Ready the PR', who: LEAD,
    text: 'Mark the PR ready for review and notify the operator with a short summary.',
  });
  const merge = e.step('after', ready, 'decide', {
    name: 'Merge', who: LEAD,
    text: 'Auto-merge is {{params.auto_merge}}. Answer merge only if it is true and CI on the PR passed; otherwise leave it for the operator.',
  });
  e.rename(merge, 'yes', 'merge');
  e.rename(merge, 'no', 'leave');
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
  shipIt(e, decide);
  e.params({
    idea: { type: 'string', required: true, description: 'The idea to build' },
    auto_merge: { type: 'boolean', default: false, description: 'Merge on green CI without asking' },
  });
  return e;
}

// advanced is example 1: acceptance criteria, four reviewers who must all
// approve, the lead's minors and criteria decisions, and the settings gates.
export function advanced() {
  const e = new InlineEditor('Advanced implementation');
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
  // Four reviewers vote at once; all must approve.
  const review = e.step('after', implement, 'decide', { name: 'Review' });
  e.rename(review, 'yes', 'approved');
  e.rename(review, 'no', 'changes');
  e.aim(review, 'changes', implement);
  e.decider(review, ANYONE, 'Security review of the PR: injection, secrets, permissions. Approve it or ask for changes, with your reasons.');
  e.decider(review, ANYONE, 'Product review of the PR: is the feature usable as the idea meant it? Approve it or ask for changes, with your reasons.');
  e.decider(review, ANYONE, 'Engineering review of the PR: structure, tests, maintainability. Approve it or ask for changes, with your reasons.');
  // The first decider is the decision's own performer: give it who and focus.
  e.deciderWho(review, 0, ANYONE);
  e.land(`ask ${review}.0`, setDeciderAsk(e.view, review, 0, 'Cold review of the PR, as someone who did not build it. Approve it or ask for changes, with your reasons.'));
  // The lead checks the result against the criteria; off the criteria, it
  // may amend them only if the operator allowed it, else back it goes.
  const conform = e.step('after', review, 'decide', {
    name: 'Meets the criteria', who: LEAD, text: 'Does the implementation, as reviewed, meet the acceptance criteria?',
  });
  e.rename(conform, 'yes', 'meets');
  shipIt(e, conform);
  const amend = e.add('branch', conform, 'decide').id;
  e.drop(conform, 'no');
  const amendGate = e.name(amend, 'Amend the criteria');
  e.text(amendGate, 'The operator allowed amending: {{params.allow_amend}}. Only if that is true and the gap is minor, answer amend; otherwise rework.');
  e.who(amendGate, LEAD);
  e.rename(conform, 'other', 'misses');
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
  const fix = e.add('branch', minors, 'agent').id;
  e.drop(minors, 'no');
  e.rename(minors, 'other', 'fix-minors');
  const fixed = e.name(fix, 'Fix the minors');
  e.text(fixed, 'Fix the minor review findings on the same branch.');
  e.who(fixed, IMPLEMENTOR);
  e.land(`constraint ${fixed}`, m.updateStep(e.view, fixed, (s) => setStepConstraint(s, { mode: 'same', step: implement })));
  e.params({
    idea: { type: 'string', required: true, description: 'The idea to build' },
    allow_amend: { type: 'boolean', default: false, description: 'The lead may make minor changes to the criteria and design' },
    auto_merge: { type: 'boolean', default: false, description: 'Merge on green CI without asking' },
  });
  return e;
}

export function nodeNamed(view, name) {
  return Object.entries(view.template.nodes).find(([, node]) => node.name === name)?.[0] || '';
}
