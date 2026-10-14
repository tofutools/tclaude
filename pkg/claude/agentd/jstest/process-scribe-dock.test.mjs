// Tests for the process editor's docked scribe (E4): chat mode reads the
// scribe's replies and mails the human's messages; terminal mode opens the
// scribe's terminal pane; context actions carry bounded editor context.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const SCRIBE = Object.freeze({
  agentId: `agt_${'a'.repeat(32)}`, convId: 'scribe-conv-1', name: 'process-scribe-k3x',
  scope: { kind: 'process-template', id: 'dock-flow' }, scopeLabel: 'process template dock-flow', online: true,
});

function view() {
  return {
    template: {
      id: 'dock-flow', name: 'Dock flow', start: 'build',
      nodes: { build: { type: 'task', name: 'Build', performer: { kind: 'agent' } }, done: { type: 'end' } },
    },
    edges: [{ from: '', outcome: 'start', to: 'build' }, { from: 'build', outcome: 'pass', to: 'done' }],
    layout: { nodes: {} }, currentRef: `dock-flow@sha256:${'1'.repeat(64)}`, sourceHash: '2'.repeat(64), semanticHash: 'semantic-1',
  };
}

async function openEditor(t, { scribes = [SCRIBE], chat = [] } = {}) {
  // Registered BEFORE the harness: after-hooks run FIFO, and the dock's chat
  // poll must stop while the DOM still exists, or its timer holds node open.
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
    return { ok: true, status: 202, json: async () => ({ id: 1, queued: true }) };
  };
  t.after(() => {
    for (const [key, name] of [['raf', 'requestAnimationFrame'], ['css', 'CSS'], ['fetch', 'fetch']]) {
      if (previous[key] === undefined) delete globalThis[name];
      else globalThis[name] = previous[key];
    }
  });
  const [{ openTemplateEditor }, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/process-editor.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  dashPrefs.removeItem('tclaude.dash.processEditor.scribeMode');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opened = [];
  const chatReads = [];
  await harness.act(async () => {
    editor = await openTemplateEditor(host, {
      id: 'dock-flow', view: view(),
      config: {
        validation: { delayMs: 60_000, fetchFn: async () => ({ ok: true, json: async () => ({ diagnostics: [] }) }) },
        onScribe: async () => true,
        scribeDock: {
          scribes: () => scribes,
          openTerminal: (scribe) => { opened.push(scribe.agentId); return true; },
          fetchChat: async (agentId) => { chatReads.push(agentId); return chat; },
        },
      },
    });
  });
  const dock = () => host.querySelector('.process-scribe-dock');
  const click = async (selector) => {
    const el = host.querySelector(selector);
    assert.ok(el, `${selector} is rendered`);
    await harness.act(() => harness.fireEvent(el, 'click'));
  };
  const settle = () => harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  return { harness, host, editor, dock, click, settle, posts, opened, chatReads, dashPrefs };
}

test('the header toggles a docked chat that shows the scribe conversation', async (t) => {
  const chat = [
    { id: 'h1', role: 'human', body: 'Add a review gate after build.', at: '' },
    { id: 's1', role: 'scribe', body: 'Added it; saved as a new version for you to review.', at: '' },
  ];
  const { editor, dock, click, settle, chatReads } = await openEditor(t, { chat });
  assert.equal(dock(), null, 'the dock starts closed');
  await click('.process-scribe-action');
  await settle();
  assert.equal(editor.scribeDock.open, true);
  assert.equal(dock().dataset.scribeDock, 'chat');
  assert.match(dock().textContent, /process-scribe-k3x/);
  assert.match(dock().textContent, /scope: process template dock-flow/);
  assert.deepEqual(chatReads, [SCRIBE.agentId]);
  assert.deepEqual([...dock().querySelectorAll('[data-scribe-msg]')].map((el) => el.dataset.scribeMsg), ['human', 'scribe']);
  assert.match(dock().textContent, /saved as a new version/);
  await click('.psd-close');
  assert.equal(dock(), null);
});

test('chat sends operator mail; context actions carry bounded editor context', async (t) => {
  const { harness, editor, dock, click, settle, posts } = await openEditor(t);
  await click('.process-scribe-action');
  await settle();
  const input = dock().querySelector('.psd-input');
  Object.defineProperty(input, 'value', { configurable: true, writable: true, value: 'Rename build to Compile.' });
  await harness.act(() => harness.fireEvent(input, 'input'));
  await harness.act(() => harness.fireEvent(dock().querySelector('.psd-compose'), 'submit'));
  await settle();
  const sent = posts.filter((post) => post.url === '/api/operator-message');
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0].body, { to: SCRIBE.convId, subject: 'process editor', body: 'Rename build to Compile.' });

  assert.ok(dock().querySelector('[data-scribe-action="selection"]').disabled, 'send selection needs a selection');
  await harness.act(() => editor.setSelection({ type: 'node', id: 'build' }));
  assert.match(dock().textContent, /selection: Build/);
  await click('[data-scribe-action="explain"]');
  await settle();
  const explain = posts.filter((post) => post.url === '/api/operator-message').at(-1).body.body;
  assert.match(explain, /^Explain what the selected node does/);
  assert.match(explain, /BEGIN BOUNDED EDITOR CONTEXT/);
  assert.match(explain, /"build"/);
});

test('terminal mode opens the scribe terminal once and is remembered', async (t) => {
  const { dock, click, settle, opened, dashPrefs } = await openEditor(t);
  await click('.process-scribe-action');
  await click('[data-scribe-mode="terminal"]');
  await settle();
  assert.equal(dock().dataset.scribeDock, 'terminal');
  assert.deepEqual(opened, [SCRIBE.agentId]);
  assert.equal(dock().querySelector('.psd-log'), null, 'no chat log in terminal mode');
  await click('[data-scribe-open-terminal]');
  assert.deepEqual(opened, [SCRIBE.agentId, SCRIBE.agentId]);
  assert.equal(dashPrefs.getItem('tclaude.dash.processEditor.scribeMode'), 'terminal');
});

test('without a scoped scribe the dock offers to summon one', async (t) => {
  const other = { ...SCRIBE, scope: { kind: 'process-template', id: 'other' } };
  const { editor, dock, click, settle } = await openEditor(t, { scribes: [other] });
  let summoned = 0;
  editor.requestScribe = async () => { summoned += 1; return true; };
  await click('.process-scribe-action');
  await settle();
  assert.match(dock().textContent, /No scribe is scoped to this template/);
  assert.equal(dock().querySelector('.psd-input'), null);
  await click('.psd-summon .process-action');
  assert.equal(summoned, 1);
});
