// Tests for the inline mini editor's graph operations (mock M1, tcl-jqmrj5):
// each operation on a quick-start starter builds exactly the fixture
// TestMiniEditCasesAreRunnable proves valid and executable, and the
// operations keep the shapes the runtime needs.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as m from '../dashboard/js/process-mini-edit.js';
import { AGENT_VERDICTS, replaceStep, setStepChoices, setStepLoop, setStepText, stepListToView } from '../dashboard/js/process-step-list.js';
import { STARTERS } from '../dashboard/js/process-quickstart.js';

const fixture = JSON.parse(readFileSync(new URL('./process-mini-edit-cases.json', import.meta.url), 'utf8'));
const roster = [{ name: 'checkout', id: 'a1', role: 'dev', online: true }, { name: 'review-bot', id: 'a2', role: 'reviewer', online: true }];
const ship = stepListToView(STARTERS[0].build(roster).list);
const check = stepListToView(STARTERS[2].build(roster).list);

// agentDecides makes the starter's review an agent decider that sends
// changes back to implement, the way the step list's "◇ agent decides" does.
function agentDecides() {
  const list = STARTERS[0].build(roster).list;
  const review = list.steps.find((step) => step.id === 'review');
  let step = setStepChoices(review, [...AGENT_VERDICTS], 'approve');
  step = { ...step, links: step.links.map((link) => (link.outcome === 'changes' ? { ...link, to: 'implement' } : link)) };
  return stepListToView(replaceStep(list, setStepLoop(step, 'changes', { maxIterations: 3, onExhausted: '' })));
}

// fill writes the text an operator types into the new step.
const fill = ({ view, id }) => m.updateStep(view, id, (step) => setStepText(step, step.node.performer.kind === 'program' ? 'make test' : 'Do the thing.')).view;

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
    'loop-final-say-hold': m.setLoopRounds(m.setOutcomeTarget(ship, 'final-say', 'hold', 'implement').view, 'final-say', 'hold', 4).view,
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

test('a "you" step branches on a new answer that rejoins; a task does not branch', () => {
  const out = m.branch(ship, 'approve', 'agent');
  assert.equal(out.outcome, 'option');
  assert.deepEqual(out.view.template.nodes.approve.performer.choices, ['ship', 'changes', 'hold', 'option']);
  assert.equal(edge(out.view, 'approve', 'option'), out.id);
  assert.equal(edge(out.view, out.id, 'next'), 'merge', 'the branch rejoins the main path');
  assert.match(m.canBranch(ship, 'implement'), /continues one way/);
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

test('a bounded loop projects as a back edge carrying its bound', () => {
  const run = m.draftProjection(ship);
  const back = run.edges.find((e) => e.from === 'approve' && e.to === 'implement');
  assert.deepEqual([back.back, back.maxIterations, back.label], [true, 3, 'changes']);
  assert.equal(run.edges.find((e) => e.from === 'implement').back, undefined, 'other edges stay forward');
});

test('a decision answer sent back is a bounded loop; a task only goes forward', () => {
  const back = (view, id, outcome) => m.targetsFor(view, id, outcome).filter((t) => t.back).map((t) => t.id);
  assert.deepEqual(back(ship, 'rework', 'next'), [], 'a task is not offered earlier work');
  assert.ok(!m.targetsFor(ship, 'rework', 'next').some((t) => t.id === 'implement'));
  assert.deepEqual(back(ship, 'approve', 'rework').sort(), ['implement', 'review'], 'a decision may go back, marked as such');

  let view = m.setOutcomeTarget(ship, 'approve', 'rework', 'implement').view;
  assert.deepEqual(view.template.nodes.approve.loops, { rework: { maxIterations: 3 } }, 'going back gets the default bound');
  view = m.setLoopRounds(view, 'approve', 'rework', 5).view;
  view = m.renameOutcome(view, 'approve', 'rework', 'changes').view;
  assert.deepEqual(view.template.nodes.approve.loops, { changes: { maxIterations: 5 } }, 'a rename keeps the bound');
  const run = m.draftProjection(view);
  assert.deepEqual(run.edges.filter((e) => e.back).map((e) => [e.from, e.to, e.maxIterations]), [['approve', 'implement', 5]]);
  assert.equal(m.setOutcomeTarget(view, 'approve', 'changes', 'merge').view.template.nodes.approve.loops, undefined, 'forward again drops it');
  assert.equal(m.changeKind(view, 'approve', 'agent').view.template.nodes.approve.loops, undefined, 'a task has no loops');
  assert.throws(() => m.setLoopRounds(ship, 'approve', 'ship', 3), /does not go back/);

  const escalating = structuredClone(view);
  escalating.template.nodes.approve.loops.changes.onExhausted = 'hold';
  const renamed = m.renameOutcome(escalating, 'approve', 'hold', 'stop').view;
  assert.equal(renamed.template.nodes.approve.loops.changes.onExhausted, 'stop', 'a rename follows the fallback');
  const removed = m.removeOutcome(renamed, 'approve', 'stop').view;
  assert.deepEqual(removed.template.nodes.approve.loops, { changes: { maxIterations: 5 } }, 'a removed fallback leaves no dangling onExhausted');
});

test('a loop keeps a way on, and its body only leaves through a decision', () => {
  const looped = m.setOutcomeTarget(ship, 'approve', 'rework', 'implement').view;
  const ids = (view, id, outcome) => m.targetsFor(view, id, outcome).map((t) => t.id);
  assert.deepEqual(ids(looped, 'review', 'next'), ['approve'], 'a task in the loop stays in it');
  assert.ok(ids(looped, 'merge', 'next').includes('end'), 'a task after the loop is unaffected');

  const you = m.insertAfter(ship, 'review', 'you');
  assert.ok(!m.targetsFor(you.view, you.id, 'continue').some((t) => t.back), 'a lone answer cannot loop: the loop would have no way on');
  assert.equal(m.canRemoveOutcome(looped, 'approve', 'ship'), '', 'hold still leads on');
  const onlyOut = m.removeOutcome(looped, 'approve', 'hold').view;
  assert.match(m.canRemoveOutcome(onlyOut, 'approve', 'ship'), /does not loop back/);
  assert.equal(m.canRemoveOutcome(onlyOut, 'approve', 'rework'), '');
  assert.equal(m.mainEdge(m.setOutcomeTarget(ship, 'approve', 'ship', 'implement').view, 'approve').outcome, 'rework',
    'the main path is a decision\'s first answer that does not loop');
});

test('alongside is offered only where the fork can close; joins move with the paths that meet', () => {
  assert.match(m.canRunAlongside(ship, 'approve'), /other outcomes/, 'its other answers would leave the branch');
  const fork = fill(m.runAlongside(ship, 'review', 'agent'));
  assert.match(m.canRunAlongside(fork, 'review'), /several paths meet/, 'a branch of an open fork cannot fork again at its join');
  const before = m.insertBefore(fork, 'approve', 'agent');
  assert.equal(before.view.template.nodes[before.id].join, 'all', 'the new step is where the paths meet now');
  assert.equal(before.view.template.nodes.approve.join, undefined);
  const deleted = m.deleteStep(fill(m.insertAfter(fork, 'approve', 'agent')), 'approve').view;
  assert.equal(deleted.template.nodes['new-step-2'].join, 'all', 'deleting the join hands it on');
  assert.match(m.canDelete(ship, 'end'), /steps still end here/);
  assert.throws(() => m.deleteStep(ship, 'end'), /steps still end here/);
});
