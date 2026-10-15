// Tests for sharing processes as .pwf files from the Templates tab: the import
// review says who signed the file, what it runs and what changes here, and an
// import applies exactly the reviewed preview.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const PREVIEW = Object.freeze({
  title: 'Ship a feature', summary: 'Implement, review, approve, merge.', root: 'ship@sha256:aa', rootId: 'ship',
  signatures: [{ keyId: 'k1', status: 'valid', trust: 'unknown key' }],
  params: [{ name: 'issue', type: 'string', required: true }],
  roles: [{ name: 'dev', steps: ['ship:build'] }],
  programs: [{ step: 'ship:merge', profile: 'git', run: 'gh', args: ['pr', 'merge'] }],
  assignees: [{ name: 'checkout', steps: ['ship:build'] }],
  humans: ['ship:approve'],
  templates: [
    { ref: 'ship@sha256:aa', id: 'ship', head: true, status: 'new' },
    { ref: 'review@sha256:bb', id: 'review', head: true, status: 'update', currentRef: 'review@sha256:cc' },
  ],
  updates: 1, applied: false, reviewDigest: 'digest-1',
});

test('importReview: signer, commands and what changes', async (t) => {
  const harness = await createPreactHarness(t);
  const { importReview } = await harness.importDashboardModule('js/process-workflow-share.js');
  const review = importReview(PREVIEW);
  assert.match(review.signer, /unknown key/);
  assert.equal(review.trusted, false);
  assert.equal(review.programs[0].command, 'gh pr merge');
  assert.equal(review.templates[1].label, 'replaces your current version');
  assert.equal(review.nothingToDo, false);

  assert.match(importReview({ ...PREVIEW, signatures: [] }).signer, /^Unsigned/);
  assert.equal(importReview({ ...PREVIEW, signatures: [{ status: 'valid', trust: 'known peer laptop' }] }).trusted, true);
  assert.match(importReview({ ...PREVIEW, signatures: [{ status: 'invalid' }] }).signer, /does not verify/);
  assert.equal(importReview({ ...PREVIEW, templates: [{ ref: 'x', id: 'x', status: 'identical' }] }).nothingToDo, true);
});

test('import flow: preview, replace consent, then apply the reviewed digest', async (t) => {
  const harness = await createPreactHarness(t);
  const calls = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), method: init.method, type: init.headers?.['Content-Type'] });
    return { ok: true, status: 200, json: async () => (String(url).includes('apply=1') ? { ...PREVIEW, applied: true } : PREVIEW) };
  };
  t.after(() => { globalThis.fetch = previous; });
  const { WorkflowImportButton } = await harness.importDashboardModule('js/process-workflow-share.js');
  let imported = null;
  const { container } = await harness.mount(harness.html`<${WorkflowImportButton} onImported=${(result) => { imported = result; }} />`);

  const input = container.querySelector('[data-pwf-file]');
  const bytes = new Uint8Array([1, 2, 3]);
  Object.defineProperty(input, 'files', { configurable: true, value: [{ name: 'ship.pwf', size: 3, arrayBuffer: async () => bytes.buffer }] });
  await harness.act(async () => { harness.fireEvent(input, 'change'); await new Promise((resolve) => setTimeout(resolve, 0)); });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

  assert.equal(calls.length, 1, 'choosing a file only previews');
  assert.equal(calls[0].url, '/v1/process/import');
  assert.equal(calls[0].type, 'application/vnd.tclaude.pwf+zip');
  const doc = harness.document;
  assert.ok(doc.querySelector('[data-pwf-command="ship:merge"]'), 'the review lists the command');
  assert.ok(doc.querySelector('[data-pwf-assignees]'), 'the review flags the non-portable step');
  assert.match(doc.querySelector('[data-pwf-signer]').textContent, /unknown key/);
  const importBtn = doc.querySelector('[data-pwf-import]');
  assert.equal(importBtn.disabled, true, 'an update needs explicit replace consent');

  const replace = doc.querySelector('[data-pwf-replace]');
  replace.checked = true;
  await harness.act(() => harness.fireEvent(replace, 'change'));
  assert.equal(doc.querySelector('[data-pwf-import]').disabled, false);
  await harness.act(async () => { doc.querySelector('[data-pwf-import]').click(); await new Promise((resolve) => setTimeout(resolve, 0)); });

  assert.equal(calls.length, 2);
  assert.equal(calls[1].url, '/v1/process/import?apply=1&review=digest-1&replace=1');
  assert.equal(imported?.applied, true);
  assert.equal(doc.querySelector('[data-pwf-import]'), null, 'the dialog closes after importing');
});

test('displayArg: the review shows argv boundaries and invisible characters', async (t) => {
  const harness = await createPreactHarness(t);
  const { displayArg, importReview } = await harness.importDashboardModule('js/process-workflow-share.js');
  assert.equal(displayArg('merge'), 'merge');
  assert.equal(displayArg('/tmp/x /home'), "'/tmp/x /home'");
  assert.equal(displayArg(''), "''");
  assert.equal(displayArg('a\nb'), 'a\\u{a}b');
  assert.equal(displayArg('x‮y'), 'x\\u{202e}y');
  const review = importReview({ ...PREVIEW, programs: [{ step: 's', run: 'rm', args: ['-rf', '/tmp/x /home'] }] });
  assert.equal(review.programs[0].command, "rm -rf '/tmp/x /home'");
});

test('import flow: a stale review shows the fresh preview and refreshes the list', async (t) => {
  const harness = await createPreactHarness(t);
  const calls = [];
  const fresh = { ...PREVIEW, updates: 0, reviewDigest: 'digest-2', templates: [{ ref: 'ship@sha256:aa', id: 'ship', head: true, status: 'new' }] };
  const previous = globalThis.fetch;
  globalThis.fetch = async (url) => {
    calls.push(String(url));
    if (calls.length === 2) {
      return { ok: false, status: 409, statusText: 'Conflict', json: async () => ({ error: 'process_import_review_stale', message: 'The workflow changed since the review.', preview: fresh }) };
    }
    return { ok: true, status: 200, json: async () => (String(url).includes('apply=1') ? { ...fresh, applied: true } : PREVIEW) };
  };
  t.after(() => { globalThis.fetch = previous; });
  const { WorkflowImportButton } = await harness.importDashboardModule('js/process-workflow-share.js');
  const refreshes = [];
  const { container } = await harness.mount(harness.html`<${WorkflowImportButton} onImported=${(result) => refreshes.push(result)} />`);
  const input = container.querySelector('[data-pwf-file]');
  Object.defineProperty(input, 'files', { configurable: true, value: [{ name: 'ship.pwf', size: 3, arrayBuffer: async () => new Uint8Array([1]).buffer }] });
  await harness.act(async () => { harness.fireEvent(input, 'change'); await new Promise((resolve) => setTimeout(resolve, 0)); });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  const doc = harness.document;
  const replace = doc.querySelector('[data-pwf-replace]');
  replace.checked = true;
  await harness.act(() => harness.fireEvent(replace, 'change'));
  await harness.act(async () => { doc.querySelector('[data-pwf-import]').click(); await new Promise((resolve) => setTimeout(resolve, 0)); });

  assert.deepEqual(refreshes, [null], 'a failed apply refreshes the list');
  assert.match(doc.querySelector('[role="alert"]').textContent, /check it again/);
  assert.equal(doc.querySelector('[data-pwf-replace]'), null, 'the fresh preview has no update to consent to');
  await harness.act(async () => { doc.querySelector('[data-pwf-import]').click(); await new Promise((resolve) => setTimeout(resolve, 0)); });
  assert.equal(calls[2], '/v1/process/import?apply=1&review=digest-2');
  assert.equal(refreshes.at(-1)?.applied, true);
});
