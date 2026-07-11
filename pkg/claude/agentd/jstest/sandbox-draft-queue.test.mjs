import test from 'node:test';
import assert from 'node:assert/strict';
import { createSandboxDraftQueue } from '../dashboard/js/sandbox-draft-queue.js';

test('two concurrent scribe drafts are reviewed sequentially without loss', () => {
  const delivered = [];
  let editorAvailable = true;
  const queue = createSandboxDraftQueue({
    canDeliver: () => editorAvailable,
    deliver: item => delivered.push(item),
  });

  assert.equal(queue.enqueue('scribe-a'), true);
  assert.deepEqual(delivered, ['scribe-a']);
  assert.equal(queue.enqueue('scribe-b'), false);
  assert.equal(queue.pendingCount(), 1);

  assert.equal(queue.release(), true);
  assert.deepEqual(delivered, ['scribe-a', 'scribe-b']);
  assert.equal(queue.pendingCount(), 0);
});

test('a draft waits for an unrelated open editor', () => {
  const delivered = [];
  let editorAvailable = false;
  const queue = createSandboxDraftQueue({
    canDeliver: () => editorAvailable,
    deliver: item => delivered.push(item),
  });

  assert.equal(queue.enqueue('scribe-a'), false);
  assert.deepEqual(delivered, []);
  editorAvailable = true;
  assert.equal(queue.poke(), true);
  assert.deepEqual(delivered, ['scribe-a']);
});
