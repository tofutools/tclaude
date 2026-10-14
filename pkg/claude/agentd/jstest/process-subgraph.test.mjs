// Editor-level tests for subprocess drill-in (E1): enter a ▣ node, see its
// ports, breadcrumb and parent minimap, edit ports, navigate by outline,
// leave, and extract the body as a template.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function view() {
  return {
    template: {
      id: 'release', name: 'Release', start: 'begin', params: { branch: { type: 'string' } },
      nodes: {
        begin: { type: 'start' },
        qa: {
          type: 'subprocess', name: 'QA suite',
          subprocess: {
            params: { target: { type: 'string' } },
            start: 'in',
            nodes: {
              in: { type: 'start', next: { pass: 'unit' } },
              unit: { type: 'task', name: 'Unit', performer: { kind: 'program', run: 'make' }, next: { pass: 'ok', fail: 'bad' } },
              ok: { type: 'end', result: 'success' },
              bad: { type: 'end', result: 'failure' },
            },
            inputs: { target: '{{ params.branch }}' },
            outputs: { ok: 'pass', bad: 'fail' },
          },
        },
        ship: { type: 'end', result: 'success' },
        stop: { type: 'end', result: 'failure' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'begin' },
      { from: 'begin', outcome: 'pass', to: 'qa' },
      { from: 'qa', outcome: 'pass', to: 'ship' },
      { from: 'qa', outcome: 'fail', to: 'stop' },
    ],
    layout: { nodes: {} },
    currentRef: `release@sha256:${'1'.repeat(64)}`, sourceHash: '2'.repeat(64), semanticHash: 'semantic-1',
  };
}

async function openEditor(t, { onOpenTemplate } = {}) {
  let editor;
  t.after(() => {
    try { editor?.destroy(); } catch { /* DOM already gone */ }
    editor?.validation?.destroy?.();
  });
  const harness = await createPreactHarness(t);
  const previous = { raf: globalThis.requestAnimationFrame, css: globalThis.CSS, fetch: globalThis.fetch };
  globalThis.requestAnimationFrame = () => 0;
  globalThis.CSS = { escape: (value) => String(value) };
  const posts = [];
  globalThis.fetch = async (url, init = {}) => {
    posts.push({ url: String(url), body: init.body ? JSON.parse(init.body) : null });
    return { ok: true, status: 201, json: async () => ({ id: 'qa-suite' }) };
  };
  t.after(() => {
    for (const [key, name] of [['raf', 'requestAnimationFrame'], ['css', 'CSS'], ['fetch', 'fetch']]) {
      if (previous[key] === undefined) delete globalThis[name];
      else globalThis[name] = previous[key];
    }
  });
  const { openTemplateEditor } = await harness.importDashboardModule('js/process-editor.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.act(async () => {
    editor = await openTemplateEditor(host, {
      id: 'release', view: view(),
      config: {
        validation: { delayMs: 60_000, fetchFn: async () => ({ ok: true, json: async () => ({ diagnostics: [] }) }) },
        onOpenTemplate,
      },
    });
  });
  const q = (selector) => host.querySelector(selector);
  const click = async (selector) => {
    const el = q(selector);
    assert.ok(el, `${selector} is rendered`);
    await harness.act(() => harness.fireEvent(el, 'click'));
  };
  const nodeIDs = () => [...host.querySelectorAll('.process-node')].map((el) => el.dataset.nodeId).sort();
  return { harness, host, editor, q, click, nodeIDs, posts };
}

test('double-clicking a ▣ node enters its body with ports, breadcrumb and minimap; Esc leaves', async (t) => {
  const { harness, editor, q, click, nodeIDs } = await openEditor(t);
  assert.ok(q('[data-node-id="qa"].process-node-subprocess'), 'the ▣ node renders as a subprocess');
  await harness.act(() => harness.fireEvent(q('[data-node-id="qa"]'), 'dblclick'));
  assert.deepEqual(editor.model.scopePath(), ['qa']);
  assert.deepEqual(nodeIDs(), ['@in', '@out:fail', '@out:pass', 'bad', 'in', 'ok', 'unit'], 'the body plus its in/out ports');
  assert.ok(q('[data-node-id="@in"].process-node-port-in'));
  assert.deepEqual([...q('[data-scope-bar]').querySelectorAll('.pscope-crumb')].map((el) => el.textContent.trim()), ['Release', '▣ QA suite']);
  assert.ok(q('[data-scope-minimap] .pscope-mini-node.is-current'), 'the minimap marks where we are in the parent');
  assert.equal(editor.dirty, false);

  await harness.act(() => harness.fireEvent(q('.process-editor'), 'keydown', { key: 'Escape' }));
  assert.deepEqual(editor.model.scopePath(), []);
  assert.deepEqual(editor.selection, { type: 'node', id: 'qa' }, 'leaving selects the ▣ node');
  assert.equal(q('[data-scope-bar]'), null);
});

test('ports edit the parent ▣ node and undo across scopes keeps one history', async (t) => {
  const { harness, editor, q, click } = await openEditor(t);
  await harness.act(() => editor.enterSubprocess('qa'));
  await harness.act(() => harness.fireEvent(q('[data-node-id="@out:pass"]'), 'click'));
  assert.ok(q('[data-scope-ports]'), 'clicking a port opens the port panel');
  const output = q('[data-node-field="scope.outputs.bad"]');
  Object.defineProperty(output, 'value', { configurable: true, writable: true, value: 'pass' });
  await harness.act(() => harness.fireEvent(output, 'change'));
  const input = q('[data-node-field="scope.inputs.target"]');
  Object.defineProperty(input, 'value', { configurable: true, writable: true, value: 'main' });
  await harness.act(() => harness.fireEvent(input, 'change'));
  assert.deepEqual(editor.model.scopePorts().outputs, { ok: 'pass', bad: 'pass' });
  assert.deepEqual(editor.model.scopePorts().inputs, { target: 'main' });
  await harness.act(() => editor.renameNode('unit', 'Unit tests'));

  await click('[data-scope-action="exit"]');
  const saved = editor.model.saveBody().template.nodes.qa.subprocess;
  assert.equal(saved.nodes.unit.name, 'Unit tests');
  assert.deepEqual(saved.outputs, { ok: 'pass', bad: 'pass' });
  await harness.act(() => { editor.applyHistory('undo'); editor.applyHistory('undo'); editor.applyHistory('undo'); });
  assert.equal(editor.dirty, false, 'three edits in the body undo from the parent');
});

test('the outline lists nested bodies and navigates into them', async (t) => {
  const { harness, editor, q } = await openEditor(t);
  assert.ok(q('[data-scope-outline] [data-outline-node="qa/unit"]'));
  await harness.act(() => harness.fireEvent(q('[data-outline-node="qa/unit"]'), 'click'));
  assert.deepEqual(editor.model.scopePath(), ['qa']);
  assert.deepEqual(editor.selection, { type: 'node', id: 'unit' });
  await harness.act(() => harness.fireEvent(q('[data-outline-node="ship"]'), 'click'));
  assert.deepEqual(editor.model.scopePath(), []);
  assert.deepEqual(editor.selection, { type: 'node', id: 'ship' });
});

test('extract as template saves the body and swaps the node to a reference', async (t) => {
  const opened = [];
  const { harness, host, editor, q, click, posts } = await openEditor(t, { onOpenTemplate: (id) => opened.push(id) });
  await harness.act(() => editor.enterSubprocess('qa'));
  await click('[data-scope-action="extract"]');
  const submit = [...host.ownerDocument.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Extract');
  assert.ok(submit, 'a name prompt opens');
  await harness.act(async () => {
    harness.fireEvent(submit.closest('form') || submit, submit.closest('form') ? 'submit' : 'click');
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  const post = posts.find((p) => p.url === '/v1/process/templates');
  assert.ok(post, 'the body is saved as a new template');
  assert.equal(post.body.template.name, 'QA suite');
  assert.ok(post.body.edges.some((e) => e.from === '' && e.to === 'in'));
  assert.deepEqual(editor.model.scopePath(), []);
  assert.deepEqual(editor.model.node('qa').subprocess, {
    templateRef: 'qa-suite', inputs: { target: '{{ params.branch }}' }, outputs: { ok: 'pass', bad: 'fail' },
  });
  assert.ok(editor.dirty);
  // A referenced body is not entered; the inspector offers to open it.
  await harness.act(() => harness.fireEvent(q('[data-node-id="qa"]'), 'dblclick'));
  assert.deepEqual(editor.model.scopePath(), []);
  await harness.act(() => editor.openNodeSettings('qa'));
  await click('[data-subprocess-open-template]');
  assert.deepEqual(opened, ['qa-suite']);
});

test('scribe context from inside a scope names body items by namespaced id on the stored template', async (t) => {
  const { harness, editor } = await openEditor(t);
  await harness.act(() => editor.enterSubprocess('qa'));
  await harness.act(() => editor.setSelection({ type: 'node', id: 'unit' }));
  const { scribeContextInput } = await harness.importDashboardModule('js/process-editor.js');
  const input = scribeContextInput(editor.model, editor.selection, null);
  assert.deepEqual(input.selection, [{ type: 'node', id: 'qa/unit' }]);
  assert.equal(input.template.nodes['qa/unit'].name, 'Unit');
  assert.ok(input.template.nodes.qa && input.template.nodes.ship, 'the root template is described');
  assert.ok(input.edges.some((e) => e.from === 'qa/unit' && e.to === 'qa/ok'));
});
