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
  await actions.loadRunView('run/with space', 25, 25);
  assert.equal(requests.at(-1).path, '/v1/process/runs/run%2Fwith%20space/view?detailOffset=25&detailLimit=25');
  assert.equal(requests.at(-1).options.credentials, 'same-origin');
  const item = { id: 'i/1', run: 'r', node: 'n', kind: 'decision', status: 'pending', summary: 'Choose', availableActions: ['approve'] };
  state.worklistRequest.commitRequest(state.worklistRequest.beginRequest(), { items: [item], degradedRuns: [] });
  assert.equal(await actions.submitWorklistAction(item.id, 'approve'), false); assert.equal(state.missingComments.value.has(item.id), true);
  state.setDraft(item.id, 'ok'); assert.equal(await actions.submitWorklistAction(item.id, 'approve'), true);
  const post = requests.find((request) => request.options.method === 'POST'); assert.equal(post.path, '/v1/process/worklist/i%2F1/action');
  assert.equal(JSON.parse(post.options.body).action, 'approve');

  let observedHead = null;
  state.setEditor({
    model: { template: { id: 'fresh' }, currentRef: '', sourceHash: '', dirty: false },
    observeExternalHead(head) { observedHead = head; },
  });
  await actions.load('templates', { quiet: true });
  assert.equal(observedHead, null, 'a list row without a latest generation is ignored');

  let discardPrompts = 0;
  state.setEditor({ dirty: true, model: { dirty: false } });
  const guarded = createProcessesActions({
    state, fetchImpl,
    confirmDiscard: async () => { discardPrompts += 1; return false; },
  });
  assert.equal(await guarded.closeCanvas(), false, 'a staged dialog draft participates in editor navigation guards');
  assert.equal(discardPrompts, 1);
});

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

test('instantiate actions load an exact ref, POST string params, and navigate to its viewer', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const ref = `release@sha256:${'a'.repeat(64)}`;
  const requests = [];
  const attemptID = '11111111-2222-4333-8444-555555555555';
  const actions = createProcessesActions({ state, notify() {}, dispatchNavigated() {}, mintAttemptID: () => attemptID, fetchImpl: async (path, options = {}) => {
    requests.push({ path, options });
    if (path.startsWith('/v1/process/templates/release?version=')) return { ok: true, json: async () => ({
      currentRef: ref, template: { id: 'release', params: { issue: { type: 'string', required: true } } },
    }) };
    if (path === '/v1/process/runs' && options.method === 'POST') return { ok: true, json: async () => ({
      run: { id: `release-${attemptID}`, templateRef: ref, createdAt: '2026-07-14T00:00:00Z', updatedAt: '2026-07-14T00:00:00Z' },
    }) };
    if (path === '/v1/process/runs') return { ok: true, json: async () => ({ runs: [] }) };
    throw new Error(`unexpected ${path}`);
  } });
  assert.equal(await actions.openInstantiation({ id: 'release', ref }), true);
  assert.equal(state.instantiation.value.ref, ref);
  assert.equal(state.instantiation.value.phase, 'ready');
  assert.equal(await actions.submitInstantiation({ issue: 'TCL-300', retries: '2', approved: 'true' }), true);
  const post = requests.find((request) => request.path === '/v1/process/runs' && request.options.method === 'POST');
  assert.deepEqual(JSON.parse(post.options.body), {
    templateRef: ref, runId: `release-${attemptID}`, params: { issue: 'TCL-300', retries: '2', approved: 'true' },
  });
  assert.equal(state.subtab.value, 'runs');
  assert.deepEqual(state.canvas.value, { kind: 'viewer', id: `release-${attemptID}`, key: `release-${attemptID}` });
  assert.equal(state.instantiation.value, null);
});

test('successful instantiation moves focus from the removed invoker into the new viewer', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
    harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const ref = `focus-success@sha256:${'f'.repeat(64)}`;
  const runID = 'focus-success-run';
  const actions = createProcessesActions({
    state, notify() {}, dispatchNavigated() {}, mintAttemptID: () => 'focus-success-attempt',
    fetchImpl: async (path, options = {}) => {
      if (path === '/v1/process/templates') return { ok: true, json: async () => ({ templates: [{
        id: 'focus-success', latestVersion: { ref, sourceHash: 'focus-success-source' },
      }] }) };
      if (path.startsWith('/v1/process/templates/focus-success?version=')) return { ok: true, json: async () => ({
        currentRef: ref, template: { id: 'focus-success', params: {} },
      }) };
      if (path === '/v1/process/runs' && options.method === 'POST') return { ok: true, json: async () => ({
        run: { id: runID, templateRef: ref, createdAt: '2026-07-14T00:00:00Z', updatedAt: '2026-07-14T00:00:00Z' },
      }) };
      if (path === '/v1/process/runs') return { ok: true, json: async () => ({ runs: [] }) };
      throw new Error(`unexpected ${path}`);
    },
  });
  await actions.load('templates');
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await harness.act(() => Promise.resolve());
  const invoker = mounted.container.querySelector('[data-process-action="instantiate"]');
  invoker.focus();
  await harness.act(() => harness.fireEvent(invoker, 'click'));
  for (let i = 0; i < 10 && state.instantiation.value?.phase !== 'ready'; i++) await harness.act(() => Promise.resolve());
  assert.equal(state.instantiation.value?.phase, 'ready');
  const form = mounted.container.querySelector('.process-instantiate-dialog');
  assert.ok(form);
  await harness.act(() => harness.fireEvent(form, 'submit'));
  for (let i = 0; i < 10; i++) await harness.act(() => Promise.resolve());
  const back = mounted.container.querySelector('#process-viewer-view [data-process-close-view]');
  assert.ok(back);
  assert.equal(mounted.container.contains(invoker), false, 'viewer navigation removes the template-list invoker');
  assert.equal(harness.document.activeElement, back, 'focus lands on the viewer back control instead of body');
  await mounted.unmount();
});

test('instantiate retry keeps one strong attempt id and recovers a committed run after its response is lost', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const ref = `retry@sha256:${'e'.repeat(64)}`;
  const attemptID = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
  let minted = 0;
  let durableRun = null;
  let durableCreates = 0;
  const posted = [];
  const actions = createProcessesActions({
    state,
    notify() {},
    dispatchNavigated() {},
    mintAttemptID() { minted++; return attemptID; },
    fetchImpl: async (path, options = {}) => {
      if (path === '/v1/process/runs' && options.method === 'POST') {
        const request = JSON.parse(options.body);
        posted.push(request);
        if (!durableRun) {
          durableCreates++;
          durableRun = { id: request.runId, templateRef: request.templateRef, createdAt: '2026-07-14T00:00:00Z', updatedAt: '2026-07-14T00:00:00Z' };
          throw new TypeError('response lost after commit');
        }
        assert.equal(request.runId, durableRun.id, 'retry must address the committed logical attempt');
        return { ok: true, json: async () => ({ run: durableRun }) };
      }
      if (path === '/v1/process/runs') return { ok: true, json: async () => ({ runs: [durableRun] }) };
      throw new Error(`unexpected ${path}`);
    },
  });

  assert.equal(await actions.openInstantiation({
    id: 'retry', ref, template: { id: 'retry', params: { issue: { type: 'string', required: true } } },
  }), true);
  assert.equal(await actions.submitInstantiation({ issue: 'TCL-300' }), false, 'lost response remains retryable');
  assert.equal(state.instantiation.value.runId, `retry-${attemptID}`);
  assert.equal(await actions.submitInstantiation({ issue: 'TCL-300' }), true, 'retry recovers committed run');
  assert.equal(minted, 1, 'one open instantiation mints one logical-attempt id');
  assert.equal(durableCreates, 1, 'backend simulation commits only one durable run');
  assert.equal(posted.length, 2);
  assert.equal(posted[0].runId, posted[1].runId);
  assert.deepEqual(state.canvas.value, { kind: 'viewer', id: durableRun.id, key: durableRun.id });
});

test('list instantiation loading transition initializes every declared default exactly once', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const ref = `defaults@sha256:${'c'.repeat(64)}`;
  state.setInstantiation({ key: 'list-defaults', id: 'defaults', ref, phase: 'loading', error: '', template: null });
  let submitted = null;
  const actions = {
    refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {}, closeCanvas() {},
    closeInstantiation() { state.setInstantiation(null); }, submitInstantiation(params) { submitted = params; },
  };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  assert.match(mounted.container.querySelector('.process-instantiate-dialog').textContent, /Loading exact template version/);
  await harness.act(() => state.setInstantiation({
    key: 'list-defaults', id: 'defaults', ref, phase: 'ready', error: '',
    template: { id: 'defaults', params: {
      issue: { type: 'string', default: 'TCL-300' },
      retries: { type: 'number', default: 2 },
      approved: { type: 'boolean', default: true },
    } },
  }));
  await new Promise(resolve => queueMicrotask(resolve));
  const dialog = mounted.container.querySelector('.process-instantiate-dialog');
  assert.equal(dialog.querySelector('[data-process-param-input="issue"]').value, 'TCL-300');
  assert.equal(dialog.querySelector('[data-process-param-input="retries"]').value, '2');
  assert.equal(dialog.querySelector('[data-process-param-input="approved"]').getAttribute('value'), 'true');
  harness.fireEvent(dialog, 'submit');
  assert.deepEqual(submitted, { approved: 'true', issue: 'TCL-300', retries: '2' });
  const issue = dialog.querySelector('[data-process-param-input="issue"]');
  issue.value = 'user edit'; harness.fireEvent(issue, 'input'); await harness.act(() => Promise.resolve());
  await harness.act(() => state.setInstantiation({
    ...state.instantiation.value,
    template: { id: 'defaults', params: {
      issue: { type: 'string', default: 'replacement default' },
      retries: { type: 'number', default: 9 },
      approved: { type: 'boolean', default: false },
    } },
  }));
  assert.equal(dialog.querySelector('[data-process-param-input="issue"]').value, 'user edit', 'same-ref refresh preserves edits');
  await mounted.unmount();
});

test('instantiate dialog renders typed/defaulted/required inputs and canonical values', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const ref = `typed@sha256:${'b'.repeat(64)}`;
  state.setInstantiation({
    key: 'typed', id: 'typed', ref, phase: 'ready', error: '',
    template: { id: 'typed', name: 'Typed run', params: {
      issue: { type: 'string', description: 'Issue id', required: true },
      retries: { type: 'number', default: 2 },
      approved: { type: 'boolean', default: true },
      legacy: { type: 'custom-kind', default: 'raw' },
    } },
  });
  let submitted = null;
  const actions = {
    refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {}, closeCanvas() {},
    closeInstantiation() {}, submitInstantiation(params) { submitted = params; },
  };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  const dialog = harness.document.querySelector('.process-instantiate-dialog');
  assert.ok(dialog);
  const issue = dialog.querySelector('[data-process-param-input="issue"]');
  const retries = dialog.querySelector('[data-process-param-input="retries"]');
  const approved = dialog.querySelector('[data-process-param-input="approved"]');
  const legacy = dialog.querySelector('[data-process-param-input="legacy"]');
  assert.equal(issue.type, 'text'); assert.equal(issue.hasAttribute('required'), true);
  assert.equal(retries.type, 'number'); assert.equal(retries.value, '2');
  assert.equal(approved.tagName, 'SELECT'); assert.equal(approved.getAttribute('value'), 'true');
  assert.equal(legacy.type, 'text'); assert.equal(legacy.value, 'raw');
  assert.match(dialog.querySelector('[data-process-param="issue"]').textContent, /Issue id/);
  issue.value = 'TCL-300'; harness.fireEvent(issue, 'input'); await harness.act(() => Promise.resolve());
  retries.value = '9007199254740993'; harness.fireEvent(retries, 'input'); await harness.act(() => Promise.resolve());
  for (const option of approved.options) option.selected = option.value === 'false';
  harness.fireEvent(approved, 'change'); await harness.act(() => Promise.resolve());
  harness.fireEvent(dialog, 'submit');
  assert.deepEqual(submitted, { approved: 'false', issue: 'TCL-300', legacy: 'raw', retries: '9007199254740993' });
  await mounted.unmount();
});

test('optional booleans stay omitted while explicit and defaulted false values are submitted', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  state.setInstantiation({
    key: 'boolean-omission', id: 'boolean-omission', ref: `boolean-omission@sha256:${'9'.repeat(64)}`, phase: 'ready', error: '',
    template: { id: 'boolean-omission', params: {
      defaultFalse: { type: 'boolean', default: false },
      optional: { type: 'boolean' },
    } },
  });
  const submissions = [];
  const actions = {
    refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {}, closeCanvas() {},
    closeInstantiation() {}, submitInstantiation(params) { submissions.push(params); },
  };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  const dialog = mounted.container.querySelector('.process-instantiate-dialog');
  const defaultFalse = dialog.querySelector('[data-process-param-input="defaultFalse"]');
  const optional = dialog.querySelector('[data-process-param-input="optional"]');
  assert.equal(defaultFalse.getAttribute('value'), 'false', 'a declared false default remains selected');
  assert.equal(optional.getAttribute('value'), '', 'an optional boolean without a default begins unset');
  harness.fireEvent(dialog, 'submit');
  assert.deepEqual(submissions.at(-1), { defaultFalse: 'false' }, 'untouched optional boolean is omitted');
  for (const option of optional.options) option.selected = option.value === 'false';
  harness.fireEvent(optional, 'change'); await harness.act(() => Promise.resolve());
  harness.fireEvent(dialog, 'submit');
  assert.deepEqual(submissions.at(-1), { defaultFalse: 'false', optional: 'false' }, 'explicit false is retained');
  await mounted.unmount();
});

test('instantiate dialog owns focus, traps Tab, restores the invoker, and cannot dismiss while busy', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const ref = `focus@sha256:${'d'.repeat(64)}`;
  const spec = (key, params = { issue: { type: 'string', required: true } }) => ({
    key, id: 'focus', ref, phase: 'ready', error: '', template: { id: 'focus', params },
  });
  const actions = {
    refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {}, closeCanvas() {},
    closeInstantiation() { if (!state.mutation.value.busy) state.setInstantiation(null); }, submitInstantiation() {},
  };
  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.textContent = 'instantiate focus'; invoker.focus();
  state.setInstantiation(spec('focus-1'));
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} />`);
  await new Promise(resolve => queueMicrotask(resolve));
  let overlay = mounted.container.querySelector('.process-instantiate-modal');
  let first = overlay.querySelector('[data-process-param-input="issue"]');
  let create = overlay.querySelector('button[type="submit"]');
  assert.equal(harness.document.activeElement, first, 'the first declared param receives initial focus');
  create.focus(); await harness.act(() => harness.fireEvent(create, 'keydown', { key: 'Tab' }));
  assert.equal(harness.document.activeElement, first, 'Tab wraps to the first dialog control');
  await harness.act(() => harness.fireEvent(first, 'keydown', { key: 'Tab', shiftKey: true }));
  assert.equal(harness.document.activeElement, create, 'Shift+Tab wraps to the last dialog control');
  await harness.act(() => harness.fireEvent(create, 'keydown', { key: 'Escape' }));
  assert.equal(state.instantiation.value, null);
  assert.equal(harness.document.activeElement, invoker, 'Escape restores focus to the invoker');

  invoker.focus(); await harness.act(() => state.setInstantiation(spec('focus-empty', {})));
  await new Promise(resolve => queueMicrotask(resolve));
  overlay = mounted.container.querySelector('.process-instantiate-modal');
  create = overlay.querySelector('button[type="submit"]');
  assert.equal(harness.document.activeElement, create, 'Create run receives initial focus when there are no params');
  await harness.act(() => harness.fireEvent(overlay, 'click'));
  assert.equal(state.instantiation.value, null, 'backdrop closes when idle');
  assert.equal(harness.document.activeElement, invoker, 'backdrop close restores focus');

  invoker.focus(); state.beginMutation(); await harness.act(() => state.setInstantiation(spec('focus-busy')));
  await new Promise(resolve => queueMicrotask(resolve));
  overlay = mounted.container.querySelector('.process-instantiate-modal');
  first = overlay.querySelector('[data-process-param-input="issue"]');
  await harness.act(() => harness.fireEvent(first, 'keydown', { key: 'Escape' }));
  assert.ok(state.instantiation.value, 'busy Escape cannot dismiss');
  await harness.act(() => harness.fireEvent(overlay, 'click'));
  assert.ok(state.instantiation.value, 'busy backdrop cannot dismiss');
  state.endMutation(); state.setInstantiation(null);
  await mounted.unmount(); invoker.remove();
});

test('a successful Worklist action supersedes an older poll with an authoritative refresh', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-actions.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  const item = { id: 'item-1', run: 'run-1', node: 'review', kind: 'review-needed', status: 'pending', summary: 'Review', availableActions: ['approve'] };
  state.worklistRequest.commitRequest(state.worklistRequest.beginRequest(), { items: [item], degradedRuns: [] });
  state.setDraft(item.id, 'approved');
  const oldPoll = deferred(); const freshPoll = deferred(); let gets = 0;
  const actions = createProcessesActions({ state, fetchImpl: async (path, options = {}) => {
    if (options.method === 'POST') return { ok: true, json: async () => ({}) };
    assert.equal(path, '/v1/process/worklist');
    gets += 1;
    return gets === 1 ? oldPoll.promise : freshPoll.promise;
  } });

  const stale = actions.load('worklist', { quiet: true });
  assert.equal(await actions.submitWorklistAction(item.id, 'approve'), true);
  assert.equal(gets, 2, 'the post-success refresh starts even while the old poll is pending');
  freshPoll.resolve({ ok: true, json: async () => ({ items: [], degradedRuns: [] }) });
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.deepEqual(state.view.value.worklist.items, [], 'the authoritative refresh removes the resolved item');

  oldPoll.resolve({ ok: true, json: async () => ({ items: [item], degradedRuns: [] }) });
  assert.equal(await stale, false, 'the superseded poll cannot commit');
  assert.deepEqual(state.view.value.worklist.items, [], 'the resolved item never becomes visible again');
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
  const first = { id: 'a', blank: false, key: 'a:1' };
  const mounted = await harness.mount(harness.html`<${ProcessEditorBoundary} spec=${first} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openEditor=${openEditor} />`);
  assert.equal(mounts, 1);
  assert.equal(received.id, 'a');
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
  harness.fireEvent(status.querySelector('.wl-link'), 'click');
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

test('canvas views retain their parent Processes subtab selection', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: prefs() });
  state.setSubtab('runs'); state.setCanvas({ kind: 'viewer', id: 'run-1', key: 'run-1' });
  const actions = {
    refreshActive() {}, load() {}, activateSubtab() {}, closeCanvas() {},
    loadRunView: async () => ({ run: { id: 'run-1' }, viewerV2: { routingAvailable: false, routingUnavailableReason: 'routing_absent' }, report: { nodes: {} } }),
  };
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
  assert.match(state.rename.value.error, /changed since the rename dialog opened/);
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
