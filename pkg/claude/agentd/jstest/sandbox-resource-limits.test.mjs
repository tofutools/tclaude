import assert from 'node:assert/strict';
import test from 'node:test';

import {
  sandboxProfileSummary,
  sandboxResourceLimitErrors,
  sandboxResourceLimitsForWire,
} from '../dashboard/js/sandbox-profiles-data.js';

test('resource limits trim their authored spelling and serialize CPU as a number', () => {
  assert.deepEqual(
    sandboxResourceLimitsForWire({ memory: ' 4GiB ', cpu: '0.5', pids: ' 512 ' }),
    { memory: '4GiB', cpu: 0.5, pids: 512 },
  );
  assert.deepEqual(sandboxResourceLimitsForWire({ memory: '', cpu: '', pids: '' }), {});
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
  assert.deepEqual(sandboxResourceLimitErrors({ pids: '512' }), []);
  assert.match(sandboxResourceLimitErrors({ pids: '0' })[0], /between 1 and/);
  assert.match(sandboxResourceLimitErrors({ pids: '2.5' })[0], /whole number/);
  assert.match(sandboxResourceLimitErrors({ pids: '-1' })[0], /whole number/);
});

// A count JavaScript cannot represent must not be waved through: Number() turns
// it into Infinity, JSON.stringify writes that as null, and the server decodes
// null as no ceiling at all — the authored restriction silently deleted on save.
test('an unrepresentable PID count is refused rather than serialized away', () => {
  const huge = '9'.repeat(309);
  assert.equal(Number.isFinite(Number(huge)), false, 'the hazard this pins is real');
  assert.match(sandboxResourceLimitErrors({ pids: huge })[0], /between 1 and/);
  assert.match(sandboxResourceLimitErrors({ pids: String(Number.MAX_SAFE_INTEGER + 2) })[0], /between 1 and/);
  assert.deepEqual(sandboxResourceLimitErrors({ pids: String(Number.MAX_SAFE_INTEGER) }), [],
    'the representable ceiling itself still saves');
});

test('profile summary discloses configured resource limits', () => {
  assert.match(
    sandboxProfileSummary({ resource_limits: { memory: '4GiB', cpu: 2, pids: 512 } }),
    /memory 4GiB .* CPU 2 .* PIDs 512/,
  );
});

test('profile summary discloses Darwin Mach registration', () => {
  assert.equal(
    sandboxProfileSummary({ darwin_allow_mach_register: true }),
    'Mach registration',
  );
});
