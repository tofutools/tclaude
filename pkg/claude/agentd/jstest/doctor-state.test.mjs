import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Doctor is active only on Diagnostics › Doctor and counts what needs attention', async (t) => {
  const harness = await createPreactHarness(t);
  const { createDoctorState } = await harness.importDashboardModule('js/doctor-state.js');
  const activeTab = harness.signals.signal('groups');
  const subtab = harness.signals.signal('audit');
  const state = createDoctorState({ activeTab, subtab });

  assert.equal(state.active.value, false);
  activeTab.value = 'diagnostics';
  assert.equal(state.active.value, false, 'another Diagnostics view is not the Doctor view');
  subtab.value = 'doctor';
  assert.equal(state.active.value, true);
  activeTab.value = 'config';
  assert.equal(state.active.value, false, 'a remembered Doctor subtab is inactive off the Diagnostics tab');

  state.checks.value = [
    { id: 'sandbox-default', category: 'decisions', severity: 'decision', title: 'Choose' },
    { id: 'bwrap', category: 'sandbox', severity: 'blocking', title: 'bwrap' },
    { id: 'hooks-claude', category: 'setup', severity: 'warning', title: 'hooks' },
    { id: 'git', category: 'tools', severity: 'ok', title: 'git' },
  ];
  const counts = state.counts.value;
  assert.equal(counts.decision, 1);
  assert.equal(counts.blocking, 1);
  assert.equal(counts.attention, 3, 'ok/info never need attention');
  assert.deepEqual(state.visible.value.map((c) => c.id), ['sandbox-default', 'bwrap', 'hooks-claude']);
  state.category.value = 'tools';
  assert.deepEqual(state.visible.value.map((c) => c.id), ['git']);
});
