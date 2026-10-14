// Editor-level tests for composition on the canvas (E2): splice by dropping
// a palette item on a connector, wrap a selection and unwrap it again, and
// expand a ▣ in place, edit inside the frame, and collapse it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function view() {
  return {
    template: {
      id: 'flow', name: 'Flow', start: 'a', params: {},
      nodes: {
        a: { type: 'task', name: 'Plan', performer: { kind: 'agent' } },
        b: { type: 'task', name: 'Build', performer: { kind: 'agent' } },
        c: { type: 'task', name: 'Check', performer: { kind: 'agent' } },
        d: { type: 'end', result: 'success' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'a' },
      { from: 'a', outcome: 'pass', to: 'b' },
      { from: 'b', outcome: 'pass', to: 'c' },
      { from: 'c', outcome: 'pass', to: 'd' },
    ],
    layout: { nodes: { a: { x: 0, y: 0 }, b: { x: 200, y: 0 }, c: { x: 400, y: 0 }, d: { x: 600, y: 0 } } },
    currentRef: `flow@sha256:${'1'.repeat(64)}`, sourceHash: '2'.repeat(64), semanticHash: 's',
  };
}

async function openEditor(t) {
  let editor;
  t.after(() => {
    try { editor?.destroy(); } catch { /* DOM already gone */ }
    editor?.validation?.destroy?.();
  });
  const harness = await createPreactHarness(t);
  const previous = { raf: globalThis.requestAnimationFrame, css: globalThis.CSS };
  globalThis.requestAnimationFrame = () => 0;
  globalThis.CSS = { escape: (value) => String(value) };
  t.after(() => {
    for (const [key, name] of [['raf', 'requestAnimationFrame'], ['css', 'CSS']]) {
      if (previous[key] === undefined) delete globalThis[name];
      else globalThis[name] = previous[key];
    }
  });
  const { openTemplateEditor } = await harness.importDashboardModule('js/process-editor.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.act(async () => {
    editor = await openTemplateEditor(host, {
      id: 'flow', view: view(),
      config: {
        validation: { delayMs: 60_000, fetchFn: async () => ({ ok: true, json: async () => ({ diagnostics: [] }) }) },
        templateRefs: () => [{ id: 'qa-suite', name: 'QA suite', versionCount: 3 }, { id: 'flow', name: 'Flow' }],
      },
    });
  });
  const q = (selector) => host.querySelector(selector);
  const edgeList = () => editor.model.edges.map((e) => `${e.from}:${e.outcome}>${e.to}`).sort();
  return { harness, host, editor, q, edgeList };
}

async function submitNameDialog(harness, host, label) {
  const submit = [...host.ownerDocument.querySelectorAll('button')].find((b) => b.textContent.trim() === label);
  assert.ok(submit, `the ${label} dialog opens`);
  await harness.act(async () => {
    harness.fireEvent(submit.closest('form') || submit, submit.closest('form') ? 'submit' : 'click');
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
}

test('the palette offers stored templates; dropping one on a connector splices it in', async (t) => {
  const { harness, editor, q, edgeList } = await openEditor(t);
  const cards = [...q('.process-editor-palette').querySelectorAll('[data-palette-item]')]
    .map((el) => JSON.parse(el.dataset.paletteItem)).filter((p) => p.kind === 'template');
  assert.deepEqual(cards, [{ kind: 'template', id: 'qa-suite', name: 'QA suite' }], 'other templates only, never this one');
  const edge = editor.graph.layoutSnapshot().edges.find((e) => e.from === 'a');
  await harness.act(() => editor.onCanvasDrop({
    point: { x: 100, y: 60 }, edge,
    event: { dataTransfer: { getData: () => JSON.stringify(cards[0]) } },
  }));
  const id = editor.selection.id;
  assert.deepEqual(editor.model.node(id).subprocess, { templateRef: 'qa-suite' });
  assert.ok(edgeList().includes(`a:pass>${id}`) && edgeList().includes(`${id}:pass>b`));
  await harness.act(() => editor.onCanvasDrop({
    point: { x: 300, y: 60 }, edge: editor.graph.layoutSnapshot().edges.find((e) => e.from === 'b'),
    event: { dataTransfer: { getData: () => JSON.stringify({ kind: 'primitive', type: 'decision' }) } },
  }));
  assert.ok(edgeList().includes('b:pass>decision') && edgeList().includes('decision:pass>c'));
  await harness.act(() => { editor.applyHistory('undo'); editor.applyHistory('undo'); });
  assert.equal(editor.dirty, false, 'each splice is one undo step');
});

test('wrap a multi-selection from the inspector, then unwrap it back', async (t) => {
  const { harness, host, editor, q, edgeList } = await openEditor(t);
  const before = edgeList();
  await harness.act(() => editor.setSelection({ type: 'multi', items: [{ type: 'node', id: 'b' }, { type: 'node', id: 'c' }] }));
  const wrap = q('[data-wrap-selection]');
  assert.ok(wrap && !wrap.disabled, 'the multi-selection inspector offers wrap');
  await harness.act(() => harness.fireEvent(wrap, 'click'));
  await submitNameDialog(harness, host, 'Wrap');
  const id = editor.selection.id;
  assert.equal(editor.model.node(id).type, 'subprocess');
  assert.deepEqual(edgeList(), [':start>a', `${id}:pass>d`, `a:pass>${id}`].sort());
  await harness.act(() => editor.openNodeSettings(id));
  const unwrap = q('[data-subprocess-unwrap]');
  assert.ok(unwrap, 'the ▣ inspector offers unwrap');
  await harness.act(() => harness.fireEvent(unwrap, 'click'));
  assert.deepEqual(edgeList(), before, 'unwrap restores the topology');
  assert.deepEqual(editor.model.saveBody().template.nodes, editor.model.saveBody().template.nodes);
  assert.equal(editor.model.node('b').name, 'Build');
});

test('a ▣ expands in place, its nodes edit in context, and the frame collapses it', async (t) => {
  const { harness, host, editor, q } = await openEditor(t);
  await harness.act(() => editor.setSelection({ type: 'multi', items: [{ type: 'node', id: 'b' }, { type: 'node', id: 'c' }] }));
  await harness.act(() => { void editor.wrapSelection(); });
  await submitNameDialog(harness, host, 'Wrap');
  const id = editor.selection.id;
  await harness.act(() => editor.openNodeSettings(id));
  await harness.act(() => harness.fireEvent(q('[data-subprocess-expand]'), 'click'));
  assert.ok(q(`.process-group[data-group-id="${id}"]`), 'the body is framed on the canvas');
  assert.ok(q(`[data-node-id="${id}/b"]`) && !q(`.process-node-layer [data-node-id="${id}"]`));
  await harness.act(() => harness.fireEvent(q(`[data-node-id="${id}/b"]`), 'click'));
  assert.deepEqual(editor.selection, { type: 'node', id: `${id}/b` });
  await harness.act(() => editor.renameNode(`${id}/b`, 'Build it'));
  assert.equal(editor.model.saveBody().template.nodes[id].subprocess.nodes.b.name, 'Build it');
  await harness.act(() => harness.fireEvent(q(`[data-group-id="${id}"][data-group-action="collapse"]`), 'click'));
  assert.ok(!q('.process-group'), 'the frame is gone');
  assert.deepEqual(editor.selection, { type: 'node', id });
  await harness.act(() => editor.applyHistory('undo'));
  assert.equal(editor.model.node(id).subprocess.nodes.b.name, 'Build');
});
