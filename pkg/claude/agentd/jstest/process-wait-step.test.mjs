// Unit tests for process-wait-step.js, the editors' pure layer for a wait
// step: the duration grammar (mirroring model.ParseWaitDuration), switching
// between a time and a condition, the check knobs, and the routes a check
// wait that can time out needs.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  parseWaitDuration, formatWaitDuration, durationFeedback, waitMode, setWaitMode, checkSource, setCheckSource,
  setCheckField, setCheckPerformerField, waitRouteHint, waitOutcomeFor, describeWait, checkMoreSet, waitSub,
} from '../dashboard/js/process-wait-step.js';
import { ProcessEditModel } from '../dashboard/js/process-edit-model.js';

test('parseWaitDuration reads the same grammar as the engine', () => {
  const minutes = (text) => parseWaitDuration(text).ms / 60000;
  assert.equal(minutes('90m'), 90);
  assert.equal(minutes('1h30m'), 90);
  assert.equal(minutes('3 days'), 3 * 24 * 60);
  assert.equal(minutes('1 hour 30 minutes'), 90);
  assert.equal(minutes('2 weeks and 1 day'), 15 * 24 * 60);
  assert.equal(minutes('1d12h'), 36 * 60);
  assert.match(parseWaitDuration('soon').error, /not a duration/);
  assert.match(parseWaitDuration('3 fortnights').error, /unknown unit/);
  assert.match(parseWaitDuration('0m').error, /positive/);
  assert.match(parseWaitDuration('11 years').error, /unknown unit/);
  assert.match(parseWaitDuration('600w').error, /longer/);
});

test('durationFeedback says what a field read', () => {
  assert.deepEqual(durationFeedback('3d'), { text: '= 3 days' });
  assert.deepEqual(durationFeedback('90m'), { text: '= 1 hour 30 minutes' });
  assert.equal(durationFeedback('soon').error, true);
  assert.deepEqual(durationFeedback(''), { text: '' });
  assert.deepEqual(durationFeedback('{{ params.cool_off }}'), { text: '' }, 'a reference is bound at run time');
  assert.equal(formatWaitDuration(61000), '1 minute 1 second');
});

test('a wait switches between a time and a condition', () => {
  const node = { type: 'wait', wait: { duration: '3d', until: '2026-12-01T00:00:00Z' } };
  assert.equal(waitMode(node), 'time');
  setWaitMode(node, 'check');
  assert.equal(waitMode(node), 'check');
  assert.deepEqual(node.wait, { check: { every: '5m', maxChecks: 12, when: '' } });
  setCheckSource(node, 'program');
  assert.equal(checkSource(node.wait.check), 'program');
  assert.deepEqual(node.wait.check.performer, { kind: 'program', profile: '', run: '' });
  setCheckPerformerField(node, 'run', ' ./ready.sh ');
  assert.equal(node.wait.check.performer.run, './ready.sh');
  assert.equal(setCheckField(node, 'timeout', '2m'), '');
  setCheckSource(node, 'agent');
  assert.equal(node.wait.check.timeout, undefined, 'an agent check has no per-check timeout');
  setCheckPerformerField(node, 'role', 'dev');
  assert.deepEqual(node.wait.check.performer, { kind: 'agent', prompt: '', role: 'dev' });
  setWaitMode(node, 'time');
  assert.equal(node.wait, undefined, 'a time wait starts empty');
});

test('setCheckField validates counts and the factor, and fixed drops its knobs', () => {
  const node = { type: 'wait', wait: { check: { when: 'params.ok == true', every: '5m', maxChecks: 3 } } };
  assert.match(setCheckField(node, 'maxChecks', '0'), /at least 1/);
  assert.match(setCheckField(node, 'maxChecks', '2.5'), /whole number/);
  assert.equal(node.wait.check.maxChecks, 3, 'a refused value leaves the node alone');
  assert.match(setCheckField(node, 'factor', '1'), /above 1/);
  assert.equal(setCheckField(node, 'backoff', 'exponential'), '');
  assert.equal(setCheckField(node, 'factor', '3'), '');
  assert.equal(setCheckField(node, 'maxEvery', '1h'), '');
  assert.ok(checkMoreSet(node.wait.check));
  setCheckField(node, 'backoff', 'fixed');
  assert.deepEqual(Object.keys(node.wait.check).sort(), ['every', 'maxChecks', 'when']);
  setCheckField(node, 'maxChecks', '');
  assert.equal(node.wait.check.maxChecks, undefined);
  setCheckField(node, 'maxFailedChecks', '5');
  assert.equal(node.wait.check.maxFailedChecks, 5);
});

test('a check wait that times out asks for met and timedOut routes', () => {
  const node = { type: 'wait', wait: { check: { when: 'params.ok == true', every: '5m', maxChecks: 3 } } };
  assert.equal(waitRouteHint(node, ['next']), '');
  setCheckField(node, 'onLimit', 'timedOut');
  assert.match(waitRouteHint(node, ['met']), /a connector labelled timedOut/);
  assert.equal(waitRouteHint(node, ['met', 'timedOut']), '');
  assert.equal(waitOutcomeFor(node, []), 'met');
  assert.equal(waitOutcomeFor(node, ['met']), 'timedOut');
  setCheckField(node, 'onLimit', 'fail');
  assert.equal(node.wait.check.onLimit, undefined, 'fail is the default');
});

test('the full editor names a wait\'s new connectors met, then timedOut', () => {
  const model = new ProcessEditModel({
    template: {
      apiVersion: 'tclaude.dev/v1alpha1', kind: 'ProcessTemplate', id: 'w', start: 'hold',
      nodes: {
        hold: { type: 'wait', wait: { check: { when: 'params.ok == true', every: '5m', maxChecks: 3, onLimit: 'timedOut' } } },
        done: { type: 'end', result: 'success' },
      },
    },
    edges: [{ from: '', outcome: 'start', to: 'hold' }],
  });
  assert.equal(model.freeOutcome('hold'), 'met');
  model.addEdge('hold', 'met', 'done');
  assert.equal(model.freeOutcome('hold'), 'timedOut');
});

test('describeWait and waitSub say a wait in words', () => {
  assert.equal(describeWait({ wait: { duration: '3d' } }), 'Wait 3 days');
  assert.equal(describeWait({ wait: { duration: '2h', until: '{{ params.at }}' } }), 'Wait 2 hours or until {{ params.at }}');
  assert.equal(describeWait({ wait: { check: { when: 'outputs.state == "ready"', every: '5m', backoff: 'exponential', maxEvery: '1h', maxChecks: 20, within: '1d', onLimit: 'timedOut' } } }),
    'Check every 5 minutes, backing off ×2 up to 1 hour until outputs.state == "ready"; gives up after 20 checks or 1 day, then takes timedOut');
  assert.equal(waitSub({ wait: { check: { every: '10m' } } }), 'checks every 10 minutes');
  assert.equal(waitSub({ wait: { duration: '3 days' } }), '3 days');
});

test('the inline editor adds a wait and keeps its routes right as its limit changes', async () => {
  const { insertAfter, setMiniWaitMode, setWaitLimit, miniKind, mainEdge } = await import('../dashboard/js/process-mini-edit.js');
  let view = {
    template: { start: 'start', nodes: { start: { type: 'start' }, work: { type: 'task', name: 'Work', performer: { kind: 'agent', prompt: 'x' } },
      done: { type: 'end', result: 'success' }, failed: { type: 'end', result: 'failure' } } },
    edges: [{ from: 'start', outcome: 'next', to: 'work' }, { from: 'work', outcome: 'next', to: 'done' }],
    layout: {},
  };
  const added = insertAfter(view, 'work', 'wait');
  view = added.view;
  const id = added.id;
  assert.equal(miniKind(view.template.nodes[id]), 'wait');
  assert.deepEqual(view.template.nodes[id].wait, { duration: '1h' });
  view = setMiniWaitMode(view, id, 'check').view;
  view = setWaitLimit(view, id, 'timedOut').view;
  const routes = view.edges.filter((edge) => edge.from === id).map((edge) => `${edge.outcome}->${edge.to}`).sort();
  assert.deepEqual(routes, ['met->done', 'timedOut->failed']);
  assert.equal(mainEdge(view, id).outcome, 'met');
  view = setMiniWaitMode(view, id, 'time').view;
  assert.deepEqual(view.edges.filter((edge) => edge.from === id).map((edge) => `${edge.outcome}->${edge.to}`), ['next->done']);
});

test('inside a loop, a wait\'s timed-out route stays in the loop', async () => {
  const { setMiniWaitMode, setWaitLimit } = await import('../dashboard/js/process-mini-edit.js');
  let view = {
    template: { start: 'start', nodes: {
      start: { type: 'start' }, build: { type: 'task', name: 'Build', performer: { kind: 'agent', prompt: 'x' } },
      hold: { type: 'wait', name: 'Wait', wait: { duration: '1h' } },
      review: { type: 'decision', name: 'Review', performer: { kind: 'agent', prompt: 'ok?' }, loops: { changes: { maxIterations: 3 } } },
      done: { type: 'end', result: 'success' }, failed: { type: 'end', result: 'failure' } } },
    edges: [{ from: 'start', outcome: 'next', to: 'build' }, { from: 'build', outcome: 'next', to: 'hold' }, { from: 'hold', outcome: 'next', to: 'review' },
      { from: 'review', outcome: 'approve', to: 'done' }, { from: 'review', outcome: 'changes', to: 'build' }],
    layout: {},
  };
  view = setMiniWaitMode(view, 'hold', 'check').view;
  view = setWaitLimit(view, 'hold', 'timedOut').view;
  assert.deepEqual(view.edges.filter((edge) => edge.from === 'hold').map((edge) => `${edge.outcome}->${edge.to}`).sort(), ['met->review', 'timedOut->review']);
});
