// Selecting steps in the inline editor (groups-mini-editor.js) and removing
// them with Delete after a confirmation (tk_d90d2b7a): click, shift/ctrl-click,
// a box dragged on the empty canvas, Escape.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function threeSteps() {
  const task = (name) => ({ type: 'task', name, performer: { kind: 'agent', role: 'dev', prompt: `Do ${name}.` } });
  return {
    template: {
      id: 'tpl_1', name: 'Ship it', start: 'start',
      nodes: { start: { type: 'start' }, a: task('A'), b: task('B'), c: task('C'), done: { type: 'end', result: 'success' } },
    },
    edges: [
      { from: '', outcome: 'start', to: 'start' },
      { from: 'start', outcome: 'next', to: 'a' },
      { from: 'a', outcome: 'next', to: 'b' },
      { from: 'b', outcome: 'next', to: 'c' },
      { from: 'c', outcome: 'next', to: 'done' },
    ],
    layout: { nodes: {} },
    sourceHash: 'h1', currentRef: 'tpl_1@sha256:a',
  };
}

async function mountEditor(t, confirm) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-mini-editor.js');
  const saves = [];
  const api = { saveTemplate: async (id, draft) => { saves.push(structuredClone(draft)); return { ref: 'tpl_1@sha256:s', sourceHash: 'h2' }; } };
  const stored = threeSteps();
  const mounted = await harness.mount(harness.html`<${mod.MiniGraphEditor} group=${{ name: 'webshop' }} roster=${[]}
    process=${{ templateId: 'tpl_1', name: 'Ship it', ran: true }} view=${stored} stored=${stored} api=${api} confirm=${confirm}
    onClose=${() => {}} onSteps=${() => {}} />`);
  const root = mounted.container;
  const node = (id) => root.querySelector(`[data-process-node="${id}"]`);
  const click = (id, init = {}) => harness.act(async () => { harness.fireEvent(node(id), 'click', init); });
  const key = (target, name) => harness.act(async () => { harness.fireEvent(target, 'keydown', { key: name }); });
  const picked = () => [...root.querySelectorAll('.pk-gnode.selected')].map((el) => el.dataset.processNode);
  return { harness, root, node, click, key, picked };
}

test('click picks one, shift/ctrl-click adds, Delete asks, then removes them as one undoable edit', async (t) => {
  const asked = [];
  let answer = false;
  const { harness, root, node, click, key, picked } = await mountEditor(t, async (options) => { asked.push(options); return answer; });
  await click('a');
  assert.deepEqual(picked(), ['a']);
  assert.ok(root.querySelector('[data-mini-inspector="a"]'), 'one step opens its inspector');
  await click('c', { shiftKey: true });
  assert.deepEqual(picked(), ['a', 'c']);
  assert.equal(root.querySelector('[data-mini-inspector]'), null, 'several: no inspector');
  await click('b', { ctrlKey: true });
  await click('c', { ctrlKey: true });
  assert.deepEqual(picked(), ['a', 'b'], 'ctrl-click takes one back out');

  // Typing in a field never deletes.
  await key(root.querySelector('input') || root, 'Delete');
  // Cancel keeps everything.
  await key(node('a'), 'Delete');
  assert.equal(asked.length, 1);
  assert.equal(asked[0].title, 'Remove 2 steps: A, B?');
  assert.match(asked[0].body, /stays connected/);
  assert.ok(node('a') && node('b'));
  assert.deepEqual(picked(), ['a', 'b'], 'a cancelled delete keeps the selection');

  answer = true;
  await key(node('a'), 'Backspace');
  assert.equal(node('a'), null);
  assert.equal(node('b'), null);
  assert.ok(node('c'));
  assert.deepEqual(picked(), []);
  // One edit: one undo brings both back.
  await harness.act(async () => { root.querySelector('[data-mini-undo]').click(); });
  assert.ok(node('a') && node('b'), 'one undo restores both');
});

test('a box on the empty canvas selects what it touches; the start and a used end are kept, with a note', async (t) => {
  const asked = [];
  const { harness, root, node, key, picked } = await mountEditor(t, async (options) => { asked.push(options); return true; });
  const canvas = root.querySelector('[data-mini-canvas]');
  const pointer = (type, x, y, init = {}) => harness.act(async () => {
    harness.fireEvent(canvas, type, { button: 0, pointerType: 'mouse', pointerId: 1, clientX: x, clientY: y, ...init });
  });
  await pointer('pointerdown', 0, 0);
  await pointer('pointermove', 4000, 4000);
  assert.ok(root.querySelector('[data-mini-marquee]'), 'the box is drawn while dragging');
  await pointer('pointerup', 4000, 4000);
  await harness.act(async () => { harness.fireEvent(canvas, 'click'); });
  assert.equal(root.querySelector('[data-mini-marquee]'), null);
  assert.deepEqual(picked().sort(), ['a', 'b', 'c', 'done', 'start'], 'the click ending the box does not clear it');

  await key(canvas, 'Escape');
  assert.deepEqual(picked(), [], 'Escape clears it');

  // A touch drag scrolls; it draws no box.
  await pointer('pointerdown', 0, 0, { pointerType: 'touch' });
  await pointer('pointermove', 4000, 4000, { pointerType: 'touch' });
  assert.equal(root.querySelector('[data-mini-marquee]'), null);
  await pointer('pointerup', 4000, 4000, { pointerType: 'touch' });

  await pointer('pointerdown', 0, 0);
  await pointer('pointermove', 4000, 4000);
  await pointer('pointerup', 4000, 4000);
  await key(canvas, 'Delete');
  assert.equal(asked.length, 1);
  assert.equal(asked[0].title, 'Remove 3 steps: A, B, C?');
  assert.match(asked[0].body, /Kept: .*start cannot be deleted.*steps still end here/);
  assert.equal(node('a'), null);
  assert.ok(node('start') && node('done'));
});
