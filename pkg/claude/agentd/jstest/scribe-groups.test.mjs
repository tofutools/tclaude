import test from 'node:test';
import assert from 'node:assert/strict';
import { scribeGroupVisible } from '../dashboard/js/scribe-groups.js';

test('ordinary groups are always visible', () => {
  assert.equal(scribeGroupVisible({ scribe: false, online: 0 }), true);
});

test('a scribe group with an online member is always visible', () => {
  assert.equal(scribeGroupVisible({ scribe: true, online: 1 }), true);
  assert.equal(scribeGroupVisible({ scribe: true, online: 2 }, false), true);
});

test('an offline scribe group follows the opt-in preference', () => {
  assert.equal(scribeGroupVisible({ scribe: true, online: 0 }), false);
  assert.equal(scribeGroupVisible({ scribe: true, online: 0 }, true), true);
});
