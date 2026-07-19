// Covers BOTH process-template delete affordances — the row trash button and
// the drag-to-bin gesture — plus the shared commit they route through.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const prefs = () => {
  const values = new Map();
  return { getItem: (key) => values.get(key) || null, setItem: (key, value) => values.set(key, value) };
};

// templateListFetch serves one template list and records every request, so a
// test can assert the exact DELETE that went out.
function templateListFetch(requests, { deleteResponse } = {}) {
  return async (path, options = {}) => {
    requests.push({ path, method: options.method || 'GET' });
    if (options.method === 'DELETE') return deleteResponse();
    // The list keeps serving the row: the refused-delete test asserts the row
    // survives, and the happy path asserts the DELETE that went out rather than
    // a simulated server-side removal.
    if (path === '/v1/process/templates') {
      return { ok: true, json: async () => ({ templates: [{
        id: 'release', name: 'Release train', versionCount: 3, latestVersion: { ref: `release@sha256:${'a'.repeat(64)}`, sourceHash: 'src' },
      }] }) };
    }
    if (path === '/v1/process/runs') return { ok: true, json: async () => ({ runs: [] }) };
    throw new Error(`unexpected ${path}`);
  };
}

async function mountTemplates(t, { deleteResponse, confirm = async () => true }) {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'),
    harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = [];
  const confirmCalls = [];
  const actions = createProcessesActions({
    state,
    notify() {},
    dispatchNavigated() {},
    fetchImpl: templateListFetch(requests, { deleteResponse }),
    confirm: async (spec) => { confirmCalls.push(spec); return confirm(spec); },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());
  return { harness, mounted, state, actions, requests, confirmCalls };
}

test('the row delete button confirms, DELETEs the template, and refreshes the list', async (t) => {
  const { harness, mounted, requests, confirmCalls, state } = await mountTemplates(t, {
    deleteResponse: () => ({ ok: true, status: 200, json: async () => ({ deleted: 'release' }) }),
  });

  const button = mounted.container.querySelector('[data-process-action="delete"]');
  assert.ok(button, 'every template row offers a delete action');
  assert.equal(button.getAttribute('aria-label'), 'Delete Release train');

  await harness.act(() => harness.fireEvent(button, 'click'));
  for (let i = 0; i < 10; i++) await harness.act(() => Promise.resolve());

  assert.equal(confirmCalls.length, 1, 'delete is never silent');
  assert.match(confirmCalls[0].body, /permanently deletes/);
  assert.match(confirmCalls[0].body, /all 3 versions/, 'the confirm names how much history goes away');
  assert.equal(confirmCalls[0].meta, 'release');

  const sent = requests.filter((request) => request.method === 'DELETE');
  assert.deepEqual(sent.map((request) => request.path), ['/v1/process/templates/release']);
  assert.match(state.view.value.notice, /Deleted Release train/);
});

// The editor owns a URL of its own (/processes/templates/<id>). Deleting the
// template it has open must close it AND fix the address bar, as a correction
// rather than a push — a Back entry pointing at a deleted editor can only fail
// to restore.
test('deleting the open template closes its editor and corrects the URL', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'),
    harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const navigations = [];
  const actions = createProcessesActions({
    state, notify() {}, confirm: async () => true,
    dispatchNavigated: (location, options = {}) => navigations.push({ location, ...options }),
    fetchImpl: async (path, options = {}) => {
      if (options.method === 'DELETE') return { ok: true, status: 200, json: async () => ({ deleted: 'release' }) };
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [] }) };
      throw new Error(`unexpected ${path}`);
    },
  });

  state.setCanvas({ kind: 'editor', id: 'release', key: 'release:1' });
  state.setEditor({ model: { template: { id: 'release' }, dirty: false } });

  await actions.deleteTemplate({ id: 'release', name: 'Release train', versionCount: 1 });

  assert.equal(state.currentEditor(), null, 'the editor for the deleted template is closed');
  assert.equal(state.canvas.value, null);
  const correction = navigations.at(-1);
  assert.ok(correction, 'the router is told the URL changed');
  assert.equal(correction.correction, true, 'it replaces rather than pushes');
  assert.equal(correction.location?.templateId ?? '', '', 'the URL no longer names the deleted template');
});

// Deleting some OTHER template must leave an open editor alone.
test('deleting a different template leaves the open editor untouched', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'),
    harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const navigations = [];
  const actions = createProcessesActions({
    state, notify() {}, confirm: async () => true,
    dispatchNavigated: (location, options = {}) => navigations.push({ location, ...options }),
    fetchImpl: async (path, options = {}) => {
      if (options.method === 'DELETE') return { ok: true, status: 200, json: async () => ({ deleted: 'other' }) };
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [] }) };
      throw new Error(`unexpected ${path}`);
    },
  });

  state.setCanvas({ kind: 'editor', id: 'release', key: 'release:1' });
  state.setEditor({ model: { template: { id: 'release' }, dirty: false } });

  await actions.deleteTemplate({ id: 'other', name: 'Other', versionCount: 1 });

  assert.ok(state.currentEditor(), 'an unrelated delete must not close the editor');
  assert.equal(navigations.length, 0, 'and must not touch the URL');
});

test('declining the confirm sends no request at all', async (t) => {
  const { harness, mounted, requests } = await mountTemplates(t, {
    deleteResponse: () => { throw new Error('must not reach the network'); },
    confirm: async () => false,
  });

  const button = mounted.container.querySelector('[data-process-action="delete"]');
  await harness.act(() => harness.fireEvent(button, 'click'));
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());

  assert.equal(requests.filter((request) => request.method === 'DELETE').length, 0);
});

test('a template still needed by unfinished runs reports which runs block it', async (t) => {
  const { harness, mounted, state } = await mountTemplates(t, {
    deleteResponse: () => ({
      ok: false, status: 409,
      json: async () => ({ code: 'process_template_in_use', runIds: ['run-a', 'run-b', 'run-c', 'run-d'] }),
    }),
  });

  const button = mounted.container.querySelector('[data-process-action="delete"]');
  await harness.act(() => harness.fireEvent(button, 'click'));
  for (let i = 0; i < 10; i++) await harness.act(() => Promise.resolve());

  const notice = state.view.value.notice;
  assert.match(notice, /4 runs still need it/);
  assert.match(notice, /run-a, run-b, run-c and 1 more/, 'the blocking list stays bounded');
  assert.match(notice, /Finish or cancel them first/, 'the notice says what to do next');
  // The row must survive a refused delete.
  assert.ok(mounted.container.querySelector('[data-process-template="release"]'));
});

test('wizard mode speaks the rite vocabulary in the confirm', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'),
    harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const confirmCalls = [];
  const actions = createProcessesActions({
    state, notify() {}, dispatchNavigated() {},
    fetchImpl: async () => ({ ok: true, status: 200, json: async () => ({ deleted: 'release' }) }),
    confirm: async (spec) => { confirmCalls.push(spec); return false; },
  });

  harness.window.document.body.classList.add('wizard');
  t.after(() => harness.window.document.body.classList.remove('wizard'));
  await actions.deleteTemplate({ id: 'release', name: 'Release train', versionCount: 3 });

  assert.equal(confirmCalls[0].title, 'Unmake this rite?');
  assert.equal(confirmCalls[0].okLabel, 'Unmake rite');
  assert.match(confirmCalls[0].body, /inscribed version/);
});

test('dragging a template row to the bin runs the same delete commit', async (t) => {
  const harness = await createPreactHarness(t);
  const { bindProcessTemplateDnd, setProcessTemplateDeleteHandler } =
    await harness.importDashboardModule('js/process-template-dnd.js');
  const doc = harness.window.document;

  // The bin lives in dashboard.html, outside the island; stand in for it.
  const trash = doc.createElement('div');
  trash.id = 'dnd-trash';
  doc.body.appendChild(trash);
  const pill = doc.createElement('div');
  pill.id = 'dnd-pill';
  doc.body.appendChild(pill);
  const row = doc.createElement('tr');
  row.setAttribute('data-process-template-drag', 'release');
  row.setAttribute('data-process-template-name', 'Release train');
  row.setAttribute('data-process-template-versions', '3');
  doc.body.appendChild(row);

  const deleted = [];
  setProcessTemplateDeleteHandler((spec) => { deleted.push(spec); });
  const unbind = bindProcessTemplateDnd();
  t.after(() => { unbind(); setProcessTemplateDeleteHandler(null); trash.remove(); pill.remove(); row.remove(); });

  const transfer = { data: {}, setData(type, value) { this.data[type] = value; }, get types() { return Object.keys(this.data); } };
  harness.fireEvent(row, 'dragstart', { dataTransfer: transfer, target: row });

  assert.equal(transfer.data['application/x-tclaude-process-template'], 'release');
  assert.equal(transfer.data['text/plain'], undefined, 'text/plain is withheld so dnd.js ignores this drag');
  assert.ok(trash.classList.contains('show'), 'the bin appears for the gesture');
  assert.ok(trash.classList.contains('dnd-trash-template-mode'), 'the bin speaks the template label voice');

  harness.fireEvent(trash, 'drop', { dataTransfer: transfer, target: trash });

  assert.deepEqual(deleted, [{ id: 'release', name: 'Release train', versionCount: 3 }]);
  assert.equal(trash.classList.contains('show'), false, 'the bin is torn down after the drop');
  assert.equal(trash.classList.contains('dnd-trash-template-mode'), false);
});

// Regression: unbinding mid-gesture used to strand the shared bin in template
// mode, so the NEXT agent retire-drag showed "Delete"/"Unmake".
test('unbinding mid-drag restores the bin for the other drag modules', async (t) => {
  const harness = await createPreactHarness(t);
  const { bindProcessTemplateDnd, setProcessTemplateDeleteHandler } =
    await harness.importDashboardModule('js/process-template-dnd.js');
  const doc = harness.window.document;
  const trash = doc.createElement('div');
  trash.id = 'dnd-trash';
  doc.body.appendChild(trash);
  const row = doc.createElement('tr');
  row.setAttribute('data-process-template-drag', 'release');
  doc.body.appendChild(row);

  setProcessTemplateDeleteHandler(() => {});
  const unbind = bindProcessTemplateDnd();
  t.after(() => { setProcessTemplateDeleteHandler(null); trash.remove(); row.remove(); });

  const transfer = { data: {}, setData(type, value) { this.data[type] = value; }, get types() { return Object.keys(this.data); } };
  harness.fireEvent(row, 'dragstart', { dataTransfer: transfer, target: row });
  assert.ok(trash.classList.contains('dnd-trash-template-mode'));

  unbind();

  assert.equal(trash.classList.contains('show'), false, 'the bin is hidden again');
  assert.equal(trash.classList.contains('dnd-trash-template-mode'), false,
    'the bin returns to the agent label voice');
});

// Regression: dragend bubbles, so an unguarded document handler would let ANY
// other module's drag ending clear the shared bin and pill.
test('another module\'s dragend does not clear the shared bin', async (t) => {
  const harness = await createPreactHarness(t);
  const { bindProcessTemplateDnd, setProcessTemplateDeleteHandler } =
    await harness.importDashboardModule('js/process-template-dnd.js');
  const doc = harness.window.document;
  const trash = doc.createElement('div');
  trash.id = 'dnd-trash';
  trash.classList.add('show');
  doc.body.appendChild(trash);
  // A foreign drag source — not one of our rows.
  const foreign = doc.createElement('div');
  doc.body.appendChild(foreign);

  setProcessTemplateDeleteHandler(() => {});
  const unbind = bindProcessTemplateDnd();
  t.after(() => { unbind(); setProcessTemplateDeleteHandler(null); trash.remove(); foreign.remove(); });

  harness.fireEvent(foreign, 'dragend', { target: foreign });

  assert.ok(trash.classList.contains('show'),
    'a foreign dragend must leave the shared bin alone');
});

test('a store with unreadable runs explains repair rather than completion', async (t) => {
  const { harness, mounted, state } = await mountTemplates(t, {
    deleteResponse: () => ({
      ok: false, status: 409,
      json: async () => ({ code: 'process_template_in_use', runIds: [], unreadableRunIds: ['run-corrupt'] }),
    }),
  });

  const button = mounted.container.querySelector('[data-process-action="delete"]');
  await harness.act(() => harness.fireEvent(button, 'click'));
  for (let i = 0; i < 10; i++) await harness.act(() => Promise.resolve());

  const notice = state.view.value.notice;
  assert.match(notice, /run-corrupt/);
  assert.match(notice, /Repair or remove them first/);
  assert.doesNotMatch(notice, /Finish or cancel/, 'an unreadable run is not something you can finish');
});

test('a cancelled drag tears the bin down without deleting', async (t) => {
  const harness = await createPreactHarness(t);
  const { bindProcessTemplateDnd, setProcessTemplateDeleteHandler } =
    await harness.importDashboardModule('js/process-template-dnd.js');
  const doc = harness.window.document;
  const trash = doc.createElement('div');
  trash.id = 'dnd-trash';
  doc.body.appendChild(trash);
  const row = doc.createElement('tr');
  row.setAttribute('data-process-template-drag', 'release');
  doc.body.appendChild(row);

  const deleted = [];
  setProcessTemplateDeleteHandler((spec) => { deleted.push(spec); });
  const unbind = bindProcessTemplateDnd();
  t.after(() => { unbind(); setProcessTemplateDeleteHandler(null); trash.remove(); row.remove(); });

  const transfer = { data: {}, setData(type, value) { this.data[type] = value; }, get types() { return Object.keys(this.data); } };
  harness.fireEvent(row, 'dragstart', { dataTransfer: transfer, target: row });
  assert.ok(trash.classList.contains('show'));

  // Escape / release over nothing.
  harness.fireEvent(row, 'dragend', { dataTransfer: transfer, target: row });

  assert.deepEqual(deleted, [], 'a cancelled drag never deletes');
  assert.equal(trash.classList.contains('show'), false);
});
