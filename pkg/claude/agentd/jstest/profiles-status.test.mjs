import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('disabled profile detail status takes precedence over operator-only', async (t) => {
  const harness = await createPreactHarness(t);
  const { profileDetailChips } = await harness.importDashboardModule('js/profiles.js');
  assert.deepEqual(
    profileDetailChips({ disabled: true, disabled_reason: 'maintenance', operator_only: true }),
    ['🚫 disabled · maintenance'],
  );
  assert.deepEqual(profileDetailChips({ operator_only: true }), ['👤 operator only']);
});
