import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function memoryPrefs(seed) {
  const values = new Map(Object.entries(seed));
  return {
    getItem: (key) => values.has(key) ? values.get(key) : null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
    values,
  };
}

test('group rename migrates every name-keyed dashboard preference', async (t) => {
  const harness = await createPreactHarness(t);
  const { migrateGroupRenamePrefs } = await harness.importDashboardModule('js/group-rename-prefs.js');
  const prefs = memoryPrefs({
    'tclaude.dash.group.alpha': '1',
    'tclaude.dash.group.offline.alpha': 'hide',
    'tclaude.dash.quickpin.alpha': '1',
    'tclaude.dash.forcefold.alpha': '1',
    'tclaude.dash.mail.groupexp.alpha': '1',
    'tclaude.dash.groupOrder': JSON.stringify(['gamma', 'alpha', 'beta']),
    'tclaude.dash.spawn.lastGroup': 'alpha',
    'tclaude.dash.mail.mailbox': 'group:alpha',
  });

  migrateGroupRenamePrefs('alpha', 'delta', prefs);

  for (const prefix of [
    'tclaude.dash.group.',
    'tclaude.dash.group.offline.',
    'tclaude.dash.quickpin.',
    'tclaude.dash.forcefold.',
    'tclaude.dash.mail.groupexp.',
  ]) {
    assert.equal(prefs.getItem(prefix + 'alpha'), null);
    assert.notEqual(prefs.getItem(prefix + 'delta'), null);
  }
  assert.deepEqual(JSON.parse(prefs.getItem('tclaude.dash.groupOrder')), ['gamma', 'delta', 'beta']);
  assert.equal(prefs.getItem('tclaude.dash.spawn.lastGroup'), 'delta');
  assert.equal(prefs.getItem('tclaude.dash.mail.mailbox'), 'group:delta');
});

test('group rename leaves unrelated and malformed aggregate preferences unchanged', async (t) => {
  const harness = await createPreactHarness(t);
  const { migrateGroupRenamePrefs } = await harness.importDashboardModule('js/group-rename-prefs.js');
  const prefs = memoryPrefs({
    'tclaude.dash.groupOrder': '{bad',
    'tclaude.dash.spawn.lastGroup': 'beta',
    'tclaude.dash.mail.mailbox': 'group:beta',
  });

  migrateGroupRenamePrefs('alpha', 'delta', prefs);

  assert.equal(prefs.getItem('tclaude.dash.groupOrder'), '{bad');
  assert.equal(prefs.getItem('tclaude.dash.spawn.lastGroup'), 'beta');
  assert.equal(prefs.getItem('tclaude.dash.mail.mailbox'), 'group:beta');
});

test('group rename clears stale destination state and keeps the source order position', async (t) => {
  const harness = await createPreactHarness(t);
  const { migrateGroupRenamePrefs } = await harness.importDashboardModule('js/group-rename-prefs.js');
  const prefs = memoryPrefs({
    'tclaude.dash.quickpin.delta': '1',
    'tclaude.dash.forcefold.delta': '1',
    'tclaude.dash.groupOrder': JSON.stringify(['gamma', 'delta', 'beta', 'alpha', 'omega']),
    'tclaude.dash.spawn.lastGroup': 'delta',
    'tclaude.dash.mail.mailbox': 'group:delta',
  });

  migrateGroupRenamePrefs('alpha', 'delta', prefs);

  assert.equal(prefs.getItem('tclaude.dash.quickpin.delta'), null);
  assert.equal(prefs.getItem('tclaude.dash.forcefold.delta'), null);
  assert.deepEqual(JSON.parse(prefs.getItem('tclaude.dash.groupOrder')),
    ['gamma', 'beta', 'delta', 'omega']);
  assert.equal(prefs.getItem('tclaude.dash.spawn.lastGroup'), null);
  assert.equal(prefs.getItem('tclaude.dash.mail.mailbox'), null);
});
