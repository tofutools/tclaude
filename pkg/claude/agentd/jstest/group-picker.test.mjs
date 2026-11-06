// tcl-kduiih: every group choice is a type-to-filter box (GroupCombobox, on
// ChoiceCombobox), so hundreds of groups never render as one flat <select>.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function mountGroups(harness, props) {
  const { GroupCombobox } = await harness.importDashboardModule('js/group-picker.js');
  const changes = [];
  function Holder() {
    const [value, setValue] = harness.hooks.useState(props.value ?? '');
    return harness.html`<form onSubmit=${(event) => { event.preventDefault(); changes.push('submit'); }}>
      <label class="row">Group <${GroupCombobox} id="g" ...${props} value=${value} onChange=${(next) => { changes.push(next); setValue(next); }} /></label></form>`;
  }
  const { container } = await harness.mount(harness.html`<${Holder} />`);
  const input = container.querySelector('#g-input');
  return { container, changes, input, box: () => container.querySelector('#g') };
}

test('hundreds of groups open to a summary, and typing finds one to pick', async (t) => {
  const harness = await createPreactHarness(t);
  const groups = Array.from({ length: 200 }, (_, i) => ({ value: `team-${i}`, parent: i % 2 ? 'web' : 'infra' }));
  const { container, changes, input, box } = await mountGroups(harness, { groups });
  await harness.act(() => harness.fireEvent(input, 'focus'));
  assert.equal(container.querySelectorAll('[role="option"]').length, 0, 'nothing typed: no flat list of 200');
  await harness.input(input, 'team-17');
  assert.equal(container.querySelector('[role="option"]').dataset.key, 'team-17');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(changes, ['team-17']);
  assert.equal(box().dataset.value, 'team-17');
  harness.fireEvent(input, 'blur');
  await harness.act(() => Promise.resolve());
  assert.equal(input.value, 'team-17', 'the box shows the chosen group, as a <select> did');
});

test('a none label makes the choice clearable, and a lost group stays marked', async (t) => {
  const harness = await createPreactHarness(t);
  const { container, changes, box } = await mountGroups(harness, { groups: ['crew', 'ops'], value: 'gone', noneLabel: 'No group' });
  assert.equal(container.querySelector('#g-input').value, 'gone (no such group)');
  const label = container.querySelector('label.row');
  assert.equal([...label.querySelectorAll('button, input')][0].id, 'g-input', 'the label\'s first control is the box, not ×');
  await harness.act(() => harness.fireEvent(container.querySelector('.choice-combobox-clear'), 'click'));
  assert.deepEqual(changes, ['']);
  assert.equal(box().dataset.value, '');
  assert.equal(container.querySelector('#g-input').value, 'No group');
  assert.equal(container.querySelector('.choice-combobox-clear'), null, 'nothing to clear');
});

test('without a none label a choice cannot be emptied; an action box stays empty', async (t) => {
  const harness = await createPreactHarness(t);
  const required = await mountGroups(harness, { groups: ['crew', 'ops'], value: 'crew' });
  assert.equal(required.container.querySelector('.choice-combobox-clear'), null);
  const enter = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperty(enter, 'key', { value: 'Enter' });
  await harness.input(required.input, 'zzz');
  await harness.act(() => required.input.dispatchEvent(enter));
  assert.equal(enter.defaultPrevented, true, 'Enter does not submit the form');
  assert.deepEqual(required.changes, []);

  const menu = await mountGroups(harness, { groups: ['crew', 'ops'], action: true });
  await harness.input(menu.input, 'ops');
  await harness.act(() => harness.fireEvent(menu.input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(menu.changes, ['ops']);
  assert.equal(menu.box().dataset.value, '', 'a menu does not keep a value');
  assert.equal(menu.input.value, '', 'nor shows one');
});

test('a chosen group stays chosen when picked again, and typing after a pick starts a search', async (t) => {
  const harness = await createPreactHarness(t);
  const { container, changes, input, box } = await mountGroups(harness, { groups: ['crew', 'crane', 'ops'], value: 'crew' });
  await harness.act(() => harness.fireEvent(input, 'focus'));
  assert.equal(container.querySelector('[role="listbox"]'), null, 'focus alone does not open it, as a dialog focuses it on open');
  await harness.act(() => harness.fireEvent(input, 'click'));
  await harness.act(() => harness.fireEvent(container.querySelector('[data-key="crew"]'), 'mousedown'));
  assert.deepEqual(changes, [], 'picking the chosen row again changes nothing, as in a <select>');
  assert.equal(box().dataset.value, 'crew');
  // Closed and focused, the box shows "crew"; typing adds to it in the DOM.
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Escape' }));
  await harness.input(input, 'crewra');
  assert.deepEqual([...container.querySelectorAll('[role="option"]')].map((row) => row.dataset.key), ['crane'],
    'what was typed after the label is the search');
});
