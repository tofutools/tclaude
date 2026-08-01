import assert from 'node:assert/strict';
import test from 'node:test';

import {
  sandboxProfileSummary,
  sandboxResourceLimitErrors,
  sandboxResourceLimitsForWire,
} from '../dashboard/js/sandbox-profiles-data.js';

test('resource limits trim their authored spelling and serialize CPU as a number', () => {
  assert.deepEqual(
    sandboxResourceLimitsForWire({ memory: ' 4GiB ', cpu: '0.5' }),
    { memory: '4GiB', cpu: 0.5 },
  );
  assert.deepEqual(sandboxResourceLimitsForWire({ memory: '', cpu: '' }), {});
});

test('resource limit validation matches the editor contract', () => {
  assert.deepEqual(sandboxResourceLimitErrors({ memory: '512mIb', cpu: '2.5' }), []);
  assert.match(sandboxResourceLimitErrors({ memory: '512' })[0], /unit/);
  assert.match(sandboxResourceLimitErrors({ memory: 0 })[0], /unit/);
  assert.deepEqual(sandboxResourceLimitsForWire({ memory: 0 }), { memory: '0' });
  assert.match(sandboxResourceLimitErrors({ memory: '0GiB' })[0], /greater than zero/);
  assert.match(sandboxResourceLimitErrors({ memory: '0gib' })[0], /greater than zero/);
  assert.match(sandboxResourceLimitErrors({ cpu: '500m' })[0], /cores/);
  assert.match(sandboxResourceLimitErrors({ cpu: '0.009' })[0], /at least 0.01/);
});

test('profile summary discloses configured resource limits', () => {
  assert.match(
    sandboxProfileSummary({ resource_limits: { memory: '4GiB', cpu: 2 } }),
    /memory 4GiB .* CPU 2/,
  );
});
