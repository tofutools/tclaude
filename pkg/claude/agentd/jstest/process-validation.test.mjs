// Node tests for the pure live-validation logic (TCL-299): the debounce +
// sequence-guard scheduler, diagnostics→badge mapping, and graph decoration.
// These import the exact files shipped to the browser.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  LiveValidation, ValidationScheduler, diagnosticIdentity, resolveDiagnosticFocus,
  mapDiagnostics, decorateGraph, splitEdgeTarget, severityGlyph,
} from '../dashboard/js/process-validation.js';
import { ProcessEditModel, blankEditView, graphEdgeID } from '../dashboard/js/process-edit-model.js';

// fakeTimers collects scheduled callbacks so tests fire the debounce by hand.
function fakeTimers() {
  let nextID = 1;
  const pending = new Map();
  return {
    set(fn, ms) {
      const id = nextID++;
      pending.set(id, { fn, ms });
      return id;
    },
    clear(id) {
      pending.delete(id);
    },
    fire() {
      const jobs = [...pending.values()];
      pending.clear();
      for (const job of jobs) job.fn();
    },
    count() {
      return pending.size;
    },
  };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const tick = () => new Promise((resolve) => setImmediate(resolve));

test('debounce collapses rapid schedules into one run', async () => {
  const timers = fakeTimers();
  const runs = [];
  const results = [];
  const scheduler = new ValidationScheduler({
    run: async (payload) => { runs.push(payload); return [{ code: payload }]; },
    onResult: (diags) => results.push(diags),
    timers,
  });
  scheduler.schedule(() => 'a');
  scheduler.schedule(() => 'b');
  scheduler.schedule(() => 'c');
  assert.equal(timers.count(), 1, 'earlier timers are cleared');
  timers.fire();
  await tick();
  assert.deepEqual(runs, ['c'], 'only the last scheduled payload runs');
  assert.deepEqual(results, [[{ code: 'c' }]]);
});

test('flush replaces a pending debounce with one immediate validation round', async () => {
  const timers = fakeTimers();
  const runs = [];
  const scheduler = new ValidationScheduler({
    run: async (payload) => { runs.push(payload); return []; },
    timers,
  });
  scheduler.schedule(() => 'debounced');
  assert.equal(scheduler.flush(() => 'now'), true);
  assert.equal(timers.count(), 0);
  await tick();
  assert.deepEqual(runs, ['now']);
});

test('issue navigation starts at the expected end and wraps', () => {
  const focused = [];
  const button = { focus() {} };
  const entries = [
    { code: 'first', scope: 'template', targetId: '' },
    { code: 'middle', scope: 'node', targetId: 'work' },
    { code: 'last', scope: 'edge', targetId: 'start:fail' },
  ];
  const fake = {
    mapped: { entries }, issueCursor: -1,
    panel: { open: false }, list: { querySelector: () => button },
    focusEntry: (entry) => focused.push(entry.code),
    focusIssueAt: LiveValidation.prototype.focusIssueAt,
  };
  assert.equal(LiveValidation.prototype.focusIssue.call(fake, -1), true);
  assert.equal(fake.issueCursor, 2);
  assert.equal(fake.panel.open, true);
  LiveValidation.prototype.focusIssue.call(fake, 1);
  assert.equal(fake.issueCursor, 0);
  assert.deepEqual(focused, ['last', 'first']);
});

test('an explicitly focused issue seeds subsequent next and previous navigation', () => {
  const focused = [];
  const entries = [
    { code: 'first', scope: 'template', targetId: '' },
    { code: 'middle', scope: 'node', targetId: 'work' },
    { code: 'last', scope: 'edge', targetId: 'start:fail' },
  ];
  const fake = {
    editor: { publish() {} }, panel: { open: false }, focusRequest: 0,
    mapped: { entries }, issueCursor: -1,
    focusEntry: (entry) => focused.push(entry.code),
    focusIssueAt: LiveValidation.prototype.focusIssueAt,
  };
  assert.equal(LiveValidation.prototype.focusIssueAt.call(fake, 1), true);
  assert.equal(fake.issueCursor, 1);
  LiveValidation.prototype.focusIssue.call(fake, 1);
  LiveValidation.prototype.focusIssue.call(fake, -1);
  assert.equal(fake.issueCursor, 1);
  assert.deepEqual(focused, ['middle', 'last', 'middle']);
});

test('out-of-order responses are discarded (sequence guard)', async () => {
  const timers = fakeTimers();
  const inFlight = [];
  const results = [];
  const scheduler = new ValidationScheduler({
    run: (payload) => {
      const gate = deferred();
      inFlight.push({ payload, gate });
      return gate.promise;
    },
    onResult: (diags) => results.push(diags),
    timers,
  });
  scheduler.schedule(() => 'first');
  timers.fire();
  scheduler.schedule(() => 'second');
  timers.fire();
  await tick();
  assert.equal(inFlight.length, 2);
  // The NEWER response lands first, then the older one arrives late.
  inFlight[1].gate.resolve([{ code: 'second' }]);
  await tick();
  inFlight[0].gate.resolve([{ code: 'first' }]);
  await tick();
  assert.deepEqual(results, [[{ code: 'second' }]], 'the stale response never overwrites the newer one');
});

test('unserializable drafts and failed rounds skip without crashing', async () => {
  const timers = fakeTimers();
  const results = [];
  let mode = 'throw-payload';
  const scheduler = new ValidationScheduler({
    run: async () => {
      if (mode === 'reject') throw new Error('network down');
      if (mode === 'null') return null;
      return [{ code: 'ok' }];
    },
    onResult: (diags) => results.push(diags),
    timers,
  });
  scheduler.schedule(() => { throw new Error('cannot serialize'); });
  timers.fire();
  await tick();
  mode = 'reject';
  scheduler.schedule(() => 'payload');
  timers.fire();
  await tick();
  mode = 'null';
  scheduler.schedule(() => 'payload');
  timers.fire();
  await tick();
  assert.deepEqual(results, [], 'skipped rounds never emit results');
  mode = 'ok';
  scheduler.schedule(() => 'payload');
  timers.fire();
  await tick();
  assert.deepEqual(results, [[{ code: 'ok' }]], 'the loop recovers on the next good round');
});

test('destroy drops in-flight responses and future schedules', async () => {
  const timers = fakeTimers();
  const results = [];
  const gate = deferred();
  const scheduler = new ValidationScheduler({
    run: () => gate.promise,
    onResult: (diags) => results.push(diags),
    timers,
  });
  scheduler.schedule(() => 'payload');
  timers.fire();
  await tick();
  scheduler.destroy();
  gate.resolve([{ code: 'late' }]);
  await tick();
  scheduler.schedule(() => 'payload');
  assert.equal(timers.count(), 0, 'destroyed schedulers arm no timers');
  assert.deepEqual(results, []);
});

test('splitEdgeTarget splits on the first colon only', () => {
  assert.deepEqual(splitEdgeTarget('work.impl:pass'), { from: 'work.impl', outcome: 'pass' });
  assert.deepEqual(splitEdgeTarget('a:b:c'), { from: 'a', outcome: 'b:c' });
  assert.equal(splitEdgeTarget('no-colon'), null);
  assert.equal(splitEdgeTarget(''), null);
});

function modelWithEdge() {
  const view = blankEditView('fixture');
  view.template.nodes.work = { type: 'task', performer: { kind: 'agent', prompt: 'x' } };
  view.edges.push({ from: 'start', outcome: 'fail', to: 'work' });
  return new ProcessEditModel(view);
}

test('mapDiagnostics anchors node and edge scopes and counts severities', () => {
  const model = modelWithEdge();
  const mapped = mapDiagnostics([
    { scope: 'node', targetId: 'work', severity: 'error', code: 'missing_next', message: 'm1' },
    { scope: 'node', targetId: 'work', severity: 'warning', code: 'w', message: 'm2' },
    { scope: 'edge', targetId: 'start:fail', severity: 'warning', code: 'dead_edge', message: 'm3' },
    { scope: 'template', targetId: '', severity: 'error', code: 'missing_id', message: 'm4' },
  ], model);
  assert.deepEqual(mapped.nodes.get('work'), { error: 1, warning: 1 });
  assert.deepEqual(mapped.edges.get(graphEdgeID('start', 'fail')), { error: 0, warning: 1 });
  assert.equal(mapped.errorCount, 2);
  assert.equal(mapped.warningCount, 2);
  assert.equal(mapped.entries.length, 4);
  assert.equal(mapped.entries[0].severity, 'error', 'errors sort first');
});

test('mapDiagnostics drops anchors for targets missing from the current model', () => {
  const model = modelWithEdge();
  const mapped = mapDiagnostics([
    { scope: 'node', targetId: 'deleted-node', severity: 'error', code: 'x', message: 'm' },
    { scope: 'edge', targetId: 'start:gone', severity: 'error', code: 'x', message: 'm' },
    { scope: 'edge', targetId: 'deleted:gone', severity: 'error', code: 'x', message: 'm' },
  ], model);
  assert.equal(mapped.nodes.size, 1, 'a dangling edge falls back to its surviving source node');
  assert.deepEqual(mapped.nodes.get('start'), { error: 1, warning: 0 });
  assert.equal(mapped.edges.size, 0);
  // All three stay listed in the panel even without a badge anchor.
  assert.equal(mapped.entries.length, 3);
});

test('decorateGraph sets node overlays and edge badges (never color-only)', () => {
  const model = modelWithEdge();
  const mapped = mapDiagnostics([
    { scope: 'node', targetId: 'work', severity: 'error', code: 'a', message: 'm' },
    { scope: 'node', targetId: 'work', severity: 'warning', code: 'b', message: 'm' },
    { scope: 'edge', targetId: 'start:fail', severity: 'warning', code: 'c', message: 'm' },
    { scope: 'edge', targetId: 'start:fail', severity: 'warning', code: 'd', message: 'm2' },
  ], model);
  const graph = decorateGraph(model.graph(), mapped);
  const work = graph.nodes.find((node) => node.id === 'work');
  assert.equal(work.overlay.glyph, severityGlyph('error'), 'error outranks warning on a shared anchor');
  assert.equal(work.overlay.severity, 'error');
  assert.equal(work.overlay.badge, '×2');
  assert.deepEqual(work.overlay.issues, [
    'a: m',
    'b: m',
  ], 'the marker carries exact node-local diagnostic detail');
  const start = graph.nodes.find((node) => node.id === 'start');
  assert.equal(start.overlay, undefined, 'clean nodes stay undecorated');
  const edge = graph.edges.find((candidate) => candidate.id === graphEdgeID('start', 'fail'));
  assert.equal(edge.badge, severityGlyph('warning'));
  assert.equal(edge.badgeSeverity, 'warning');
  assert.deepEqual(edge.issues, ['c: m', 'd: m2'],
    'the edge marker carries all exact edge-local diagnostic detail');
});

test('decorateGraph preserves foreign overlay fields and badges', () => {
  const model = modelWithEdge();
  const mapped = mapDiagnostics([
    { scope: 'node', targetId: 'work', severity: 'warning', code: 'a', message: 'm' },
  ], model);
  const graph = model.graph();
  const work = graph.nodes.find((node) => node.id === 'work');
  // A future run view may already decorate the node (state overlay); one
  // validation diagnostic must not blank its badge or status.
  work.overlay = { status: 'running', badge: '↻2' };
  decorateGraph(graph, mapped);
  assert.equal(work.overlay.badge, '↻2', 'single-diagnostic decoration keeps a foreign badge');
  assert.equal(work.overlay.status, 'running');
  assert.equal(work.overlay.severity, 'warning');
});

test('currentIssue returns only the explicitly focused stable diagnostic entry', () => {
  const entry = { code: 'dead_edge', scope: 'edge', targetId: 'start:fail', edge: { from: 'start', outcome: 'fail' } };
  const identity = diagnosticIdentity(entry);
  const focused = { mapped: { entries: [entry] }, issueCursor: 0, focusedIssueIdentity: identity };
  assert.equal(LiveValidation.prototype.currentIssue.call({ ...focused, issueCursor: -1 }), null);
  assert.equal(LiveValidation.prototype.currentIssue.call(focused), entry);
  assert.equal(LiveValidation.prototype.currentIssue.call({ ...focused, issueCursor: 2 }), null);

  const unrelated = { code: 'missing_next', scope: 'node', targetId: 'work' };
  assert.equal(LiveValidation.prototype.currentIssue.call({
    mapped: { entries: [unrelated] }, issueCursor: 0, focusedIssueIdentity: identity,
  }), null, 'an unrelated issue reusing the old array index is never current');

  const duplicate = { ...entry, message: 'another issue with the same stable identity' };
  const duplicateFocus = {
    mapped: { entries: [entry, duplicate] }, issueCursor: -1,
    focusedIssueIdentity: '', focusedIssueAmbiguous: false,
    panel: { open: false }, list: { querySelector: () => null }, focusEntry() {},
  };
  assert.equal(LiveValidation.prototype.focusIssueAt.call(duplicateFocus, 0, { focusButton: false }), true);
  assert.equal(duplicateFocus.focusedIssueAmbiguous, true);
  assert.equal(LiveValidation.prototype.currentIssue.call(duplicateFocus), null,
    'same-list duplicate identities fail closed immediately after focus');
});

test('focused diagnostics follow stable identity or clear when no longer exact', () => {
  const selected = { code: 'dead_edge', scope: 'edge', targetId: 'start:fail', message: 'selected' };
  const other = { code: 'missing_next', scope: 'node', targetId: 'work', message: 'other' };
  const inserted = { code: 'missing_id', scope: 'template', targetId: '', message: 'inserted' };
  const identity = diagnosticIdentity(selected);

  assert.equal(resolveDiagnosticFocus([other, selected], { identity }), 1,
    'reordering preserves the exact focused diagnostic');
  assert.equal(resolveDiagnosticFocus([inserted, other, selected], { identity }), 2,
    'insertion before the focused diagnostic preserves identity');
  assert.equal(resolveDiagnosticFocus([other], { identity }), -1,
    'removal clears focus instead of reusing the old index');

  const duplicateLooking = { ...selected, message: 'different row with the same stable identity' };
  assert.equal(resolveDiagnosticFocus([selected, duplicateLooking], { identity }), -1,
    'duplicate-looking diagnostics clear focus instead of guessing');
  assert.equal(resolveDiagnosticFocus([selected], { identity, ambiguous: true }), -1,
    'a focus that began ambiguous stays cleared after results change');
});
