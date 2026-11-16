// Process draft hygiene in the inline editor (groups-mini-editor.js and
// process-step-ids.js): a step's id follows its name until the process runs,
// a draft with no steps is not kept when it is left, and a name another
// process of the group has is flagged.

import test from 'node:test';
import assert from 'node:assert/strict';
import { installFastClock } from './fast-clock.mjs';
import { createPreactHarness } from './preact-harness.mjs';
import { followName, rekeyStep, stepSlug } from '../dashboard/js/process-step-ids.js';

installFastClock(0.05);

function view(nodes, { name = 'Ship it' } = {}) {
  const ids = Object.keys(nodes);
  const edges = [{ from: '', outcome: 'start', to: ids[0] }];
  for (let i = 0; i < ids.length - 1; i += 1) {
    if (nodes[ids[i]].type !== 'end') edges.push({ from: ids[i], outcome: 'next', to: ids[i + 1] });
  }
  return {
    template: { id: 'tpl_1', name, start: ids[0], nodes: structuredClone(nodes) },
    edges,
    layout: { nodes: Object.fromEntries(ids.map((id, i) => [id, { x: i * 160, y: 0 }])) },
    sourceHash: 'h1', currentRef: 'tpl_1@sha256:a',
  };
}

const agentStep = (prompt, extra = {}) => ({ type: 'task', performer: { kind: 'agent', prompt, role: 'dev' }, ...extra });
const STEPS = {
  start: { type: 'start' },
  'new-step': agentStep('Build it', { name: 'New step' }),
  'new-step-2': { type: 'task', name: 'New step', performer: { kind: 'agent', prompt: 'Review it', role: 'dev', differentFrom: ['new-step'] } },
  done: { type: 'end', result: 'success' },
};
const EMPTY = { start: { type: 'start' }, done: { type: 'end', result: 'success' } };

test('a step moves to the id its name makes, with every reference to it', () => {
  assert.equal(stepSlug('Build it, then ship!'), 'build-it-then-ship');
  const named = view(STEPS);
  named.template.nodes['new-step'].name = 'Build it';
  const { view: moved, id } = followName(named, 'new-step');
  assert.equal(id, 'build-it');
  assert.deepEqual(Object.keys(moved.template.nodes), ['start', 'build-it', 'new-step-2', 'done'], 'in the same place');
  assert.ok(moved.edges.some((e) => e.from === 'start' && e.to === 'build-it'));
  assert.ok(moved.edges.some((e) => e.from === 'build-it' && e.to === 'new-step-2'));
  assert.ok(!JSON.stringify(moved.edges).includes('"new-step"'));
  assert.deepEqual(moved.layout.nodes['build-it'], { x: 160, y: 0 });
  assert.deepEqual(moved.template.nodes['new-step-2'].performer.differentFrom, ['build-it'], 'a constraint naming it follows');
  // A name another step's id already has is made unique; one that already matches stays.
  moved.template.nodes['new-step-2'].name = 'Build it';
  assert.equal(followName(moved, 'new-step-2').id, 'build-it-2');
  assert.equal(followName(moved, 'build-it').view, moved, 'nothing to move');
  // The start pointer follows too.
  assert.equal(rekeyStep(view({ first: agentStep('x'), done: { type: 'end' } }), 'first', 'renamed').template.start, 'renamed');
});

async function mountEditor(t, { scribes = [], nodes = STEPS, ran = false, otherNames = [], onAbandon = null, onDiscard = null, onClose = () => {}, onSteps = () => {} } = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-mini-editor.js');
  const saves = [];
  const api = {
    saveTemplate: async (id, draft, meta) => { saves.push(structuredClone(draft)); return { ref: `tpl_1@sha256:s${saves.length}`, sourceHash: `h${saves.length + 1}` }; },
  };
  const stored = view(nodes);
  const mounted = await harness.mount(harness.html`<${mod.MiniGraphEditor} group=${{ name: 'webshop' }} roster=${[]}
    process=${{ templateId: 'tpl_1', name: 'Ship it', ran }} view=${stored} stored=${stored} api=${api} draft=${true}
    scribes=${scribes} notify=${() => {}} otherNames=${otherNames} onAbandon=${onAbandon} onDiscard=${onDiscard} onClose=${onClose} onSteps=${onSteps} />`);
  return { harness, root: mounted.container, saves, unmount: mounted.unmount };
}

async function waitFor(harness, condition, message) {
  for (let attempt = 0; attempt < 400; attempt += 1) {
    if (condition()) return;
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 20)));
  }
  assert.ok(condition(), message);
}

async function rename(harness, root, id, name) {
  await harness.act(async () => { root.querySelector(`[data-process-node="${id}"]`).click(); });
  const field = root.querySelector(`[data-mini-inspector="${id}"] [data-mini-name]`);
  await harness.act(async () => { field.value = name; harness.fireEvent(field, 'input'); harness.fireEvent(field, 'change'); });
}

test('renaming a step of a process that has never run moves its id, and it saves that way', async (t) => {
  const { harness, root, saves } = await mountEditor(t);
  await rename(harness, root, 'new-step', 'Build it');
  assert.ok(root.querySelector('[data-mini-inspector="build-it"]'), 'the panel stays on the step, under its new id');
  await waitFor(harness, () => saves.length > 0, 'the draft saves');
  const saved = saves.at(-1);
  assert.ok(saved.template.nodes['build-it'], 'saved as build-it');
  assert.equal(saved.template.nodes['new-step'], undefined);
  assert.deepEqual(saved.template.nodes['new-step-2'].performer.differentFrom, ['build-it']);
});

test('once the process has run, a rename keeps the id runs know it by', async (t) => {
  const { harness, root, saves } = await mountEditor(t, { ran: true });
  await rename(harness, root, 'new-step', 'Build it');
  await waitFor(harness, () => saves.length > 0, 'the draft saves');
  assert.equal(saves.at(-1).template.nodes['new-step'].name, 'Build it');
  assert.equal(saves.at(-1).template.nodes['build-it'], undefined);
});

test('a draft with no steps is dropped when the editor is left; one with steps, or handed on, is kept', async (t) => {
  let abandoned = [];
  let s = await mountEditor(t, { nodes: EMPTY, onAbandon: (id, options) => abandoned.push([id, options]) });
  await s.unmount();
  assert.deepEqual(abandoned, [['tpl_1', undefined]], 'left with no steps: dropped');

  abandoned = [];
  s = await mountEditor(t, { nodes: EMPTY, onAbandon: (id, options) => abandoned.push([id, options]) });
  s.harness.window.dispatchEvent(new s.harness.window.Event('pagehide'));
  assert.deepEqual(abandoned, [['tpl_1', { keepalive: true }]], 'the browser going drops it too, with a request that outlives the page');
  await s.unmount();
  assert.equal(abandoned.length, 1, 'once');

  abandoned = [];
  s = await mountEditor(t, { onAbandon: (id) => abandoned.push(id) });
  await s.unmount();
  assert.deepEqual(abandoned, [], 'a draft with steps is kept');

  s = await mountEditor(t, { nodes: EMPTY, onAbandon: (id) => abandoned.push(id) });
  await s.harness.act(async () => { s.root.querySelector('[data-mini-steps]').click(); });
  await s.unmount();
  assert.deepEqual(abandoned, [], 'handed to the step list: kept');

  s = await mountEditor(t, { nodes: EMPTY, scribes: [{ scope: { id: 'tpl_1' }, online: false }], onAbandon: (id) => abandoned.push(id) });
  await s.unmount();
  assert.deepEqual(abandoned, [], 'a scribe is drafting it: kept');
});

test('✕ drops a named draft with no steps; Save keeps it', async (t) => {
  const discarded = [];
  let closed = 0;
  let s = await mountEditor(t, { nodes: EMPTY, onDiscard: (id) => discarded.push(id), onClose: () => { closed += 1; } });
  const title = s.root.querySelector('[data-mini-title]');
  await s.harness.act(async () => { title.value = 'Nightly'; harness_change(s.harness, title); });
  await s.harness.act(async () => { s.root.querySelector('[data-mini-close]').click(); });
  assert.deepEqual(discarded, ['tpl_1'], '✕: no steps, not kept');

  s = await mountEditor(t, { nodes: EMPTY, onDiscard: (id) => discarded.push(id), onClose: () => { closed += 1; } });
  const named = s.root.querySelector('[data-mini-title]');
  await s.harness.act(async () => { named.value = 'Nightly'; harness_change(s.harness, named); });
  await s.harness.act(async () => { s.root.querySelector('[data-mini-save]').click(); });
  await waitFor(s.harness, () => closed === 1, 'Save closes it');
  assert.deepEqual(discarded, ['tpl_1'], 'Save keeps the named one');
});

function harness_change(harness, el) {
  harness.fireEvent(el, 'input');
  harness.fireEvent(el, 'change');
}

test('a name another process of the group has is flagged, the placeholder aside', async (t) => {
  const { harness, root } = await mountEditor(t, { otherNames: ['Nightly check', 'Untitled process'] });
  const title = root.querySelector('[data-mini-title]');
  assert.equal(root.querySelector('[data-mini-duplicate]'), null);
  await harness.act(async () => { title.value = 'nightly check '; harness.fireEvent(title, 'input'); });
  assert.match(root.querySelector('[data-mini-duplicate]').textContent, /Another process in webshop is called Nightly check/);
  await harness.act(async () => { title.value = 'Untitled process'; harness.fireEvent(title, 'input'); });
  assert.equal(root.querySelector('[data-mini-duplicate]'), null, 'every new draft starts with that name');
});
