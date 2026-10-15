// Tests for the inline mini editor's graph operations (mock M1, tcl-jqmrj5):
// each operation on a quick-start starter builds exactly the fixture
// TestMiniEditCasesAreRunnable proves valid and executable, and the
// operations keep the shapes the runtime needs.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as m from '../dashboard/js/process-mini-edit.js';
import { setStepText, stepListToView } from '../dashboard/js/process-step-list.js';
import { STARTERS } from '../dashboard/js/process-quickstart.js';

const fixture = JSON.parse(readFileSync(new URL('./process-mini-edit-cases.json', import.meta.url), 'utf8'));
const roster = [{ name: 'checkout', id: 'a1', role: 'dev', online: true }, { name: 'review-bot', id: 'a2', role: 'reviewer', online: true }];
const ship = stepListToView(STARTERS[0].build(roster).list);
const check = stepListToView(STARTERS[2].build(roster).list);

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
  assert.equal(edge(before.view, 'final-say', 'ship'), before.id);
  assert.equal(edge(before.view, before.id, 'next'), 'merge');
  assert.equal(m.canInsertBefore(ship, 'start'), 'nothing runs before the start');
  assert.equal(m.canInsertAfter(ship, 'end'), 'nothing runs after an end');
});

test('a "you" step branches on a new answer that rejoins; a task does not branch', () => {
  const out = m.branch(ship, 'approve', 'agent');
  assert.equal(out.outcome, 'option');
  assert.deepEqual(out.view.template.nodes.approve.performer.choices, ['ship', 'rework', 'hold', 'option']);
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
  assert.match(m.canRunAlongside(ship, 'merge'), /several steps leading/);
  assert.match(m.canRunAlongside(ship, 'start'), /only a step/);
});

test('delete rewires to the main path and removes the steps only it led to', () => {
  const review = m.deleteStep(ship, 'review');
  assert.equal(edge(review.view, 'implement', 'next'), 'approve');
  assert.deepEqual(review.orphans, []);
  const approve = m.deleteStep(ship, 'approve');
  assert.deepEqual(approve.orphans.sort(), ['final-say', 'rework', 'stopped'], 'the stop end only its answers reached goes too');
  assert.deepEqual(m.deleteOrphans(ship, 'approve').sort(), ['Final say', 'Rework']);
  assert.throws(() => m.deleteStep(ship, 'start'), /start cannot be deleted/);
});

test('change kind: a "you" step is a decision, a task keeps one route', () => {
  const you = m.changeKind(ship, 'implement', 'you').view;
  assert.equal(you.template.nodes.implement.type, 'decision');
  assert.deepEqual(you.template.nodes.implement.performer.choices, ['continue']);
  assert.equal(edge(you, 'implement', 'continue'), 'review');
  assert.equal(you.template.nodes.implement.performer.ask, 'Implement {{params.issue}} on a branch and open a PR.', 'the text moves along');
  const cmd = m.changeKind(ship, 'approve', 'command');
  assert.equal(cmd.view.template.nodes.approve.type, 'task');
  assert.deepEqual(cmd.view.edges.filter((e) => e.from === 'approve'), [{ from: 'approve', outcome: 'next', to: 'merge' }]);
  assert.deepEqual(cmd.orphans.sort(), ['final-say', 'rework', 'stopped']);
});

test('outcomes: retarget, add, rename and remove', () => {
  let view = m.setOutcomeTarget(ship, 'approve', 'hold', 'merge').view;
  assert.equal(edge(view, 'approve', 'hold'), 'merge');
  const added = m.addOutcome(view, 'approve', 'end');
  view = added.view;
  assert.equal(added.outcome, 'option');
  view = m.renameOutcome(view, 'approve', 'option', 'skip').view;
  assert.deepEqual(view.template.nodes.approve.performer.choices, ['ship', 'rework', 'hold', 'skip']);
  assert.equal(edge(view, 'approve', 'skip'), 'end');
  assert.throws(() => m.renameOutcome(view, 'approve', 'skip', 'ship'), /already an outcome/);
  view = m.removeOutcome(view, 'approve', 'skip').view;
  assert.deepEqual(view.template.nodes.approve.performer.choices, ['ship', 'rework', 'hold']);
  assert.deepEqual(m.outcomesOf(view, 'approve').map((o) => [o.outcome, o.main]), [['ship', true], ['rework', false], ['hold', false]]);
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
  assert.equal(run.edges.find((e) => e.from === 'approve' && e.to === 'rework').label, 'rework');
  assert.deepEqual(m.changeSummary(ship, m.insertAfter(ship, 'implement', 'agent').view), ['added “New step”', 'Implement changed']);
});
