import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function editView() {
  return {
    template: {
      id: 'inspector-flow', start: 'begin', params: { ticket: { type: 'string' } },
      nodes: {
        begin: { type: 'start' },
        spec: { type: 'task', name: 'Write spec', performer: { kind: 'agent', prompt: 'Spec it' }, captures: ['spec'] },
        build: { type: 'task', name: 'Build', performer: { kind: 'agent' } },
        gate: { type: 'decision', name: 'Ship?', performer: { kind: 'human', ask: 'Ship it?' } },
        pause: { type: 'wait', name: 'Cool down', wait: { duration: '5m' } },
        locked: { type: 'task', name: 'Locked', performer: { kind: 'program', run: 'make' } },
        done: { type: 'end', result: 'success' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'begin' },
      { from: 'begin', outcome: 'pass', to: 'spec' },
      { from: 'spec', outcome: 'pass', to: 'build' },
      { from: 'build', outcome: 'pass', to: 'gate' },
      { from: 'gate', outcome: 'yes', to: 'pause' },
      { from: 'gate', outcome: 'no', to: 'build' },
      { from: 'pause', outcome: 'pass', to: 'locked' },
      { from: 'locked', outcome: 'pass', to: 'done' },
    ],
    layout: { nodes: {} }, sourceHash: 'source', semanticHash: 'semantic',
  };
}

async function openEditor(t, view = editView(), harness = null) {
  harness ||= await createPreactHarness(t);
  const previous = { raf: globalThis.requestAnimationFrame, css: globalThis.CSS };
  globalThis.requestAnimationFrame = () => 0;
  globalThis.CSS = { escape: (value) => String(value) };
  t.after(() => {
    if (previous.raf === undefined) delete globalThis.requestAnimationFrame;
    else globalThis.requestAnimationFrame = previous.raf;
    if (previous.css === undefined) delete globalThis.CSS;
    else globalThis.CSS = previous.css;
  });
  const [{ openTemplateEditor }, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/process-editor.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  let editor;
  await harness.act(async () => {
    editor = await openTemplateEditor(host, {
      id: view.template.id, view,
      config: {
        nodeEditable: (id) => id !== 'locked',
        validation: { delayMs: 60_000, fetchFn: async () => ({ ok: true, json: async () => ({ diagnostics: [] }) }) },
      },
    });
  });
  // The harness may have torn the DOM down first; the live-validation timer
  // must stop either way or it holds the test process open.
  t.after(() => {
    try { editor?.destroy(); } catch { /* DOM already gone */ }
    editor?.validation?.destroy?.();
  });
  const open = async (id) => { await harness.act(() => editor.openNodeSettings(id)); };
  const dock = () => host.querySelector('[data-node-inspector]');
  const blocks = () => [...dock().querySelectorAll('[data-node-block]')].map((el) => el.dataset.nodeBlock);
  const field = (key) => dock().querySelector(`[data-node-field="${key}"]`);
  const set = async (key, value) => {
    const el = field(key);
    assert.ok(el, `field ${key} is rendered`);
    if (el.type === 'checkbox') el.checked = value;
    else Object.defineProperty(el, 'value', { configurable: true, writable: true, value });
    await harness.act(() => harness.fireEvent(el, 'change'));
  };
  const click = async (selector) => {
    const el = dock().querySelector(selector);
    assert.ok(el, `${selector} is rendered`);
    await harness.act(() => harness.fireEvent(el, 'click'));
  };
  return { harness, host, editor, dashPrefs, open, dock, blocks, field, set, click };
}

test('opening a node docks the inspector beside the canvas with the label focused', async (t) => {
  const { harness, host, editor, open, dock } = await openEditor(t);
  await open('build');
  assert.deepEqual(editor.selection, { type: 'node', id: 'build' });
  assert.equal(dock().dataset.nodeInspector, 'build');
  assert.ok(host.querySelector('.process-editor-body > [data-node-inspector]'), 'docked inside the editor body');
  assert.equal(harness.document.activeElement, dock().querySelector('[data-node-field="meta.name"]'));
  assert.equal(host.querySelector('.process-node-modal'), null);
  await harness.act(() => harness.fireEvent(dock().querySelector('.pni-close'), 'click'));
  assert.equal(editor.selection, null);
  assert.equal(host.querySelector('[data-node-inspector]'), null);
});

test('each node kind gets its required blocks, present optional ones, and only valid additions', async (t) => {
  const { open, blocks, dock } = await openEditor(t);
  const addable = () => [...dock().querySelectorAll('[data-add-block]')].map((el) => el.dataset.addBlock);
  await open('spec');
  assert.deepEqual(blocks(), ['performer', 'prompt', 'captures', 'outcomes']);
  assert.deepEqual(addable(), ['stages', 'inputs', 'retry', 'bounds', 'about']);
  await open('build');
  assert.deepEqual(blocks(), ['performer', 'inputs', 'outcomes'], 'upstream captures show the Inputs block');
  assert.match(dock().querySelector('[data-node-block="inputs"]').textContent, /spec ← Write spec/);
  assert.match(dock().querySelector('[data-node-block="inputs"]').textContent, /params: ticket/);
  await open('gate');
  assert.deepEqual(blocks(), ['performer', 'prompt', 'outcomes']);
  assert.deepEqual(addable(), ['inputs', 'bounds', 'about'], 'decisions offer no task-only blocks');
  assert.match(dock().querySelector('[data-node-block="outcomes"]').textContent, /yes→Cool down/);
  await open('pause');
  assert.deepEqual(blocks(), ['wait', 'outcomes']);
  await open('locked');
  assert.deepEqual(addable(), [], 'read-only nodes add nothing');
  assert.ok(dock().classList.contains('is-readonly'));
  assert.ok([...dock().querySelectorAll('input, textarea, select')].every((el) => el.disabled), 'every control is disabled');
  assert.equal(dock().querySelector('.pni-block-remove'), null);
});

test('every block round-trips through save and reload, one undo step per edit', async (t) => {
  const first = await openEditor(t);
  const { editor, open, set, click, dock } = first;
  await open('build');
  const undo = () => editor.model.undoStack.length;
  let before = undo();
  await set('performer.profile', 'dev');
  assert.equal(undo(), before + 1, 'a field edit is one undoable step');
  await set('performer.model', 'opus');
  await click('[data-add-block="prompt"]');
  await set('performer.prompt', 'Implement {{ticket}}');
  await click('[data-add-block="stages"]');
  await set('plan.enabled', true);
  await set('plan.approval', 'human');
  await click('.process-node-checks .process-node-add');
  await set('review.enabled', true);
  await click('[data-add-block="retry"]');
  await set('retry.maxAttempts', '3');
  await set('retry.onFail', 'feedback-same-session');
  await click('[data-add-block="captures"]');
  await set('captures', 'patch\npatch\nnotes');
  await click('[data-add-block="bounds"]');
  await set('performer.timeout', '2h');
  await set('performer.contact.cadence', '30m');
  await click('[data-add-block="about"]');
  await set('meta.description', 'Builds the feature');
  await set('meta.doc', 'Longer notes');
  await set('meta.name', 'Build it');
  await open('pause');
  await set('wait.duration', '10m');
  before = undo();
  await open('gate');
  await set('performer.ask', 'Ship now?');
  assert.equal(undo(), before + 1);

  const body = editor.model.saveBody();
  const build = body.template.nodes.build;
  assert.deepEqual(build.performer, { kind: 'agent', profile: 'dev', model: 'opus', prompt: 'Implement {{ticket}}', timeout: '2h', contact: { cadence: '30m' } });
  assert.equal(build.plan.approval, 'human');
  assert.deepEqual(build.checks.map((check) => check.id), ['check']);
  assert.equal(build.review.performer.kind, 'human');
  assert.deepEqual(build.retry, { maxAttempts: 3, onFail: 'feedback-same-session' });
  assert.deepEqual(build.captures, ['patch', 'notes']);
  assert.equal(build.description, 'Builds the feature');
  assert.equal(build.doc, 'Longer notes');
  assert.equal(build.name, 'Build it');
  assert.equal(body.template.nodes.pause.wait.duration, '10m');
  assert.equal(body.template.nodes.gate.performer.ask, 'Ship now?');

  const reloaded = await openEditor(t, { ...body, semanticHash: 'semantic' }, first.harness);
  await reloaded.open('build');
  assert.deepEqual(reloaded.blocks(), ['performer', 'prompt', 'stages', 'inputs', 'retry', 'captures', 'outcomes', 'bounds', 'about'],
    'blocks with data reopen without being added');
  for (const [key, value] of [
    ['performer.profile', 'dev'], ['performer.model', 'opus'], ['performer.prompt', 'Implement {{ticket}}'],
    ['retry.maxAttempts', '3'],
    ['captures', 'patch\nnotes'], ['performer.timeout', '2h'], ['performer.contact.cadence', '30m'],
    ['meta.description', 'Builds the feature'], ['meta.name', 'Build it'],
  ]) assert.equal(reloaded.field(key).value, value, `${key} reloads`);
  // linkedom does not reflect a <select>'s value; the saved body above
  // already pins plan approval and the retry mode.
  assert.ok(reloaded.field('plan.approval') && reloaded.field('retry.onFail'));
  const checked = (el) => el.checked === true || el.hasAttribute('checked');
  assert.ok(checked(reloaded.field('plan.enabled')));
  assert.ok(checked(reloaded.field('review.enabled')));
  assert.match(reloaded.dock().querySelector('.pni-stage-chips').textContent, /plan→work→check→review/);
  void dock;
});

test('removing an optional block clears its slice as one undoable edit', async (t) => {
  const { harness, editor, open, set, click, blocks } = await openEditor(t);
  await open('build');
  await click('[data-add-block="retry"]');
  await set('retry.maxAttempts', '4');
  assert.ok(blocks().includes('retry'));
  await click('[aria-label="Remove Retry"]');
  assert.equal(editor.model.node('build').retry, undefined);
  assert.ok(!blocks().includes('retry'));
  await harness.act(() => editor.applyHistory('undo'));
  assert.deepEqual(editor.model.node('build').retry, { maxAttempts: 4 });
  assert.ok(blocks().includes('retry'), 'undo brings the block back with its data');
});

test('a refused edit leaves the model alone and snaps the field back', async (t) => {
  const { editor, open, set, click, field, host } = await openEditor(t);
  await open('build');
  await click('[data-add-block="stages"]');
  await click('.process-node-checks .process-node-add');
  await click('.process-node-checks .process-node-add');
  await set('checks.1.id', 'check');
  assert.deepEqual(editor.model.node('build').checks.map((check) => check.id), ['check', 'check-2']);
  assert.equal(field('checks.1.id').value, 'check-2');
  assert.match(host.textContent, /duplicate check id check/);
});

test('blocks reorder by button or drag, and the order persists per node kind', async (t) => {
  const { harness, open, blocks, click, dashPrefs, dock } = await openEditor(t);
  dashPrefs.removeItem('tclaude.dash.processInspector.blockOrder.task');
  await open('spec');
  await click('[aria-label="Move Outcomes up"]');
  assert.deepEqual(blocks(), ['performer', 'prompt', 'outcomes', 'captures']);
  const transfer = new Map();
  const dataTransfer = { types: ['application/x-tclaude-node-block'], setData: (k, v) => transfer.set(k, v), getData: (k) => transfer.get(k) };
  const grip = dock().querySelector('[data-node-block="captures"] .pni-grip');
  await harness.act(() => harness.fireEvent(grip, 'dragstart', { dataTransfer }));
  await harness.act(() => harness.fireEvent(dock().querySelector('[data-node-block="performer"]'), 'drop', { dataTransfer }));
  assert.deepEqual(blocks(), ['captures', 'performer', 'prompt', 'outcomes']);
  await open('build');
  assert.deepEqual(blocks(), ['performer', 'inputs', 'outcomes'], 'another task follows the saved task order');
  assert.match(dashPrefs.getItem('tclaude.dash.processInspector.blockOrder.task'), /"captures","performer"/);
});

test('node diagnostics are listed at the top of the inspector', async (t) => {
  const { harness, editor, open, dock } = await openEditor(t);
  await harness.act(() => editor.validation.applyDiagnostics([
    { severity: 'error', scope: 'node', targetId: 'build', message: 'performer profile dev is unknown' },
    { severity: 'warning', scope: 'node', targetId: 'spec', message: 'elsewhere' },
  ]));
  await open('build');
  assert.deepEqual([...dock().querySelectorAll('.pni-diag')].map((li) => li.textContent), ['✕ performer profile dev is unknown']);
});
