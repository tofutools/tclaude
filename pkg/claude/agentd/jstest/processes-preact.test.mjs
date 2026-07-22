import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { createPreactHarness } from './preact-harness.mjs';

const prefs = () => { const values = new Map(); return { getItem: (key) => values.get(key) || null, setItem: (key, value) => values.set(key, value) }; };
const deferred = () => { let resolve; const promise = new Promise((done) => { resolve = done; }); return { promise, resolve }; };



test('process scribe actions send bounded structured scope, exact grants, and recover visibly', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  dashboardState.snapshot.value = { groups: [] };
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = []; const notices = []; const confirmations = [];
  const actions = createProcessesActions({
    state, dashboardOrigin: 'https://dashboard.example', notify: (...args) => notices.push(args),
    confirm: async (options) => { confirmations.push(options); return true; },
    fetchImpl: async (path, options) => {
      requests.push({ path, options });
      return { ok: true, json: async () => ({ name: 'process-scribe-12345678', conv_id: 'scribe-conv', reused: true, focus_mode: 'native' }) };
    },
  });
  const hash = 'a'.repeat(64); const sourceHash = 'b'.repeat(64);
  const result = await actions.summonScribe({
    kind: 'template', id: 'release-flow', currentRef: `release-flow@sha256:${hash}`, sourceHash, isNew: false,
  }, {
    prompt: 'Fix the focused validation issue.',
    context: {
      version: 1, kind: 'current-diagnostic',
      template: { templateId: 'release-flow', currentRef: `release-flow@sha256:${hash}`, sourceHash, isNew: false },
      diagnostic: { identity: { code: 'missing_performer', scope: 'node', targetId: 'build' }, severity: 'error', message: 'performer required', nodeId: 'build' },
    },
  });
  assert.equal(result.conv_id, 'scribe-conv');
  const request = requests[0]; const body = JSON.parse(request.options.body);
  assert.equal(request.path, '/api/scribe');
  assert.equal(request.options.credentials, 'same-origin');
  assert.deepEqual(body.scope, { kind: 'process-template', id: 'release-flow' });
  assert.deepEqual(body.slugs, ['process.templates.read', 'process.templates.manage']);
  assert.equal(body.exclusive, true, 'the two process-template grants are the complete effective capability set');
  assert.deepEqual({ url: body.task_ref_url, label: body.task_ref_label }, {
    url: 'https://dashboard.example/processes/templates', label: 'process: release-flow',
  });
  assert.match(body.brief, new RegExp(sourceHash));
  assert.match(body.brief, /BEGIN HUMAN REQUEST.*Fix the focused validation issue.*END HUMAN REQUEST/s);
  assert.match(body.brief, /BEGIN BOUNDED EDITOR CONTEXT.*missing_performer.*END BOUNDED EDITOR CONTEXT/s);
  assert.match(body.brief, /never an alternate source of truth.*Reread the canonical template.*CAS-save/s);
  assert.match(state.notice.value, /Reopened process scribe/);
  assert.match(confirmations[0].body, /process\.templates\.read and process\.templates\.manage/);
  assert.match(confirmations[0].body, /never instantiates or runs a process/);

  dashboardState.snapshot.value = { groups: [{
    name: 'process-scribe', scribe: true, members: [{
      agent_id: `agt_${'f'.repeat(32)}`, conv_id: 'same-scope-conv', title: 'same-scope-scribe', online: true,
      descr: 'Reusable scribe scope: process-template/release-flow',
    }],
  }] };
  let reusePrompt;
  const compatibilityFallback = createProcessesActions({
    state, dashboardOrigin: 'https://dashboard.example', notify() {},
    confirm: async (options) => { reusePrompt = options; return true; },
    fetchImpl: async () => ({ ok: true, json: async () => ({
      name: 'process-scribe-fresh', conv_id: 'fresh-conv', reused: false, focus_mode: 'native',
    }) }),
  });
  assert.equal((await compatibilityFallback.summonScribe({
    kind: 'template', id: 'release-flow', currentRef: `release-flow@sha256:${hash}`, sourceHash, isNew: false,
  })).reused, false);
  assert.equal(reusePrompt.title, 'Reuse or replace process scribe?');
  assert.equal(reusePrompt.okLabel, 'Reuse or summon');
  assert.match(reusePrompt.body, /reuses a live same-scope scribe only when its exact permissions match.*no active temporary elevation/);
  assert.match(reusePrompt.body, /otherwise this approval creates a fresh scribe.*leaving the existing scribe unchanged/);

  const deniedRequests = [];
  const denied = createProcessesActions({
    state, dashboardOrigin: 'https://dashboard.example', confirm: async () => false,
    fetchImpl: async (...args) => { deniedRequests.push(args); throw new Error('must not fetch'); },
  });
  assert.equal(await denied.summonScribe({ kind: 'library' }), null);
  assert.equal(deniedRequests.length, 0, 'denying the explicit grant prompt performs no retry or mutation');
  assert.match(state.notice.value, /no permissions or sessions changed/);

  const failed = createProcessesActions({
    state, dashboardOrigin: 'https://dashboard.example', notify: (...args) => notices.push(args),
    confirm: async () => true,
    fetchImpl: async () => ({ ok: false, status: 503, statusText: 'Unavailable', json: async () => ({ message: 'spawn binary missing' }) }),
  });
  assert.equal(await failed.summonScribe({ kind: 'library' }), null);
  assert.match(state.notice.value, /Process scribe unavailable: spawn binary missing/);
  assert.match(notices.at(-1)[0], /Check the agent daemon and Ask & scribe defaults, then retry/);
  assert.equal(notices.at(-1)[1], true);

  const existingAgent = `agt_${'c'.repeat(32)}`;
  dashboardState.snapshot.value = { groups: [{
    name: 'process-scribe', scribe: true, members: [{
      agent_id: existingAgent, conv_id: 'existing-conv', title: 'existing-scribe', online: true,
      descr: 'Reusable scribe scope: process-template/other-flow',
    }],
  }] };
  let transitionPrompt;
  const transitioning = createProcessesActions({
    state, dashboardOrigin: 'https://dashboard.example',
    confirm: async (options) => { transitionPrompt = options; return false; },
    fetchImpl: async () => { throw new Error('must not cross scope after denied transition'); },
  });
  assert.equal(await transitioning.summonScribe({ kind: 'library' }), null);
  assert.match(transitionPrompt.body, /will not be reused or have its permissions changed/);
  assert.equal(transitionPrompt.okLabel, 'Start separate scribe');
});

test('process scribe freshness guard runs after approval and immediately before POST', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  dashboardState.snapshot.value = { groups: [] };
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const approval = deferred();
  let fresh = true;
  const requests = [];
  const actions = createProcessesActions({
    state, dashboardOrigin: 'https://dashboard.example', confirm: () => approval.promise,
    fetchImpl: async (...args) => { requests.push(args); throw new Error('stale context must not POST'); },
  });
  const pending = actions.summonScribe({
    kind: 'template', id: 'release-flow', currentRef: `release-flow@sha256:${'a'.repeat(64)}`,
    sourceHash: 'b'.repeat(64), isNew: false,
  }, { freshnessGuard: () => fresh });
  fresh = false;
  approval.resolve(true);
  assert.equal(await pending, null);
  assert.equal(requests.length, 0, 'a mutation during the permission/reuse confirmation prevents /api/scribe');
  assert.match(state.notice.value, /editor context changed during approval/);

  const order = [];
  const accepted = createProcessesActions({
    state, dashboardOrigin: 'https://dashboard.example',
    confirm: async () => { order.push('confirm'); return true; },
    fetchImpl: async () => { order.push('post'); return { ok: true, json: async () => ({ focus_mode: 'native' }) }; },
  });
  await accepted.summonScribe({ kind: 'library' }, {
    freshnessGuard: () => { order.push('freshness'); return true; },
  });
  assert.deepEqual(order, ['confirm', 'freshness', 'post'], 'no asynchronous boundary remains between freshness and POST');
});

test('process scribe lifecycle distinguishes stop, retire, and stale recovery', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const agentId = `agt_${'d'.repeat(32)}`;
  const scribe = { agentId, convId: 'scribe-conv', name: 'process-scribe-12345678', online: true, scopeLabel: 'template release-flow' };
  const requests = []; const prompts = [];
  const actions = createProcessesActions({
    state, confirm: async (options) => { prompts.push(options); return true; }, notify() {},
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (path.endsWith('/stop')) return { ok: true, json: async () => ({ action: 'soft_stopped' }) };
      if (path.includes('/retire?')) return { ok: true, json: async () => ({ outcome: { retired: true }, shutdown: { action: 'soft_stopped' } }) };
      if (path.startsWith('/api/open-window/')) return { ok: true, json: async () => ({ mode: 'native' }) };
      throw new Error(`unexpected ${path}`);
    },
  });
  assert.equal(await actions.openScribe(scribe), true);
  assert.equal(await actions.stopScribe(scribe), true);
  assert.equal(await actions.retireScribe(scribe), true);
  assert.equal(requests[0].path, `/api/open-window/${agentId}`);
  assert.deepEqual(JSON.parse(requests[1].options.body), { force: false });
  assert.match(prompts[0].body, /does not delete process templates.*conversation.*permissions.*local editor work/);
  assert.match(requests[2].path, new RegExp(`/api/agents/${agentId}/retire\\?`));
  const retireURL = new URL(requests[2].path, 'https://dashboard.example');
  assert.equal(retireURL.searchParams.get('delete_worktree'), '0');
  assert.equal(retireURL.searchParams.get('shutdown'), '1');
  assert.match(prompts[1].body, /revokes its permissions/);
  assert.match(prompts[1].body, /process templates, versions, and local editor work are not deleted/);
  assert.match(state.notice.value, /stop was requested but is not yet confirmed.*Refresh Processes.*force-stop.*Agents/);

  const stale = createProcessesActions({
    state, confirm: async () => true,
    fetchImpl: async () => ({ ok: false, status: 404, statusText: 'Not Found', json: async () => ({ message: 'agent already retired' }) }),
  });
  assert.equal(await stale.retireScribe(scribe), false);
  assert.match(state.notice.value, /Refresh Processes.*already retired.*summon a new scribe/);
  assert.equal(await stale.stopScribe({ ...scribe, online: false }), false);
  assert.match(state.notice.value, /already stopped.*summon a fresh scribe/);

  const stopError = createProcessesActions({
    state, confirm: async () => true,
    fetchImpl: async () => ({ ok: true, json: async () => ({ action: 'error', detail: 'tmux pane did not accept exit' }) }),
  });
  assert.equal(await stopError.stopScribe(scribe), false);
  assert.match(state.notice.value, /Could not stop.*tmux pane did not accept exit/);
  assert.doesNotMatch(state.notice.value, /Stopped process-scribe/);

  const alreadyOffline = createProcessesActions({
    state, confirm: async () => true,
    fetchImpl: async () => ({ ok: true, json: async () => ({ action: 'skipped:already_offline' }) }),
  });
  assert.equal(await alreadyOffline.stopScribe(scribe), true);
  assert.match(state.notice.value, /was already stopped.*permissions.*unchanged.*Retire it or summon a fresh scribe/);

  const gracefulStop = createProcessesActions({
    state, confirm: async () => true, notify() {},
    fetchImpl: async () => ({ ok: true, json: async () => ({ action: 'soft_stopped' }) }),
  });
  assert.equal(await gracefulStop.stopScribe(scribe), true);
  assert.match(state.notice.value, /Asked.*to stop.*confirm it is offline.*force-stop it from Agents.*permissions.*unchanged/);
  assert.doesNotMatch(state.notice.value, /^Stopped/);

  const hardFallback = createProcessesActions({
    state, confirm: async () => true, notify() {},
    fetchImpl: async () => ({ ok: true, json: async () => ({ action: 'killed_no_soft_exit' }) }),
  });
  assert.equal(await hardFallback.stopScribe(scribe), true);
  assert.match(state.notice.value, /^Stopped.*templates and editor work are unchanged/);

  const retirementNotices = [];
  const retiredButRunning = createProcessesActions({
    state, confirm: async () => true, notify: (...args) => retirementNotices.push(args),
    fetchImpl: async () => ({ ok: true, json: async () => ({
      outcome: { retired: true }, shutdown: { action: 'error', detail: 'soft exit timed out' },
    }) }),
  });
  assert.equal(await retiredButRunning.retireScribe(scribe), true, 'access revocation succeeds even when shutdown fails');
  assert.match(state.notice.value, /Retired.*revoked its access.*session may still be running.*soft exit timed out.*Stop the conversation from Agents/);
  assert.equal(retirementNotices.at(-1)[1], true, 'partial shutdown failure is surfaced as an error notification');
});

test('process actor presentation renders only validated attribution and marks navigation only for a live match', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ processActorPresentation, createProcessesActions }, { createProcessesState }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/processes-actions.js'), harness.importDashboardModule('js/processes-state.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  const agentId = `agt_${'a'.repeat(32)}`;
  const snapshot = { agents: [{ agent_id: agentId, title: 'release-scribe', online: true }] };
  assert.deepEqual(processActorPresentation(snapshot, `agent:${agentId}`), {
    label: 'agent release-scribe', live: true, agentId,
  });
  assert.deepEqual(processActorPresentation({ agents: [] }, `agent:${agentId}`), {
    label: `agent ${agentId.slice(0, 12)}…`, live: false, agentId,
  });
  assert.deepEqual(processActorPresentation(snapshot, 'human:operator'), {
    label: 'the operator', live: false, agentId: '',
  });
  assert.deepEqual(processActorPresentation(snapshot, 'agent:agt_legacy123'), {
    label: 'agent agt_legacy12…', live: false, agentId: '',
  }, 'valid legacy actor refs stay attributed but cannot become navigation targets');
  assert.equal(processActorPresentation(snapshot, 'agent:not-a-stable-id'), null);
  assert.equal(processActorPresentation(snapshot, ''), null, 'unknown actor stays unattributed');

  dashboardState.snapshot.value = snapshot;
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = [];
  const actions = createProcessesActions({ state, fetchImpl: async (path, options) => {
    requests.push({ path, options });
    return { ok: true, json: async () => ({ mode: 'native' }) };
  } });
  assert.equal(await actions.openActor(`agent:${agentId}`), true);
  assert.equal(requests[0].path, `/api/open-window/${agentId}`);
  assert.equal(requests[0].options.method, 'POST');
  assert.equal(await actions.openActor(`agent:${'b'.repeat(32)}`), false, 'an unattributed/offline actor is not presented as a live navigation target');
});









test('template list refresh publishes the matching head to the persistent editor', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const observed = [];
  state.setEditor({
    model: { template: { id: 'release' }, currentRef: 'release@sha256:old', sourceHash: 'source-old', dirty: false },
    observeExternalHead(head) { observed.push(head); },
  });
  const actions = createProcessesActions({
    state,
    fetchImpl: async () => ({ ok: true, json: async () => ({ templates: [
      { id: 'other', latestVersion: { ref: 'other@sha256:x', sourceHash: 'source-x' } },
      { id: 'release', name: 'Renamed release', latestVersion: { ref: 'release@sha256:new', sourceHash: 'source-new' } },
    ] }) }),
  });
  await actions.load('templates', { quiet: true });
  assert.deepEqual(observed, [{ id: 'release', ref: 'release@sha256:new', sourceHash: 'source-new' }]);
  assert.equal(state.view.value.templates[1].name, 'Renamed release', 'the same refresh updates keyed list data');
});

test('a source-only head change refreshes the list and notifies the editor', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const observed = []; let listCalls = 0;
  state.setEditor({
    model: { template: { id: 'release' }, currentRef: 'release@sha256:same', sourceHash: 'source-a' },
    observeExternalHead(head) { observed.push(head); },
  });
  const actions = createProcessesActions({ state, fetchImpl: async (path) => {
    if (path === '/v1/process/template-heads') return { ok: true, json: async () => ({ heads: [
      { id: 'release', ref: 'release@sha256:same', sourceHash: 'source-b' },
    ] }) };
    listCalls += 1;
    const sourceHash = listCalls === 1 ? 'source-a' : 'source-b';
    return { ok: true, json: async () => ({ templates: [
      { id: 'release', latestVersion: { ref: 'release@sha256:same', sourceHash } },
    ] }) };
  } });
  await actions.load('templates', { quiet: true });
  observed.length = 0;
  await actions.observeTemplateHeads();
  assert.equal(listCalls, 2, 'layout/source-only authority changes trigger the change-driven full list read');
  assert.deepEqual(observed.at(-1), { id: 'release', ref: 'release@sha256:same', sourceHash: 'source-b' });
});


test('head observation is single-flight and full list refreshes only after a generation change', async (t) => {
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
      return { ok: true, json: async () => ({ heads: [{ id: 'release', ref: 'release@sha256:b', sourceHash: 'source-b' }] }) };
    }
    if (path === '/v1/process/templates') {
      listCalls += 1;
      if (delayList) return slowList.promise;
      const ref = listCalls === 1 ? 'release@sha256:a' : 'release@sha256:b';
      const sourceHash = `source-${ref.at(-1)}`;
      return { ok: true, json: async () => ({ templates: [{ id: 'release', name: `Release ${ref.at(-1)}`, latestVersion: { ref, sourceHash } }] }) };
    }
    throw new Error(`unexpected ${path}`);
  } });

  await actions.load('templates', { quiet: true });
  const pendingHead = actions.observeTemplateHeads();
  assert.equal(await actions.observeTemplateHeads(), false, 'a slow head GET cannot overlap another tick');
  assert.equal(headCalls, 1);
  slowHead.resolve({ ok: true, json: async () => ({ heads: [{ id: 'release', ref: 'release@sha256:a', sourceHash: 'source-a' }] }) });
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
  slowList.resolve({ ok: true, json: async () => ({ templates: [{ id: 'release', latestVersion: { ref: 'release@sha256:b', sourceHash: 'source-b' } }] }) });
  await pendingList;
  assert.equal(listCalls, 3, 'the slow request was not superseded or starved');
});

test('an empty committed head set does not cause repeated full template scans', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  let listCalls = 0; let headCalls = 0;
  const actions = createProcessesActions({ state, fetchImpl: async (path) => {
    if (path === '/v1/process/templates') {
      listCalls += 1;
      return { ok: true, json: async () => ({ templates: [] }) };
    }
    headCalls += 1;
    return { ok: true, json: async () => ({ heads: [] }) };
  } });
  await actions.load('templates', { quiet: true });
  await actions.observeTemplateHeads();
  await actions.observeTemplateHeads();
  assert.equal(headCalls, 2);
  assert.equal(listCalls, 1, 'skipped first-create/orphan directories cannot churn the list signature');
});

test('a head response captured before a local layout-only save cannot observe after SourceHash advances', async (t) => {
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
  state.setEditor({ model, observeExternalHead(head) { observed.push(head); } });
  const slowHead = deferred(); let slowList = null; let listCalls = 0;
  const actions = createProcessesActions({ state, fetchImpl: async (path) => {
    if (path === '/v1/process/templates') {
      listCalls += 1;
      if (slowList) return slowList.promise;
      return { ok: true, json: async () => ({ templates: [{ id: 'release', latestVersion: { ref: 'release@sha256:a', sourceHash: 'source-a' } }] }) };
    }
    if (path === '/v1/process/template-heads') return slowHead.promise;
    throw new Error(`unexpected ${path}`);
  } });
  await actions.load('templates', { quiet: true });
  observed.length = 0;

  const pending = actions.observeTemplateHeads(); // GET snapshots A.
  const savedAtRev = model.rev;
  model.setTemplateMeta({ name: 'edit made while POST B is pending' });
  model.markSaved({ ref: 'release@sha256:a', sourceHash: 'source-b', semanticHash: 'semantic-a' }, savedAtRev);
  assert.equal(model.dirty, true, 'the in-flight edit remains dirty after save B');
  slowHead.resolve({ ok: true, json: async () => ({ heads: [{ id: 'release', ref: 'release@sha256:a', sourceHash: 'source-a' }] }) });
  await pending;

  assert.deepEqual(observed, [], 'the stale A response is generation-bound and ignored');
  assert.equal(model.currentRef, 'release@sha256:a');
  assert.equal(model.sourceHash, 'source-b');
  assert.equal(listCalls, 1, 'stale A also cannot trigger an unnecessary version scan');

  // The expensive list path carries the same exact editor/model/ref binding.
  // Its rows may commit to the list, but its old head cannot touch editor B
  // after another local save advances the editor to C.
  slowList = deferred();
  const pendingList = actions.load('templates', { quiet: true });
  const savedAtB = model.rev;
  model.setTemplateMeta({ description: 'another edit while POST C is pending' });
  model.markSaved({ ref: 'release@sha256:a', sourceHash: 'source-c', semanticHash: 'semantic-a' }, savedAtB);
  slowList.resolve({ ok: true, json: async () => ({ templates: [{ id: 'release', latestVersion: { ref: 'release@sha256:a', sourceHash: 'source-b' } }] }) });
  await pendingList;
  assert.deepEqual(observed, [], 'the stale full-list B response is generation-bound too');
  assert.equal(model.currentRef, 'release@sha256:a');
  assert.equal(model.sourceHash, 'source-c');
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
  let summoned = null;
  const summonScribe = async (value) => { summoned = value; return { ok: true }; };
  const actions = { summonScribe };
  const openEditor = async (_mount, options) => {
    mounts += 1; received = options;
    return { model: { dirty: false }, destroy() { destroys += 1; } };
  };
  const first = { id: 'a', name: 'Template A', blank: false, key: 'a:1' };
  const mounted = await harness.mount(harness.html`<${ProcessEditorBoundary} spec=${first} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openEditor=${openEditor} />`);
  assert.equal(mounts, 1);
  assert.equal(received.id, 'a');
  assert.equal(received.name, 'Template A', 'the creation/list handoff preserves the display name');
  assert.equal(received.view, undefined);
  assert.equal(received.blank, false);
  assert.equal(received.config.confirmDiscard, confirmDiscard, 'the shared discard dialog reaches node editor transactions');
  assert.deepEqual(await received.config.onScribe({ kind: 'library' }), { ok: true });
  assert.deepEqual(summoned, { kind: 'library' }, 'the scoped scribe action reaches the imperative editor');
  state.setNotice('unrelated');
  await mounted.rerender(harness.html`<${ProcessEditorBoundary} spec=${first} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openEditor=${openEditor} />`);
  assert.equal(mounts, 1, 'unrelated parent state does not recreate graph');
  await mounted.rerender(harness.html`<${ProcessEditorBoundary} spec=${{ id: 'b', blank: false, key: 'b:1' }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openEditor=${openEditor} />`);
  assert.equal(mounts, 2); assert.equal(destroys, 1);
  await mounted.unmount(); assert.equal(destroys, 2);
});

test('process template library renders both-skin scribe entry and sends library scope', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  state.templatesRequest.commitRequest(state.templatesRequest.beginRequest(), { templates: [] });
  const scopes = [];
  const actions = {
    refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {}, closeCanvas() {},
    summonScribe(scope) { scopes.push(scope); },
  };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  const button = mounted.container.querySelector('#process-scribe-library');
  assert.ok(button);
  assert.match(button.querySelector('.process-scribe-plain').textContent, /Edit with agent/);
  assert.match(button.querySelector('.process-scribe-wizard').textContent, /process scribe/);
  harness.fireEvent(button, 'click');
  assert.deepEqual(scopes, [{ kind: 'library' }]);
  await mounted.unmount();
});

test('Processes surfaces scoped scribe lifecycle and latest-version actor attribution', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  const agentId = `agt_${'e'.repeat(32)}`;
  dashboardState.snapshot.value = {
    agents: [{ agent_id: agentId, title: 'release-scribe', online: true }],
    groups: [{ name: 'process-scribe', scribe: true, members: [{
      agent_id: agentId, conv_id: 'scribe-conv', title: 'release-scribe', online: true,
      descr: 'Reusable scribe scope: process-template/release-flow',
      task_ref_url: 'https://dash.example/processes/templates', task_ref_label: 'process: release-flow',
    }] }],
  };
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  state.templatesRequest.commitRequest(state.templatesRequest.beginRequest(), { templates: [{
    id: 'release-flow', latestVersion: { semanticHash: 'abcdef123456', actor: `agent:${agentId}` }, versionCount: 2,
  }] });
  const called = [];
  const actions = {
    refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {}, closeCanvas() {},
    openInstantiation() {}, summonScribe() {}, describeActor: () => ({ label: 'agent release-scribe' }),
    openScribe: (scribe) => called.push(['open', scribe.agentId]),
    stopScribe: (scribe) => called.push(['stop', scribe.agentId]),
    retireScribe: (scribe) => called.push(['retire', scribe.agentId]),
  };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  const status = mounted.container.querySelector(`[data-process-scribe="${agentId}"]`);
  assert.ok(status);
  assert.match(status.textContent, /active.*release-scribe.*template release-flow.*process: release-flow.*stop.*retire/s);
  assert.match(mounted.container.querySelector('.process-version-actor').textContent, /by agent release-scribe/);
  harness.fireEvent(status.querySelector('.process-scribe-open'), 'click');
  harness.fireEvent(status.querySelector('[data-process-scribe-action="stop"]'), 'click');
  harness.fireEvent(status.querySelector('[data-process-scribe-action="retire"]'), 'click');
  assert.deepEqual(called, [['open', agentId], ['stop', agentId], ['retire', agentId]]);
  await mounted.unmount();
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



test('renaming a template round-trips the head edit view and changes only the display name', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const head = {
    template: { id: 'ship-it', name: 'Old name', description: 'keep me', nodes: { a: {} } },
    edges: [{ from: 'a', to: 'b', outcome: '' }], layout: { nodes: { a: { x: 4, y: 9 } } },
    sourceHash: 'head-source', currentRef: `ship-it@sha256:${'a'.repeat(64)}`,
    // Read-only view fields the save handler rejects as unknown.
    semanticHash: 'a'.repeat(64), source: 'id: ship-it\n', diagnostics: [], authorship: [],
  };
  const requests = [];
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [{ id: 'ship-it', name: 'Old name', latestVersion: { sourceHash: 'head-source' } }] }) };
      if (path === '/v1/process/templates/ship-it' && options.method === 'POST') return { ok: true, status: 201, json: async () => ({ ref: 'ship-it@sha256:new' }) };
      if (path === '/v1/process/templates/ship-it') return { ok: true, json: async () => head };
      throw new Error(`unexpected ${path}`);
    },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());

  const invoker = mounted.container.querySelector('[data-process-action="rename"]');
  assert.ok(invoker, 'the templates list offers a rename affordance');
  await harness.act(() => harness.fireEvent(invoker, 'click'));
  const input = mounted.container.querySelector('[data-process-rename-input]');
  assert.equal(input.value, 'Old name', 'the dialog opens on the current display name');
  assert.equal(harness.document.activeElement, input);

  input.value = 'New name';
  await harness.act(() => harness.fireEvent(input, 'input'));
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('.process-rename-dialog'), 'submit'));
  for (let i = 0; i < 10 && state.rename.value; i++) await harness.act(() => Promise.resolve());

  const post = requests.find((request) => request.options.method === 'POST');
  assert.equal(post.path, '/v1/process/templates/ship-it');
  const body = JSON.parse(post.options.body);
  // The save handler sets DisallowUnknownFields, so forwarding the whole head
  // edit view would 400 in production while still satisfying every value
  // assertion below. Pin the key set so that refactor cannot pass silently.
  assert.deepEqual(Object.keys(body).sort(), ['edges', 'layout', 'sourceHash', 'template']);
  assert.equal(body.template.name, 'New name');
  assert.equal(body.template.id, 'ship-it', 'the immutable id is preserved');
  assert.equal(body.template.description, 'keep me', 'unrelated semantics survive the rename');
  assert.deepEqual(body.edges, head.edges, 'edges round-trip untouched');
  assert.deepEqual(body.layout, head.layout, 'editor layout round-trips untouched');
  assert.equal(body.sourceHash, 'head-source', 'the rename saves against the observed version');
  assert.equal(state.rename.value, null, 'a successful rename closes the dialog');
  await mounted.unmount();
});

test('a rename losing the CAS race reports the conflict and keeps the dialog open', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const notices = [];
  const actions = createProcessesActions({
    state, notify(message) { notices.push(message); },
    fetchImpl: async (path, options = {}) => {
      if (options.method === 'POST') return { ok: false, status: 409, json: async () => ({ code: 'process_template_conflict' }) };
      return { ok: true, json: async () => ({ template: { id: 'racy', name: 'Before' }, sourceHash: 'stale' }) };
    },
  });
  await actions.openRename({ id: 'racy', name: 'Before' });
  assert.equal(await actions.submitRename('After'), false);
  assert.match(state.rename.value.error, /changed while you were renaming it/);
  assert.equal(state.rename.value.id, 'racy', 'the dialog stays open on the same template');
  assert.equal(state.mutation.value.busy, false, 'the mutation gate is released after a conflict');
  assert.match(notices.at(-1), /rename failed/);
});

test('renaming to the unchanged current name closes without touching the store', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  let calls = 0;
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async () => { calls += 1; return { ok: true, json: async () => ({}) }; },
  });
  await actions.openRename({ id: 'stable', name: 'Same' });
  assert.equal(await actions.submitRename('  Same  '), true, 'a whitespace-only edit is still a no-op');
  assert.equal(calls, 0, 'an unchanged name commits no new version');
  assert.equal(state.rename.value, null);
});

test('a rename saves against the version observed when the dialog opened, not the head read at submit', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = [];
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (options.method === 'POST') return { ok: true, status: 201, json: async () => ({ ref: 'drifted@sha256:new' }) };
      // The head moved while the dialog sat open: a concurrent writer committed
      // a newer version than the one the operator was looking at.
      return { ok: true, json: async () => ({ template: { id: 'drifted', name: 'Renamed by an agent' }, sourceHash: 'v2-after-agent-edit' }) };
    },
  });
  await actions.openRename({ id: 'drifted', name: 'What the operator saw', sourceHash: 'v1-when-dialog-opened' });
  await actions.submitRename('Operator name');
  const body = JSON.parse(requests.find((request) => request.options.method === 'POST').options.body);
  assert.equal(body.sourceHash, 'v1-when-dialog-opened',
    'saving against the submit-time head would silently clobber the concurrent edit instead of conflicting');
});

test('a rename with no observed version still saves against the head it read', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = [];
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (options.method === 'POST') return { ok: true, status: 201, json: async () => ({}) };
      return { ok: true, json: async () => ({ template: { id: 'hashless' }, sourceHash: 'head-only' }) };
    },
  });
  await actions.openRename({ id: 'hashless', name: '' });
  assert.equal(await actions.submitRename('Named at last'), true);
  const body = JSON.parse(requests.find((request) => request.options.method === 'POST').options.body);
  assert.equal(body.sourceHash, 'head-only', 'a list row without a published hash still renames rather than dead-locking');
  assert.equal(body.template.name, 'Named at last');
});

test('Ctrl/Cmd+Enter confirms the rename dialog and plain Enter is left to the form', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const submitted = [];
  const actions = {
    refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {}, closeCanvas() {},
    closeRename() { state.setRename(null); }, submitRename(name) { submitted.push(name); },
  };
  state.setRename({ key: 'hotkey-1', id: 'hotkey', name: 'Before', sourceHash: 'v1', error: '' });
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());
  const input = mounted.container.querySelector('[data-process-rename-input]');
  input.value = 'After';
  await harness.act(() => harness.fireEvent(input, 'input'));

  // A bare Enter must not be intercepted: the browser's native form submission
  // already handles it, and swallowing it here would double-submit.
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(submitted, [], 'plain Enter is left to native form submission');

  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter', metaKey: true }));
  assert.deepEqual(submitted, ['After'], 'Cmd+Enter confirms with the current draft');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.deepEqual(submitted, ['After', 'After'], 'Ctrl+Enter confirms on non-mac keyboards too');

  // An IME candidate commit also arrives as Enter; submitting there would eat
  // the composition instead of confirming it.
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter', metaKey: true, isComposing: true }));
  assert.equal(submitted.length, 2, 'an IME composition commit is not a confirm');
  await mounted.unmount();
});

test('the Templates list renames inline on click, committing an immediate CAS save', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = [];
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [{
        id: 'inline', name: 'Old inline name', latestVersion: { sourceHash: 'inline-v1' },
      }] }) };
      if (options.method === 'POST') return { ok: true, status: 201, json: async () => ({ ref: 'inline@sha256:new' }) };
      return { ok: true, json: async () => ({ template: { id: 'inline', name: 'Old inline name' }, sourceHash: 'inline-v1' }) };
    },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());

  const trigger = mounted.container.querySelector('[data-process-name-edit="inline"]');
  assert.ok(trigger, 'the name itself is the rename affordance');
  assert.equal(trigger.textContent, 'Old inline name');
  await harness.act(() => harness.fireEvent(trigger, 'click'));
  const input = mounted.container.querySelector('[data-process-name-input="inline"]');
  assert.ok(input, 'clicking the name swaps in an editor');
  assert.equal(harness.document.activeElement, input);

  input.value = 'Renamed inline';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  for (let i = 0; i < 10 && !requests.some((r) => r.options.method === 'POST'); i++) await harness.act(() => Promise.resolve());
  const post = requests.find((request) => request.options.method === 'POST');
  const body = JSON.parse(post.options.body);
  assert.equal(body.template.name, 'Renamed inline');
  assert.equal(body.sourceHash, 'inline-v1', 'the inline edit still uses the row version as its CAS baseline');
  await mounted.unmount();
});

test('Escape abandons an inline list rename without saving', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  let posts = 0;
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async (path, options = {}) => {
      if (options.method === 'POST') posts += 1;
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [{ id: 'escaped', name: 'Keep me' }] }) };
      return { ok: true, json: async () => ({ template: { id: 'escaped' }, sourceHash: 'x' }) };
    },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-name-edit="escaped"]'), 'click'));
  const input = mounted.container.querySelector('[data-process-name-input="escaped"]');
  input.value = 'Discard this';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Escape' }));
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  assert.equal(posts, 0, 'Escape commits nothing');
  assert.equal(mounted.container.querySelector('[data-process-name-edit="escaped"]').textContent, 'Keep me');
  await mounted.unmount();
});

test('regression: an inline list rename never flashes the rename dialog open', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async (path, options = {}) => {
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [{
        id: 'noflash', name: 'Before', latestVersion: { sourceHash: 'v1' },
      }] }) };
      if (options.method === 'POST') return { ok: true, status: 201, json: async () => ({ ref: 'noflash@sha256:new' }) };
      return { ok: true, json: async () => ({ template: { id: 'noflash', name: 'Before' }, sourceHash: 'v1' }) };
    },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-name-edit="noflash"]'), 'click'));

  // Watch dialog state across the whole commit: the bug was openRename setting
  // it just long enough to render before submitRename cleared it again.
  const seen = [];
  const stop = state.rename.subscribe((value) => seen.push(value));
  const input = mounted.container.querySelector('[data-process-name-input="noflash"]');
  input.value = 'After';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  for (let i = 0; i < 10; i++) await harness.act(() => Promise.resolve());
  stop();

  assert.deepEqual(seen.filter(Boolean), [], 'the dialog is never opened by an inline rename');
  assert.equal(mounted.container.querySelector('.process-rename-dialog'), null);
  await mounted.unmount();
});

test('regression: opening the list name editor prefills the current name', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  state.templatesRequest.commitRequest(state.templatesRequest.beginRequest(), {
    templates: [{ id: 'prefill', name: 'Existing name' }],
  });
  const actions = {
    refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {},
    closeCanvas() {}, describeActor: () => null, openInstantiation() {}, openRename() {}, renameTemplate() {},
  };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-name-edit="prefill"]'), 'click'));
  assert.equal(mounted.container.querySelector('[data-process-name-input="prefill"]').value, 'Existing name',
    'a rename starts from the current name, not an empty box');
  await mounted.unmount();
});

// A Templates list wired to one row whose head carries a full graph, so a
// description edit can be checked for what it leaves alone as well as what it
// changes.
async function mountDescribableList(t, { row = {}, head = {}, post } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const template = { id: 'described', name: 'Release train', description: 'Old description', latestVersion: { sourceHash: 'row-v1' }, ...row };
  const editHead = {
    template: { id: 'described', name: 'Release train', description: 'Old description', nodes: { a: {} } },
    edges: [{ from: 'a', to: 'b', outcome: '' }], layout: { nodes: { a: { x: 4, y: 9 } } },
    sourceHash: 'row-v1', semanticHash: 'a'.repeat(64), source: 'id: described\n', diagnostics: [], authorship: [],
    ...head,
  };
  const requests = [];
  const notices = [];
  const actions = createProcessesActions({
    state, notify(message) { notices.push(message); },
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [template] }) };
      if (options.method === 'POST') return post ? post() : { ok: true, status: 201, json: async () => ({ ref: 'described@sha256:new' }) };
      return { ok: true, json: async () => editHead };
    },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());
  const posts = () => requests.filter((request) => request.options.method === 'POST');
  const settle = async () => { for (let i = 0; i < 10; i++) await harness.act(() => Promise.resolve()); };
  return { harness, state, mounted, requests, notices, posts, settle, editHead };
}

test('the Templates list edits a description inline and commits only that field', async (t) => {
  const { harness, mounted, posts, settle, editHead } = await mountDescribableList(t);
  const trigger = mounted.container.querySelector('[data-process-description-edit="described"]');
  assert.ok(trigger, 'the description itself is the edit affordance');
  assert.equal(trigger.textContent, 'Old description');
  assert.equal(trigger.getAttribute('aria-label'), 'Description for Release train',
    'the control names the template it describes, not just "edit"');
  assert.notEqual(trigger.getAttribute('title'), null, 'the affordance is discoverable on hover too');

  await harness.act(() => harness.fireEvent(trigger, 'click'));
  const input = mounted.container.querySelector('[data-process-description-input="described"]');
  assert.ok(input, 'clicking the description swaps in an editor');
  assert.equal(harness.document.activeElement, input, 'the editor takes focus when opened');
  assert.equal(input.value, 'Old description', 'the edit starts from the current description');

  input.value = 'Ships the release train';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  await settle();

  assert.equal(posts().length, 1, 'Enter commits exactly one save');
  const body = JSON.parse(posts()[0].options.body);
  // The save handler sets DisallowUnknownFields, so forwarding the whole head
  // edit view would 400 in production while still satisfying the value
  // assertions below. Pin the key set so that refactor cannot pass silently.
  assert.deepEqual(Object.keys(body).sort(), ['edges', 'layout', 'sourceHash', 'template']);
  assert.equal(body.template.description, 'Ships the release train');
  assert.equal(body.template.name, 'Release train', 'the display name survives a description edit');
  assert.equal(body.template.id, 'described', 'the immutable id is preserved');
  assert.deepEqual(body.template.nodes, editHead.template.nodes, 'the graph survives untouched');
  assert.deepEqual(body.edges, editHead.edges, 'edges round-trip untouched');
  assert.deepEqual(body.layout, editHead.layout, 'editor layout round-trips untouched');
  assert.equal(body.sourceHash, 'row-v1', 'the edit saves against the version the row was showing');
  await mounted.unmount();
});

test('an inline description commits once on blur, and never twice after Enter', async (t) => {
  const { harness, mounted, posts, settle } = await mountDescribableList(t);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-description-edit="described"]'), 'click'));
  const input = mounted.container.querySelector('[data-process-description-input="described"]');
  input.value = 'Committed by blur';
  await harness.act(() => harness.fireEvent(input, 'blur'));
  await settle();
  assert.equal(posts().length, 1, 'blur commits the edit');
  assert.equal(JSON.parse(posts()[0].options.body).template.description, 'Committed by blur');

  // Enter unmounts the input; the trailing blur must not commit a second time
  // from a dead ref.
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-description-edit="described"]'), 'click'));
  const second = mounted.container.querySelector('[data-process-description-input="described"]');
  second.value = 'Committed by Enter';
  await harness.act(() => harness.fireEvent(second, 'keydown', { key: 'Enter' }));
  await harness.act(() => harness.fireEvent(second, 'blur'));
  await settle();
  assert.equal(posts().length, 2, 'Enter followed by the trailing blur is a single commit');
  await mounted.unmount();
});

test('Escape and unchanged text abandon an inline description edit without saving', async (t) => {
  const { harness, mounted, posts, settle } = await mountDescribableList(t);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-description-edit="described"]'), 'click'));
  const input = mounted.container.querySelector('[data-process-description-input="described"]');
  input.value = 'Discard this';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Escape' }));
  await settle();
  assert.equal(posts().length, 0, 'Escape commits nothing');
  assert.equal(mounted.container.querySelector('[data-process-description-edit="described"]').textContent, 'Old description');

  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-description-edit="described"]'), 'click'));
  const again = mounted.container.querySelector('[data-process-description-input="described"]');
  again.value = 'Old description';
  await harness.act(() => harness.fireEvent(again, 'keydown', { key: 'Enter' }));
  await settle();
  assert.equal(posts().length, 0, 'retyping the same description commits no new version');
  await mounted.unmount();
});

test('an empty inline description clears the stored value and offers an empty-state affordance', async (t) => {
  const { harness, state, mounted, posts, settle } = await mountDescribableList(t);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-description-edit="described"]'), 'click'));
  const input = mounted.container.querySelector('[data-process-description-input="described"]');
  input.value = '   ';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  await settle();
  assert.equal(posts().length, 1);
  assert.equal(JSON.parse(posts()[0].options.body).template.description, '', 'whitespace-only input clears the description');
  assert.match(state.notice.value, /Cleared the description for described/);

  state.templatesRequest.commitRequest(state.templatesRequest.beginRequest(), { templates: [{ id: 'described', name: 'Release train' }] });
  await harness.act(() => Promise.resolve());
  const empty = mounted.container.querySelector('[data-process-description-edit="described"]');
  assert.match(empty.textContent, /add a description/i, 'an empty description still offers a visible way in');
  await mounted.unmount();
});

test('an inline description edit disabled while another template mutation runs', async (t) => {
  const { harness, state, mounted } = await mountDescribableList(t);
  assert.equal(mounted.container.querySelector('[data-process-description-edit="described"]').disabled, false);
  state.beginMutation();
  await harness.act(() => Promise.resolve());
  assert.equal(mounted.container.querySelector('[data-process-description-edit="described"]').disabled, true,
    'a second concurrent template mutation cannot be started from the description cell');
  state.endMutation();
  await mounted.unmount();
});

test('an inline description losing the CAS race reports a description-specific conflict', async (t) => {
  const { harness, state, mounted, notices, settle } = await mountDescribableList(t, {
    post: () => ({ ok: false, status: 409, json: async () => ({ code: 'process_template_conflict' }) }),
  });
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-description-edit="described"]'), 'click'));
  const input = mounted.container.querySelector('[data-process-description-input="described"]');
  input.value = 'Written over a concurrent edit';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  await settle();

  assert.match(state.notice.value, /Description update failed/);
  assert.match(state.notice.value, /changed while you were editing its description/);
  assert.doesNotMatch(state.notice.value, /Rename failed/, 'the conflict names the action the operator actually took');
  assert.doesNotMatch(state.notice.value, /Written over a concurrent edit/, 'the notice does not reproduce the description body');
  assert.doesNotMatch(notices.join('\n'), /Written over a concurrent edit/, 'nor does the notification channel');
  assert.equal(state.mutation.value.busy, false, 'the mutation gate is released after a conflict');
  // The row keeps showing truthful server state rather than the rejected draft.
  assert.equal(mounted.container.querySelector('[data-process-description-edit="described"]').textContent, 'Old description');
  await mounted.unmount();
});

// The list keeps refreshing under an open editor: the snapshot poll observes
// moved heads and reloads the rows. If the open editor tracked those refreshed
// props, Enter would save against the NEW head -- which the server accepts --
// and the concurrent writer's change would be overwritten instead of
// conflicting. The edit session must stay pinned to what the operator opened on.
test('a list refresh under an open description editor cannot substitute a newer CAS baseline', async (t) => {
  const { harness, state, mounted, posts, settle } = await mountDescribableList(t, {
    post: () => ({ ok: false, status: 409, json: async () => ({ code: 'process_template_conflict' }) }),
  });
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-description-edit="described"]'), 'click'));
  const input = mounted.container.querySelector('[data-process-description-input="described"]');
  input.value = 'my stale draft';

  // A concurrent writer moves the head while the operator is still typing, and
  // the ordinary refresh publishes the newer row.
  state.templatesRequest.commitRequest(state.templatesRequest.beginRequest(), {
    templates: [{ id: 'described', name: 'Release train', description: 'concurrent description', latestVersion: { sourceHash: 'row-v2' } }],
  });
  await harness.act(() => Promise.resolve());
  const live = mounted.container.querySelector('[data-process-description-input="described"]');
  assert.equal(live, input, 'the refresh does not tear down the open editor');
  assert.equal(live.value, 'my stale draft', 'nor does it discard the operator draft');

  await harness.act(() => harness.fireEvent(live, 'keydown', { key: 'Enter' }));
  await settle();

  assert.equal(posts().length, 1);
  const body = JSON.parse(posts()[0].options.body);
  assert.equal(body.sourceHash, 'row-v1',
    'committing against the refreshed head would silently overwrite the concurrent description instead of conflicting');
  assert.equal(body.template.description, 'my stale draft');
  // The save is therefore refused, and the row keeps showing the concurrent
  // writer's truthful state rather than the rejected draft.
  assert.match(state.notice.value, /changed while you were editing its description/);
  await harness.act(() => Promise.resolve());
  assert.equal(mounted.container.querySelector('[data-process-description-edit="described"]').textContent, 'concurrent description');
  await mounted.unmount();
});

test('a list refresh under an open name editor cannot substitute a newer CAS baseline', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = [];
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [{ id: 'racy', name: 'What the operator saw', latestVersion: { sourceHash: 'name-v1' } }] }) };
      if (options.method === 'POST') return { ok: true, status: 201, json: async () => ({ ref: 'racy@sha256:new' }) };
      return { ok: true, json: async () => ({ template: { id: 'racy', name: 'Renamed by an agent' }, sourceHash: 'name-v2' }) };
    },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-name-edit="racy"]'), 'click'));
  const input = mounted.container.querySelector('[data-process-name-input="racy"]');
  input.value = 'Operator name';
  state.templatesRequest.commitRequest(state.templatesRequest.beginRequest(), {
    templates: [{ id: 'racy', name: 'Renamed by an agent', latestVersion: { sourceHash: 'name-v2' } }],
  });
  await harness.act(() => Promise.resolve());
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-name-input="racy"]'), 'keydown', { key: 'Enter' }));
  for (let i = 0; i < 10 && !requests.some((r) => r.options.method === 'POST'); i++) await harness.act(() => Promise.resolve());
  const body = JSON.parse(requests.find((request) => request.options.method === 'POST').options.body);
  assert.equal(body.sourceHash, 'name-v1', 'the shared edit session pins the inline rename baseline too');
  await mounted.unmount();
});

test('creating a template persists the named scaffold and prepopulates the editor with its generated id', async (t) => {
  const harness = await createPreactHarness(t);
  const previous = { raf: globalThis.requestAnimationFrame, css: globalThis.CSS };
  globalThis.requestAnimationFrame = () => 1;
  globalThis.CSS = { escape: (value) => String(value) };
  t.after(() => {
    if (previous.raf === undefined) delete globalThis.requestAnimationFrame;
    else globalThis.requestAnimationFrame = previous.raf;
    if (previous.css === undefined) delete globalThis.CSS;
    else globalThis.CSS = previous.css;
  });
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const requests = [];
  const actions = createProcessesActions({
    state, notify() {},
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (path === '/v1/process/templates' && options.method === 'POST') {
        return {
          ok: true, status: 201, statusText: 'Created',
          json: async () => ({
            id: '9f3c2b1a4d5e6f708192a3b4c5d6e7f8',
            ref: '9f3c2b1a4d5e6f708192a3b4c5d6e7f8@sha256:new',
            sourceHash: 'source-new', semanticHash: 'semantic-new', diagnostics: [],
          }),
        };
      }
      return { ok: true, status: 200, statusText: 'OK', json: async () => ({ templates: [] }) };
    },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());

  await harness.act(() => harness.fireEvent(mounted.container.querySelector('#process-template-new'), 'click'));
  const input = mounted.container.querySelector('[data-process-create-input]');
  assert.ok(input, 'creation prompts for a display name');
  assert.equal(mounted.container.querySelector('.process-editor-id-input'), null,
    'creation never offers an id field');

  input.value = 'Release train';
  await harness.act(() => harness.fireEvent(input, 'input'));
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('.process-rename-dialog'), 'submit'));
  for (let i = 0; i < 10 && !state.canvas.value; i++) await harness.act(() => Promise.resolve());

  assert.equal(state.canvas.value?.kind, 'editor');
  assert.equal(state.canvas.value?.blank, false, 'the editor opens the persisted backend generation');
  assert.equal(state.canvas.value?.name, 'Release train', 'the editor opens on the chosen name');
  assert.equal(state.canvas.value?.id, '9f3c2b1a4d5e6f708192a3b4c5d6e7f8',
    'the editor uses the id assigned by the backend');
  assert.equal(state.canvas.value?.view?.template.name, 'Release train');
  assert.equal(state.canvas.value?.view?.template.id, '9f3c2b1a4d5e6f708192a3b4c5d6e7f8');
  assert.equal(state.canvas.value?.view?.currentRef, '9f3c2b1a4d5e6f708192a3b4c5d6e7f8@sha256:new');
  assert.equal(state.canvas.value?.view?.sourceHash, 'source-new');
  assert.equal(state.create.value, null, 'the prompt closes once the editor opens');
  for (let i = 0; i < 20 && !state.currentEditor(); i++) {
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 5)));
  }
  assert.equal(state.currentEditor()?.model?.template?.id, '9f3c2b1a4d5e6f708192a3b4c5d6e7f8');
  assert.equal(state.currentEditor()?.model?.template?.name, 'Release train');
  assert.equal(state.currentEditor()?.model?.sourceHash, 'source-new',
    'the created editor starts on the backend-confirmed CAS generation');
  assert.equal(mounted.container.querySelector('[data-process-title-edit]')?.textContent, 'Release train',
    'the rendered editor title is prepopulated from the creation dialog');
  const create = requests.find((request) => request.options.method === 'POST');
  assert.equal(create.path, '/v1/process/templates');
  assert.equal(create.options.credentials, 'same-origin');
  assert.match(create.options.headers['Idempotency-Key'], /^[0-9a-f-]{36}$/);
  assert.equal(create.options.headers['X-Tclaude-Request-Digest'], createHash('sha256')
    .update(`POST\x00/v1/process/templates\x00${create.options.body}`).digest('hex'));
  const payload = JSON.parse(create.options.body);
  assert.equal(payload.template.name, 'Release train');
  assert.equal(Object.hasOwn(payload.template, 'id'), false, 'the browser never proposes a permanent id');
  assert.equal(Object.hasOwn(payload, 'sourceHash'), false, 'creation has no prior CAS generation');
  await mounted.unmount();
});

test('an empty creation name cannot be submitted', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const actions = createProcessesActions({ state, notify() {}, fetchImpl: async () => ({ ok: true, json: async () => ({}) }) });
  await actions.openCreate();
  assert.equal(await actions.submitCreate('   '), false, 'a whitespace-only name is not a name');
  assert.ok(state.create.value, 'the prompt stays open');
  assert.equal(state.canvas.value, null, 'no editor opens without a name');
});
// The browser URL must name the template being edited, so the editor is
// deep-linkable and browser Back walks in and out of it.
test('Processes URL location follows the open template editor and restores from it', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'),
    harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const fetchImpl = async (path) => ({ ok: true, json: async () => (
    path.includes('worklist') ? { items: [], degradedRuns: [] }
      : path.includes('templates') ? { templates: [{ id: 'release-train' }] }
        : { runs: [] }) });
  // Capture the correction flag alongside the location: a correction must
  // REPLACE the current history entry, so the two are not interchangeable.
  const announced = [];
  const record = (location, { correction = false } = {}) => announced.push({ location, correction });
  const actions = createProcessesActions({ state, fetchImpl, notify() {}, dispatchNavigated: record });
  const lastLocation = () => announced.at(-1)?.location;

  // Opening the editor puts the template id in the URL...
  await actions.openEditor('release-train');
  assert.deepEqual(lastLocation(),
    { tab: 'processes', subtab: 'templates', selection: 'release-train' });
  assert.equal(announced.at(-1).correction, false, 'opening is a real navigation, not a fix-up');

  // ...and the "← templates" back button drops the segment again, so the two
  // are distinct history entries.
  assert.equal(await actions.closeCanvas(), true);
  assert.deepEqual(lastLocation(), { tab: 'processes', subtab: 'templates' });

  // A blank scaffold has no saved id to address yet — it stays on the list
  // path. Assert on the announcement COUNT too: the previous close already left
  // the bare templates location on the tail, so a deepEqual alone would still
  // pass if openEditor announced nothing at all.
  const beforeBlank = announced.length;
  await actions.openEditor('new-process', true);
  assert.equal(announced.length, beforeBlank + 1, 'opening the scaffold did announce');
  assert.deepEqual(lastLocation(), { tab: 'processes', subtab: 'templates' },
    'an unsaved blank template is not addressable');

  // A deep link / reload / browser Back drives applyLocation, which reopens the
  // editor but must NOT announce back — the URL is already where it wants to be.
  const settled = announced.length;
  assert.equal(await actions.applyLocation(
    { tab: 'processes', subtab: 'templates', selection: 'release-train' }), true);
  assert.equal(state.canvas.value.kind, 'editor');
  assert.equal(state.canvas.value.id, 'release-train');
  assert.equal(announced.length, settled, 'a router-driven restore forges no history entry');

  // Re-applying the location already on screen is a no-op.
  assert.equal(await actions.applyLocation(
    { tab: 'processes', subtab: 'templates', selection: 'release-train' }), true);
  assert.equal(state.canvas.value.id, 'release-train');

  // An unsaved editor refuses a Back-driven restore, so the operator keeps
  // their work.
  state.setEditor({ dirty: true, model: { dirty: true } });
  const guarded = createProcessesActions({
    state, fetchImpl, notify() {}, dispatchNavigated: record,
    confirmDiscard: async () => false,
  });
  assert.equal(await guarded.applyLocation({ tab: 'processes', subtab: 'templates' }), false);
  assert.equal(state.canvas.value.id, 'release-train', 'the unsaved editor stayed open');
  guarded.correctLocation();
  assert.deepEqual(lastLocation(),
    { tab: 'processes', subtab: 'templates', selection: 'release-train' },
    'the URL is corrected back to the view that is actually showing');
  assert.equal(announced.at(-1).correction, true, 'and as a replace, not a push');
});

// The wiring the history router depends on: it dispatches `tclaude:restore-location`
// (js/nav-history.js) and the mounted island must act on it — that is what makes
// a pasted /processes/templates/<id> URL, a reload, or a Back actually land in
// the editor.
test('a router restore-location event opens the editor on the mounted Processes island', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { mountProcessesIsland }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'),
    harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const announced = [];
  const actions = createProcessesActions({
    state, notify() {}, dispatchNavigated: (location, { correction = false } = {}) =>
      announced.push({ location, correction }),
    fetchImpl: async () => ({ ok: true, json: async () => ({ templates: [{ id: 'release-train' }] }) }),
  });
  // Go through the real mount path, not just <ProcessesApp>: the restore
  // listener is registered there precisely so it is attached synchronously,
  // before the router can dispatch into it.
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  await harness.act(async () => mountProcessesIsland({
    host, state, actions, confirmDiscard: async () => true,
    registerCleanup: (fn) => cleanups.push(fn),
  }));
  await harness.act(() => Promise.resolve());

  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', {
    detail: { location: { tab: 'processes', subtab: 'templates', selection: 'release-train' } },
  }));
  for (let i = 0; i < 10 && state.canvas.value === null; i++) await harness.act(() => Promise.resolve());

  assert.equal(state.canvas.value?.kind, 'editor');
  assert.equal(state.canvas.value?.id, 'release-train', 'the URL id selected the template');
  assert.deepEqual(announced, [], 'restoring from the URL announces nothing back to the router');
  assert.ok(host.querySelector('#process-editor-view'), 'the editor view is on screen');

  // A location for a different tab is not ours to act on.
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', {
    detail: { location: { tab: 'groups' } },
  }));
  await harness.act(() => Promise.resolve());
  assert.equal(state.canvas.value?.id, 'release-train', 'another tab’s location is ignored');

  // Cleanup must detach the listener, or a remounted island would double-apply.
  for (const fn of cleanups.reverse()) fn();
  const settled = announced.length;
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', {
    detail: { location: { tab: 'processes', subtab: 'templates', selection: 'another-template' } },
  }));
  await harness.act(() => Promise.resolve());
  assert.equal(announced.length, settled, 'the restore listener is removed on cleanup');
  host.remove();
});

test('a failed template creation stays in the dialog and surfaces the backend error', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const notices = [];
  const actions = createProcessesActions({
    state, notify: (...args) => notices.push(args),
    fetchImpl: async () => ({
      ok: false, status: 503, statusText: 'Unavailable',
      json: async () => ({ message: 'store unavailable' }),
    }),
  });
  actions.openCreate();

  assert.equal(await actions.submitCreate('Release train'), false);
  assert.equal(state.create.value?.name, 'Release train');
  assert.equal(state.create.value?.error, 'store unavailable');
  assert.equal(state.create.value?.attempt, null, 'a definitive response gets a fresh key on retry');
  assert.equal(state.canvas.value, null, 'a failed create never opens a fictional template id');
  assert.match(state.notice.value, /Template creation failed: store unavailable/);
  assert.match(notices.at(-1)[0], /process template creation failed: store unavailable/);
  assert.equal(state.mutation.value.busy, false);
});

test('an unknown template-create outcome blocks automatic retry until the operator reconciles it', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const attemptIDs = [
    '11111111-2222-4333-8444-555555555555',
    '22222222-3333-4444-8555-666666666666',
  ];
  let calls = 0;
  let minted = 0;
  const actions = createProcessesActions({
    state, notify() {}, mintAttemptID: () => attemptIDs[minted++],
    fetchImpl: async () => {
      calls += 1;
      return calls === 1
        ? {
          ok: false, status: 409, statusText: 'Conflict',
          json: async () => ({
            code: 'idempotency_unknown',
            error: 'the previous agentd stopped while this operation was pending; its outcome is unknown',
          }),
        }
        : {
          ok: false, status: 503, statusText: 'Unavailable',
          json: async () => ({ error: 'store unavailable' }),
        };
    },
  });
  actions.openCreate();

  assert.equal(await actions.submitCreate('Release train'), false);
  assert.equal(state.create.value?.attempt?.key, attemptIDs[0]);
  assert.equal(state.create.value?.attempt?.blocked, true);
  assert.match(state.create.value?.error, /Refresh Templates to reconcile/);

  assert.equal(await actions.submitCreate('Release train'), false);
  assert.equal(calls, 1, 'the same ambiguous mutation is not sent again or silently reminted');
  assert.equal(minted, 1);

  assert.equal(await actions.submitCreate('Different template'), false,
    'changing the name is an explicit new logical creation');
  assert.equal(calls, 2);
  assert.equal(minted, 2);
  assert.equal(state.create.value?.attempt, null, 'the definitive new-attempt failure can be retried normally');
});

test('an ambiguous template-create failure retries with the same durable attempt key', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const attemptID = '11111111-2222-4333-8444-555555555555';
  const generatedID = '9f3c2b1a4d5e6f708192a3b4c5d6e7f8';
  let minted = 0;
  const requests = [];
  const actions = createProcessesActions({
    state, notify() {}, mintAttemptID: () => { minted += 1; return attemptID; },
    fetchImpl: async (path, options = {}) => {
      requests.push({ path, options });
      if (options.method === 'POST' && requests.filter((request) => request.options.method === 'POST').length === 1) {
        throw new Error('connection reset after commit');
      }
      if (options.method === 'POST') {
        return {
          ok: true, status: 201, statusText: 'Created',
          json: async () => ({
            id: generatedID, ref: `${generatedID}@sha256:new`,
            sourceHash: 'source-new', semanticHash: 'semantic-new', diagnostics: [],
          }),
        };
      }
      return { ok: true, json: async () => ({ templates: [] }) };
    },
  });
  actions.openCreate();

  assert.equal(await actions.submitCreate('Release train'), false);
  assert.equal(state.create.value?.attempt?.key, attemptID);
  assert.match(state.notice.value, /connection reset after commit/);
  assert.equal(await actions.submitCreate('Release train'), true);

  const posts = requests.filter((request) => request.options.method === 'POST');
  assert.equal(posts.length, 2);
  assert.equal(minted, 1, 'one logical create keeps one idempotency key across an ambiguous retry');
  assert.equal(posts[0].options.body, posts[1].options.body);
  assert.equal(posts[0].options.headers['Idempotency-Key'], attemptID);
  assert.equal(posts[1].options.headers['Idempotency-Key'], attemptID);
  assert.equal(posts[0].options.headers['X-Tclaude-Request-Digest'],
    posts[1].options.headers['X-Tclaude-Request-Digest']);
  assert.equal(state.canvas.value?.id, generatedID,
    'the retry adopts the original backend result instead of minting a second template');
});
