import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t, { prompts = [{ name: 'review', body: 'Review this.\n' }], confirmDiscard = async () => true } = {}) {
  const harness = await createPreactHarness(t);
  const calls = [];
  let rows = prompts;
  t.mock.method(globalThis, 'fetch', async (url, options = {}) => {
    const method = options.method || 'GET';
    calls.push({ url, ...options, method });
    const name = decodeURIComponent(url.split('/')[3] || '');
    if (method === 'GET') return { ok: true, status: 200, json: async () => rows };
    if (method === 'DELETE') rows = rows.filter((row) => row.name !== name);
    else {
      const row = JSON.parse(options.body);
      rows = rows.filter((item) => item.name !== name);
      rows.push(row);
    }
    return { ok: true, status: method === 'DELETE' ? 204 : 200, json: async () => ({}) };
  });
  const { SavedPromptPicker } = await harness.importDashboardModule('js/saved-prompts.js');
  const { useRef, useState } = harness.hooks;
  function Composer() {
    const [body, setBody] = useState('before after');
    const ref = useRef(null);
    return harness.html`<div><textarea aria-label="Message" ref=${ref} value=${body}
      onInput=${(event) => setBody(event.currentTarget.value)}/>
      <${SavedPromptPicker} value=${body} onChange=${setBody} textareaRef=${ref} confirmDiscard=${confirmDiscard}/></div>`;
  }
  const mounted = await harness.mount(harness.html`<${Composer}/>`);
  await harness.act(() => Promise.resolve());
  return { harness, host: mounted.container, mounted, calls };
}

async function flush(harness) {
  for (let turn = 0; turn < 8; turn++) await harness.act(() => Promise.resolve());
}

async function select(harness, host, label, value) {
  const field = host.querySelector(`[aria-label="${label}"]`);
  // LinkeDOM does not implement the browser's writable select.value.
  Object.defineProperty(field, 'value', { configurable: true, writable: true, value });
  await harness.act(async () => { harness.fireEvent(field, 'change'); await Promise.resolve(); });
  await flush(harness);
}

test('insertion replaces the selection, restores the caret, and only edits the composer', async (t) => {
  const { harness, host, mounted, calls } = await setup(t);
  const body = host.querySelector('textarea');
  body.selectionStart = 7; body.selectionEnd = 12;
  body.setSelectionRange = (start, end) => { body.selectionStart = start; body.selectionEnd = end; };
  await select(harness, host, 'Insert saved prompt', 'review');
  assert.equal(body.value, 'before Review this.\n');
  assert.equal(body.selectionStart, body.value.length);
  assert.equal(body.selectionEnd, body.value.length);
  assert.equal(harness.document.activeElement, body);
  assert.equal(calls.every((call) => call.method === 'GET'), true);
  await mounted.unmount(); await mounted.unmount();
});

test('overflow preserves the draft and selected text', async (t) => {
  const { harness, host, mounted } = await setup(t, { prompts: [{ name: 'long', body: 'x'.repeat(16384) }] });
  const body = host.querySelector('textarea');
  await select(harness, host, 'Insert saved prompt', 'long');
  assert.equal(body.value, 'before after');
  assert.match(host.querySelector('[role="alert"]').textContent, /exceed/);
  await mounted.unmount();
});

test('manager creates, edits, renames and confirms deletion without changing the message', async (t) => {
  const { harness, host, mounted, calls } = await setup(t);
  await harness.act(() => harness.getByRole(host, 'button', { name: 'Manage prompts…' }).click());
  await harness.input(host.querySelector('[aria-label="Prompt name"]'), 'new');
  await harness.input(host.querySelector('[aria-label="Prompt body"]'), 'New text');
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Save prompt' }).click(); await Promise.resolve(); });
  await flush(harness);
  assert.deepEqual(JSON.parse(calls.find((call) => call.method === 'POST').body), { name: 'new', body: 'New text' });
  await select(harness, host, 'Edit saved prompt', 'new');
  await harness.input(host.querySelector('[aria-label="Prompt name"]'), 'renamed');
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Save prompt' }).click(); await Promise.resolve(); });
  await flush(harness);
  assert.equal(calls.find((call) => call.method === 'PATCH').url, '/api/saved-prompts/new');
  await select(harness, host, 'Edit saved prompt', 'renamed');
  await harness.act(() => harness.getByRole(host, 'button', { name: 'Delete prompt' }).click());
  assert.equal(calls.some((call) => call.method === 'DELETE'), false);
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Confirm delete' }).click(); await Promise.resolve(); });
  await flush(harness);
  assert.equal(calls.find((call) => call.method === 'DELETE').url, '/api/saved-prompts/renamed');
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Close' }).click(); await Promise.resolve(); });
  await flush(harness);
  assert.equal(host.querySelector('#saved-prompts-manage-modal'), null);
  assert.equal(host.querySelector('textarea').value, 'before after');
  await mounted.unmount();
});

test('dirty prompt navigation and dismissal require confirmation, with errors retaining the draft', async (t) => {
  let allow = false;
  let confirmations = 0;
  const { harness, host, mounted } = await setup(t, { confirmDiscard: async () => { confirmations++; return allow; } });
  await harness.act(() => harness.getByRole(host, 'button', { name: 'Manage prompts…' }).click());
  await harness.input(host.querySelector('[aria-label="Prompt name"]'), 'draft');
  await select(harness, host, 'Edit saved prompt', 'review');
  assert.equal(host.querySelector('[aria-label="Prompt name"]').value, 'draft');
  assert.equal(host.querySelector('[aria-label="Edit saved prompt"]').value, '');
  t.mock.method(globalThis, 'fetch', async () => ({ ok: false, status: 403, text: async () => 'write refused' }));
  await harness.input(host.querySelector('[aria-label="Prompt body"]'), 'keep this');
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Save prompt' }).click(); await Promise.resolve(); });
  await flush(harness);
  assert.match(host.querySelector('#saved-prompts-manage-modal [role="alert"]').textContent, /write refused/);
  assert.equal(host.querySelector('[aria-label="Prompt body"]').value, 'keep this');
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Close' }).click(); await Promise.resolve(); });
  await flush(harness);
  assert.ok(host.querySelector('#saved-prompts-manage-modal'));
  allow = true;
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Close' }).click(); await Promise.resolve(); });
  await flush(harness);
  assert.equal(host.querySelector('#saved-prompts-manage-modal'), null);
  assert.equal(confirmations, 3);
  await mounted.unmount();
});

test('Unicode overflow uses UTF-8 bytes and leaves the draft untouched', async (t) => {
  const { harness, host, mounted } = await setup(t, { prompts: [{ name: 'unicode', body: 'é'.repeat(8192) }] });
  await select(harness, host, 'Insert saved prompt', 'unicode');
  assert.equal(host.querySelector('textarea').value, 'before after');
  assert.match(host.querySelector('[role="alert"]').textContent, /16384-byte/);
  await mounted.unmount();
});

test('manager traps Tab, closes on Escape, and contains the parent submit hotkey', async (t) => {
  const { harness, host, mounted } = await setup(t);
  let parentSubmits = 0;
  host.addEventListener('keydown', (event) => { if (event.key === 'Enter' && event.ctrlKey) parentSubmits++; });
  const manage = harness.getByRole(host, 'button', { name: 'Manage prompts…' });
  manage.focus();
  await harness.act(() => manage.click());
  const name = host.querySelector('[aria-label="Prompt name"]');
  await harness.act(() => harness.fireEvent(name, 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.equal(parentSubmits, 0);
  const last = harness.getByRole(host, 'button', { name: 'Save prompt' });
  // Make Save enabled so it is the last focusable control.
  await harness.input(name, 'new');
  await harness.input(host.querySelector('[aria-label="Prompt body"]'), 'text');
  last.focus();
  await harness.act(() => harness.fireEvent(last, 'keydown', { key: 'Tab' }));
  assert.equal(harness.document.activeElement, host.querySelector('[aria-label="Edit saved prompt"]'));
  await harness.act(async () => { harness.fireEvent(name, 'keydown', { key: 'Escape' }); await Promise.resolve(); });
  await flush(harness);
  assert.equal(host.querySelector('#saved-prompts-manage-modal'), null);
  assert.equal(harness.document.activeElement, manage);
  await mounted.unmount();
});
