// Editor-level tests for the on-canvas proposal review (E4): a newer saved
// version (the scribe's) is painted over the live draft, each change is
// accepted or rejected, and the merge lands as one undoable edit.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function baseView() {
  return {
    template: {
      id: 'review-flow', name: 'Review flow', start: 'build',
      nodes: {
        build: { type: 'task', name: 'Build', performer: { kind: 'agent' } },
        test: { type: 'task', name: 'Test', performer: { kind: 'agent' } },
        done: { type: 'end', result: 'success' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'build' },
      { from: 'build', outcome: 'pass', to: 'test' },
      { from: 'test', outcome: 'pass', to: 'done' },
    ],
    layout: { nodes: { build: { x: 0, y: 0 }, test: { x: 200, y: 0 }, done: { x: 400, y: 0 } } },
    currentRef: 'review-flow@sha256:1111', sourceHash: 'source-1', semanticHash: 'semantic-1',
  };
}

// The scribe's version: a human review gate between test and done, and a
// renamed build node.
function proposalView() {
  const view = structuredClone(baseView());
  view.template.nodes.build.name = 'Build it';
  view.template.nodes.gate = { type: 'decision', name: 'Review', performer: { kind: 'human', ask: 'Ship?' } };
  view.edges = view.edges.filter((edge) => edge.from !== 'test');
  view.edges.push({ from: 'test', outcome: 'pass', to: 'gate' }, { from: 'gate', outcome: 'yes', to: 'done' });
  view.layout.nodes.gate = { x: 300, y: 120 };
  return Object.assign(view, { currentRef: 'review-flow@sha256:2222', sourceHash: 'source-2', semanticHash: 'semantic-2222abcd' });
}

async function openEditor(t) {
  const harness = await createPreactHarness(t);
  const previous = { raf: globalThis.requestAnimationFrame, css: globalThis.CSS };
  globalThis.requestAnimationFrame = () => 0;
  globalThis.CSS = { escape: (value) => String(value) };
  t.after(() => {
    if (previous.raf === undefined) delete globalThis.requestAnimationFrame;
    else globalThis.requestAnimationFrame = previous.raf;
    if (previous.css === undefined) delete globalThis.CSS;
    else globalThis.CSS = previous.css;
  });
  const { openTemplateEditor } = await harness.importDashboardModule('js/process-editor.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const view = baseView();
  let editor;
  await harness.act(async () => {
    editor = await openTemplateEditor(host, {
      id: view.template.id, view,
      config: { validation: { delayMs: 60_000, fetchFn: async () => ({ ok: true, json: async () => ({ diagnostics: [] }) }) } },
    });
  });
  t.after(() => {
    try { editor?.destroy(); } catch { /* DOM already gone */ }
    editor?.validation?.destroy?.();
  });
  const proposal = proposalView();
  const arrive = async () => {
    editor.externalChange = {
      kind: editor.dirty ? 'dirty' : 'clean', ref: proposal.currentRef, sourceHash: proposal.sourceHash,
      review: { view: proposal, summary: null },
    };
    await harness.act(() => editor.renderExternalChange());
  };
  const bar = () => host.querySelector('[data-diff-review]');
  const click = async (selector, root = host) => {
    const el = root.querySelector(selector);
    assert.ok(el, `${selector} is rendered`);
    await harness.act(() => harness.fireEvent(el, 'click'));
  };
  const key = async (k) => {
    await harness.act(() => harness.fireEvent(host.querySelector('.process-editor'), 'keydown', { key: k }));
  };
  return { harness, host, editor, proposal, arrive, bar, click, key };
}

test('Review on canvas paints the proposal and settles change by change', async (t) => {
  const { host, editor, arrive, bar, click, key } = await openEditor(t);
  await arrive();
  await click('.process-editor-external [data-review-on-canvas]');
  assert.ok(editor.diffReview, 'the review is open');
  assert.match(bar().textContent, /\+2 added/);
  assert.match(bar().textContent, /~2 changed/);
  assert.match(bar().textContent, /0 of 4 decided/);
  assert.ok(host.querySelector('[data-node-id="gate"].is-diff-added'), 'the proposed node is drawn on the canvas');
  assert.ok(host.querySelector('[data-node-id="build"].is-diff-changed'));
  assert.equal(bar().querySelector('[data-diff-current]').dataset.diffCurrent, 'node:gate');

  assert.equal(editor.renameNode('build', 'nope'), undefined, 'the draft is frozen while reviewing');
  assert.equal(editor.model.node('build').name, 'Build');

  await key('a');
  assert.match(bar().textContent, /1 of 4 decided/);
  assert.equal(bar().querySelector('[data-diff-current]').dataset.diffCurrent, 'node:build', 'accepting steps to the next pending change');
  await click('[data-diff-action="reject"]', bar());
  assert.match(bar().textContent, /2 of 4 decided/);
  await click('[data-diff-action="accept-all"]', bar());

  assert.equal(editor.diffReview, null, 'deciding the last change merges and closes the review');
  assert.equal(bar(), null);
  assert.ok(editor.model.node('gate'), 'the accepted node is in the draft');
  assert.equal(editor.model.node('build').name, 'Build', 'the rejected rename is not');
  assert.equal(editor.model.findEdge('test', 'pass').to, 'gate');
  assert.equal(editor.model.currentRef, 'review-flow@sha256:2222', 'the draft is now based on the reviewed version');
  assert.ok(editor.dirty, 'a partial accept leaves the human\'s choices to save');
  assert.match(host.textContent, /3 accepted, 1 rejected/);

  editor.applyHistory('undo');
  assert.equal(editor.model.node('gate'), undefined, 'the whole merge is one undo step');
});

test('accepting everything is clean; Escape closes without merging', async (t) => {
  const { editor, arrive, bar, click, key } = await openEditor(t);
  await arrive();
  await click('.process-editor-external [data-review-on-canvas]');
  await key('Escape');
  assert.equal(editor.diffReview, null);
  assert.equal(editor.model.node('gate'), undefined, 'closing merges nothing');
  assert.equal(bar(), null);

  await click('.process-editor-external [data-review-on-canvas]');
  await click('[data-diff-action="accept-all"]', bar());
  assert.equal(editor.model.node('build').name, 'Build it');
  assert.equal(editor.dirty, false, 'accepting every change is exactly the saved version');
});

test('a proposal over an unsaved local edit asks before taking it', async (t) => {
  const { harness, host, editor, arrive, bar, click } = await openEditor(t);
  await harness.act(() => editor.renameNode('build', 'Build locally'));
  assert.ok(editor.dirty);
  await arrive();
  await click('.process-editor-external [data-review-on-canvas]');
  assert.match(bar().textContent, /1 conflict/);
  await click('[data-diff-action="accept-all"]', bar());
  const keep = [...host.ownerDocument.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Keep my edits');
  assert.ok(keep, 'the conflict prompt offers to keep local edits');
  await harness.act(() => harness.fireEvent(keep, 'click'));
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  assert.equal(editor.diffReview, null);
  assert.equal(editor.model.node('build').name, 'Build locally', 'the local edit survives');
  assert.ok(editor.model.node('gate'), 'the non-conflicting changes were accepted');
});

test('a validation round during the review keeps the proposal painted', async (t) => {
  const { harness, host, editor, arrive, click } = await openEditor(t);
  await arrive();
  await click('.process-editor-external [data-review-on-canvas]');
  await harness.act(() => editor.validation.applyDiagnostics([]));
  assert.ok(host.querySelector('[data-node-id="gate"].is-diff-added'), 'diagnostics repaint keeps the overlay');
});
