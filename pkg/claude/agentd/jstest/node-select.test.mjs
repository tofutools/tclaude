// tcl-vwuc0b: the one node selector — a dropdown with a checkbox per node
// (multiple) or one pick (single), the same on every tab that narrows by node.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const nodes = [
  { id: 'inst_self', name: 'desk', color: '#58a6ff', local: true, online: true },
  { id: 'inst_forge', name: 'forge', color: '#f0883e', online: true },
  { id: 'inst_lab', name: 'lab', color: '#39c5cf', online: false },
];

test('values, summaries and toggles: every node ticked, or none, is all', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/node-select.js');
  assert.equal(m.nodeSelectSummary(nodes, [], { multiple: true }), 'All nodes · 3');
  assert.equal(m.nodeSelectSummary(nodes, ['inst_self'], { multiple: true }), '⌂ desk');
  assert.equal(m.nodeSelectSummary(nodes, ['inst_forge', 'inst_lab'], { multiple: true }), 'forge, lab');
  assert.equal(m.nodeSelectSummary([...nodes, { id: 'inst_four', name: 'four' }], ['inst_self', 'inst_forge', 'inst_lab'], { multiple: true }), '3 of 4 nodes');
  assert.equal(m.nodeSelectSummary(nodes, 'inst_lab'), 'lab');
  assert.equal(m.nodeSelectSummary(nodes, ''), 'All nodes · 3');
  assert.deepEqual(m.toggledValue(nodes, [], 'inst_lab'), ['inst_self', 'inst_forge'], 'unticking one of all');
  assert.deepEqual(m.toggledValue(nodes, ['inst_self', 'inst_forge'], 'inst_lab'), [], 'ticking the last one back is all');
  assert.deepEqual(m.toggledValue(nodes, ['inst_forge'], 'inst_forge'), [], 'unticking the only one is all');
});

test('multiple: checkboxes, only, all, badges and notes; single: one pick closes it; locks hold', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/node-select.js');
  const picks = [];
  let mounted = await harness.mount(harness.html`<${m.NodeSelect} id="ns" multiple label="Mail from" nodes=${nodes} value=${['inst_self', 'inst_forge']}
    onChange=${(v) => picks.push(v)} badge=${(n) => (n.id === 'inst_forge' ? 4 : 0)} note=${(n) => (n.id === 'inst_lab' ? 'offline' : '')} />`);
  const q = (s) => mounted.container.querySelector(s);
  const click = (el) => harness.act(() => harness.fireEvent(el, 'click'));
  assert.match(q('.node-select-button').textContent, /Mail from3? ?⌂ desk, forge\s*4/, 'the summary and the picked nodes\' unread');
  assert.equal(q('.node-select-menu'), null);
  await click(q('.node-select-button'));
  assert.equal(q('[data-node-option="inst_forge"] input').hasAttribute('checked'), true);
  assert.equal(q('[data-node-option="inst_lab"] input').hasAttribute('checked'), false);
  assert.match(q('[data-node-option="inst_lab"]').textContent, /offline/);
  await click(q('[data-node-option="inst_lab"] .node-pop-name'));
  await click(q('[data-node-option="inst_forge"] .node-pop-only'));
  await click(q('[data-node-option=""] .node-pop-name'));
  assert.deepEqual(picks, [[], ['inst_forge'], []]);
  assert.ok(q('.node-select-menu'), 'picking several keeps it open');
  // A pick the caller ignores repaints the boxes from the value.
  const box = q('[data-node-option="inst_lab"] input');
  box.checked = true;
  await harness.act(() => harness.fireEvent(box, 'change'));
  assert.equal(box.checked, false, 'lab is still not picked');
  await mounted.unmount();

  picks.length = 0;
  mounted = await harness.mount(harness.html`<${m.NodeSelect} id="one" nodes=${nodes} value="" onChange=${(v) => picks.push(v)}
    lock=${(n) => (n.online ? '' : 'offline: nothing to show')} />`);
  await click(q('.node-select-button'));
  assert.equal(mounted.container.querySelector('[data-node-option="inst_lab"] .node-pop-name').disabled, true);
  await click(mounted.container.querySelector('[data-node-option="inst_lab"] .node-pop-name'));
  assert.deepEqual(picks, [], 'a locked node is not picked');
  await click(mounted.container.querySelector('[data-node-option="inst_forge"] .node-pop-name'));
  assert.deepEqual(picks, ['inst_forge']);
  assert.equal(mounted.container.querySelector('.node-select-menu'), null, 'one pick closes it');
  await mounted.unmount();
});

test('a long list offers a filter', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/node-select.js');
  const many = Array.from({ length: m.NODE_SELECT_FILTER_AT }, (_, i) => ({ id: `inst_n${i}aaa`, name: `node-${i}`, online: true }));
  const mounted = await harness.mount(harness.html`<${m.NodeSelect} multiple nodes=${many} value=${[]} onChange=${() => {}} />`);
  const c = mounted.container;
  await harness.act(() => harness.fireEvent(c.querySelector('.node-select-button'), 'click'));
  const box = c.querySelector('.node-pop-filter input');
  await harness.act(() => { box.value = 'node-3'; harness.fireEvent(box, 'input'); });
  assert.deepEqual([...c.querySelectorAll('[data-node-option]')].map((el) => el.dataset.nodeOption), ['', 'inst_n3aaa']);
  await mounted.unmount();
});
