// Tests for the inline mini editor's graph operations (mock M1, tcl-jqmrj5):
// each operation on a step-list process (the former quick-start starters,
// kept as fixtures in process-quickstart-starters.json) builds exactly the fixture
// TestMiniEditCasesAreRunnable proves valid and executable, and the
// operations keep the shapes the runtime needs.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as m from '../dashboard/js/process-mini-edit.js';
import { AGENT_VERDICTS, replaceStep, setStepChoices, setStepLoop, setStepText, stepListToView, viewToStepList } from '../dashboard/js/process-step-list.js';

const fixture = JSON.parse(readFileSync(new URL('./process-mini-edit-cases.json', import.meta.url), 'utf8'));
const steps = JSON.parse(readFileSync(new URL('./process-quickstart-starters.json', import.meta.url), 'utf8')).starters;
const ship = steps['build-review-ship'];
const check = steps['check-fix'];
// forward is the starter with its changes loop pointed on to merge instead:
// a decision with no loop yet, for the tests that send an answer back.
const forward = m.setOutcomeTarget(ship, 'approve', 'changes', 'merge').view;

// agentDecides makes the starter's review an agent decider that sends
// changes back to implement, the way the step list's "◇ agent decides" does.
function agentDecides() {
  const list = viewToStepList(ship).list;
  const review = list.steps.find((step) => step.id === 'review');
  let step = setStepChoices(review, [...AGENT_VERDICTS], 'approve');
  step = { ...step, links: step.links.map((link) => (link.outcome === 'changes' ? { ...link, to: 'implement' } : link)) };
  return stepListToView(replaceStep(list, setStepLoop(step, 'changes', { maxIterations: 3, onExhausted: '' })));
}

// fill writes the text an operator types into the new step.
const fill = ({ view, id }) => m.updateStep(view, id, (step) => setStepText(step, step.node.performer.kind === 'program' ? 'make test' : 'Do the thing.')).view;

// operatorsCase is the case that made branching hard to find: a first step
// right after start, with Outputs and nobody picked yet, which ⑂ branches
// through an agent decision put after it.
function operatorsCase() {
  const first = m.insertAfter(ship, 'start', 'agent');
  const view = m.updateStep(fill(first), first.id, (step) => ({ ...step, node: { ...step.node, captures: ['banana'] } })).view;
  return { first: first.id, out: m.branch(view, first.id, 'decide') };
}

// threeWay is a decision after implement with three named answers, the
// third going straight to the end.
function threeWay(kind = 'decide') {
  const out = m.insertAfter(ship, 'implement', kind);
  let view = fill(out);
  view = m.renameOutcome(view, out.id, 'yes', 'small').view;
  view = m.renameOutcome(view, out.id, 'no', 'large').view;
  const added = m.addOutcome(view, out.id, 'end');
  view = m.renameOutcome(added.view, out.id, added.outcome, 'skip').view;
  return { view, id: out.id };
}

function ruleCases() {
  const rule = m.setDecisionWho(ship, 'approve', { mode: 'rule' });
  return {
    'rule-approve': rule.view,
    'rule-approve-cases': m.setRule(rule.view, 'approve', {
      cases: [
        { when: 'steps.review.result == failed', then: 'changes' },
        { when: 'steps.implement.result == succeeded and params.issue != ""', then: 'ship' },
      ],
      else: 'hold',
    }).view,
  };
}

function cases() {
  return {
    'after-implement-agent': fill(m.insertAfter(ship, 'implement', 'agent')),
    'after-start-you': fill(m.insertAfter(ship, 'start', 'you')),
    'before-merge-command': fill(m.insertBefore(ship, 'merge', 'command')),
    'branch-approve-agent': fill(m.branch(ship, 'approve', 'agent')),
    'alongside-review-agent': fill(m.runAlongside(ship, 'review', 'agent')),
    'alongside-last-command': fill(m.runAlongside(check, 'verify', 'command')),
    'duplicate-review': m.duplicate(ship, 'review').view,
    'duplicate-approve': m.duplicate(ship, 'approve').view,
    'kind-implement-you': m.changeKind(ship, 'implement', 'you').view,
    'kind-approve-command': fill(m.changeKind(ship, 'approve', 'command')),
    'delete-review': m.deleteStep(ship, 'review').view,
    'delete-approve': m.deleteStep(ship, 'approve').view,
    'before-join-agent': fill(m.insertBefore(fill(m.runAlongside(ship, 'review', 'agent')), 'approve', 'agent')),
    'delete-join': m.deleteStep(fill(m.insertAfter(fill(m.runAlongside(ship, 'review', 'agent')), 'approve', 'agent')), 'approve').view,
    'agent-decider-loop': agentDecides(),
    'branch-review-decides': m.branch(ship, 'review', m.REVIEW).view,
    'branch-first-step-with-outputs': fill(operatorsCase().out),
    'branch-merge-you-decide': fill(m.branch(ship, 'merge', 'decide-you')),
    'first-step-decision': fill(m.insertAfter(ship, 'start', 'decide')),
    'decision-three-way-agent': threeWay('decide').view,
    'decision-three-way-you': threeWay('decide-you').view,
    'alongside-after-decision': fill(m.runAlongside(fill(m.insertAfter(ship, 'implement', 'decide')), 'review', 'agent')),
    'kind-decision-command': fill(m.changeKind(fill(m.insertAfter(ship, 'implement', 'decide')), 'decision', 'command')),
    'kind-decision-you': m.changeKind(fill(m.insertAfter(ship, 'implement', 'decide')), 'decision', 'you').view,
    'loop-approve-rounds': m.setLoopRounds(ship, 'approve', 'changes', 4).view,
    ...trackCases(),
    ...ruleCases(),
  };
}

// trackCases add reviewer tracks after implement: one, two alongside each
// other, a third from the second's ⫴, a plain step as a track, and a track
// that asks you once its rounds run out.
function trackCases() {
  const one = m.addReviewerTrack(ship, 'implement');
  const two = m.addReviewerTrack(one.view, 'implement');
  return {
    'reviewer-track-one': one.view,
    'reviewer-tracks-two': two.view,
    'reviewer-tracks-three': m.runAlongside(two.view, two.id, m.REVIEWER_TRACK).view,
    'reviewer-tracks-plain-track': fill(m.runAlongside(two.view, two.id, 'agent')),
    'reviewer-track-ask-me': m.setExhaustion(two.view, two.id, 'changes', m.ASK_YOU).view,
    'reviewer-track-ask-me-left': m.setExhaustion(m.setExhaustion(two.view, two.id, 'changes', m.ASK_YOU).view, two.id, 'changes', m.FAIL_RUN).view,
    'reviewer-track-ask-me-then-two': m.addReviewerTrack(m.setExhaustion(one.view, one.id, 'changes', m.ASK_YOU).view, 'implement').view,
    'reviewer-track-fix-deleted': m.deleteStep(two.view, m.reviewLoop(two.view, two.id).fix).view,
  };
}

const edge = (view, from, outcome) => view.edges.find((e) => e.from === from && e.outcome === outcome)?.to;

test('every operation builds exactly the fixture the Go test proves runnable', () => {
  assert.deepEqual(cases(), fixture.cases);
});

test('insert after and before keep the main path connected', () => {
  const after = m.insertAfter(ship, 'implement', 'agent');
  assert.equal(edge(after.view, 'implement', 'next'), after.id);
  assert.equal(edge(after.view, after.id, 'next'), 'review');
  const before = m.insertBefore(ship, 'merge', 'command');
  assert.equal(edge(before.view, 'approve', 'ship'), before.id, 'every edge into the step now enters the new one');
  assert.equal(edge(before.view, before.id, 'next'), 'merge');
  // A branch rejoins at merge, so merge has two ways in; both move to the new step.
  const branched = m.branch(ship, 'approve', 'agent');
  const both = m.insertBefore(branched.view, 'merge', 'command');
  assert.equal(edge(both.view, 'approve', 'ship'), both.id);
  assert.equal(edge(both.view, branched.id, 'next'), both.id);
  assert.equal(m.canInsertBefore(ship, 'start'), 'nothing runs before the start');
  assert.equal(m.canInsertAfter(ship, 'end'), 'nothing runs after an end');
});

test('a "you" step branches on a new answer that rejoins; a task branches through a decision after it', () => {
  const out = m.branch(ship, 'approve', 'agent');
  assert.equal(out.outcome, 'option');
  assert.deepEqual(out.view.template.nodes.approve.performer.choices, ['ship', 'changes', 'hold', 'option']);
  assert.equal(edge(out.view, 'approve', 'option'), out.id);
  assert.equal(edge(out.view, out.id, 'next'), 'merge', 'the branch rejoins the main path');
  // Every step branches now; a task that cannot decide itself offers only a
  // decision after it.
  assert.equal(m.canBranch(ship, 'implement'), '');
  assert.equal(m.canBranch(ship, 'merge'), '');
  assert.deepEqual(m.branchKinds(ship, 'implement'), ['decide', 'decide-you']);
  assert.deepEqual(m.branchKinds(ship, 'merge'), ['decide', 'decide-you']);
  assert.deepEqual(m.branchKinds(ship, 'review'), [m.REVIEW, 'decide', 'decide-you']);
  assert.equal(m.branchKinds(ship, 'approve'), null, 'a decision branches on a new outcome');
  assert.throws(() => m.branch(ship, 'merge', 'agent'), /through a decision/);
  // A command branches through a decision you take.
  const you = m.branch(ship, 'merge', 'decide-you');
  const decision = you.view.template.nodes[you.id];
  assert.equal(decision.type, 'decision');
  assert.deepEqual(decision.performer.choices, ['yes', 'no']);
  assert.equal(edge(you.view, 'merge', 'next'), you.id);
  assert.equal(edge(you.view, you.id, 'yes'), 'end');
  assert.equal(edge(you.view, you.id, 'no'), 'end');
});

test('⑂ on the operator\'s first step with outputs and nobody yet puts an agent decision after it', () => {
  const { first, out } = operatorsCase();
  const node = out.view.template.nodes[first];
  assert.equal(node.type, 'task', 'the step stays a task');
  assert.deepEqual(node.captures, ['banana'], 'with its outputs');
  const decision = out.view.template.nodes[out.id];
  assert.equal(decision.type, 'decision');
  assert.equal(decision.performer.kind, 'agent');
  assert.equal(m.miniKind(decision), 'agent');
  assert.equal(edge(out.view, first, 'next'), out.id);
  assert.equal(edge(out.view, out.id, 'yes'), 'implement');
  assert.equal(edge(out.view, out.id, 'no'), 'implement');
  assert.equal(decision.loops, undefined, 'no answer goes back, so no loop');
  // Who decides is picked on the new decision; it stays a decision.
  const who = m.setDecisionWho(out.view, out.id, { mode: 'you' }).view.template.nodes[out.id];
  assert.equal(who.type, 'decision');
  assert.deepEqual(who.performer.choices, ['yes', 'no']);
});

test('the step after a new decision has one step before it, and its answers draw as one edge', () => {
  const view = m.insertAfter(ship, 'implement', 'decide').view;
  assert.equal(m.canRunAlongside(view, 'review'), '', 'yes and no both lead to review, from one step');
  assert.equal(m.canDecide(view, 'review'), '', 'and its agent can still decide, sending changes back to the decision');
  const along = m.runAlongside(view, 'review', 'agent');
  assert.equal(edge(along.view, 'decision', 'yes'), along.fork);
  assert.equal(edge(along.view, 'decision', 'no'), along.fork);
  const drawn = m.draftProjection(view).edges.filter((e) => e.from === 'decision');
  assert.deepEqual(drawn.map((e) => [e.to, e.label]), [['review', 'yes / no']], 'one arrow, both answers named');
});

test('a decision changes kind: to a command it continues one way, to you it keeps its answers', () => {
  const view = threeWay('decide').view;
  const command = m.changeKind(view, 'decision', 'command').view;
  assert.equal(command.template.nodes.decision.type, 'task');
  assert.deepEqual(command.edges.filter((e) => e.from === 'decision').map((e) => [e.outcome, e.to]), [['next', 'review']]);
  const you = m.changeKind(view, 'decision', 'you').view.template.nodes.decision;
  assert.equal(you.type, 'decision');
  assert.deepEqual(you.performer.choices, ['small', 'large', 'skip']);
});

test('a decision step has named answers: renamed, added to, each with its own next step', () => {
  const { view, id } = threeWay('decide-you');
  assert.deepEqual(view.template.nodes[id].performer.choices, ['small', 'large', 'skip']);
  assert.equal(edge(view, id, 'small'), 'review');
  assert.equal(edge(view, id, 'skip'), 'end');
  const first = m.insertAfter(ship, 'start', 'decide');
  assert.equal(edge(first.view, 'start', 'next'), first.id, 'a decision can be the first step');
  assert.equal(edge(first.view, first.id, 'yes'), 'implement');
});

test('⑂ on an agent step makes its agent decide: approved goes on, changes go back, then the run fails', () => {
  const out = m.branch(ship, 'review', m.REVIEW);
  const review = out.view.template.nodes.review;
  assert.equal(out.id, 'review');
  assert.equal(review.type, 'decision');
  assert.deepEqual(review.performer, ship.template.nodes.review.performer, 'the agent, its prompt and role stay');
  assert.equal(m.miniKind(review), 'agent');
  assert.equal(edge(out.view, 'review', 'approved'), 'approve');
  assert.equal(edge(out.view, 'review', 'changes'), 'implement');
  assert.equal(edge(out.view, 'review', 'next'), undefined, 'the pass edge became approved');
  const giveUp = review.loops.changes.onExhausted;
  assert.equal(review.loops.changes.maxIterations, 3);
  assert.equal(out.view.template.nodes[edge(out.view, 'review', giveUp)].result, 'failed', 'past the last round the run fails');
  // A Who change keeps it deciding.
  const who = m.setDecisionWho(out.view, 'review', { mode: 'anyone' }).view.template.nodes.review;
  assert.equal(who.type, 'decision');
  assert.equal(who.performer.anyone, true);
  assert.deepEqual(Object.keys(who.loops), ['changes']);
});

test('an agent step with outputs, a first step, or a command does not decide', () => {
  const captures = m.updateStep(ship, 'review', (step) => ({ ...step, node: { ...step.node, captures: ['pr'] } })).view;
  assert.match(m.canDecide(captures, 'review'), /clear its outputs/);
  assert.deepEqual(m.branchKinds(captures, 'review'), ['decide', 'decide-you'], 'it branches through a decision after it instead');
  assert.throws(() => m.toAgentDecision(ship, 'implement'), /one step before it/);
  assert.throws(() => m.toAgentDecision(ship, 'merge'), /command/);
  const checks = m.updateStep(ship, 'review', (step) => ({ ...step, node: { ...step.node, checks: [{ run: 'make test' }] } })).view;
  assert.match(m.canDecide(checks, 'review'), /full editor/, 'a compound task keeps its stages');
  const twoWays = { ...ship, edges: [...ship.edges, { from: 'review', outcome: 'other', to: 'merge' }] };
  assert.match(m.canDecide(twoWays, 'review'), /more than one way out/);
});

test('its agent does not decide where going back would overlap another loop', () => {
  // A step after approve: going back to approve would share only approve
  // with approve's own loop back to implement (loop_overlap).
  const after = m.insertAfter(ship, 'approve', 'agent');
  assert.match(m.canDecide(after.view, after.id), /cross another loop/);
  // review → implement nests inside approve's loop: fine.
  assert.equal(m.canDecide(ship, 'review'), '');
});

test('alongside fans out through a parallel node and joins at what came next', () => {
  const out = m.runAlongside(ship, 'review', 'agent');
  assert.equal(out.view.template.nodes[out.fork].type, 'parallel');
  assert.equal(edge(out.view, 'implement', 'next'), out.fork);
  assert.deepEqual(out.view.edges.filter((e) => e.from === out.fork).map((e) => e.to).sort(), [out.id, 'review'].sort());
  assert.equal(out.view.template.nodes.approve.join, 'all');
  assert.match(m.canRunAlongside(ship, 'implement'), /several steps leading/, 'the start and the changes loop both lead to it');
  assert.match(m.canRunAlongside(ship, 'start'), /only a step/);
});

test('delete rewires to the main path and removes the steps only it led to', () => {
  const review = m.deleteStep(ship, 'review');
  assert.equal(edge(review.view, 'implement', 'next'), 'approve');
  assert.deepEqual(review.orphans, []);
  const approve = m.deleteStep(ship, 'approve');
  assert.deepEqual(approve.orphans, ['stopped'], 'the stop end only its answers reached goes too');
  assert.deepEqual(m.deleteOrphans(ship, 'approve'), [], 'an end is not a step to name');
  const branched = m.branch(ship, 'approve', 'agent').view;
  assert.deepEqual(m.deleteOrphans(branched, 'approve'), ['New step'], 'a step only its answers reach is named');
  assert.throws(() => m.deleteStep(ship, 'start'), /start cannot be deleted/);
});

test('change kind: a "you" step is a decision, a task keeps one route', () => {
  const you = m.changeKind(ship, 'implement', 'you').view;
  assert.equal(you.template.nodes.implement.type, 'decision');
  assert.deepEqual(you.template.nodes.implement.performer.choices, ['continue']);
  assert.equal(edge(you, 'implement', 'continue'), 'review');
  assert.equal(you.template.nodes.implement.performer.ask, 'Implement {{params.issue}} on a branch and open a PR. When it comes back for changes, address the notes on the same PR.', 'the text moves along');
  const cmd = m.changeKind(ship, 'approve', 'command');
  assert.equal(cmd.view.template.nodes.approve.type, 'task');
  assert.deepEqual(cmd.view.edges.filter((e) => e.from === 'approve'), [{ from: 'approve', outcome: 'next', to: 'merge' }]);
  assert.deepEqual(cmd.orphans, ['stopped']);
  assert.equal(cmd.view.template.nodes.approve.loops, undefined, 'only a decision bounds a loop');
});

test('outcomes: retarget, add, rename and remove', () => {
  let view = m.setOutcomeTarget(ship, 'approve', 'hold', 'merge').view;
  assert.equal(edge(view, 'approve', 'hold'), 'merge');
  const added = m.addOutcome(view, 'approve', 'end');
  view = added.view;
  assert.equal(added.outcome, 'option');
  view = m.renameOutcome(view, 'approve', 'option', 'skip').view;
  assert.deepEqual(view.template.nodes.approve.performer.choices, ['ship', 'changes', 'hold', 'skip']);
  assert.equal(edge(view, 'approve', 'skip'), 'end');
  assert.throws(() => m.renameOutcome(view, 'approve', 'skip', 'ship'), /already an outcome/);
  view = m.removeOutcome(view, 'approve', 'skip').view;
  assert.deepEqual(view.template.nodes.approve.performer.choices, ['ship', 'changes', 'hold']);
  assert.deepEqual(m.outcomesOf(view, 'approve').map((o) => [o.outcome, o.main]), [['ship', true], ['changes', false], ['hold', false]]);
});

test('the draft projects onto the kit: kinds, who lines, outcome labels', () => {
  const run = m.draftProjection(ship, { whoOf: (id) => (id === 'implement' ? 'any dev' : '') });
  const node = (id) => run.nodes.find((n) => n.id === id);
  assert.deepEqual(node('implement').performer, { kind: 'agent' });
  assert.equal(node('implement').sub, 'any dev');
  assert.equal(node('approve').kind, 'decision');
  assert.equal(node('merge').performer.kind, 'program');
  assert.equal(node('start').kind, 'start');
  assert.equal(run.edges.find((e) => e.from === 'implement').label, '', 'a plain pass is unlabelled');
  assert.equal(run.edges.find((e) => e.from === 'approve' && e.to === 'stopped').label, 'hold');
  assert.deepEqual(m.changeSummary(ship, m.insertAfter(ship, 'implement', 'agent').view), ['added “New step”', 'Implement changed']);
});

test('a gate carries its who as a caption under the diamond; a plain decision does not', () => {
  const whoOf = (id, node) => (node.deciders?.length ? `${node.deciders.length} deciders · all ${node.agree}` : 'you');
  assert.equal(m.draftProjection(ship, { whoOf }).nodes.find((n) => n.id === 'approve').caption, undefined);
  const gated = structuredClone(ship);
  gated.template.nodes.approve.deciders = [{ kind: 'agent', role: 'reviewer', prompt: 'Review.' }, { kind: 'human', ask: 'Approve?' }];
  gated.template.nodes.approve.agree = 'ship';
  assert.equal(m.draftProjection(gated, { whoOf }).nodes.find((n) => n.id === 'approve').caption, '2 deciders · all ship');
});

test('changing a step\'s kind drops a retry the new kind cannot run', () => {
  const retrying = structuredClone(ship);
  retrying.template.nodes.implement.retry = { maxAttempts: 3, onFail: 'feedback-same-session' };
  const asked = m.changeKind(retrying, 'implement', 'you').view;
  assert.equal(asked.template.nodes.implement.retry, undefined, 'a question has no retries');
  const command = m.changeKind(retrying, 'implement', 'command').view;
  assert.deepEqual(command.template.nodes.implement.retry, { maxAttempts: 3 }, 'a command keeps its budget, not a session to hand back');
});

test('a step that retries if it fails projects its retries for the ↻N badge', () => {
  const retrying = structuredClone(ship);
  retrying.template.nodes.merge.retry = { maxAttempts: 3 };
  retrying.template.nodes.implement.retry = { maxAttempts: 1 };
  const run = m.draftProjection(retrying);
  assert.equal(run.nodes.find((n) => n.id === 'merge').retries, 2);
  assert.equal(run.nodes.find((n) => n.id === 'implement').retries, undefined, 'held without retrying: no badge');
  assert.equal(run.nodes.find((n) => n.id === 'review').retries, undefined);
});

test('a bounded loop projects as a back edge carrying its bound', () => {
  const run = m.draftProjection(ship);
  const back = run.edges.find((e) => e.from === 'approve' && e.to === 'implement');
  assert.deepEqual([back.back, back.maxIterations, back.label], [true, 3, 'changes']);
  assert.equal(run.edges.find((e) => e.from === 'implement').back, undefined, 'other edges stay forward');
});

test('a decision answer sent back is a bounded loop; a task only goes forward', () => {
  const back = (view, id, outcome) => m.targetsFor(view, id, outcome).filter((t) => t.back).map((t) => t.id);
  assert.deepEqual(back(forward, 'review', 'next'), [], 'a task is not offered earlier work');
  assert.ok(!m.targetsFor(forward, 'review', 'next').some((t) => t.id === 'implement'));
  assert.deepEqual(back(forward, 'approve', 'changes').sort(), ['implement', 'review'], 'a decision may go back, marked as such');

  let view = m.setOutcomeTarget(forward, 'approve', 'changes', 'implement').view;
  assert.deepEqual(view.template.nodes.approve.loops, { changes: { maxIterations: 3 } }, 'going back gets the default bound');
  view = m.setLoopRounds(view, 'approve', 'changes', 5).view;
  view = m.renameOutcome(view, 'approve', 'changes', 'redo').view;
  assert.deepEqual(view.template.nodes.approve.loops, { redo: { maxIterations: 5 } }, 'a rename keeps the bound');
  const run = m.draftProjection(view);
  assert.deepEqual(run.edges.filter((e) => e.back).map((e) => [e.from, e.to, e.maxIterations]), [['approve', 'implement', 5]]);
  assert.equal(m.setOutcomeTarget(view, 'approve', 'redo', 'merge').view.template.nodes.approve.loops, undefined, 'forward again drops it');
  assert.equal(m.changeKind(view, 'approve', 'agent').view.template.nodes.approve.loops, undefined, 'a task has no loops');
  assert.throws(() => m.setLoopRounds(ship, 'approve', 'ship', 3), /does not go back/);

  const escalating = structuredClone(view);
  escalating.template.nodes.approve.loops.redo.onExhausted = 'hold';
  const renamed = m.renameOutcome(escalating, 'approve', 'hold', 'stop').view;
  assert.equal(renamed.template.nodes.approve.loops.redo.onExhausted, 'stop', 'a rename follows the fallback');
  const removed = m.removeOutcome(renamed, 'approve', 'stop').view;
  assert.deepEqual(removed.template.nodes.approve.loops, { redo: { maxIterations: 5 } }, 'a removed fallback leaves no dangling onExhausted');
});

test('a loop keeps a way on, and its body only leaves through a decision', () => {
  const looped = m.setOutcomeTarget(forward, 'approve', 'changes', 'implement').view;
  const ids = (view, id, outcome) => m.targetsFor(view, id, outcome).map((t) => t.id);
  assert.deepEqual(ids(looped, 'review', 'next'), ['approve'], 'a task in the loop stays in it');
  assert.ok(ids(looped, 'merge', 'next').includes('end'), 'a task after the loop is unaffected');

  const you = m.insertAfter(forward, 'review', 'you');
  assert.ok(!m.targetsFor(you.view, you.id, 'continue').some((t) => t.back), 'a lone answer cannot loop: the loop would have no way on');
  assert.equal(m.canRemoveOutcome(looped, 'approve', 'ship'), '', 'hold still leads on');
  const onlyOut = m.removeOutcome(looped, 'approve', 'hold').view;
  assert.match(m.canRemoveOutcome(onlyOut, 'approve', 'ship'), /does not loop back/);
  assert.equal(m.canRemoveOutcome(onlyOut, 'approve', 'changes'), '');
  assert.equal(m.mainEdge(m.setOutcomeTarget(forward, 'approve', 'ship', 'implement').view, 'approve').outcome, 'changes',
    'the main path is a decision\'s first answer that does not loop');
});

test('alongside is offered only where the fork can close or widen; joins move with the paths that meet', () => {
  assert.match(m.canRunAlongside(ship, 'approve'), /other outcomes/, 'its other answers would leave the branch');
  const fork = fill(m.runAlongside(ship, 'review', 'agent'));
  assert.equal(m.canRunAlongside(fork, 'review'), '', 'a track of a fork gets another track on the same fork');
  const third = m.runAlongside(fork, 'review', 'agent');
  const forks = (v) => Object.values(v.template.nodes).filter((n) => n.type === 'parallel').length;
  assert.equal(forks(third.view), 1, 'no second fork');
  assert.equal(third.fork, Object.keys(fork.template.nodes).find((id) => fork.template.nodes[id].type === 'parallel'));
  assert.ok(third.view.edges.some((e) => e.from === third.id && e.to === 'approve'), 'it joins where the others do');
  const longer = m.insertAfter(fork, 'review', 'agent');
  assert.match(m.canRunAlongside(longer.view, longer.id), /several paths meet/, 'the last step of a longer branch cannot fork again at its join');
  const before = m.insertBefore(fork, 'approve', 'agent');
  assert.equal(before.view.template.nodes[before.id].join, 'all', 'the new step is where the paths meet now');
  assert.equal(before.view.template.nodes.approve.join, undefined);
  const deleted = m.deleteStep(fill(m.insertAfter(fork, 'approve', 'agent')), 'approve').view;
  assert.equal(deleted.template.nodes['new-step-2'].join, 'all', 'deleting the join hands it on');
  assert.match(m.canDelete(ship, 'end'), /steps still end here/);
  assert.throws(() => m.deleteStep(ship, 'end'), /steps still end here/);
});

test('a decision has no sub line, so its fallback marks its label', () => {
  const view = {
    template: { nodes: {
      ask: { type: 'decision', name: 'Review', performer: { kind: 'agent', role: 'qa' } },
      work: { type: 'task', name: 'Work', performer: { kind: 'agent', role: 'dev' } },
    } },
    edges: [],
  };
  const { nodes } = m.draftProjection(view, { noteOf: () => '↯ fail' });
  assert.equal(nodes.find((node) => node.id === 'ask').label, 'Review ↯ fail');
  assert.equal(nodes.find((node) => node.id === 'work').label, 'Work', 'a task shows it on its sub line');
});

test('another decider makes a decision a gate; one left makes it plain again', () => {
  assert.equal(m.canAddDecider(ship, 'merge'), 'only a decision is decided by several');
  const { view: gate } = m.addDecider(ship, 'approve', { mode: 'role', role: 'reviewer' });
  const node = gate.template.nodes.approve;
  assert.equal(m.miniKind(node), 'gate');
  assert.equal(node.performer, undefined);
  assert.equal(node.agree, 'ship', 'an approving outcome is what all must choose');
  assert.deepEqual(node.deciders.map(m.deciderWho), [{ mode: 'you' }, { mode: 'role', role: 'reviewer' }]);
  assert.equal(node.deciders[0].choices, undefined, 'a gate votes with its outcomes');
  assert.equal(node.deciders[1].prompt, node.deciders[0].ask, 'the new decider is asked the same');
  assert.equal(node.loops.changes.onExhausted, 'hold');
  assert.throws(() => m.changeKind(gate, 'approve', 'agent'));

  const { view: plain } = m.removeDecider(gate, 'approve', 1);
  const back = plain.template.nodes.approve;
  assert.equal(back.deciders, undefined);
  assert.equal(back.agree, undefined);
  assert.deepEqual(back.performer.choices, ['ship', 'changes', 'hold'], 'a person deciding alone answers with the outcomes');
});

test('a gate keeps its deciders, text and agree in step', () => {
  let { view } = m.addDecider(ship, 'approve', { mode: 'role', role: 'reviewer' });
  ({ view } = m.setDecider(view, 'approve', 1, { mode: 'lead' }));
  assert.deepEqual(m.deciderWho(view.template.nodes.approve.deciders[1]), { mode: 'lead' });
  ({ view } = m.setGateText(view, 'approve', 'Is it good?'));
  const [person, agent] = view.template.nodes.approve.deciders;
  assert.equal(person.ask, 'Is it good?');
  assert.equal(agent.prompt, 'Is it good?');
  ({ view } = m.renameOutcome(view, 'approve', 'ship', 'go'));
  assert.equal(view.template.nodes.approve.agree, 'go', 'renaming the agreed outcome follows it');
  ({ view } = m.setAgree(view, 'approve', 'hold'));
  assert.equal(view.template.nodes.approve.agree, 'hold');
  ({ view } = m.removeOutcome(view, 'approve', 'hold'));
  assert.notEqual(view.template.nodes.approve.agree, 'hold', 'removing the agreed outcome picks another');
  for (let i = 2; i < 8; i++) ({ view } = m.addDecider(view, 'approve', { mode: 'anyone' }));
  assert.match(m.canAddDecider(view, 'approve'), /at most 8/);
});

test('a gate stays valid as its outcomes change', () => {
  let { view } = m.addDecider(ship, 'approve', { mode: 'role', role: 'reviewer' });
  ({ view } = m.addOutcome(view, 'approve', 'merge'));
  const other = m.outcomesOf(view, 'approve').map((o) => o.outcome).find((o) => !['ship', 'changes', 'hold'].includes(o));
  ({ view } = m.setOutcomeTarget(view, 'approve', other, 'implement'));
  const gate = view.template.nodes.approve;
  assert.ok(gate.loops[other]?.onExhausted, 'a new loop on a gate gets its way out');
  assert.notEqual(gate.loops[other].onExhausted, gate.agree);
  ({ view } = m.setOutcomeTarget(view, 'approve', 'ship', 'implement'));
  const after = view.template.nodes.approve;
  assert.ok(!after.loops?.[after.agree], 'what all must choose never goes back');
  // A gate keeps two outcomes (no loop here, so no give-up is added).
  let small = m.addDecider(forward, 'approve', { mode: 'anyone' }).view;
  small = m.removeOutcome(small, 'approve', 'hold').view;
  assert.match(m.canRemoveOutcome(small, 'approve', 'ship') || m.canRemoveOutcome(small, 'approve', 'changes'), /gate needs/);
});

test('outputs belong to one task: not kept by a "you" step or a duplicate', async () => {
  const { setStepOutputs } = await import('../dashboard/js/process-step-list.js');
  const withOut = m.updateStep(ship, 'review', (s) => setStepOutputs(s, 'notes')).view;
  assert.deepEqual(withOut.template.nodes.review.captures, ['notes']);
  assert.equal(m.changeKind(withOut, 'review', 'you').view.template.nodes.review.captures, undefined);
  const dup = m.duplicate(withOut, 'review');
  assert.equal(dup.view.template.nodes[dup.id].captures, undefined);
  assert.deepEqual(dup.view.template.nodes.review.captures, ['notes']);
});

test('a used-up gate loop never goes on through what all must choose: it gives up', () => {
  // approve with only ship and changes ↺: the agree outcome is the one way on.
  const plain = m.removeOutcome(ship, 'approve', 'hold').view;
  const { view } = m.addDecider(plain, 'approve', { mode: 'role', role: 'reviewer' });
  const gate = view.template.nodes.approve;
  assert.equal(gate.agree, 'ship');
  assert.notEqual(gate.loops.changes.onExhausted, 'ship', 'three rejections must not ship');
  assert.equal(gate.loops.changes.onExhausted, m.GIVE_UP);
  const end = view.template.nodes[edge(view, 'approve', m.GIVE_UP)];
  assert.deepEqual([end.type, end.result], ['end', 'failed']);
  assert.deepEqual(m.exhaustionOptions(view, 'approve', 'changes').map((o) => o.label), ['fail the run', 'ask me']);
  assert.match(m.canRemoveOutcome(view, 'approve', m.GIVE_UP), /runs out of rounds/, 'its way out stays while it is chosen');
  const loop = m.draftProjection(view).edges.find((e) => e.from === 'approve' && e.outcome === undefined && e.back);
  assert.equal(loop?.exhausted, 'fail', 'the loop edge says where it goes');
});

test('when rounds run out: pick another way, fail the run, or (a plain decision) ask again', () => {
  const options = m.exhaustionOptions(ship, 'approve', 'changes');
  assert.deepEqual(options.map((o) => o.value), ['', 'ship', 'hold', m.FAIL_RUN]);
  let { view } = m.setExhaustion(ship, 'approve', 'changes', 'hold');
  assert.equal(view.template.nodes.approve.loops.changes.onExhausted, 'hold');
  ({ view } = m.setExhaustion(view, 'approve', 'changes', m.FAIL_RUN));
  assert.equal(view.template.nodes.approve.loops.changes.onExhausted, m.GIVE_UP);
  assert.ok(view.template.nodes.approve.performer.choices.includes(m.GIVE_UP), 'a person can answer it too');
  ({ view } = m.setExhaustion(view, 'approve', 'changes', ''));
  assert.deepEqual(view.template.nodes.approve.loops.changes, { maxIterations: 3 });
  const gate = m.addDecider(ship, 'approve', { mode: 'anyone' }).view;
  assert.throws(() => m.setExhaustion(gate, 'approve', 'changes', ''), /needs somewhere to go/);
  // A template that already exhausts to agree shows it, marked.
  const stored = structuredClone(gate);
  stored.template.nodes.approve.loops.changes.onExhausted = 'ship';
  assert.ok(m.exhaustionOptions(stored, 'approve', 'changes').find((o) => o.value === 'ship')?.skipsReview);
  assert.ok(!m.exhaustionOptions(gate, 'approve', 'changes').some((o) => o.value === 'ship'), 'never offered otherwise');
});

test('an agent deciding a decision keeps it a decision: outcomes and loop stay', () => {
  const { view } = m.setDecisionWho(ship, 'approve', { mode: 'role', role: 'reviewer' });
  const node = view.template.nodes.approve;
  assert.equal(node.type, 'decision');
  assert.equal(node.performer.kind, 'agent');
  assert.equal(node.performer.role, 'reviewer');
  assert.equal(node.performer.choices, undefined, 'an agent reports one of the outcomes');
  assert.deepEqual(m.outcomesOf(view, 'approve').map((o) => o.outcome), m.outcomesOf(ship, 'approve').map((o) => o.outcome));
  assert.equal(node.loops.changes.maxIterations, ship.template.nodes.approve.loops.changes.maxIterations);
  assert.equal(node.loops.changes.onExhausted, 'hold', 'an agent asked again could only pass it on: it goes somewhere safe');
  const plain = m.removeOutcome(ship, 'approve', 'hold').view;
  const agent = m.setDecisionWho(plain, 'approve', { mode: 'anyone' }).view;
  assert.equal(agent.template.nodes.approve.loops.changes.onExhausted, m.GIVE_UP, 'never ship');
  assert.ok(!m.exhaustionOptions(agent, 'approve', 'changes').some((o) => o.value === '' || o.value === 'ship'));
  const { view: back } = m.setDecisionWho(view, 'approve', { mode: 'you' });
  assert.deepEqual(back.template.nodes.approve.performer.choices, ['ship', 'changes', 'hold']);
  assert.throws(() => m.setDecisionWho(ship, 'approve', { mode: 'command' }));
});

test('a "give-up" outcome that no longer fails is not mistaken for failing the run', () => {
  let { view } = m.setExhaustion(ship, 'approve', 'changes', m.FAIL_RUN);
  ({ view } = m.setOutcomeTarget(view, 'approve', m.GIVE_UP, 'merge'));
  const options = m.exhaustionOptions(view, 'approve', 'changes');
  assert.equal(new Set(options.map((o) => o.value)).size, options.length, 'no duplicate option values');
  ({ view } = m.setExhaustion(view, 'approve', 'changes', m.FAIL_RUN));
  const way = view.template.nodes.approve.loops.changes.onExhausted;
  assert.equal(view.template.nodes[edge(view, 'approve', way)].result, 'failed', 'fail the run goes to a failed end');
});

test('an agent decision the operator did not touch keeps its behaviour through other edits', () => {
  const agent = structuredClone(m.setDecisionWho(ship, 'approve', { mode: 'anyone' }).view);
  delete agent.template.nodes.approve.loops.changes.onExhausted; // as a stored template may have it
  const { view } = m.updateStep(agent, 'implement', (step) => setStepText(step, 'Do it differently.'));
  assert.equal(view.template.nodes.approve.loops.changes.onExhausted, undefined);
  assert.equal(edge(view, 'approve', m.GIVE_UP), undefined, 'no give-up appears');
});

test('inside a parallel branch "fail the run" is offered: a failed end there ends the run', () => {
  // A decision on one branch of review ⫴ new step, looping back to review.
  let { view } = m.runAlongside(ship, 'review', 'agent');
  const branch = Object.keys(view.template.nodes).find((id) => !ship.template.nodes[id] && view.template.nodes[id].type !== 'parallel');
  ({ view } = m.changeKind(view, branch, 'you'));
  assert.ok(m.inParallelBranch(view, branch));
  assert.ok(!m.inParallelBranch(view, 'implement'));
  assert.ok(m.exhaustionOptions(view, branch, 'continue').some((o) => o.value === m.FAIL_RUN));
});

test('a gate inside a parallel branch gives up into a failed end, never its agree', () => {
  const fan = steps['plan-fan-out-merge'];
  const part = Object.keys(fan.template.nodes).find((id) => /part/.test(id));
  let { view, id } = m.insertAfter(fan, part, 'you');
  ({ view } = m.addOutcome(view, id, edge(view, id, 'continue')));
  const redo = m.outcomesOf(view, id).map((o) => o.outcome).find((o) => o !== 'continue');
  ({ view } = m.setOutcomeTarget(view, id, redo, part));
  assert.ok(m.inParallelBranch(view, id), 'the fixture: a decision on a branch');
  ({ view } = m.addDecider(view, id, { mode: 'anyone' }));
  const node = view.template.nodes[id];
  const way = node.loops[redo].onExhausted;
  assert.notEqual(way, node.agree);
  const to = view.edges.find((e) => e.from === id && e.outcome === way).to;
  assert.equal(view.template.nodes[to].result, 'failed', 'a used-up gate fails the run');
});

test('a reviewer track reviews a step\'s work in a loop that starts at the review', () => {
  const { view, id } = m.addReviewerTrack(ship, 'implement');
  const loop = m.reviewLoop(view, id);
  assert.equal(loop.outcome, 'changes');
  assert.equal(edge(view, 'implement', 'next'), id, 'the review comes right after the work');
  assert.equal(edge(view, id, 'approved'), 'review', 'approved goes on to what came next');
  assert.equal(edge(view, loop.fix, 'next'), id, 'the fix goes back to the review');
  assert.equal(view.template.nodes[loop.fix].performer.sameAs, 'implement', 'fixes go to the implementer');
  const bound = view.template.nodes[id].loops.changes;
  assert.equal(view.template.nodes[edge(view, id, bound.onExhausted)].result, 'failed', 'a used-up track fails the run');
});

test('a second reviewer track runs alongside the first; a third joins the same fork', () => {
  const one = m.addReviewerTrack(ship, 'implement');
  const two = m.addReviewerTrack(one.view, 'implement');
  const fork = edge(two.view, 'implement', 'next');
  assert.equal(two.view.template.nodes[fork].type, 'parallel');
  assert.deepEqual(two.view.edges.filter((e) => e.from === fork).map((e) => e.to).sort(), [one.id, two.id].sort());
  assert.equal(two.view.template.nodes.review.join, 'all', 'both tracks join where the review led on');
  assert.equal(m.canAddReviewerTrack(two.view, two.id), '');
  const three = m.runAlongside(two.view, two.id, m.REVIEWER_TRACK);
  assert.equal(edge(three.view, 'implement', 'next'), fork, 'no second fork');
  assert.equal(three.view.edges.filter((e) => e.from === fork).length, 3);
  assert.equal(edge(three.view, three.id, 'approved'), 'review');
  assert.match(three.view.template.nodes[three.id].name, /Review 3/);
});

test('a used-up agent track can ask you: count it as approved, or fail the run', () => {
  const two = m.addReviewerTrack(m.addReviewerTrack(ship, 'implement').view, 'implement');
  const options = m.exhaustionOptions(two.view, two.id, 'changes');
  assert.deepEqual(options.map((o) => o.label), ['fail the run', 'ask me']);
  const { view } = m.setExhaustion(two.view, two.id, 'changes', m.ASK_YOU);
  const way = view.template.nodes[two.id].loops.changes.onExhausted;
  const ask = edge(view, two.id, way);
  assert.equal(view.template.nodes[ask].performer.kind, 'human');
  assert.equal(edge(view, ask, 'count as approved'), 'review', 'counted as approved, it joins the others');
  assert.equal(view.template.nodes[edge(view, ask, 'fail the run')].result, 'failed');
  assert.equal(m.mainEdge(view, two.id).outcome, 'approved', 'asking you is not the review\'s way on');
  assert.deepEqual(m.exhaustionOptions(view, two.id, 'changes').map((o) => [o.label, o.value === way]),
    [['fail the run', false], ['ask me', true]]);
});

const outs = (view, id) => view.edges.filter((e) => e.from === id);

test('leaving "ask me" removes its step; choosing it again adds one', () => {
  const two = m.addReviewerTrack(m.addReviewerTrack(ship, 'implement').view, 'implement');
  const asked = m.setExhaustion(two.view, two.id, 'changes', m.ASK_YOU).view;
  const failing = m.setExhaustion(asked, two.id, 'changes', m.FAIL_RUN).view;
  assert.deepEqual(outs(failing, two.id).map((e) => e.outcome).sort(), ['approved', 'changes', 'give-up'], 'no ask-you outcome is left');
  const humans = (view) => Object.values(view.template.nodes).filter((node) => node.performer?.kind === 'human').length;
  assert.equal(humans(failing), humans(two.view), 'the ask step is gone');
  assert.deepEqual(m.exhaustionOptions(failing, two.id, 'changes').map((o) => o.label), ['fail the run', 'ask me']);
  assert.equal(m.canRunAlongside(failing, two.id), '');
  const again = m.setExhaustion(failing, two.id, 'changes', m.ASK_YOU).view;
  assert.equal(humans(again), humans(two.view) + 1, 'one ask step');
});

test('a second reviewer track can follow "ask me" on the first', () => {
  const one = m.addReviewerTrack(ship, 'implement');
  const asked = m.setExhaustion(one.view, one.id, 'changes', m.ASK_YOU).view;
  assert.equal(m.canAddReviewerTrack(asked, 'implement'), '');
  const two = m.addReviewerTrack(asked, 'implement');
  assert.equal(two.view.template.nodes[m.mainEdge(two.view, one.id).to].join, 'all');
});

test('deleting a track\'s fix drops its review\'s loop too', () => {
  const one = m.addReviewerTrack(ship, 'implement');
  const fix = m.reviewLoop(one.view, one.id).fix;
  const { view } = m.deleteStep(one.view, fix);
  assert.ok(!outs(view, one.id).some((e) => e.to === one.id), 'the review does not loop onto itself');
  assert.ok(!outs(view, one.id).some((e) => e.outcome === 'changes'));
});

test('alongside on a lone review adds a second reviewer track', () => {
  const one = m.addReviewerTrack(ship, 'implement');
  assert.equal(m.canRunAlongside(one.view, one.id), '');
  const two = m.runAlongside(one.view, one.id, 'agent');
  assert.equal(Object.keys(two.view.template.nodes).filter((id) => m.reviewLoop(two.view, id)).length, 2);
});

test('a decision decided by a rule: cases on what ran before, then else', () => {
  const { view } = m.setDecisionWho(ship, 'approve', { mode: 'rule' });
  const rule = view.template.nodes.approve.performer;
  assert.deepEqual(rule, { kind: 'rule', cases: [{ when: 'steps.implement.result == succeeded', then: 'ship' }], else: 'hold' });
  assert.equal(m.miniKind(view.template.nodes.approve), 'rule');
  assert.equal(view.template.nodes.approve.loops.changes.onExhausted, 'hold', 'its loop keeps a way out');
  assert.deepEqual(m.ruleOperands(view, 'approve').map((o) => o.value),
    ['steps.implement.result', 'steps.review.result', 'params.issue'], 'the steps before it and the params');
  const renamed = m.renameOutcome(view, 'approve', 'ship', 'merge').view;
  assert.equal(renamed.template.nodes.approve.performer.cases[0].then, 'merge', 'a renamed outcome follows into the rule');
  assert.throws(() => m.setRule(view, 'approve', { cases: [{ when: 'params.issue == x', then: 'nope' }], else: 'hold' }), /not an outcome/);
  const agent = m.setDecisionWho(view, 'approve', { mode: 'role', role: 'dev' }).view;
  assert.equal(agent.template.nodes.approve.performer.kind, 'agent');
  assert.equal(agent.template.nodes.approve.performer.cases, undefined, 'the cases go with the rule');
});

test('a rule keeps a case: the outcome every case picks cannot be removed', () => {
  const { view } = m.setDecisionWho(ship, 'approve', { mode: 'rule' });
  assert.match(m.canRemoveOutcome(view, 'approve', 'ship'), /every case of the rule picks it/);
  const two = m.setRule(view, 'approve', { cases: [{ when: 'steps.review.result == failed', then: 'changes' }, { when: 'steps.implement.result == succeeded', then: 'ship' }], else: 'hold' }).view;
  assert.equal(m.canRemoveOutcome(two, 'approve', 'ship'), '');
  assert.equal(m.removeOutcome(two, 'approve', 'ship').view.template.nodes.approve.performer.cases.length, 1);
});
