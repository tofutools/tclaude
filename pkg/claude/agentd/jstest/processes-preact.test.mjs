import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const prefs = () => { const values = new Map(); return { getItem: (key) => values.get(key) || null, setItem: (key, value) => values.set(key, value) }; };
const deferred = () => { let resolve; const promise = new Promise((done) => { resolve = done; }); return { promise, resolve }; };

test('Processes state owns subtab, requests, worklist views, drafts, and stale rejection', async (t) => {
  const harness = await createPreactHarness(t);
  const { createProcessesState } = await harness.importDashboardModule('js/processes-state.js');
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs(), now: () => 1000 });
  const old = state.templatesRequest.beginRequest(); const fresh = state.templatesRequest.beginRequest();
  assert.equal(state.templatesRequest.commitRequest(old, { templates: [{ id: 'old' }] }), false);
  assert.equal(state.templatesRequest.commitRequest(fresh, { templates: [{ id: 'new' }] }), true);
  assert.equal(state.view.value.templates[0].id, 'new');
  state.setSubtab('worklist'); state.setWorklistView('review'); state.setDraft('item-1', 'ship it'); state.requireComment('item-1');
  assert.equal(state.view.value.subtab, 'worklist'); assert.equal(state.view.value.worklistView, 'review');
  assert.equal(state.view.value.drafts['item-1'], 'ship it'); assert.equal(state.view.value.missingComments.has('item-1'), true);
  state.pruneWorklistState([]); assert.deepEqual(state.view.value.drafts, {}); assert.equal(state.view.value.missingComments.size, 0);
});

test('Processes actions preserve API routes, single-flight loads, comment gate, and retained idempotency', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = []; let resolveOld;
  const fetchImpl = async (path, options = {}) => {
    requests.push({ path, options });
    if (path === '/v1/process/templates' && requests.filter((r) => r.path === path).length === 1) return new Promise((resolve) => { resolveOld = resolve; });
    if (options.method === 'POST') return { ok: true, json: async () => ({}) };
    return { ok: true, json: async () => path.includes('worklist') ? ({ items: [], degradedRuns: [] }) : path.includes('templates') ? ({ templates: [{ id: 'fresh' }] }) : ({ runs: [] }) };
  };
  const actions = createProcessesActions({ state, fetchImpl, notify() {} });
  const stale = actions.load('templates'); const duplicate = actions.load('templates');
  assert.equal(await duplicate, false, 'a loading template request is single-flight');
  resolveOld({ ok: true, json: async () => ({ templates: [{ id: 'old' }] }) }); await stale;
  await actions.load('templates');
  assert.equal(state.view.value.templates[0].id, 'fresh');
  const item = { id: 'i/1', run: 'r', node: 'n', kind: 'decision', status: 'pending', summary: 'Choose', availableActions: ['approve'] };
  state.worklistRequest.commitRequest(state.worklistRequest.beginRequest(), { items: [item], degradedRuns: [] });
  assert.equal(await actions.submitWorklistAction(item.id, 'approve'), false); assert.equal(state.missingComments.value.has(item.id), true);
  state.setDraft(item.id, 'ok'); assert.equal(await actions.submitWorklistAction(item.id, 'approve'), true);
  const post = requests.find((request) => request.options.method === 'POST'); assert.equal(post.path, '/v1/process/worklist/i%2F1/action');
  assert.equal(JSON.parse(post.options.body).action, 'approve');

  let observedRef = '';
  state.setEditor({
    model: { template: { id: 'fresh' }, currentRef: '', dirty: false },
    observeExternalRef(ref) { observedRef = ref; },
  });
  await actions.load('templates', { quiet: true });
  assert.equal(observedRef, '', 'a list row without a latest ref is ignored');

  let discardPrompts = 0;
  state.setEditor({ dirty: true, model: { dirty: false } });
  const guarded = createProcessesActions({
    state, fetchImpl,
    confirmDiscard: async () => { discardPrompts += 1; return false; },
  });
  assert.equal(await guarded.closeCanvas(), false, 'a staged dialog draft participates in editor navigation guards');
  assert.equal(discardPrompts, 1);
});

test('template list refresh publishes the matching head to the persistent editor', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const observed = [];
  state.setEditor({
    model: { template: { id: 'release' }, currentRef: 'release@sha256:old', dirty: false },
    observeExternalRef(ref) { observed.push(ref); },
  });
  const actions = createProcessesActions({
    state,
    fetchImpl: async () => ({ ok: true, json: async () => ({ templates: [
      { id: 'other', latestVersion: { ref: 'other@sha256:x' } },
      { id: 'release', name: 'Renamed release', latestVersion: { ref: 'release@sha256:new' } },
    ] }) }),
  });
  await actions.load('templates', { quiet: true });
  assert.deepEqual(observed, ['release@sha256:new']);
  assert.equal(state.view.value.templates[1].name, 'Renamed release', 'the same refresh updates keyed list data');
});

test('snapshot cadence always refreshes worklist and observes heads only for Templates', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const loaded = []; let headObservations = 0;
  const actions = { refreshActive() {}, load(name, options) { loaded.push([name, options]); }, observeTemplateHeads() { headObservations += 1; }, activateSubtab() {}, openEditor() {}, openViewer() {}, closeCanvas() {} };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:snapshot'));
  assert.deepEqual(loaded, [['worklist', { quiet: true }]], 'Templates keeps the cross-subtab Worklist badge live');
  assert.equal(headObservations, 1);
  loaded.length = 0;
  state.setSubtab('runs');
  await harness.act(() => Promise.resolve());
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:snapshot'));
  assert.deepEqual(loaded, [['worklist', { quiet: true }]], 'Runs still refreshes the Worklist badge');
  assert.equal(headObservations, 1, 'Runs does not poll template heads');
  loaded.length = 0;
  state.setSubtab('worklist');
  await harness.act(() => Promise.resolve());
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:snapshot'));
  assert.deepEqual(loaded, [['worklist', { quiet: true }]]);
  assert.equal(headObservations, 1, 'Worklist does not duplicate its own request with a head observation');
  await mounted.unmount();
});

test('head observation is single-flight and full list refreshes only after a head set/ref change', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const slowHead = deferred(); const slowList = deferred();
  let headCalls = 0; let listCalls = 0; let delayList = false;
  const actions = createProcessesActions({ state, fetchImpl: async (path) => {
    if (path === '/v1/process/template-heads') {
      headCalls += 1;
      if (headCalls === 1) return slowHead.promise;
      return { ok: true, json: async () => ({ heads: [{ id: 'release', ref: 'release@sha256:b' }] }) };
    }
    if (path === '/v1/process/templates') {
      listCalls += 1;
      if (delayList) return slowList.promise;
      const ref = listCalls === 1 ? 'release@sha256:a' : 'release@sha256:b';
      return { ok: true, json: async () => ({ templates: [{ id: 'release', name: `Release ${ref.at(-1)}`, latestVersion: { ref } }] }) };
    }
    throw new Error(`unexpected ${path}`);
  } });

  await actions.load('templates', { quiet: true });
  const pendingHead = actions.observeTemplateHeads();
  assert.equal(await actions.observeTemplateHeads(), false, 'a slow head GET cannot overlap another tick');
  assert.equal(headCalls, 1);
  slowHead.resolve({ ok: true, json: async () => ({ heads: [{ id: 'release', ref: 'release@sha256:a' }] }) });
  assert.equal(await pendingHead, true);
  assert.equal(listCalls, 1, 'an unchanged head does not rescan template versions');

  await actions.observeTemplateHeads();
  assert.equal(listCalls, 2, 'a changed ref triggers one full list refresh');
  assert.equal(state.view.value.templates[0].name, 'Release b');

  delayList = true;
  const pendingList = actions.load('templates', { quiet: true });
  assert.equal(state.templatesRequest.request.value.phase, 'refreshing');
  assert.equal(await actions.load('templates', { quiet: true }), false, 'refreshing is also single-flight');
  assert.equal(await actions.observeTemplateHeads(), false, 'head observation cannot overlap the full list refresh');
  slowList.resolve({ ok: true, json: async () => ({ templates: [{ id: 'release', latestVersion: { ref: 'release@sha256:b' } }] }) });
  await pendingList;
  assert.equal(listCalls, 3, 'the slow request was not superseded or starved');
});

test('a head response captured before a local save cannot observe after markSaved advances currentRef', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessEditModel }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/process-edit-model.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const model = new ProcessEditModel({
    template: { id: 'release', name: 'A', start: 'begin', nodes: { begin: { type: 'start' } } },
    edges: [], layout: {}, sourceHash: 'source-a', semanticHash: 'semantic-a', currentRef: 'release@sha256:a',
  });
  const observed = [];
  state.setEditor({ model, observeExternalRef(ref) { observed.push(ref); } });
  const slowHead = deferred(); let slowList = null; let listCalls = 0;
  const actions = createProcessesActions({ state, fetchImpl: async (path) => {
    if (path === '/v1/process/templates') {
      listCalls += 1;
      if (slowList) return slowList.promise;
      return { ok: true, json: async () => ({ templates: [{ id: 'release', latestVersion: { ref: 'release@sha256:a' } }] }) };
    }
    if (path === '/v1/process/template-heads') return slowHead.promise;
    throw new Error(`unexpected ${path}`);
  } });
  await actions.load('templates', { quiet: true });
  observed.length = 0;

  const pending = actions.observeTemplateHeads(); // GET snapshots A.
  const savedAtRev = model.rev;
  model.setTemplateMeta({ name: 'edit made while POST B is pending' });
  model.markSaved({ ref: 'release@sha256:b', sourceHash: 'source-b', semanticHash: 'semantic-b' }, savedAtRev);
  assert.equal(model.dirty, true, 'the in-flight edit remains dirty after save B');
  slowHead.resolve({ ok: true, json: async () => ({ heads: [{ id: 'release', ref: 'release@sha256:a' }] }) });
  await pending;

  assert.deepEqual(observed, [], 'the stale A response is generation-bound and ignored');
  assert.equal(model.currentRef, 'release@sha256:b');
  assert.equal(listCalls, 1, 'stale A also cannot trigger an unnecessary version scan');

  // The expensive list path carries the same exact editor/model/ref binding.
  // Its rows may commit to the list, but its old head cannot touch editor B
  // after another local save advances the editor to C.
  slowList = deferred();
  const pendingList = actions.load('templates', { quiet: true });
  const savedAtB = model.rev;
  model.setTemplateMeta({ description: 'another edit while POST C is pending' });
  model.markSaved({ ref: 'release@sha256:c', sourceHash: 'source-c', semanticHash: 'semantic-c' }, savedAtB);
  slowList.resolve({ ok: true, json: async () => ({ templates: [{ id: 'release', latestVersion: { ref: 'release@sha256:b' } }] }) });
  await pendingList;
  assert.deepEqual(observed, [], 'the stale full-list B response is generation-bound too');
  assert.equal(model.currentRef, 'release@sha256:c');
  assert.equal(model.dirty, true);
});

test('imperative editor boundary mounts once, survives parent updates, updates by spec, and disposes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessEditorBoundary }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  let mounts = 0; let destroys = 0; let received = null;
  const confirmDiscard = async () => true;
  const openEditor = async (_mount, options) => {
    mounts += 1; received = options;
    return { model: { dirty: false }, destroy() { destroys += 1; } };
  };
  const first = { id: 'a', blank: false, key: 'a:1' };
  const mounted = await harness.mount(harness.html`<${ProcessEditorBoundary} spec=${first} state=${state} confirmDiscard=${confirmDiscard} openEditor=${openEditor} />`);
  assert.equal(mounts, 1);
  assert.equal(received.id, 'a');
  assert.equal(received.blank, false);
  assert.equal(received.config.confirmDiscard, confirmDiscard, 'the shared discard dialog reaches node editor transactions');
  state.setNotice('unrelated');
  await mounted.rerender(harness.html`<${ProcessEditorBoundary} spec=${first} state=${state} confirmDiscard=${confirmDiscard} openEditor=${openEditor} />`);
  assert.equal(mounts, 1, 'unrelated parent state does not recreate graph');
  await mounted.rerender(harness.html`<${ProcessEditorBoundary} spec=${{ id: 'b', blank: false, key: 'b:1' }} state=${state} confirmDiscard=${confirmDiscard} openEditor=${openEditor} />`);
  assert.equal(mounts, 2); assert.equal(destroys, 1);
  await mounted.unmount(); assert.equal(destroys, 2);
});

test('editor boundary exposes startup failures inside the canvas', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessEditorBoundary }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const openEditor = async () => { throw new Error('invalid template'); };
  const mounted = await harness.mount(harness.html`<${ProcessEditorBoundary} spec=${{ id: 'bad', blank: false, key: 'bad:1' }} state=${state} openEditor=${openEditor} />`);
  await harness.act(() => Promise.resolve());
  assert.match(mounted.container.querySelector('#process-editor-canvas [role="alert"]').textContent, /Could not open editor: invalid template/);
  assert.match(state.notice.value, /Could not open editor: invalid template/);
  await mounted.unmount();
});

test('canvas views retain their parent Processes subtab selection', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  state.setSubtab('runs'); state.setCanvas({ kind: 'viewer', id: 'run-1', key: 'run-1' });
  const actions = { refreshActive() {}, load() {}, activateSubtab() {}, closeCanvas() {} };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  assert.equal(mounted.container.querySelector('[data-process-subtab="runs"]').getAttribute('aria-selected'), 'true');
  assert.equal(mounted.container.querySelectorAll('[role="tab"][aria-selected="true"]').length, 1);
  await mounted.unmount();
});

test('Processes component renders keyed lists, worklist counts, degraded state, and preserves drafts', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs(), now: () => Date.parse('2026-01-01T00:00:00Z') });
  state.setSubtab('worklist'); state.setDraft('item-1', 'keep this draft');
  const item = { id: 'item-1', run: 'run-1', node: 'review', kind: 'review-needed', status: 'pending', summary: 'Review it', assignee: 'human:operator', createdAt: '2025-12-31T23:00:00Z', availableActions: ['approve'] };
  state.worklistRequest.commitRequest(state.worklistRequest.beginRequest(), { items: [item], degradedRuns: [{ run: 'broken-run', error: 'corrupt' }] });
  const actions = { refreshActive() {}, load() {}, activateSubtab() {}, openEditor() {}, openViewer() {}, closeCanvas() {}, openRunInList() {}, submitWorklistAction() {} };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  assert.equal(mounted.container.querySelectorAll('.wl-row').length, 1);
  assert.equal(mounted.container.querySelector('#process-worklist-badge').textContent, '1');
  assert.match(mounted.container.querySelector('#process-worklist-degraded').textContent, /broken-run/);
  const input = mounted.container.querySelector('[data-worklist-comment="item-1"]'); input.focus();
  state.worklistRequest.commitRequest(state.worklistRequest.beginRequest(), { items: [{ ...item, summary: 'Updated' }], degradedRuns: [] });
  await harness.act(() => Promise.resolve());
  const current = mounted.container.querySelector('[data-worklist-comment="item-1"]');
  assert.equal(current, input); assert.equal(current.value, 'keep this draft'); assert.equal(harness.document.activeElement, current);
  await mounted.unmount();
});
