import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { comboPick } from './combobox-helpers.mjs';

// The template, role and saved-prompt choices (registry-picker.js,
// tcl-986r2g): registries grow to tens of entries, so these are
// type-to-filter boxes, not <select>s.

test('namedItems name each entry, hint its description, and find it by that too', async (t) => {
  const harness = await createPreactHarness(t);
  const { namedItems } = await harness.importDashboardModule('js/registry-picker.js');
  const items = namedItems([
    { name: 'reviewer', descr: 'Checks\\n  correctness' },
    { name: 'tpl_a', label: 'Alpha', description: 'the alpha process', group: 'team' },
    { name: 'brief', body: 'Please review the diff.' },
    { name: 'gone', unpickable: 'not in this library' },
    '', null, 'plain',
  ]);
  assert.deepEqual(items.map((item) => [item.key, item.label, item.hint, item.group]), [
    ['reviewer', 'reviewer', 'Checks\\n correctness', ''],
    ['tpl_a', 'Alpha', 'the alpha process', 'team'],
    ['brief', 'brief', 'Please review the diff.', ''],
    ['gone', 'gone', '', ''],
    ['plain', 'plain', '', ''],
  ]);
  assert.equal(items[3].disabled, true);
  assert.equal(items[3].reason, 'not in this library');
  assert.equal(namedItems([{ name: 'x', descr: 'd' }], { hint: (e) => `hint ${e.name}` })[0].hint, 'hint x');
});

test('a role is found by typing and picked; an add box stays empty after a pick', async (t) => {
  const harness = await createPreactHarness(t);
  const { RoleCombobox } = await harness.importDashboardModule('js/registry-picker.js');
  const roles = Array.from({ length: 50 }, (_, i) => ({ name: `role-${i}`, descr: i === 37 ? 'the release captain' : '' }));
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const changes = [];
  const mounted = await harness.mount(harness.html`<${RoleCombobox} id="add" action=${true} value="" entries=${roles}
    placeholder="＋ Add role…" onChange=${(next) => changes.push(next)} />`, host);
  const box = host.querySelector('#add');
  await harness.input(box.querySelector('input[role="combobox"]'), 'release captain');
  assert.deepEqual([...box.querySelectorAll('[role="option"]')].map((row) => row.dataset.key), ['role-37'], 'found by its description');
  await comboPick(harness, box, 'role-37');
  assert.deepEqual(changes, ['role-37']);
  assert.equal(box.querySelector('input').value, '', 'an add box is a menu: it stays empty');
  await mounted.unmount();
});
