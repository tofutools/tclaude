import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

// The shared type-to-filter dropdown (filter-combobox.js). It has to stay
// usable over hundreds of values: ranked like the Ctrl-K palette, cut at a cap
// with the true count, grouped by kind and group, and never a flat list.

function catalogue(count) {
  const groups = ['crew', 'infra', 'web'];
  return Array.from({ length: count }, (_, i) => ({
    key: `${i % 2 ? 'board' : 'process_template'}:item-${i}`,
    label: `${i % 5 === 0 ? 'Release' : 'Roadmap'} ${i}`,
    kind: i % 2 ? 'board' : 'process_template',
    group: i % 7 === 0 ? '' : groups[i % groups.length],
  }));
}

async function mountCombobox(harness, props) {
  const { FilterCombobox } = await harness.importDashboardModule('js/filter-combobox.js');
  const changes = [];
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  // The value is held the way a caller holds it, so a choice re-renders.
  function Holder() {
    const [value, setValue] = harness.hooks.useState(props.value);
    return harness.html`<${FilterCombobox} ...${props} value=${value}
      onChange=${(next) => { changes.push(next); setValue(next); }} />`;
  }
  const mounted = await harness.mount(harness.html`<${Holder} />`, host);
  return { host, mounted, changes, input: host.querySelector('input[role="combobox"]') };
}

test('query prefixes narrow by kind and group, and ranking follows the palette', async (t) => {
  const harness = await createPreactHarness(t);
  const { filterComboItems, parseComboQuery } = await harness.importDashboardModule('js/filter-combobox.js');
  assert.deepEqual(parseComboQuery('board:rel group:infra x', ['board']), { kind: 'board', group: 'infra', text: 'rel x' });
  assert.deepEqual(parseComboQuery('nokind:rel', ['board']), { kind: '', group: '', text: 'nokind:rel' },
    'a prefix that names no kind is searched as typed');

  const items = catalogue(60);
  const boards = filterComboItems(items, 'board:release');
  assert.ok(boards.length > 0);
  assert.ok(boards.every((item) => item.kind === 'board' && /Release/.test(item.label)));
  assert.ok(filterComboItems(items, 'group:infra').every((item) => item.group === 'infra'));
  const ranked = filterComboItems([{ key: 'a', label: 'Unrelease' }, { key: 'b', label: 'Release notes' }], 'release');
  assert.deepEqual(ranked.map((item) => item.key), ['b', 'a'], 'a prefix hit outranks a mid-word one');
});

test('a long catalogue opens to a group summary and cuts results at the cap', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, input } = await mountCombobox(harness, {
    items: catalogue(300), value: [], cap: 8, kindLabels: { board: 'Boards', process_template: 'Process templates' },
  });
  assertAbsent(host.querySelector('[role="listbox"]'), 'closed until focused');
  await harness.act(() => harness.fireEvent(input, 'focus'));
  assert.equal(input.getAttribute('aria-expanded'), 'true');
  assert.equal(host.querySelectorAll('[role="option"]').length, 0, 'nothing typed: a summary, not rows');
  assert.ok(host.querySelectorAll('.filter-combobox-group-sum').length <= 5);
  assert.match(host.querySelector('.filter-combobox-tip').textContent, /300/);

  await harness.act(() => harness.fireEvent(host.querySelector('.filter-combobox-group-sum[data-group="crew"]'), 'mousedown'));
  assert.equal(input.value, 'group:crew ', 'a group in the summary narrows the search to it');

  await harness.input(input, 'roadmap');
  const options = host.querySelectorAll('[role="option"]');
  assert.equal(options.length, 8, 'at most cap rows render');
  assert.match(host.querySelector('.filter-combobox-foot').textContent, /Showing 8 of \d+ matches\. Refine your search/);
  const kinds = [...host.querySelectorAll('.filter-combobox-kind')].map((node) => node.textContent);
  assert.deepEqual([...new Set(kinds)], kinds, 'each kind heads one section');
  assert.ok(kinds.every((kind) => ['Boards', 'Process templates'].includes(kind)));
});

test('the keyboard moves, picks, toggles and completes; a disabled row is not choosable', async (t) => {
  const harness = await createPreactHarness(t);
  const items = [
    { key: 'board:plan', label: 'Plan', kind: 'board' },
    { key: 'board:planning', label: 'Planning', kind: 'board' },
    { key: 'board:locked', label: 'Planet', kind: 'board', disabled: true, reason: 'locked by the operator' },
  ];
  const { host, input, changes } = await mountCombobox(harness, { items, value: [] });
  await harness.act(() => harness.fireEvent(input, 'focus'));
  await harness.input(input, 'plan');
  assert.equal(input.getAttribute('aria-activedescendant'), host.querySelector('[role="option"]').id);
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'ArrowDown' }));
  assert.equal(host.querySelector('.highlighted').dataset.key, 'board:planning');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Tab' }));
  assert.equal(input.value, 'Planning', 'Tab completes to the highlighted row');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(changes.at(-1), ['board:planning']);
  assert.equal(input.value, '', 'choosing clears the search');

  const disabled = host.querySelector('[data-key="board:locked"]');
  assert.equal(disabled.getAttribute('aria-disabled'), 'true');
  assert.match(disabled.textContent, /locked by the operator/);
  await harness.act(() => harness.fireEvent(disabled, 'mousedown'));
  assert.equal(changes.length, 1, 'a disabled row changes nothing');

  await harness.act(() => harness.fireEvent(host.querySelector('[data-key="board:planning"]'), 'mousedown'));
  assert.deepEqual(changes.at(-1), [], 'choosing a chosen row again removes it');
});

test('single-select and the opt-in free-text commit', async (t) => {
  const harness = await createPreactHarness(t);
  const items = [{ key: 'alpha', label: 'alpha' }, { key: 'beta', label: 'beta' }];
  let opened = await mountCombobox(harness, { items, value: '', multi: false });
  await harness.act(() => harness.fireEvent(opened.input, 'focus'));
  await harness.act(() => harness.fireEvent(opened.host.querySelector('[data-key="beta"]'), 'mousedown'));
  assert.deepEqual(opened.changes, ['beta']);
  assertAbsent(opened.host.querySelector('[role="listbox"]'), 'a single pick closes the list');
  await harness.input(opened.input, 'gamma');
  await harness.act(() => harness.fireEvent(opened.input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(opened.changes, ['beta'], 'no free text unless opted in');
  await opened.mounted.unmount();

  opened = await mountCombobox(harness, { items, value: ['alpha'], freeText: (text) => (text.startsWith('g') ? text : '') });
  await harness.act(() => harness.fireEvent(opened.input, 'focus'));
  await harness.input(opened.input, 'gamma');
  assert.match(opened.host.querySelector('.filter-combobox-empty').textContent, /Enter.*adds “gamma”/);
  await harness.act(() => harness.fireEvent(opened.input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(opened.changes.at(-1), ['alpha', 'gamma']);
  await harness.input(opened.input, 'zeta');
  await harness.act(() => harness.fireEvent(opened.input, 'keydown', { key: 'Enter' }));
  assert.equal(opened.changes.length, 1, 'the free-text hook may refuse a value');
});
