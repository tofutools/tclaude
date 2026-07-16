import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function openBlank(t) {
  const harness = await createPreactHarness(t);
  const previous = {
    raf: globalThis.requestAnimationFrame,
    css: globalThis.CSS,
  };
  globalThis.requestAnimationFrame = () => 1;
  globalThis.CSS = { escape: (value) => String(value) };
  t.after(() => {
    if (previous.raf === undefined) delete globalThis.requestAnimationFrame;
    else globalThis.requestAnimationFrame = previous.raf;
    if (previous.css === undefined) delete globalThis.CSS;
    else globalThis.CSS = previous.css;
  });
  const { openTemplateEditor } = await harness.importDashboardModule('js/process-editor.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  let editor;
  await harness.act(async () => {
    editor = await openTemplateEditor(host, {
      id: 'preact-flow', blank: true,
      config: {
        validation: {
          delayMs: 60_000,
          fetchFn: async () => ({ ok: true, json: async () => ({ diagnostics: [] }) }),
        },
      },
    });
  });
  t.after(() => editor?.destroy());
  return { harness, host, editor };
}

test('Preact editor shell keeps one graph host across chrome, selection, and model snapshots', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const graphHost = host.querySelector('.process-editor-canvas-host');
  const graphRoot = host.querySelector('.process-graph');
  assert.ok(graphHost && graphRoot);
  assert.equal(host.querySelector('.process-editor-title')?.textContent, undefined,
    'a blank template owns an editable id field');

  await harness.act(() => editor.status('poll updated'));
  assert.equal(host.querySelector('.process-editor-canvas-host'), graphHost);
  assert.equal(host.querySelector('.process-graph'), graphRoot);
  assert.match(host.querySelector('.process-editor-status').textContent, /poll updated/);

  await harness.act(() => editor.setSelection({ type: 'template' }));
  assert.equal(host.querySelector('.process-editor-canvas-host'), graphHost);
  assert.ok(host.querySelector('.process-editor-inspector [aria-label="Template display name"]'));
  await harness.act(() => editor.addNodeType('task', { x: 400, y: 200 }));
  assert.equal(host.querySelector('.process-editor-canvas-host'), graphHost);
  assert.equal(host.querySelector('.process-graph'), graphRoot,
    'setGraph updates the opaque widget without remounting its host/root');

  editor.destroy();
  editor.destroy();
  assert.equal(host.childNodes.length, 0);
});

test('Preact join select renders and publishes canonical node.join values', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  Object.assign(editor.model.template.nodes, {
    left: { type: 'task' },
    right: { type: 'task' },
    'join-all': { type: 'task', join: 'all' },
    'join-any': { type: 'task', join: 'any' },
  });
  Object.assign(editor.model.layout.nodes, {
    left: { x: 50, y: 100 },
    right: { x: 50, y: 300 },
    'join-all': { x: 300, y: 100 },
    'join-any': { x: 300, y: 300 },
  });
  editor.model.edges.push(
    { from: 'left', outcome: 'all', to: 'join-all' },
    { from: 'right', outcome: 'all', to: 'join-all' },
    { from: 'left', outcome: 'any', to: 'join-any' },
    { from: 'right', outcome: 'any', to: 'join-any' },
  );
  await harness.act(() => editor.refresh());

  const selectJoin = async (id) => {
    await harness.act(() => editor.setSelection({ type: 'node', id }));
    const select = host.querySelector('[aria-label="Join semantics"]');
    assert.ok(select, `${id} has a fan-in join control`);
    return select;
  };
  const choose = async (select, value) => {
    [...select.options].forEach((option) => option.removeAttribute('selected'));
    const option = [...select.options].find((candidate) => candidate.value === value);
    assert.ok(option, `join select contains ${value || 'unset'}`);
    option.setAttribute('selected', '');
    Object.defineProperty(select, 'value', { configurable: true, writable: true, value });
    await harness.act(() => harness.fireEvent(select, 'change'));
  };
  const selectedValue = (select) => select.value
    ?? select.getAttribute('value')
    ?? [...select.options].find((option) => option.selected)?.getAttribute('value')
    ?? '';

  let select = await selectJoin('join-all');
  assert.equal(selectedValue(select), 'all');
  select = await selectJoin('join-any');
  assert.equal(selectedValue(select), 'any');

  select = await selectJoin('join-all');
  await choose(select, 'any');
  assert.equal(editor.model.template.nodes['join-all'].join, 'any');
  await harness.act(() => editor.setSelection(null));
  select = await selectJoin('join-all');
  assert.equal(selectedValue(select), 'any');

  await choose(select, '');
  assert.equal(Object.hasOwn(editor.model.template.nodes['join-all'], 'join'), false);
  await harness.act(() => editor.setSelection(null));
  select = await selectJoin('join-all');
  assert.equal(selectedValue(select), '');
  editor.destroy();
});

test('unrelated Signals snapshots preserve an active inspector IME buffer and focus node', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  await harness.act(() => editor.setSelection({ type: 'template' }));
  const input = host.querySelector('[aria-label="Template display name"]');
  input.focus();
  input.value = '編集中';
  input.dispatchEvent(new harness.window.Event('compositionstart', { bubbles: true }));

  await harness.act(() => editor.status('validation refreshed'));
  assert.equal(host.querySelector('[aria-label="Template display name"]'), input);
  assert.equal(harness.document.activeElement, input);
  assert.equal(input.value, '編集中');

  input.dispatchEvent(new harness.window.Event('compositionend', { bubbles: true }));
  await harness.act(() => editor.status('another refresh'));
  assert.equal(input.value, '編集中', 'focused raw value remains user-owned until change commit');
  editor.destroy();
});

test('external Apply update cannot commit a focused stale inspector buffer after model replacement', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  await harness.act(() => editor.setSelection({ type: 'template' }));
  editor.blank = false;
  editor.model.currentRef = `preact-flow@sha256:${'0'.repeat(64)}`;
  editor.model.sourceHash = '1'.repeat(64);
  editor.model.semanticHash = '2'.repeat(64);
  Object.assign(editor.loadedView, {
    currentRef: editor.model.currentRef,
    sourceHash: editor.model.sourceHash,
    semanticHash: editor.model.semanticHash,
  });
  const target = structuredClone(editor.loadedView);
  target.template.name = 'External canonical name';
  target.currentRef = `preact-flow@sha256:${'a'.repeat(64)}`;
  target.sourceHash = 'b'.repeat(64);
  target.semanticHash = 'c'.repeat(64);

  const response = deferred();
  const previousFetch = globalThis.fetch;
  globalThis.fetch = () => response.promise;
  t.after(() => {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  });
  editor.externalChange = {
    kind: 'clean', ref: target.currentRef, sourceHash: target.sourceHash,
    review: null, actor: null, authoredAt: '',
  };
  await harness.act(() => editor.renderExternalChange());
  const originalReload = editor.reloadExternalChange.bind(editor);
  let reloadPromise;
  editor.reloadExternalChange = () => {
    reloadPromise = originalReload();
    return reloadPromise;
  };

  const apply = harness.getByRole(host, 'button', { name: 'Apply update' });
  await harness.act(() => harness.fireEvent(apply, 'click'));
  assert.equal(editor.externalReloadPending, true);
  const inspector = host.querySelector('.process-editor-inspector');
  assert.equal(inspector.hasAttribute('inert'), true, 'the inspector joins the pending external-update inert boundary');
  const input = host.querySelector('[aria-label="Template display name"]');
  input.focus();
  input.value = 'Stale local buffer';
  harness.fireEvent(input, 'input');
  assert.equal(editor.externalChange.ref, target.currentRef, 'raw input does not change the observed external head');

  response.resolve({ ok: true, json: async () => target });
  assert.equal(await reloadPromise, true, editor.statusState.message);
  await harness.act(async () => {});
  assert.equal(editor.model.template.name, 'External canonical name');
  assert.equal(host.querySelector('.process-editor-inspector').hasAttribute('inert'), false);
  input.blur();
  harness.fireEvent(input, 'change');
  assert.equal(editor.model.template.name, 'External canonical name',
    'a raw value buffered during the GET cannot commit after pending becomes false');
  assert.equal(input.value, 'External canonical name');
  editor.destroy();
});

test('external review summaries render bounded identity and truncation details through Preact', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  editor.externalChange = {
    kind: 'clean', ref: 'preact-flow@sha256:abcdef123456', sourceHash: 'source-new',
    review: { summary: {
      addedNodes: ['a', 'b'], addedNodeCount: 7, addedNodesTruncated: true,
      removedNodes: [], removedNodeCount: 0, changedNodes: [], changedNodeCount: 0,
      addedEdges: 0, removedEdges: 0, metadataChanged: false,
      source: {
        firstLine: 2, removedLines: 20, addedLines: 20, before: ['old'], after: ['new'], truncated: true,
        truncation: { lines: true, characters: true, bytes: true },
      },
    } },
  };
  editor.externalReviewOpen = true;
  await harness.act(() => editor.renderExternalChange());
  assert.match(host.querySelector('.process-external-graph-summary').textContent,
    /\+7 nodes \(a, b, … 5 more IDs omitted\)/);
  assert.match(host.querySelector('.process-external-source-summary').textContent,
    /source preview truncated at lines, characters, UTF-8 bytes limits/);
  editor.destroy();
});

test('production scribe modal owns focus, inertness, and every close path inside the editor island', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const invoker = host.querySelector('.process-scribe-action');
  const editorRoot = host.querySelector('.process-editor');
  const open = async (overrides = {}) => {
    invoker.focus();
    let pending;
    await harness.act(() => {
      pending = editor.scribePreviewModal({
        kind: 'selection', prompt: 'Help refine this process.',
        context: '{\n  "nodeIds": ["start"]\n}', truncated: true, ...overrides,
      });
    });
    const overlay = host.querySelector('.process-scribe-preview-overlay');
    assert.ok(overlay, 'the production island rendered the preview');
    assert.equal(editorRoot.hasAttribute('inert'), true, 'the editor chrome and graph are inert behind the preview');
    return { pending, overlay };
  };

  let opened = await open();
  let textarea = opened.overlay.querySelector('textarea');
  const send = opened.overlay.querySelector('button.primary');
  assert.equal(harness.document.activeElement, textarea);
  assert.match(opened.overlay.querySelector('pre').textContent, /"start"/);
  assert.match(opened.overlay.querySelector('.process-scribe-context-end').textContent, /visibly truncated/);
  send.focus();
  let tab = harness.fireEvent(send, 'keydown', { key: 'Tab' });
  assert.equal(tab.defaultPrevented, true);
  assert.equal(harness.document.activeElement, textarea);
  tab = harness.fireEvent(textarea, 'keydown', { key: 'Tab', shiftKey: true });
  assert.equal(tab.defaultPrevented, true);
  assert.equal(harness.document.activeElement, send);
  await harness.act(() => harness.fireEvent(textarea, 'keydown', { key: 'Escape' }));
  assert.equal(await opened.pending, null);
  assert.equal(editorRoot.hasAttribute('inert'), false);
  assert.equal(harness.document.activeElement, invoker, 'Escape restores the production invoker');

  opened = await open();
  await harness.act(() => harness.fireEvent(
    harness.getByRole(opened.overlay, 'button', { name: 'Cancel' }), 'click',
  ));
  assert.equal(await opened.pending, null);
  assert.equal(editorRoot.hasAttribute('inert'), false);
  assert.equal(harness.document.activeElement, invoker, 'Cancel restores the production invoker');

  opened = await open();
  await harness.act(() => harness.fireEvent(opened.overlay, 'mousedown'));
  assert.equal(await opened.pending, null);
  assert.equal(editorRoot.hasAttribute('inert'), false);
  assert.equal(harness.document.activeElement, invoker, 'backdrop dismissal restores the production invoker');

  opened = await open({ kind: 'diagnostic', prompt: 'Fix it.', context: '{"code":"missing_start"}', truncated: false });
  textarea = opened.overlay.querySelector('textarea');
  textarea.value = 'Preserve unrelated stages.';
  await harness.act(() => harness.fireEvent(textarea, 'input'));
  await harness.act(() => harness.fireEvent(opened.overlay.querySelector('button.primary'), 'click'));
  assert.equal(await opened.pending, 'Preserve unrelated stages.');
  assert.equal(editorRoot.hasAttribute('inert'), false);
  assert.equal(harness.document.activeElement, invoker, 'Send restores the production invoker');
  editor.destroy();
});

test('real graph adapter centers node and edge diagnostics without exposing widget state', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const svg = host.querySelector('.process-graph-svg');
  svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 800, height: 600, right: 800, bottom: 600 });
  await harness.act(() => editor.validation.applyDiagnostics([
    { severity: 'error', code: 'node-start', scope: 'node', targetId: 'start', message: 'Start needs attention' },
    { severity: 'warning', code: 'edge-pass', scope: 'edge', targetId: 'start:pass', message: 'Pass needs attention' },
  ]));

  const layout = editor.graph.layoutSnapshot();
  const nodeIndex = editor.validation.mapped.entries.findIndex((entry) => entry.scope === 'node');
  const node = layout.nodes.find((candidate) => candidate.id === 'start');
  assert.equal(editor.validation.focusIssueAt(nodeIndex, { focusButton: false }), true);
  assert.deepEqual(editor.selection, { type: 'node', id: 'start' });
  assert.equal(host.querySelector('.process-node[data-node-id="start"]').classList.contains('is-selected'), true);
  let view = editor.graph.viewSnapshot();
  assert.deepEqual(view, { x: 400 - node.x * view.k, y: 300 - node.y * view.k, k: view.k });

  const edgeIndex = editor.validation.mapped.entries.findIndex((entry) => entry.scope === 'edge');
  const edge = layout.edges.find((candidate) => candidate.from === 'start' && candidate.outcome === 'pass');
  const anchor = edge.label || node;
  assert.equal(editor.validation.focusIssueAt(edgeIndex, { focusButton: false }), true);
  assert.deepEqual(editor.selection, { type: 'edge', from: 'start', outcome: 'pass' });
  assert.equal(host.querySelector('.process-edge.is-selected')?.dataset.edgeId, edge.id);
  view = editor.graph.viewSnapshot();
  assert.deepEqual(view, { x: 400 - anchor.x * view.k, y: 300 - anchor.y * view.k, k: view.k });
  editor.destroy();
});

test('production node and params dialogs stay inside the editor island root', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  await harness.act(() => editor.openNodeSettings(editor.model.template.start));
  const nodeOverlay = host.querySelector('.process-node-modal');
  assert.ok(nodeOverlay, 'node detail is rendered by the editor island');
  assert.equal(harness.document.querySelector('.process-node-modal'), nodeOverlay);
  await harness.act(() => editor.modalDispose(null));

  await harness.act(() => editor.openParamsSettings());
  const paramsOverlay = host.querySelector('.process-param-modal');
  assert.ok(paramsOverlay, 'params form is rendered by the same editor island');
  assert.equal(harness.document.querySelector('.process-param-modal'), paramsOverlay);
  await harness.act(() => editor.modalDispose(null));
  editor.destroy();
});

test('production node dialog generations isolate forced same-turn descriptor replacement', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const nodeA = editor.model.template.start;
  const nodeB = Object.keys(editor.model.template.nodes).find((id) => id !== nodeA);
  const originalA = structuredClone(editor.model.node(nodeA));
  const originalB = structuredClone(editor.model.node(nodeB));

  await harness.act(() => editor.openNodeSettings(nodeA));
  const draftA = host.querySelector('.process-node-detail > .process-node-section .process-node-input');
  draftA.value = 'private node A draft';
  await harness.act(() => harness.fireEvent(draftA, 'change'));
  assert.equal(editor.modalDispose.isDirty(), true, 'node A owns an unsaved private draft');

  let guardedReplacement;
  await harness.act(async () => {
    guardedReplacement = await editor.openNodeSettings(nodeB);
  });
  assert.equal(guardedReplacement, false, 'ordinary replacement still honors dirty-close rejection');
  assert.equal(host.querySelector('[role="dialog"]').getAttribute('aria-label'), `Node ${nodeA}`);
  assert.equal(draftA.value, 'private node A draft', 'dirty-close rejection preserves node A draft');

  await harness.act(() => {
    editor.openModal({ kind: 'node', nodeId: nodeB, mode: 'edit' });
  });
  const replacement = host.querySelector('.process-node-modal');
  const replacementDraft = replacement.querySelector(
    '.process-node-detail > .process-node-section .process-node-input',
  );
  assert.equal(replacement.querySelector('[role="dialog"]').getAttribute('aria-label'), `Node ${nodeB}`);
  assert.equal(replacementDraft.value, originalB.name || '',
    'node B displays its own canonical draft after the forced replacement');

  await harness.act(() => harness.fireEvent(replacement.querySelector('.process-node-save'), 'click'));
  assert.deepEqual(editor.model.node(nodeA), originalA, 'node A draft was never committed');
  assert.deepEqual(editor.model.node(nodeB), originalB, 'saving node B cannot receive node A draft state');

  await harness.act(() => editor.openNodeSettings(nodeA));
  assert.equal(host.querySelector('.process-node-detail > .process-node-section .process-node-input').value,
    originalA.name || '', 'ordinary sequential editing still starts from node A canonical state');
  await harness.act(() => editor.modalDispose(null));
  editor.destroy();
});
