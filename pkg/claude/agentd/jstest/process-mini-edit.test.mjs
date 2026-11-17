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
    'branch-review-decides': m.branch(ship, 'review').view,
    'loop-approve-rounds': m.setLoopRounds(ship, 'approve', 'changes', 4).view,
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
  assert.equal(m.canBranch(ship, 'implement'), 'it needs exactly one step before it to send changes back to');
  assert.match(m.canBranch(ship, 'merge'), /a command continues one way/);
});

test('⑂ on an agent step makes its agent decide: approved goes on, changes go back, then the run fails', () => {
  const out = m.branch(ship, 'review');
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
  assert.match(m.canBranch(captures, 'review'), /clear its outputs/);
  assert.throws(() => m.toAgentDecision(ship, 'implement'), /one step before it/);
  assert.throws(() => m.toAgentDecision(ship, 'merge'), /command/);
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
  assert.deepEqual(m.exhaustionOptions(view, 'approve', 'changes').map((o) => o.label), ['fail the run']);
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

test('inside a parallel branch no end is added: the server refuses an end there', () => {
  // A decision on one branch of review ⫴ new step, looping back to review.
  let { view } = m.runAlongside(ship, 'review', 'agent');
  const branch = Object.keys(view.template.nodes).find((id) => !ship.template.nodes[id] && view.template.nodes[id].type !== 'parallel');
  ({ view } = m.changeKind(view, branch, 'you'));
  assert.ok(m.inParallelBranch(view, branch));
  assert.ok(!m.inParallelBranch(view, 'implement'));
  assert.ok(!m.exhaustionOptions(view, branch, 'continue').some((o) => o.value === m.FAIL_RUN));
});

test('a gate inside a parallel branch keeps its way out on the branch', () => {
  const fan = steps['plan-fan-out-merge'];
  const part = Object.keys(fan.template.nodes).find((id) => /part/.test(id));
  let { view, id } = m.insertAfter(fan, part, 'you');
  ({ view } = m.addOutcome(view, id, edge(view, id, 'continue')));
  const redo = m.outcomesOf(view, id).map((o) => o.outcome).find((o) => o !== 'continue');
  ({ view } = m.setOutcomeTarget(view, id, redo, part));
  assert.ok(m.inParallelBranch(view, id), 'the fixture: a decision on a branch');
  const ends = (v) => Object.values(v.template.nodes).filter((n) => n.type === 'end').length;
  const before = ends(view);
  ({ view } = m.addDecider(view, id, { mode: 'anyone' }));
  assert.equal(ends(view), before, 'no end added inside the fork');
  const way = view.template.nodes[id].loops[redo].onExhausted;
  assert.ok(m.outcomesOf(view, id).some((o) => o.outcome === way), 'it goes along an existing outcome');
  ({ view } = m.setDecisionWho(m.removeDecider(view, id, 0).view, id, { mode: 'anyone' }));
  assert.equal(ends(view), before);
});
