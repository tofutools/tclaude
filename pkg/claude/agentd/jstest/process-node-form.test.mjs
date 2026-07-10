// Unit tests for the node dialogs' pure layer (TCL-298): the
// ProcessEditModel.updateNode gate in process-edit-model.js and the form
// helpers in process-node-form.js. Run with Node's built-in test runner via
// the Go jstest wrapper; no DOM — both modules are deliberately pure so the
// exact files shipped to the browser are exercised here.

import test from 'node:test';
import assert from 'node:assert/strict';
import { ProcessEditModel } from '../dashboard/js/process-edit-model.js';
import {
  PERFORMER_KINDS, PERFORMER_FIELDS, RETRY_ON_FAIL_MODES,
  performerFieldsFor, defaultPerformer, setPerformerKind, setPerformerField,
  setContactField, setRetryField, setStageEnabled, setPlanApproval,
  addCheck, removeCheck, moveCheck, setCheckID,
  setCaptures, setWaitField, setNodeText, parseLines, formatLines,
} from '../dashboard/js/process-node-form.js';

function view() {
  return {
    template: {
      apiVersion: 'tclaude.dev/v1alpha1',
      kind: 'ProcessTemplate',
      id: 'dialog-model',
      start: 'work',
      nodes: {
        work: {
          type: 'task',
          name: 'Work',
          performer: { kind: 'agent', profile: 'dev', prompt: 'Do the thing' },
          retry: { maxAttempts: 3, onFail: 'feedback-same-session' },
        },
        gate: {
          type: 'decision',
          performer: { kind: 'human', profile: 'operator', ask: 'Continue?' },
        },
        pause: { type: 'wait', wait: { duration: '5m' } },
        done: { type: 'end', result: 'success' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'work' },
      { from: 'work', outcome: 'pass', to: 'gate' },
      { from: 'gate', outcome: 'go', to: 'pause' },
      { from: 'pause', outcome: 'pass', to: 'done' },
    ],
    layout: { nodes: {} },
    sourceHash: 'src-1',
    semanticHash: 'sem-1',
  };
}

// ---- updateNode gate --------------------------------------------------------

test('updateNode commits through the undo gate and no-ops on unchanged drafts', () => {
  const model = new ProcessEditModel(view());
  assert.equal(model.updateNode('work', (node) => { node.performer.prompt = 'New prompt'; }), true);
  assert.equal(model.dirty, true);
  assert.equal(model.template.nodes.work.performer.prompt, 'New prompt');

  // Re-committing the same value is a no-op: no dirty flip, no undo slot.
  const undoDepth = model.undoStack.length;
  assert.equal(model.updateNode('work', (node) => { node.performer.prompt = 'New prompt'; }), false);
  assert.equal(model.undoStack.length, undoDepth);

  assert.ok(model.undo());
  assert.equal(model.template.nodes.work.performer.prompt, 'Do the thing');
  assert.equal(model.dirty, false, 'undo back to saved point reads clean');
});

test('updateNode mutators work on a draft: a throw never half-applies', () => {
  const model = new ProcessEditModel(view());
  assert.throws(() => model.updateNode('work', (node) => {
    node.performer.prompt = 'poisoned';
    throw new Error('boom');
  }), /boom/);
  assert.equal(model.template.nodes.work.performer.prompt, 'Do the thing');
  assert.equal(model.dirty, false);
});

test('updateNode enforces unknown-node and read-only guards', () => {
  const model = new ProcessEditModel(view(), { nodeEditable: (id) => id !== 'work' });
  assert.throws(() => model.updateNode('missing', () => {}), /unknown node/);
  assert.throws(() => model.updateNode('work', () => {}), /read-only/);
});

// ---- performer contract ------------------------------------------------------

test('every performer field is declared for all three kinds or explicitly kind-scoped', () => {
  for (const field of PERFORMER_FIELDS) {
    assert.ok(field.kinds.length > 0, `${field.key} declares kinds`);
    for (const kind of field.kinds) assert.ok(PERFORMER_KINDS.includes(kind), `${field.key} scopes to a real kind`);
  }
  // The kind-specific sets the dialog spec names, pinned:
  const keysFor = (kind) => performerFieldsFor(kind).map((field) => field.key);
  assert.deepEqual(keysFor('human'), ['profile', 'ask', 'choices', 'assignee', 'prompt', 'timeout']);
  assert.deepEqual(keysFor('agent'), ['profile', 'prompt', 'model', 'effort', 'timeout']);
  assert.deepEqual(keysFor('program'), ['profile', 'run', 'args', 'timeout']);
});

test('setPerformerKind prunes fields the new kind does not define, keeping common ones', () => {
  const performer = {
    kind: 'agent', profile: 'dev', prompt: 'Do it', model: 'opus', effort: 'high',
    timeout: '10m', contact: { cadence: '5m', budget: 3, escalationTarget: 'human:op' },
  };
  setPerformerKind(performer, 'program');
  assert.deepEqual(performer, {
    kind: 'program', profile: 'dev', timeout: '10m',
    contact: { cadence: '5m', budget: 3, escalationTarget: 'human:op' },
  });
  assert.throws(() => setPerformerKind(performer, 'robot'), /unknown performer kind/);
});

test('prompt survives the human<->agent switch (shared field), ask does not reach agent', () => {
  const performer = { kind: 'human', ask: 'Approve?', prompt: 'Context blob', choices: ['yes', 'no'] };
  setPerformerKind(performer, 'agent');
  assert.deepEqual(performer, { kind: 'agent', prompt: 'Context blob' });
});

test('setPerformerField trims, deletes blanks, and parses list fields', () => {
  const performer = defaultPerformer('human');
  setPerformerField(performer, 'ask', '  Ship it?  ');
  setPerformerField(performer, 'choices', 'ship\n\n hold ');
  assert.deepEqual(performer, { kind: 'human', ask: 'Ship it?', choices: ['ship', 'hold'] });
  setPerformerField(performer, 'choices', '');
  setPerformerField(performer, 'ask', '   ');
  assert.deepEqual(performer, { kind: 'human' });
  assert.throws(() => setPerformerField(performer, 'nonsense', 'x'), /unknown performer field/);
});

test('contact schedule edits build and dissolve the per-slot schedule', () => {
  const performer = defaultPerformer('agent');
  setContactField(performer, 'cadence', '30m');
  setContactField(performer, 'budget', '5');
  setContactField(performer, 'escalationTarget', 'human:oncall');
  assert.deepEqual(performer.contact, { cadence: '30m', budget: 5, escalationTarget: 'human:oncall' });
  setContactField(performer, 'budget', 'not-a-number');
  assert.deepEqual(performer.contact, { cadence: '30m', escalationTarget: 'human:oncall' });
  setContactField(performer, 'cadence', '');
  setContactField(performer, 'escalationTarget', '');
  assert.equal(performer.contact, undefined, 'empty schedule is removed (kind default applies)');
});

// ---- stages, checks, retry, captures -----------------------------------------

test('plan stage toggle mints a default step and approval policy is validated', () => {
  const node = { type: 'task', performer: { kind: 'agent', profile: 'dev', prompt: 'Do it' } };
  setStageEnabled(node, 'plan', true);
  assert.deepEqual(node.plan, { id: 'plan', performer: { kind: 'agent', profile: 'dev' }, approval: 'auto' });
  setPlanApproval(node, 'human');
  assert.equal(node.plan.approval, 'human');
  node.plan.approvalRetry = { maxAttempts: 2 };
  setPlanApproval(node, 'auto');
  assert.equal(node.plan.approvalRetry, undefined, 'approvalRetry only rides with approval: human');
  setStageEnabled(node, 'plan', false);
  assert.equal(node.plan, undefined);
  assert.throws(() => setPlanApproval(node, 'human'), /plan stage is not enabled/);
  assert.throws(() => setStageEnabled(node, 'work', true), /unknown stage/);
});

test('review stage toggle defaults to a human gate', () => {
  const node = { type: 'task', performer: { kind: 'agent', profile: 'dev', prompt: 'Do it' } };
  setStageEnabled(node, 'review', true);
  assert.deepEqual(node.review, { id: 'review', performer: { kind: 'human', profile: 'dev', ask: 'Approve?' } });
  // Toggling an already-enabled stage keeps the configured step.
  node.review.performer.ask = 'Merge?';
  setStageEnabled(node, 'review', true);
  assert.equal(node.review.performer.ask, 'Merge?');
});

test('checks are ordered with unique ids: add, rename, reorder, remove', () => {
  const node = { type: 'task', performer: { kind: 'agent', prompt: 'Do it' } };
  assert.equal(addCheck(node), 'check');
  assert.equal(addCheck(node, 'agent'), 'check-2');
  assert.deepEqual(node.checks.map((check) => check.performer.kind), ['program', 'agent']);
  setCheckID(node, 0, 'tests');
  assert.throws(() => setCheckID(node, 1, 'tests'), /duplicate check id/);
  assert.throws(() => setCheckID(node, 1, '  '), /check id is required/);
  moveCheck(node, 1, -1);
  assert.deepEqual(node.checks.map((check) => check.id), ['check-2', 'tests']);
  moveCheck(node, 0, -1);
  assert.deepEqual(node.checks.map((check) => check.id), ['check-2', 'tests'], 'reorder past the edge is a no-op');
  removeCheck(node, 0);
  removeCheck(node, 0);
  assert.equal(node.checks, undefined, 'empty checks list is removed');
  assert.throws(() => removeCheck(node, 0), /no check at/);
});

test('retry policy edits validate modes upstream and dissolve when cleared', () => {
  assert.deepEqual(RETRY_ON_FAIL_MODES, ['feedback-same-session', 'fresh-attempt']);
  const node = { type: 'task' };
  setRetryField(node, 'maxAttempts', '4');
  setRetryField(node, 'onFail', 'fresh-attempt');
  assert.deepEqual(node.retry, { maxAttempts: 4, onFail: 'fresh-attempt' });
  setRetryField(node, 'maxAttempts', '0');
  setRetryField(node, 'onFail', '');
  assert.equal(node.retry, undefined);
});

test('captures parse, de-dup, and clear', () => {
  const node = { type: 'task' };
  setCaptures(node, 'diff\n test-report \ndiff');
  assert.deepEqual(node.captures, ['diff', 'test-report']);
  setCaptures(node, '');
  assert.equal(node.captures, undefined);
});

test('wait config and node text edits delete blanks', () => {
  const node = { type: 'wait', wait: { duration: '5m' } };
  setWaitField(node, 'signal', 'release-cut');
  setWaitField(node, 'duration', '');
  assert.deepEqual(node.wait, { signal: 'release-cut' });
  setWaitField(node, 'signal', '');
  assert.equal(node.wait, undefined);
  setNodeText(node, 'doc', ' Why this node exists ');
  assert.equal(node.doc, 'Why this node exists');
  setNodeText(node, 'doc', '');
  assert.equal(node.doc, undefined);
  assert.throws(() => setNodeText(node, 'result', 'x'), /unknown node text field/);
});

test('parseLines/formatLines round-trip', () => {
  assert.deepEqual(parseLines(' a \n\n b\n'), ['a', 'b']);
  assert.equal(formatLines(['a', 'b']), 'a\nb');
  assert.equal(formatLines(undefined), '');
});

// ---- dialog flows through the model gate -------------------------------------

test('the dialog edit path: performer kind switch + retry change through updateNode', () => {
  const model = new ProcessEditModel(view());
  model.updateNode('work', (node) => {
    setPerformerKind(node.performer, 'program');
    setPerformerField(node.performer, 'run', 'go');
    setPerformerField(node.performer, 'args', 'test\n./...');
    setRetryField(node, 'maxAttempts', '5');
    setRetryField(node, 'onFail', 'fresh-attempt');
  });
  const work = model.template.nodes.work;
  assert.deepEqual(work.performer, { kind: 'program', profile: 'dev', run: 'go', args: ['test', './...'] });
  assert.deepEqual(work.retry, { maxAttempts: 5, onFail: 'fresh-attempt' });
  // The save payload carries the dialog's edit — the server round-trips it
  // into canonical YAML (asserted end-to-end by the Go flow test).
  const body = model.saveBody();
  assert.deepEqual(body.template.nodes.work.performer.kind, 'program');
  assert.ok(model.dirty);
});
