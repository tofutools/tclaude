import test from 'node:test';
import assert from 'node:assert/strict';
import {
  NO_EXTERNAL_CHANGE, keepExternalChange, reconcileExternalChange, templateHeadSignature,
} from '../dashboard/js/process-external-change.js';

const observe = (previous, loadedRef, currentRef, dirty = false, loadedSourceHash = 'source-a', currentSourceHash = 'source-b') => reconcileExternalChange(previous, {
  loadedRef, loadedSourceHash, currentRef, currentSourceHash, dirty,
});

test('template head signatures are stable across ordering and include set changes', () => {
  const a = templateHeadSignature([{ id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }, { id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-a' }]);
  const b = templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-a' }, { id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }]);
  assert.equal(a, b);
  assert.notEqual(a, templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-a' }]));
  assert.notEqual(a, templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:3', sourceHash: 'source-c' }, { id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }]));
  assert.notEqual(a, templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-new' }, { id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }]));
});

test('external generation comparison ignores blanks and the loaded head', () => {
  assert.equal(observe(NO_EXTERNAL_CHANGE, '', 'release@sha256:new'), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', ''), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', false, '', 'source-b'), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', false, 'source-a', ''), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', false, 'source-a', 'source-a'), NO_EXTERNAL_CHANGE);
});

test('a moved head distinguishes clean and dirty buffers', () => {
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b'), {
    kind: 'clean', ref: 'release@sha256:b', sourceHash: 'source-b',
  });
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b', true), {
    kind: 'dirty', ref: 'release@sha256:b', sourceHash: 'source-b',
  });
});

test('a source-only generation change distinguishes clean and dirty buffers', () => {
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', false, 'source-a', 'source-b'), {
    kind: 'clean', ref: 'release@sha256:a', sourceHash: 'source-b',
  });
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', true, 'source-a', 'source-b'), {
    kind: 'dirty', ref: 'release@sha256:a', sourceHash: 'source-b',
  });
});

test('banner state follows buffer dirtiness while the same external head is pending', () => {
  const clean = observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b');
  const dirty = observe(clean, 'release@sha256:a', 'release@sha256:b', true);
  assert.deepEqual(dirty, { kind: 'dirty', ref: 'release@sha256:b', sourceHash: 'source-b' });
  assert.deepEqual(observe(dirty, 'release@sha256:a', 'release@sha256:b'), {
    kind: 'clean', ref: 'release@sha256:b', sourceHash: 'source-b',
  });
});

test('Keep editing dismisses only the observed ref and a later head resurfaces', () => {
  const dirty = observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b', true);
  const kept = keepExternalChange(dirty);
  assert.deepEqual(kept, { kind: 'kept', ref: 'release@sha256:b', sourceHash: 'source-b' });
  assert.equal(observe(kept, 'release@sha256:a', 'release@sha256:b', true), kept);
  assert.deepEqual(observe(kept, 'release@sha256:a', 'release@sha256:c', true), {
    kind: 'dirty', ref: 'release@sha256:c', sourceHash: 'source-b',
  });
  assert.deepEqual(observe(kept, 'release@sha256:a', 'release@sha256:b', true, 'source-a', 'source-c'), {
    kind: 'dirty', ref: 'release@sha256:b', sourceHash: 'source-c',
  });
  assert.equal(observe(kept, 'release@sha256:b', 'release@sha256:b', true, 'source-b', 'source-b'), NO_EXTERNAL_CHANGE);
});
