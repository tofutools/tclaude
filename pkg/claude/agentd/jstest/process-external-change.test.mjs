import test from 'node:test';
import assert from 'node:assert/strict';
import {
  NO_EXTERNAL_CHANGE, keepExternalChange, reconcileExternalChange,
} from '../dashboard/js/process-external-change.js';

const observe = (previous, loadedRef, currentRef, dirty = false) => reconcileExternalChange(previous, {
  loadedRef, currentRef, dirty,
});

test('external ref comparison ignores blanks and the loaded head', () => {
  assert.equal(observe(NO_EXTERNAL_CHANGE, '', 'release@sha256:new'), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', ''), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a'), NO_EXTERNAL_CHANGE);
});

test('a moved head distinguishes clean and dirty buffers', () => {
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b'), {
    kind: 'clean', ref: 'release@sha256:b',
  });
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b', true), {
    kind: 'dirty', ref: 'release@sha256:b',
  });
});

test('banner state follows buffer dirtiness while the same external head is pending', () => {
  const clean = observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b');
  const dirty = observe(clean, 'release@sha256:a', 'release@sha256:b', true);
  assert.deepEqual(dirty, { kind: 'dirty', ref: 'release@sha256:b' });
  assert.deepEqual(observe(dirty, 'release@sha256:a', 'release@sha256:b'), {
    kind: 'clean', ref: 'release@sha256:b',
  });
});

test('Keep editing dismisses only the observed ref and a later head resurfaces', () => {
  const dirty = observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b', true);
  const kept = keepExternalChange(dirty);
  assert.deepEqual(kept, { kind: 'kept', ref: 'release@sha256:b' });
  assert.equal(observe(kept, 'release@sha256:a', 'release@sha256:b', true), kept);
  assert.deepEqual(observe(kept, 'release@sha256:a', 'release@sha256:c', true), {
    kind: 'dirty', ref: 'release@sha256:c',
  });
  assert.equal(observe(kept, 'release@sha256:b', 'release@sha256:b', true), NO_EXTERNAL_CHANGE);
});
