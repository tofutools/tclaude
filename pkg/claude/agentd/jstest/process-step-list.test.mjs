// Tests for the quick-start step list (tcl-7p6bgt): an edit view converts to
// steps and back without loss, graphs the list cannot show are refused, and
// the step edits (who, text, choices, links, insert/remove/move) produce the
// template the server expects.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  blankStepList, expandParamShorthand, forwardTargets, joinCommand, splitCommand, insertStep, moveStep, newStep, removeStep, replaceStep,
  setStepChoices, setStepLinks, setStepText, setStepWho, stepListFlow, stepListParams,
  stepListToView, stepTargetLabel, stepText, stepWho, viewToStepList,
  backTargetLabel, backTargets, isBackLink, syncStepLoops, setOutcomeLoop, setStepLoop, stepLoop,
} from '../dashboard/js/process-step-list.js';

// viewOf builds an edit view from a template whose nodes carry `next`, the
// way the server derives edges (model.NormalizeEdges).
function viewOf(template, layout = { nodes: {} }) {
  const edges = [{ from: '', outcome: 'start', to: template.start }];
  for (const id of Object.keys(template.nodes).sort()) {
    for (const outcome of Object.keys(template.nodes[id].next || {}).sort()) {
      edges.push({ from: id, outcome, to: template.nodes[id].next[outcome] });
    }
  }
  return { template, edges, layout };
}

// normalized is what a lossless round trip must preserve.
function normalized(view) {
  const nodes = {};
  for (const id of Object.keys(view.template.nodes).sort()) {
    const { next, ...rest } = view.template.nodes[id];
    nodes[id] = rest;
  }
  const { nodes: _, start, ...top } = view.template;
  return {
    top, start, nodes,
    edges: view.edges.map((e) => `${e.from}|${e.outcome}|${e.to}`).sort(),
    layout: view.layout,
  };
}

function roundTrip(view) {
  const converted = viewToStepList(view);
  assert.equal(converted.ok, true, converted.reason);
  return stepListToView(converted.list);
}

const CODE_CHANGE = {
  apiVersion: 'tclaude.dev/v1alpha1', kind: 'ProcessTemplate', id: 'code-change-with-review', name: 'Code change with review',
  description: 'Implement an issue, run checks, review the diff, and approve merge.',
  params: { issue: { type: 'string', required: true } },
  start: 'implement',
  nodes: {
    implement: {
      type: 'task',
      performer: { kind: 'agent', profile: 'dev', prompt: 'Implement {{ params.issue }}' },
      plan: { id: 'plan', approval: 'human', performer: { kind: 'agent', profile: 'dev', prompt: 'Plan {{ params.issue }}' } },
      checks: [{ id: 'tests', performer: { kind: 'program', run: 'go', args: ['test', './...'] } }],
      retry: { maxAttempts: 3, onFail: 'feedback-same-session' },
      next: { pass: 'done', fail: 'escalate' },
    },
    escalate: {
      type: 'decision',
      performer: { kind: 'human', profile: 'operator', ask: 'Retries exhausted. Continue?' },
      next: { retry: 'implement', cancel: 'canceled' },
    },
    done: { type: 'end', result: 'success' },
    canceled: { type: 'end', result: 'canceled' },
  },
};

const SHIP_FEATURE = {
  apiVersion: 'tclaude.dev/v1alpha1', kind: 'ProcessTemplate', id: 'ship-feature', name: 'ship-feature',
  params: { issue: { type: 'string', required: true } },
  start: 'start',
  nodes: {
    start: { type: 'start', next: { next: 'implement' } },
    implement: { type: 'task', name: 'Implement', performer: { kind: 'agent', assignee: 'checkout', prompt: 'Implement {{params.issue}} on a branch.' }, next: { next: 'review' } },
    review: { type: 'decision', name: 'Review', performer: { kind: 'agent', profile: 'reviewer', prompt: 'Answer approved or changes.' }, next: { approved: 'approve', changes: 'implement' } },
    approve: { type: 'decision', name: 'Approve', performer: { kind: 'human', ask: 'Ship {{params.issue}}?', choices: ['ship', 'hold'] }, next: { ship: 'merge', hold: 'held' } },
    merge: { type: 'task', name: 'Merge', performer: { kind: 'program', run: 'gh', args: ['pr', 'merge', '--squash'] }, next: { next: 'end' } },
    end: { type: 'end' },
    held: { type: 'end', result: 'canceled', name: 'held' },
  },
};

test('a linear process with branches and loops round-trips without loss', () => {
  for (const template of [CODE_CHANGE, SHIP_FEATURE]) {
    const view = viewOf(structuredClone(template), { nodes: { implement: { x: 10, y: 20 } } });
    assert.deepEqual(normalized(roundTrip(view)), normalized(view), template.id);
  }
});

test('steps are ordered along the pass path and read as continue + then links', () => {
  const { list } = viewToStepList(viewOf(structuredClone(SHIP_FEATURE)));
  assert.deepEqual(list.steps.map((s) => s.id), ['implement', 'review', 'approve', 'merge']);
  assert.equal(list.mainEnd, 'end');
  assert.equal(list.startNode.id, 'start');
  const [implement, review, approve, merge] = list.steps;
  assert.equal(implement.cont, 'next');
  assert.deepEqual(implement.links, []);
  assert.equal(review.cont, '', 'a decision has no implicit continue');
  assert.deepEqual(review.links, [{ outcome: 'approved', to: 'approve' }, { outcome: 'changes', to: 'implement' }]);
  assert.deepEqual(approve.links.map((l) => stepTargetLabel(list, l.to)), ['4 · Merge', 'stop (canceled)'], 'answers keep their authored order');
  assert.equal(merge.cont, 'next');
  assert.deepEqual(list.steps.map(stepWho), [
    { mode: 'agent', agent: 'checkout' },
    { mode: 'role', role: 'reviewer', legacy: true },
    { mode: 'you', assignee: '' },
    { mode: 'command', run: 'gh' },
  ]);
  assert.equal(stepText(merge), 'gh pr merge --squash');
  assert.deepEqual(stepListParams(list), ['issue']);
});

test('graphs the list cannot show are refused with a reason', () => {
  const parallel = viewOf({
    id: 'p', start: 'fan',
    nodes: { fan: { type: 'parallel', next: { a: 'x', b: 'y' } }, x: { type: 'task', next: { next: 'e' } }, y: { type: 'task', next: { next: 'e' } }, e: { type: 'end' } },
  });
  assert.match(viewToStepList(parallel).reason, /fan is a parallel node/);
  const waiting = viewOf({ id: 'w', start: 'w', nodes: { w: { type: 'wait', wait: { duration: '1h' }, next: { next: 'e' } }, e: { type: 'end' } } });
  assert.match(viewToStepList(waiting).reason, /wait node/);
  const sub = viewOf({ id: 's', start: 's', nodes: { s: { type: 'subprocess', subprocess: { templateRef: 'x' }, next: { next: 'e' } }, e: { type: 'end' } } });
  assert.equal(viewToStepList(sub).ok, false);
});

test('a blank list grows into a valid linear template', () => {
  let list = blankStepList('ship-feature');
  list = insertStep(list, newStep(list, { name: 'Implement', text: 'Implement {{issue}} on a branch.', who: { mode: 'agent', agent: 'checkout' } }));
  list = insertStep(list, newStep(list, { kind: 'you', name: 'Approve', text: 'Ship it?' }));
  list = insertStep(list, newStep(list, { kind: 'command', name: 'Merge', text: 'gh pr merge --squash' }));
  const view = stepListToView(list);
  assert.equal(view.template.start, 'start');
  assert.deepEqual(view.edges.map((e) => `${e.from}|${e.outcome}|${e.to}`), [
    '|start|start', 'start|next|implement', 'implement|next|approve', 'approve|continue|merge', 'merge|next|end',
  ]);
  assert.deepEqual(view.template.nodes.implement.performer, { kind: 'agent', assignee: 'checkout', prompt: 'Implement {{params.issue}} on a branch.' });
  assert.equal(view.template.nodes.approve.type, 'decision', 'a human performs decisions only, so a you step is one');
  assert.deepEqual(view.template.nodes.approve.performer, { kind: 'human', ask: 'Ship it?', choices: ['continue'] });
  assert.deepEqual(view.template.nodes.merge.performer, { kind: 'program', run: 'gh', args: ['pr', 'merge', '--squash'] });
  assert.deepEqual(view.template.params, { issue: { type: 'string', required: true } }, '{{issue}} became a start input');
  assert.deepEqual(normalized(roundTrip(view)), normalized(view));
});

test('an empty list still starts somewhere', () => {
  const view = stepListToView(blankStepList('x'));
  assert.deepEqual(view.edges, [{ from: '', outcome: 'start', to: 'start' }, { from: 'start', outcome: 'next', to: 'end' }]);
});

test('who switches keep the step text and drop fields the new kind cannot carry', () => {
  const { list } = viewToStepList(viewOf(structuredClone(SHIP_FEATURE)));
  const implement = list.steps[0];
  const role = setStepWho(implement, { mode: 'role', role: 'dev' });
  assert.deepEqual(role.node.performer, { kind: 'agent', role: 'dev', prompt: 'Implement {{params.issue}} on a branch.' },
    'a role is performer.role');
  const legacy = setStepWho(implement, { mode: 'role', role: 'dev' }, { legacyRoles: true });
  assert.deepEqual(legacy.node.performer, { kind: 'agent', profile: 'dev', prompt: 'Implement {{params.issue}} on a branch.' });
  const anyone = setStepWho(role, { mode: 'anyone' });
  assert.deepEqual(anyone.node.performer, { kind: 'agent', anyone: true, prompt: 'Implement {{params.issue}} on a branch.' });
  const reviewer = list.steps[1];
  const spawning = { ...reviewer, node: { ...reviewer.node, performer: { ...reviewer.node.performer, spawn: 'allowed' } } };
  assert.deepEqual(setStepWho(spawning, { mode: 'role', role: 'reviewer' }).node.performer,
    { kind: 'agent', role: 'reviewer', fallback: { action: 'spawn', profile: 'reviewer' }, prompt: 'Answer approved or changes.' },
    'a legacy spawn opt-in becomes a step fallback that spawns the same profile');
  const you = setStepWho(implement, { mode: 'you' });
  assert.deepEqual(you.node.performer, { kind: 'human', ask: 'Implement {{params.issue}} on a branch.' });
  const back = setStepWho(you, { mode: 'agent', agent: 'search' });
  assert.deepEqual(back.node.performer, { kind: 'agent', assignee: 'search', prompt: 'Implement {{params.issue}} on a branch.' });
  const cmd = setStepWho(implement, { mode: 'command', run: 'make test' });
  assert.deepEqual(cmd.node.performer, { kind: 'program', run: 'make', args: ['test'] });
  const legacyRole = list.steps[1];
  assert.deepEqual(setStepWho(legacyRole, { mode: 'agent', agent: 'search' }).node.performer,
    { kind: 'agent', assignee: 'search', prompt: 'Answer approved or changes.' }, 'a legacy role does not survive a switch away');
});

test('text edits expand the {{name}} shorthand into a start input', () => {
  assert.equal(expandParamShorthand('Fix {{issue}} and {{ params.x }} and {{ branch }}'), 'Fix {{params.issue}} and {{ params.x }} and {{params.branch}}');
  let list = blankStepList('x');
  list = insertStep(list, newStep(list, { name: 'Do' }));
  list = replaceStep(list, setStepText(list.steps[0], 'Handle {{ticket}}'));
  assert.deepEqual(stepListParams(list), ['ticket']);
  assert.deepEqual(stepListToView(list).template.params, { ticket: { type: 'string', required: true } });
  list = replaceStep(list, setStepText(list.steps[0], 'No inputs now'));
  assert.deepEqual(stepListToView(list).template.params, {}, 'an unreferenced detected input goes away');
});

test('choices turn a sign-off into a decision whose answers are links', () => {
  let list = blankStepList('x');
  list = insertStep(list, newStep(list, { kind: 'you', name: 'Approve', text: 'Ship?' }));
  list = insertStep(list, newStep(list, { kind: 'command', name: 'Merge', text: 'gh pr merge' }));
  const decided = setStepChoices(list.steps[0], ['ship', 'hold', 'ship'], 'merge');
  assert.equal(decided.node.type, 'decision');
  assert.deepEqual(decided.node.performer.choices, ['ship', 'hold']);
  assert.deepEqual(decided.links, [{ outcome: 'ship', to: 'merge' }, { outcome: 'hold', to: 'merge' }]);
  list = replaceStep(list, setStepLinks(decided, [{ outcome: 'ship', to: 'merge' }, { outcome: 'hold', to: 'end' }]));
  const view = stepListToView(list);
  assert.deepEqual(view.edges.filter((e) => e.from === 'approve').map((e) => `${e.outcome}|${e.to}`), ['ship|merge', 'hold|end']);
  const signoff = setStepChoices(list.steps[0], [], 'merge');
  assert.equal(signoff.node.type, 'task');
  assert.equal(signoff.cont, 'next');
  assert.deepEqual(signoff.links, []);
  assert.equal(signoff.node.performer.choices, undefined);
});

test('links: a loop back, and a link that takes over the continue outcome', () => {
  let list = blankStepList('x');
  list = insertStep(list, newStep(list, { name: 'Run tests', kind: 'command', text: 'make test' }));
  list = insertStep(list, newStep(list, { name: 'Fix' }));
  const tests = setStepLinks(list.steps[0], [{ outcome: 'fail', to: 'fix' }, { outcome: 'fail', to: 'end' }, { outcome: '', to: 'end' }]);
  assert.deepEqual(tests.links, [{ outcome: 'fail', to: 'fix' }], 'duplicate and empty outcomes are dropped');
  assert.equal(tests.cont, 'next');
  const takeover = setStepLinks(list.steps[0], [{ outcome: 'next', to: 'end' }]);
  assert.equal(takeover.cont, '');
  list = replaceStep(list, tests);
  list = replaceStep(list, setStepLinks(list.steps[1], [{ outcome: 'retry', to: 'run-tests' }]));
  const view = stepListToView(list);
  assert.deepEqual(view.edges.map((e) => `${e.from}|${e.outcome}|${e.to}`), [
    '|start|start', 'start|next|run-tests', 'run-tests|next|fix', 'run-tests|fail|fix', 'fix|next|end', 'fix|retry|run-tests',
  ]);
  assert.deepEqual(normalized(roundTrip(view)), normalized(view));
});

test('remove retargets links to the following step; move reorders continues', () => {
  let list = blankStepList('x');
  for (const name of ['A', 'B', 'C']) list = insertStep(list, newStep(list, { name }));
  list = replaceStep(list, setStepLinks(list.steps[2], [{ outcome: 'again', to: 'b' }]));
  const removed = removeStep(list, 'b');
  assert.deepEqual(removed.steps.map((s) => s.id), ['a', 'c']);
  assert.deepEqual(removed.steps[1].links, [{ outcome: 'again', to: 'end' }], 'never back at the linking step itself');
  const moved = moveStep(list, 'c', -2);
  assert.deepEqual(moved.steps.map((s) => s.id), ['c', 'a', 'b']);
  assert.deepEqual(stepListToView(moved).edges.slice(0, 4).map((e) => `${e.from}|${e.outcome}|${e.to}`),
    ['|start|start', 'start|next|c', 'c|next|a', 'c|again|b']);
  assert.equal(moveStep(list, 'a', -1), list, 'moving past the top is a no-op');
});

test('new step ids are unique and id-shaped', () => {
  let list = blankStepList('x');
  list = insertStep(list, newStep(list, { name: 'Review' }));
  list = insertStep(list, newStep(list, { name: 'Review' }));
  list = insertStep(list, newStep(list, { name: '  Ünïcode!! ' }));
  list = insertStep(list, newStep(list, { name: 'End' }));
  assert.deepEqual(list.steps.map((s) => s.id), ['review', 'review-2', 'n-code', 'end-2']);
});

test('the flow preview lists steps top to bottom and puts jumps on the fewest lanes', () => {
  const { list } = viewToStepList(viewOf(structuredClone(SHIP_FEATURE)));
  const flow = stepListFlow(list, { whoOf: (step) => (step.id === 'implement' ? 'checkout' : '') });
  const ids = flow.rows.map((row) => row.id);
  assert.equal(flow.rows.find((row) => row.id === 'implement').who, 'checkout');
  const jump = (from, to) => flow.jumps.find((j) => ids[j.from] === from && ids[j.to] === to);
  assert.equal(jump('review', 'implement').back, true, 'an existing backward link stays visible');
  assert.equal(jump('review', 'implement').label, 'changes');
  assert.equal(flow.rows.find((row) => row.id === 'implement').down, '', 'a pass edge to the next row is a plain arrow');
  for (const a of flow.jumps) {
    for (const b of flow.jumps) {
      if (a === b || a.lane !== b.lane) continue;
      const [a1, a2] = [Math.min(a.from, a.to), Math.max(a.from, a.to)];
      const [b1, b2] = [Math.min(b.from, b.to), Math.max(b.from, b.to)];
      assert.ok(a2 < b1 || b2 < a1, 'jumps sharing a lane never overlap');
    }
  }
});

test('a decision switched to an agent or a command keeps only its continue', () => {
  let list = blankStepList('x');
  list = insertStep(list, newStep(list, { kind: 'you' }));
  list = insertStep(list, newStep(list, { kind: 'agent' }));
  let decide = setStepChoices(list.steps[0], ['ship', 'hold'], 'end');
  decide = setStepLinks({ ...decide }, [...decide.links]);
  const task = setStepChoices(setStepWho(decide, { mode: 'agent', agent: 'a' }), [], '');
  assert.equal(task.node.type, 'task');
  assert.deepEqual(task.links, []);
  const edges = stepListToView(replaceStep(list, task)).edges.filter((e) => e.from === task.id);
  assert.equal(edges.length, 1, 'a task has exactly one route');
});

test('a list with no success end round-trips without an orphan end', () => {
  const template = { start: 'a', nodes: {
    a: { type: 'task', performer: { kind: 'agent', prompt: 'x' }, next: { next: 'b' } },
    b: { type: 'decision', performer: { kind: 'human', ask: 'y?', choices: ['y', 'n'] }, next: { y: 'c', n: 'f' } },
    c: { type: 'end', result: 'canceled' }, f: { type: 'end', result: 'failed' },
  } };
  const { list } = viewToStepList(viewOf(template));
  assert.deepEqual(Object.keys(stepListToView(list).template.nodes).sort(), ['a', 'b', 'c', 'f']);
});

test('commands split into argv (programs run with no shell) and join back for display', () => {
  assert.deepEqual(splitCommand(`gh pr merge --squash`), ['gh', 'pr', 'merge', '--squash']);
  assert.deepEqual(splitCommand(`git commit -m "fix: the thing" ''`), ['git', 'commit', '-m', 'fix: the thing', '']);
  assert.deepEqual(splitCommand('  '), []);
  assert.equal(joinCommand(['git', 'commit', '-m', 'fix: the thing', '', "it's"]), `git commit -m 'fix: the thing' '' "it's"`);
  for (const line of [`git commit -m 'fix: the thing'`, 'make test']) assert.equal(joinCommand(splitCommand(line)), line);
  let list = blankStepList('x');
  list = insertStep(list, newStep(list, { kind: 'command', name: 'Lint', text: 'golangci-lint run ./...' }));
  assert.deepEqual(list.steps[0].node.performer, { kind: 'program', run: 'golangci-lint', args: ['run', './...'] });
  assert.equal(stepText(list.steps[0]), 'golangci-lint run ./...');
  list = replaceStep(list, setStepText(list.steps[0], 'make'));
  assert.deepEqual(list.steps[0].node.performer, { kind: 'program', run: 'make' });
});

test('link targets are forward only: later steps and the ends', () => {
  let list = blankStepList('x');
  for (const name of ['A', 'B', 'C']) list = insertStep(list, newStep(list, { name }));
  assert.deepEqual(forwardTargets(list, 'b'), ['c', 'end']);
  assert.deepEqual(forwardTargets(list, 'c'), ['end']);
});

test('renaming the continuing answer keeps it continuing only while it is an answer', () => {
  let list = blankStepList('x');
  list = insertStep(list, newStep(list, { kind: 'you', name: 'Gate', text: 'Go?' }));
  list = insertStep(list, newStep(list, { name: 'Work' }));
  const gate = list.steps[0];
  assert.equal(gate.cont, 'continue');
  const two = setStepChoices(gate, ['continue', 'stop'], 'end');
  assert.equal(two.cont, 'continue');
  assert.deepEqual(two.links, [{ outcome: 'stop', to: 'end' }]);
  const renamed = setStepChoices(two, ['go', 'stop'], 'work');
  assert.equal(renamed.cont, '');
  assert.deepEqual(renamed.links, [{ outcome: 'stop', to: 'end' }, { outcome: 'go', to: 'work' }]);
});

test('removing a step never points a link back at its own step', () => {
  let list = blankStepList('x');
  for (const name of ['A', 'B', 'C']) list = insertStep(list, newStep(list, { name }));
  // c links to b, then b moves below c: removing b must not make c link to c.
  list = replaceStep(list, setStepLinks(list.steps[0], [{ outcome: 'skip', to: 'c' }]));
  list = { ...list, steps: [list.steps[0], list.steps[2], list.steps[1]] };
  list = replaceStep(list, { ...list.steps[1], links: [{ outcome: 'again', to: 'b' }] });
  const removed = removeStep(list, 'b');
  assert.deepEqual(removed.steps.map((s) => [s.id, s.links]), [['a', [{ outcome: 'skip', to: 'c' }]], ['c', [{ outcome: 'again', to: 'end' }]]]);
});

// reviewLoop is implement → review → decide {approved → merge, changes ↺
// implement (max 3), escalate → stop}.
function reviewLoop() {
  return viewOf({
    apiVersion: 'tclaude.dev/v1alpha1', kind: 'ProcessTemplate', id: 'loop', start: 'implement',
    nodes: {
      implement: { type: 'task', name: 'Implement', performer: { kind: 'agent', prompt: 'Implement' }, next: { next: 'review' } },
      review: { type: 'task', name: 'Review', performer: { kind: 'agent', prompt: 'Review' }, next: { next: 'decide' } },
      decide: {
        type: 'decision', name: 'Ship it?', performer: { kind: 'human', ask: 'Ship it?', choices: ['approved', 'changes', 'escalate'] },
        next: { approved: 'merge', changes: 'implement', escalate: 'stopped' },
        loops: { changes: { maxIterations: 3, onExhausted: 'escalate' } },
      },
      merge: { type: 'task', name: 'Merge', performer: { kind: 'agent', prompt: 'Merge' }, next: { next: 'end' } },
      end: { type: 'end', result: 'success' },
      stopped: { type: 'end', result: 'canceled' },
    },
  });
}

test('a bounded loop survives the step list round trip and reads as going back', () => {
  const input = reviewLoop();
  const converted = viewToStepList(input);
  assert.equal(converted.ok, true, converted.reason);
  const { list } = converted;
  assert.deepEqual(normalized(stepListToView(list)), normalized(input));
  assert.deepEqual(backTargets(list, 'decide'), ['implement', 'review']);
  assert.deepEqual(backTargets(list, 'review'), [], 'a task never goes back');
  assert.equal(isBackLink(list, 'decide', 'implement'), true);
  assert.equal(backTargetLabel(list, 'implement'), '↺ back to 1 · Implement');
  const decide = list.steps.find((step) => step.id === 'decide');
  assert.deepEqual(stepLoop(decide, 'changes'), { maxIterations: 3, onExhausted: 'escalate' });
  const jump = stepListFlow(list).jumps.find((j) => j.label === 'changes');
  assert.equal(jump.back, true);
  assert.deepEqual(jump.loop, { maxIterations: 3, onExhausted: 'escalate' });
});

test('loop bounds are set, follow their answers, and drop when they no longer go back', () => {
  let { list } = viewToStepList(reviewLoop());
  list = setOutcomeLoop(list, 'decide', 'changes', { maxIterations: 5, onExhausted: '' });
  assert.deepEqual(list.steps.find((s) => s.id === 'decide').node.loops, { changes: { maxIterations: 5 } });
  // Removing the answer removes its bound; removing the exhaustion target clears it.
  let decide = list.steps.find((s) => s.id === 'decide');
  decide = setStepLoop(decide, 'changes', { maxIterations: 3, onExhausted: 'escalate' });
  const narrowed = setStepChoices(decide, ['approved', 'changes'], 'merge');
  assert.deepEqual(narrowed.node.loops, { changes: { maxIterations: 3 } });
  assert.equal(setStepChoices(decide, ['approved', 'escalate'], 'merge').node.loops, undefined);
  // Moving the decision above its target turns the link forward: the bound goes.
  let moved = moveStep(list, 'decide', -2);
  assert.equal(moved.steps[0].id, 'decide');
  assert.equal(moved.steps[0].node.loops, undefined);
  assert.equal(syncStepLoops(list), list, 'nothing stale, nothing changed');
  moved = removeStep(list, 'implement');
  assert.deepEqual(moved.steps.find((s) => s.id === 'decide').node.loops, { changes: { maxIterations: 5 } },
    'the link retargets to review, which is still earlier');
});

test('a move that turns a decision answer back bounds it, so the list never holds an unbounded cycle', () => {
  const { list } = viewToStepList(reviewLoop());
  const decide = list.steps.findIndex((s) => s.id === 'decide');
  const approvedTo = list.steps.find((s) => s.id === 'decide').links.find((l) => l.outcome === 'approved').to;
  const target = list.steps.findIndex((s) => s.id === approvedTo);
  assert.ok(target > decide, 'approved goes forward to start with');
  const moved = moveStep(list, approvedTo, decide - target);
  assert.deepEqual(stepLoop(moved.steps.find((s) => s.id === 'decide'), 'approved'), { maxIterations: 3, onExhausted: '' });
  assert.deepEqual(stepLoop(moved.steps.find((s) => s.id === 'decide'), 'changes'), { maxIterations: 3, onExhausted: 'escalate' },
    'an existing bound is kept');
});
