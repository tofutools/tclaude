import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const tick = () => new Promise((r) => setTimeout(r, 0));

test('a run copy locks the steps the run reached, marks them, and lets one being worked on be rerouted', async (t) => {
  const harness = await createPreactHarness(t);
  const { runCopyConfig, authoredRoot } = await harness.importDashboardModule('js/process-run-copy.js');
  assert.equal(authoredRoot('review.vote.2'), 'review');
  assert.equal(authoredRoot('build/inner'), 'build');
  const config = runCopyConfig({ runId: 'run_1', reached: ['spec', 'review'], reroutable: ['review'], direct: true, send: async () => ({}) });
  assert.equal(config.nodeEditable('spec'), false);
  assert.equal(config.nodeEditable('review.vote.1'), false, 'a reached step\'s parts are locked too');
  assert.equal(config.nodeEditable('ship'), true);
  assert.equal(config.edgeEditable({ from: 'spec', outcome: 'default', to: 'review' }), false, 'a step that went on keeps where it led');
  assert.equal(config.edgeEditable({ from: 'review', outcome: 'default', to: 'ship' }), true, 'the step being worked on may be rerouted');
  assert.equal(config.edgeEditable({ from: 'ship', outcome: 'default', to: 'end' }), true);
  assert.equal(config.edgeEditable({ from: '', outcome: 'start', to: 'spec' }), false, 'the start stays');
  const spec = { id: 'spec' };
  const review = { id: 'review' };
  const ship = { id: 'ship' };
  [spec, review, ship].forEach(config.decorateNode);
  assert.equal(spec.locked, true);
  assert.deepEqual(spec.stateOverlay, { glyph: '🔒', label: 'reached' });
  assert.equal(review.stateOverlay.label, 'reached · can reroute');
  assert.equal(ship.locked, undefined);
  assert.equal(config.runCopy.label, 'Apply to this run');
  assert.equal(config.layoutEditable('spec'), true, 'where a reached step is drawn is no change to the run');
  assert.equal(runCopyConfig({ runId: 'run_1', direct: false, send: async () => ({}) }).runCopy.label, 'Propose to the operator');
});

test('Amend run opens the copy in the editor and sends its model as an amendment, with the why', async (t) => {
  const harness = await createPreactHarness(t);
  const { RunCopyEditor } = await harness.importDashboardModule('js/process-run-copy.js');
  const calls = [];
  let opened = null;
  const api = {
    runTemplate: async () => ({
      editView: { template: { id: 'tpl', nodes: {} }, edges: [], layout: null, sourceHash: 'abc' },
      reached: ['spec'], reroutable: [], canAmendDirectly: false, stateVersion: 7,
    }),
    amendRun: async (runId, body) => { calls.push([runId, body]); return { amendment: { state: 'proposed' } }; },
  };
  const openEditor = async (mount, options) => { opened = options; return { destroy() {} }; };
  let closed = 0;
  let changed = 0;
  const mounted = await harness.mount(harness.html`<${RunCopyEditor} runId="run_1" api=${api} openEditor=${openEditor}
    onClose=${() => { closed += 1; }} onChanged=${() => { changed += 1; }} />`);
  for (let i = 0; i < 50 && !opened; i += 1) await harness.act(async () => { await tick(); });
  assert.ok(opened, 'the editor opens');
  assert.equal(opened.view.sourceHash, '', 'a run copy is no saved version');
  assert.equal(opened.config.runCopy.label, 'Propose to the operator');
  assert.match(mounted.container.querySelector('.prc-legend').textContent, /1 reached step locked/);
  await harness.act(async () => {
    const note = mounted.container.querySelector('.prc-note');
    note.value = 'PCI asked for it';
    note.dispatchEvent(new Event('input', { bubbles: true }));
    await tick();
  });
  const result = await opened.config.saveAction({ template: { id: 'tpl' }, edges: [], layout: null, sourceHash: '' });
  assert.deepEqual(calls, [['run_1', { edit: { template: { id: 'tpl' }, edges: [], layout: null }, note: 'PCI asked for it', baseStateVersion: 7 }]]);
  assert.equal(result.status, 'Proposed to the operator.');
  opened.config.onSaved(result);
  assert.equal(changed, 1);
  assert.equal(closed, 1);
});

test('a run copy may tidy and move reached steps, but not edit them', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessEditModel } = await harness.importDashboardModule('js/process-edit-model.js');
  const { runCopyConfig } = await harness.importDashboardModule('js/process-run-copy.js');
  const view = {
    template: { id: 'tpl', nodes: { start: { type: 'start' }, spec: { type: 'task', name: 'Spec' }, ship: { type: 'task', name: 'Ship' }, done: { type: 'end' } } },
    edges: [{ from: '', outcome: 'start', to: 'start' }, { from: 'start', outcome: 'default', to: 'spec' },
      { from: 'spec', outcome: 'default', to: 'ship' }, { from: 'ship', outcome: 'default', to: 'done' }],
    layout: { nodes: { spec: { x: 10, y: 10 } } },
  };
  const config = runCopyConfig({ runId: 'r', reached: ['start', 'spec'], send: async () => ({}) });
  const model = new ProcessEditModel(view, config);
  assert.equal(model.moveNodes([{ id: 'spec', x: 40, y: 50 }]), true);
  assert.equal(model.autoLayout(), true, 'tidying the whole graph is allowed');
  assert.throws(() => model.renameNode('spec', 'spec2'), /read-only/);
});
