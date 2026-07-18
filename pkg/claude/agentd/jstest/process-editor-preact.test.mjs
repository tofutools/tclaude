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

test('Preact editor projects every canonical node kind through the inside-label contract without model drift', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const cases = [
    ['task-node', 'task'],
    ['decision-node', 'decision'],
    ['parallel-node', 'parallel'],
    ['wait-node', 'wait'],
    ['start-node', 'start'],
    ['end-node', 'end'],
  ];
  editor.model.template.nodes = Object.fromEntries(cases.map(([id, type], index) => [id, {
    type,
    name: `${type} ${'W'.repeat(index + 12)} 設計レビュー🙂 ${'long'.repeat(index + 5)}`,
  }]));
  editor.model.template.start = 'start-node';
  editor.model.edges = [{ from: '', outcome: 'start', to: 'start-node' }];
  editor.model.layout.nodes = Object.fromEntries(cases.map(([id], index) => [id, {
    x: 120 + index % 3 * 230,
    y: 120 + Math.floor(index / 3) * 220,
  }]));
  const before = editor.model.saveBody();
  await harness.act(() => editor.refresh());

  for (const [id, type] of cases) {
    const node = host.querySelector(`.process-node[data-node-id="${id}"]`);
    const label = node?.querySelector('.process-node-label-inside');
    const ports = host.querySelector(`.process-node-ports[data-node-id="${id}"]`);
    assert.ok(node && label && ports, `${type} renders through the real graph adapter`);
    assert.equal(node.querySelector('.process-node-label-peripheral'), null);
    assert.ok(node.getAttribute('aria-label').startsWith(`${before.template.nodes[id].name}, ${type}`));
    assert.equal(ports.closest('.process-node'), null, `${type} connector controls remain outside the node button`);
    assert.equal(ports.querySelector('.process-port-in').getAttribute('aria-label'),
      `Input port for ${before.template.nodes[id].name}`);
    assert.equal(ports.querySelector('.process-port-out').getAttribute('aria-label'),
      `Output port for ${before.template.nodes[id].name}`);
  }
  assert.deepEqual(editor.model.saveBody(), before,
    'rendering and refreshing labels does not change names, layout, edges, or the save payload');
  editor.destroy();
});

test('Preact editor reveals only diagnostic-bearing node overlays without moving ports or changing selection semantics', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const beforeSave = editor.model.saveBody();
  const layoutGeometry = () => {
    const layout = editor.graph.layoutSnapshot();
    return {
      bounds: layout.bounds,
      nodes: layout.nodes.map(({ id, x, y, width, height, layer, pinned }) => ({ id, x, y, width, height, layer, pinned })),
      edges: layout.edges.map(({ id, from, to, path, label }) => ({ id, from, to, path, label })),
    };
  };
  const beforeGeometry = layoutGeometry();
  const portGeometry = (id) => {
    const ports = host.querySelector(`.process-node-ports[data-node-id="${id}"]`);
    return [...ports.querySelectorAll('.process-port')].map((port) => ({
      kind: port.dataset.port,
      cx: port.getAttribute('cx'), cy: port.getAttribute('cy'), r: port.getAttribute('r'),
      role: port.getAttribute('role'), tabindex: port.getAttribute('tabindex'),
      label: port.getAttribute('aria-label'),
    }));
  };
  const beforePorts = Object.fromEntries(['start', 'end'].map((id) => [id, portGeometry(id)]));
  assert.equal(host.querySelectorAll('.process-overlay-anchor').length, 0,
    'a clean production editor does not render empty overlay placeholders');

  await harness.act(() => editor.validation.applyDiagnostics([{
    severity: 'error', code: 'E_START', scope: 'node', targetId: 'start', message: 'Start needs attention',
  }]));

  const start = host.querySelector('.process-node[data-node-id="start"]');
  const end = host.querySelector('.process-node[data-node-id="end"]');
  const marker = start.querySelector('.process-overlay-anchor');
  assert.ok(marker, 'mapped validation information renders its shared anchor');
  assert.equal(end.querySelector('.process-overlay-anchor'), null, 'the clean sibling stays undecorated');
  assert.match(start.getAttribute('aria-label'), /E_START: Start needs attention/);
  assert.match(marker.querySelector('.process-overlay-tooltip').textContent, /Start needs attention/);
  assert.deepEqual(layoutGeometry(), beforeGeometry, 'overlay disclosure does not change graph geometry');
  assert.deepEqual(Object.fromEntries(['start', 'end'].map((id) => [id, portGeometry(id)])), beforePorts,
    'overlay disclosure does not move or re-role connector ports');

  await harness.act(() => harness.fireEvent(marker, 'click'));
  assert.deepEqual(editor.selection, { type: 'node', id: 'start' },
    'clicking populated disclosure retains ordinary node selection semantics');
  assert.equal(start.classList.contains('is-selected'), true);

  editor.validation.applyDiagnostics([]);
  assert.equal(host.querySelectorAll('.process-overlay-anchor').length, 0, 'clearing the diagnostic removes the anchor');
  assert.deepEqual(editor.selection, { type: 'node', id: 'start' }, 'diagnostic repaint preserves semantic selection');
  assert.deepEqual(editor.model.saveBody(), beforeSave,
    'overlay appearance, interaction, and removal do not change the round-trip payload');
  editor.destroy();
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
  const { CHANGE_SUMMARY_LIMITS, CHANGE_SUMMARY_MARKERS, summarizeTemplateChange } = await harness.importDashboardModule('js/process-external-change.js');
  const count = CHANGE_SUMMARY_LIMITS.nodeIDs + 1;
  const addedIDs = [
    `<em>unsafe</em>`,
    `a-000-${'a'.repeat((1 << 20) - 6)}`,
    ...Array.from({ length: count - 2 }, (_, index) => `a-${String(index + 1).padStart(3, '0')}`),
  ];
  const removedIDs = [
    `r-000-${'界'.repeat(100_000)}`,
    ...Array.from({ length: count - 1 }, (_, index) => `r-${String(index + 1).padStart(3, '0')}`),
  ];
  const changedIDs = [
    `c-000-${'🚀'.repeat(75_000)}`,
    ...Array.from({ length: count - 1 }, (_, index) => `c-${String(index + 1).padStart(3, '0')}`),
  ];
  const summary = summarizeTemplateChange({
    template: { id: 'bounded', nodes: Object.fromEntries([
      ...removedIDs.map((id) => [id, { type: 'task' }]),
      ...changedIDs.map((id) => [id, { type: 'task', prompt: 'before' }]),
    ]) },
    edges: [{ from: 'old', outcome: 'pass', to: 'done' }],
    source: `id: bounded\n${'界'.repeat(4_000)}\n`,
  }, {
    template: { id: 'bounded', nodes: Object.fromEntries([
      ...addedIDs.map((id) => [id, { type: 'task' }]),
      ...changedIDs.map((id) => [id, { type: 'task', prompt: 'after' }]),
    ]) },
    edges: [{ from: 'new', outcome: 'pass', to: 'done' }],
    source: `id: bounded\n${'🚀'.repeat(4_000)}\n`,
  });
  editor.externalChange = {
    kind: 'clean', ref: 'preact-flow@sha256:abcdef123456', sourceHash: 'source-new',
    review: { view: { template: { nodes: { [addedIDs[1]]: { type: 'task' } } } }, summary },
  };
  const snapshot = editor.snapshot();
  assert.equal(snapshot.external.review.view, undefined, 'the exact raw review view never crosses into DOM state');
  assert.ok(editor.externalChange.review.view.template.nodes[addedIDs[1]], 'Apply Update retains the exact view internally');
  editor.externalReviewOpen = true;
  await harness.act(() => editor.renderExternalChange());
  const graph = host.querySelector('.process-external-graph-summary').textContent;
  const source = host.querySelector('.process-external-source-summary').textContent;
  assert.match(graph, /\+13 nodes/);
  assert.match(graph, /−13 nodes/);
  assert.match(graph, /13 changed nodes/);
  assert.match(graph, /\+1 edge/);
  assert.match(graph, /−1 edge/);
  assert.ok((graph.match(/\[ID shortened\]/g) || []).length >= 3, 'each category marks its shortened long ID');
  assert.equal((graph.match(new RegExp(CHANGE_SUMMARY_MARKERS.omittedNodeIDs, 'g')) || []).length, 3,
    'each category separately marks IDs omitted by the 12-item list cap');
  assert.match(source, /source preview truncated at characters, UTF-8 bytes limits/);
  assert.equal(host.querySelector('.process-external-graph-summary em'), null,
    'ID text resembling markup remains a text node rather than becoming HTML');
  const rendered = `${graph}\n${source}`;
  assert.ok([...rendered].length <= CHANGE_SUMMARY_LIMITS.renderedCharacters);
  assert.ok(new TextEncoder().encode(rendered).length <= CHANGE_SUMMARY_LIMITS.renderedBytes);
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

test('scribe preview backdrop cancels without sending and restores editor interaction', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const invoker = host.querySelector('.process-scribe-action');
  const editorRoot = host.querySelector('.process-editor');
  let sends = 0;
  editor.options.onScribe = async () => {
    sends += 1;
    return {};
  };

  invoker.focus();
  let request;
  await harness.act(() => {
    request = editor.requestScribe('template');
  });
  const overlay = host.querySelector('.process-scribe-preview-overlay');
  assert.ok(overlay, 'the request reached the production preview');
  assert.equal(editorRoot.hasAttribute('inert'), true);
  assert.equal(harness.document.activeElement, overlay.querySelector('textarea'));

  let backdrop;
  await harness.act(() => {
    backdrop = harness.fireEvent(overlay, 'mousedown');
  });
  assert.equal(backdrop.defaultPrevented, true,
    'backdrop close cancels the native pointer focus step before restoring the invoker');
  assert.equal(await request, false);
  assert.equal(sends, 0, 'backdrop cancellation never crosses the scribe send boundary');
  assert.equal(host.querySelector('.process-scribe-preview-overlay'), null);
  assert.equal(editorRoot.hasAttribute('inert'), false);
  assert.equal(harness.document.activeElement, invoker);
  editor.destroy();
});

test('forced scribe modal disposal removes its listener, inert boundary, and focus ownership', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const invoker = host.querySelector('.process-scribe-action');
  const editorRoot = host.querySelector('.process-editor');
  const keydownListeners = new Set();
  const addEventListener = harness.document.addEventListener;
  const removeEventListener = harness.document.removeEventListener;
  harness.document.addEventListener = function addTracked(type, listener, options) {
    if (type === 'keydown') keydownListeners.add(listener);
    return addEventListener.call(this, type, listener, options);
  };
  harness.document.removeEventListener = function removeTracked(type, listener, options) {
    if (type === 'keydown') keydownListeners.delete(listener);
    return removeEventListener.call(this, type, listener, options);
  };
  t.after(() => {
    harness.document.addEventListener = addEventListener;
    harness.document.removeEventListener = removeEventListener;
  });

  invoker.focus();
  let pending;
  await harness.act(() => {
    pending = editor.scribePreviewModal({
      kind: 'template', prompt: 'Review the flow.', context: '{"templateId":"preact-flow"}',
    });
  });
  assert.equal(keydownListeners.size, 1, 'the open preview owns one document focus listener');
  assert.equal(editorRoot.hasAttribute('inert'), true);
  assert.notEqual(harness.document.activeElement, invoker);

  await harness.act(() => editor.modalDispose(null));
  assert.equal(await pending, null);
  assert.equal(host.querySelector('.process-scribe-preview-overlay'), null);
  assert.equal(editorRoot.hasAttribute('inert'), false);
  assert.equal(harness.document.activeElement, invoker);
  assert.equal(keydownListeners.size, 0, 'forced disposal removes the document focus listener');
  editor.destroy();
});

test('stacked scribe preview alone owns Tab and Escape, then returns focus to the open lower overlay', async (t) => {
  const { harness, host, editor } = await openBlank(t);
  const { ManagementOverlay } = await harness.importDashboardModule('js/management-overlay.js');
  const { useRef, useState } = harness.hooks;
  const editorInvoker = host.querySelector('.process-scribe-action');
  let lowerCloses = 0;

  function LowerOverlay() {
    const [open, setOpen] = useState(true);
    const initial = useRef(null);
    if (!open) return null;
    return harness.html`<${ManagementOverlay}
      id="scribe-preview-lower-overlay"
      labelledby="scribe-preview-lower-title"
      initialFocusRef=${initial}
      onClose=${() => { lowerCloses += 1; setOpen(false); }}
      dirty=${false}
      confirmDiscard=${async () => false}
    >
      <h3 id="scribe-preview-lower-title">Underlying workflow</h3>
      <button ref=${initial} id="scribe-preview-lower-opener" type="button">Open preview</button>
      <button id="scribe-preview-lower-last" type="button">Lower last action</button>
    </${ManagementOverlay}>`;
  }

  editorInvoker.focus();
  const lowerMount = await harness.mount(harness.html`<${LowerOverlay} />`);
  await harness.act(async () => {});
  const lower = harness.document.querySelector('#scribe-preview-lower-overlay');
  lower.style.zIndex = '100';
  const lowerOpener = lower.querySelector('#scribe-preview-lower-opener');
  assert.equal(harness.document.activeElement, lowerOpener);

  let pending;
  await harness.act(() => {
    pending = editor.scribePreviewModal({
      kind: 'selection', prompt: 'Check this selection.', context: '{"nodeIds":["start"]}',
    });
  });
  const preview = host.querySelector('.process-scribe-preview-overlay');
  preview.style.zIndex = '200';
  const textarea = preview.querySelector('textarea');
  assert.equal(harness.document.activeElement, textarea);

  const dialogPointer = harness.fireEvent(preview.querySelector('[role="dialog"]'), 'mousedown');
  assert.equal(dialogPointer.defaultPrevented, false,
    'pointer defaults inside the preview dialog remain untouched');
  const lowerPointer = harness.fireEvent(lower, 'mousedown');
  assert.equal(lowerPointer.defaultPrevented, false,
    'a synthetically targeted underlying backdrop retains its pointer default');
  assert.equal(lowerCloses, 0);
  assert.ok(host.querySelector('.process-scribe-preview-overlay'));

  lower.querySelector('#scribe-preview-lower-last').focus();
  const tab = harness.fireEvent(harness.document.activeElement, 'keydown', { key: 'Tab' });
  assert.equal(tab.defaultPrevented, true);
  assert.equal(harness.document.activeElement, textarea,
    'the top preview contains focus while the lower overlay yields');

  await harness.act(() => harness.fireEvent(textarea, 'keydown', { key: 'Escape' }));
  assert.equal(await pending, null);
  assert.equal(host.querySelector('.process-scribe-preview-overlay'), null);
  assert.ok(harness.document.querySelector('#scribe-preview-lower-overlay'));
  assert.equal(lowerCloses, 0, 'the preview Escape does not dismiss the lower overlay');
  assert.equal(harness.document.activeElement, lowerOpener,
    'preview cleanup returns focus to its still-open owning overlay');

  await harness.act(() => harness.fireEvent(lowerOpener, 'keydown', { key: 'Escape' }));
  assert.equal(harness.document.querySelector('#scribe-preview-lower-overlay'), null);
  assert.equal(lowerCloses, 1);
  assert.equal(harness.document.activeElement, editorInvoker);
  await lowerMount.unmount();
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
