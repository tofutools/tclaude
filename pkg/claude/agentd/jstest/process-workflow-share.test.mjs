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

const CONTEXT_RESPONSE = { ok: true, status: 200, json: async () => ({ groups: [{ group: 'webshop' }, { group: 'infra' }] }) };

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
  assert.equal(review.credit, '');
  assert.equal(importReview({ ...PREVIEW, author: 'Acme', licence: 'MIT' }).credit, 'By Acme (as the file says) · Licence: MIT');
  assert.equal(importReview({ ...PREVIEW, signatures: [{ status: 'valid', trust: 'known peer laptop' }] }).trusted, true);
  assert.match(importReview({ ...PREVIEW, signatures: [{ status: 'invalid' }] }).signer, /does not verify/);
  assert.equal(importReview({ ...PREVIEW, templates: [{ ref: 'x', id: 'x', status: 'identical' }] }).nothingToDo, true);
});

test('import flow: preview, replace consent, then apply the reviewed digest', async (t) => {
  const harness = await createPreactHarness(t);
  const calls = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    if (String(url) === '/v1/process/context') return CONTEXT_RESPONSE;
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
    if (String(url) === '/v1/process/context') return CONTEXT_RESPONSE;
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

test('importQuery: role mappings, rename and group, in a stable order', async (t) => {
  const harness = await createPreactHarness(t);
  const { importQuery, importReview } = await harness.importDashboardModule('js/process-workflow-share.js');
  assert.equal(importQuery({}), '');
  assert.equal(importQuery({ roleMap: { reviewer: 'qa', dev: 'dev', build: 'eng' }, rename: true, group: 'webshop' }),
    'map=build%3Deng&map=reviewer%3Dqa&rename=1&group=webshop', 'a role mapped to itself is not sent');
  const review = importReview({ ...PREVIEW, templates: [
    { ref: 'ship@sha256:aa', id: 'ship', importRef: 'ship-2@sha256:dd', importId: 'ship-2', head: true, status: 'new' },
    { ref: 'review@sha256:bb', id: 'review', importRef: 'review@sha256:ee', importId: 'review', head: true, status: 'new' },
  ] });
  assert.equal(review.templates[0].label, 'new here, as ship-2');
  assert.equal(review.templates[1].label, 'new here, roles mapped');
  assert.equal(review.renamed, true);
});

test('import flow: each choice is reviewed again, and the apply carries the reviewed choices', async (t) => {
  const harness = await createPreactHarness(t);
  const calls = [];
  const withRoles = {
    ...PREVIEW,
    roleMap: [{ name: 'dev', steps: ['ship:build'], target: 'dev', mapped: false, known: false }],
    localRoles: ['engineer', 'qa'],
  };
  const previous = globalThis.fetch;
  globalThis.fetch = async (url) => {
    if (String(url) === '/v1/process/context') return CONTEXT_RESPONSE;
    calls.push(String(url));
    const u = String(url);
    const digest = `digest-${calls.length}`;
    const renamed = u.includes('rename=1');
    const body = { ...withRoles, reviewDigest: digest, updates: renamed ? 0 : 1, rename: renamed,
      templates: renamed ? [{ ref: 'review@sha256:bb', id: 'review', importId: 'review-2', importRef: 'review-2@sha256:ff', head: true, status: 'new' }] : withRoles.templates,
      roleMap: u.includes('map=dev%3Dengineer') ? [{ ...withRoles.roleMap[0], target: 'engineer', mapped: true, known: true }] : withRoles.roleMap,
      applied: u.includes('apply=1') };
    return { ok: true, status: 200, json: async () => body };
  };
  t.after(() => { globalThis.fetch = previous; });
  const { WorkflowImportButton } = await harness.importDashboardModule('js/process-workflow-share.js');
  const { container } = await harness.mount(harness.html`<${WorkflowImportButton} onImported=${() => {}} />`);
  const input = container.querySelector('[data-pwf-file]');
  Object.defineProperty(input, 'files', { configurable: true, value: [{ name: 'ship.pwf', size: 3, arrayBuffer: async () => new Uint8Array([1]).buffer }] });
  const settle = async () => { await new Promise((resolve) => setTimeout(resolve, 0)); };
  await harness.act(async () => { harness.fireEvent(input, 'change'); await settle(); });
  await harness.act(settle);
  const doc = harness.document;
  assert.ok(doc.querySelector('[data-pwf-role-unknown]'), 'a role nothing here has is flagged');

  const roleSelect = doc.querySelector('[data-pwf-role-map="dev"]');
  Object.defineProperty(roleSelect, 'value', { configurable: true, value: 'engineer' });
  await harness.act(async () => { harness.fireEvent(roleSelect, 'change'); await settle(); });
  assert.equal(calls.at(-1), '/v1/process/import?map=dev%3Dengineer');
  assert.equal(doc.querySelector('[data-pwf-role-unknown]'), null);

  const rename = doc.querySelector('[data-pwf-rename]');
  rename.checked = true;
  await harness.act(async () => { harness.fireEvent(rename, 'change'); await settle(); });
  assert.equal(calls.at(-1), '/v1/process/import?map=dev%3Dengineer&rename=1');
  assert.equal(doc.querySelector('[data-pwf-replace]'), null, 'renaming needs no replace consent');
  assert.match(doc.querySelector('[data-pwf-template="review"]').textContent, /as review-2/);

  const group = doc.querySelector('[data-pwf-group]');
  Object.defineProperty(group, 'value', { configurable: true, value: 'webshop' });
  await harness.act(async () => { harness.fireEvent(group, 'change'); await settle(); });
  const reviewed = calls.length;
  assert.equal(calls.at(-1), '/v1/process/import?map=dev%3Dengineer&rename=1&group=webshop');

  await harness.act(async () => { doc.querySelector('[data-pwf-import]').click(); await settle(); });
  assert.equal(calls.at(-1), `/v1/process/import?apply=1&review=digest-${reviewed}&map=dev%3Dengineer&rename=1&group=webshop`);
});

test('exportWorkflow: a workflow that fails validation is shared only after the operator confirms', async (t) => {
  const harness = await createPreactHarness(t);
  const { exportWorkflow } = await harness.importDashboardModule('js/process-workflow-share.js');
  const respond = (problems) => async (url) => (url.endsWith('?check=1')
    ? { ok: true, status: 200, json: async () => ({ problems }) }
    : { ok: true, status: 200, blob: async () => 'pwf-bytes' });
  const run = async (problems, answer) => {
    const calls = [];
    const asked = [];
    const downloads = [];
    const fetchImpl = async (url, init) => { calls.push(url); return respond(problems)(url, init); };
    const result = await exportWorkflow('ship', {
      fetchImpl, download: (blob, name) => downloads.push(name), confirm: async (req) => { asked.push(req); return answer; },
    });
    return { result, calls, asked, downloads };
  };

  const clean = await run([], false);
  assert.equal(clean.result, '');
  assert.equal(clean.asked.length, 0, 'a valid workflow downloads without asking');
  assert.deepEqual(clean.downloads, ['ship.pwf']);

  const declined = await run(['template "ship": nodes.work: task has no performer'], false);
  assert.equal(declined.result, '', 'declining is not an error');
  assert.equal(declined.asked.length, 1);
  assert.match(declined.asked[0].body, /task has no performer/);
  assert.equal(declined.asked[0].okLabel, 'Share anyway');
  assert.deepEqual(declined.downloads, []);
  assert.deepEqual(declined.calls, ['/v1/process/templates/ship/export?check=1']);

  const confirmed = await run(['template "ship": nodes.work: task has no performer'], true);
  assert.deepEqual(confirmed.downloads, ['ship.pwf']);
});

test('WorkflowExportButton takes a look, a reason and a disabled state from its surface', async (t) => {
  const harness = await createPreactHarness(t);
  const { WorkflowExportButton } = await harness.importDashboardModule('js/process-workflow-share.js');
  const { container } = await harness.mount(harness.html`<${WorkflowExportButton} id="ship" className="gq-link" disabled title="Save first" />`);
  const button = container.querySelector('[data-process-action="export"]');
  assert.equal(button.className, 'gq-link');
  assert.equal(button.disabled, true);
  assert.equal(button.title, 'Save first');
});

test('exportWorkflow: paths from this machine are shared only after the operator confirms', async (t) => {
  const harness = await createPreactHarness(t);
  const { exportWorkflow } = await harness.importDashboardModule('js/process-workflow-share.js');
  const paths = [{ where: 'ship: nodes.merge.performer.run', path: '/home/alice/bin/gh', kind: 'home' }, { where: 'ship: description', path: '/opt/acme/x', kind: 'machine' }];
  const fetchImpl = async (url) => (String(url).endsWith('check=1')
    ? { ok: true, json: async () => ({ problems: [], paths }) }
    : { ok: true, blob: async () => 'file' });
  const asked = [];
  let downloads = 0;
  const run = (answer) => exportWorkflow('ship', { fetchImpl, download: () => { downloads += 1; }, confirm: async (request) => { asked.push(request); return answer; } });

  assert.equal(await run(false), '');
  assert.equal(downloads, 0);
  assert.equal(await run(true), '');
  assert.equal(downloads, 1);
  assert.match(asked[0].title, /paths from this machine/);
  assert.match(asked[0].body, /\/home\/alice\/bin\/gh \(reveals a username\), in ship: nodes\.merge\.performer\.run/);
  assert.match(asked[0].body, /\/opt\/acme\/x \(may not exist elsewhere\)/);
});

test('exportWorkflow: confirming shares named agents with their roles, and the rest stay named', async (t) => {
  const harness = await createPreactHarness(t);
  const { exportWorkflow } = await harness.importDashboardModule('js/process-workflow-share.js');
  const run = async (assignees, answer) => {
    const urls = [];
    const asked = [];
    const fetchImpl = async (url) => {
      urls.push(String(url));
      return String(url).endsWith('check=1') ? { ok: true, json: async () => ({ problems: [], paths: [], assignees }) } : { ok: true, blob: async () => 'file' };
    };
    await exportWorkflow('ship', { fetchImpl, download: () => {}, confirm: async (request) => { asked.push(request); return answer; } });
    return { urls, asked };
  };

  const both = await run([{ name: 'checkout', role: 'dev', steps: ['ship:build'] }, { name: 'solo', steps: ['ship:fix'] }], true);
  assert.equal(both.urls.at(-1), '/v1/process/templates/ship/export?assignees=roles');
  assert.equal(both.asked[0].okLabel, 'Use roles and share');
  assert.match(both.asked[0].body, /checkout \(role dev\), in ship:build/);
  assert.match(both.asked[0].body, /solo, in ship:fix/);

  const none = await run([{ name: 'solo', steps: ['ship:fix'] }], true);
  assert.equal(none.urls.at(-1), '/v1/process/templates/ship/export');
  assert.equal(none.asked[0].okLabel, 'Share anyway');

  const declined = await run([{ name: 'checkout', role: 'dev', steps: ['ship:build'] }], false);
  assert.equal(declined.urls.length, 1, 'declining downloads nothing');
});

test('import dialog: a file this node refuses can be reviewed but not imported', async (t) => {
  const harness = await createPreactHarness(t);
  const previous = globalThis.fetch;
  const refused = { ...PREVIEW, signatures: [], updates: 0, refused: 'this node refuses workflows without a valid signature (processes.refuse_unsigned_imports)' };
  globalThis.fetch = async (url) => (String(url) === '/v1/process/context' ? CONTEXT_RESPONSE : { ok: true, status: 200, json: async () => refused });
  t.after(() => { globalThis.fetch = previous; });
  const { WorkflowImportButton } = await harness.importDashboardModule('js/process-workflow-share.js');
  const { container } = await harness.mount(harness.html`<${WorkflowImportButton} />`);
  const input = container.querySelector('[data-pwf-file]');
  Object.defineProperty(input, 'files', { configurable: true, value: [{ name: 'ship.pwf', size: 3, arrayBuffer: async () => new Uint8Array([1]).buffer }] });
  await harness.act(async () => { harness.fireEvent(input, 'change'); await new Promise((resolve) => setTimeout(resolve, 0)); });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

  const doc = harness.document;
  assert.match(doc.querySelector('[data-pwf-refused]').textContent, /without a valid signature/);
  assert.ok(doc.querySelector('[data-pwf-command="ship:merge"]'), 'the review is still shown');
  const button = doc.querySelector('[data-pwf-import]');
  assert.equal(button.disabled, true);
  assert.equal(button.textContent, 'Refused');
});

// A workflow read out of a .paf shows what else the file holds. Importing lands
// nobody, and the choice of workflow is part of what is reviewed.
test('importQuery names the chosen workflow; the review keeps the envelope', async (t) => {
  const harness = await createPreactHarness(t);
  const { importQuery, importReview } = await harness.importDashboardModule('js/process-workflow-share.js');
  assert.equal(importQuery({ workflow: 'ship' }), 'workflow=ship');
  const envelope = { workflow: 'ship', workflows: [{ key: 'ship', root: 'ship@sha256:aa' }], performers: [{ role: 'dev', target: 'dev', agentKey: 'scout', name: 'scout', harness: 'claude', history: true }] };
  assert.equal(importReview({ ...PREVIEW, envelope }).envelope, envelope);
  assert.equal(importReview(PREVIEW).envelope, null, 'a .pwf has none');
});
