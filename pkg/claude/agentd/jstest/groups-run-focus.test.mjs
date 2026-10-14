import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const FIXTURE = JSON.parse(readFileSync(new URL('./process-context-fixture.json', import.meta.url), 'utf8'));

const GROUPS = [
  { name: 'frontend-squad', members: [
    { conv_id: 'c1', agent_id: 'agt_fe_lead', title: 'fe-lead' },
    { conv_id: 'c2', agent_id: 'agt_fe_forms', title: 'fe-dev-forms' },
    { conv_id: 'c3', agent_id: 'agt_fe_charts', title: 'fe-dev-charts' },
  ] },
  { name: 'infra-crew', members: [{ conv_id: 'c4', agent_id: 'agt_infra_lead', title: 'infra-lead' }] },
];

const VIEWS = {
  run_release: { id: 'run_release', awaitingDecisions: [{ nodeId: 'review', verdicts: ['approved', 'changes'] }], blocked: [] },
  run_migrate: { id: 'run_migrate', awaitingDecisions: [], blocked: [{ nodeId: 'apply', attempt: 1, reason: 'migration lock held' }] },
  run_reindex: { id: 'run_reindex', awaitingDecisions: [], blocked: [] },
  run_hotfix: { id: 'run_hotfix', awaitingDecisions: [], blocked: [] },
};

const EVENTS = [
  { sequence: 1, occurredAt: '2026-10-14T10:00:00Z', kind: 'run_started' },
  { sequence: 2, occurredAt: '2026-10-14T11:50:00Z', nodeId: 'charts', kind: 'node_completed', actor: 'fe-dev-charts' },
  { sequence: 3, occurredAt: '2026-10-14T11:51:00Z', nodeId: 'review', kind: 'decision_awaited' },
];

function fakeSource(harness) {
  const source = {
    context: harness.signals.signal(FIXTURE), status: harness.signals.signal('ready'), error: harness.signals.signal(''), refreshes: 0,
  };
  source.refresh = () => { source.refreshes++; return Promise.resolve(); };
  return source;
}

function fakeApi({ fail = null } = {}) {
  const api = { decisions: [], eventReads: [] };
  api.getRun = async (runId) => VIEWS[runId];
  api.listEvents = async (runId, after = 0) => {
    api.eventReads.push([runId, after]);
    return { events: runId === 'run_release' ? EVENTS.filter((e) => e.sequence > after) : [], more: false };
  };
  api.decide = async (runId, request) => {
    if (fail) throw new Error(fail);
    api.decisions.push([runId, request]);
    return { started: true };
  };
  return api;
}

async function settle(harness) {
  for (let i = 0; i < 4; i++) await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
}

async function mountRunFocus(t, { runId = 'run_release', api = fakeApi() } = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-run-focus.js');
  const source = fakeSource(harness);
  const calls = { selected: [], closed: 0 };
  const run = harness.signals.signal(runId);
  const mounted = await harness.mount(harness.html`<${() => html(harness, mod, source, api, run, calls)} />`);
  await settle(harness);
  return { harness, mod, source, api, calls, run, root: mounted.container };
}

function html(harness, mod, source, api, run, calls) {
  return harness.html`<${mod.GroupsRunFocus} group=${GROUPS[0]} roster=${GROUPS[0]} context=${FIXTURE} runId=${run.value}
    source=${source} api=${api} onSelectRun=${(id) => { calls.selected.push(id); run.value = id; }} onClose=${() => { calls.closed++; }} />`;
}

test('a G3 card opens its run in the single-run focus', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-processes.js');
  const opened = [];
  const focused = harness.signals.signal(null);
  const source = fakeSource(harness);
  const api = fakeApi();
  const mounted = await harness.mount(harness.html`<${() => harness.html`<${mod.GroupsProcessesFocus} groups=${GROUPS} allGroups=${GROUPS}
    source=${source} filter="running" runApi=${api} focusedRun=${focused.value}
    onFocusRun=${(value) => { opened.push(value); focused.value = value; }} />`} />`);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-process-open="run_release"]'), 'click'));
  await settle(harness);
  assert.deepEqual(opened, [{ group: 'frontend-squad', runId: 'run_release' }]);
  assert.ok(mounted.container.querySelector('[data-run-focus="run_release"]'), 'the focus replaces the cards');
  assert.equal(mounted.container.querySelector('[data-process-run-card]'), null);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-run-focus-close]'), 'click'));
  assert.deepEqual(opened.at(-1), null);
  assert.ok(mounted.container.querySelector('[data-process-run-card="run_release"]'), 'back lands on the cards');
});

test('the run list shows live, planned and recent runs; picking one selects it', async (t) => {
  const { harness, root, calls } = await mountRunFocus(t);
  const items = [...root.querySelectorAll('[data-run-list-item]')];
  assert.deepEqual(items.map((b) => b.dataset.runListItem), ['run_migrate', 'run_release', 'run_reindex', 'run_hotfix']);
  assert.equal(root.querySelector('[data-run-list-item="run_release"]').getAttribute('aria-current'), 'true');
  assert.match(root.querySelector('[data-run-list-item="run_release"]').textContent, /needs you/);
  assert.ok(root.querySelector('[data-run-planned="cron_nightly"]'));
  await harness.act(() => harness.fireEvent(root.querySelector('[data-run-list-item="run_reindex"]'), 'click'));
  await settle(harness);
  assert.deepEqual(calls.selected, ['run_reindex']);
  assert.ok(root.querySelector('[data-run-focus="run_reindex"]'));
  assert.equal(root.querySelector('[data-run-list-item="run_reindex"]').getAttribute('aria-current'), 'true');
});

test('the inspector opens on the gate waiting on you and follows node selection', async (t) => {
  const { harness, root } = await mountRunFocus(t);
  const inspector = () => root.querySelector('[data-run-inspector]');
  assert.equal(inspector().dataset.runInspector, 'review', 'the awaited gate is selected by default');
  assert.equal(root.querySelector('[data-process-node="review"]').getAttribute('aria-pressed'), 'true');
  assert.ok(root.querySelector('[data-process-node="review"]').classList.contains('selected'));
  assert.match(inspector().querySelector('.grf-node-title').textContent, /Review gate/);
  assert.match(inspector().textContent, /waiting on you/);
  const facts = Object.fromEntries([...inspector().querySelectorAll('dt')].map((dt) => [dt.textContent, dt.nextElementSibling.textContent]));
  assert.deepEqual(facts, { performer: 'any operator', upstream: 'Implement charts ✓', downstream: 'QA suite' });
  assert.deepEqual([...inspector().querySelectorAll('[data-run-verdict]')].map((b) => b.textContent), ['✓ approve', '↺ request changes']);
  assert.deepEqual([...inspector().querySelectorAll('.grf-history li')].map((li) => li.textContent.slice(li.querySelector('time').textContent.length).trim()), ['decision awaited']);

  await harness.act(() => harness.fireEvent(root.querySelector('[data-process-node="checkout"]'), 'click'));
  assert.equal(inspector().dataset.runInspector, 'checkout');
  assert.equal(root.querySelector('[data-process-node="review"]').getAttribute('aria-pressed'), 'false');
  assert.equal(inspector().querySelector('[data-run-verdict]'), null, 'only an awaited gate takes a verdict');
  assert.match(inspector().querySelector('dd').textContent, /fe-dev-forms/);
});

test('a blocked node shows why it is parked', async (t) => {
  const { harness, root } = await mountRunFocus(t, { runId: 'run_migrate' });
  await harness.act(() => harness.fireEvent(root.querySelector('[data-process-node="apply"]'), 'click'));
  assert.match(root.querySelector('.grf-blocked').textContent, /migration lock held/);
});

test('approve / request changes posts the verdict with the note and refreshes', async (t) => {
  const { harness, root, api, source } = await mountRunFocus(t);
  const note = root.querySelector('.grf-decide textarea');
  Object.defineProperty(note, 'value', { configurable: true, writable: true, value: '  tighten the copy ' });
  await harness.act(() => harness.fireEvent(note, 'input'));
  const before = source.refreshes;
  await harness.act(() => harness.fireEvent(root.querySelector('[data-run-verdict="changes"]'), 'click'));
  await settle(harness);
  assert.deepEqual(api.decisions, [['run_release', { nodeId: 'review', verdict: 'changes', evidence: 'tighten the copy', attempt: 0 }]]);
  assert.equal(source.refreshes, before + 1, 'the context refreshes so the cards and chips follow');
  assert.ok(api.eventReads.some(([runId, after]) => runId === 'run_release' && after === 3), 'later reads continue from the last event');
});

test('a refused verdict stays on screen as an error', async (t) => {
  const { harness, root } = await mountRunFocus(t, { api: fakeApi({ fail: 'decision window is stale' }) });
  await harness.act(() => harness.fireEvent(root.querySelector('[data-run-verdict="approved"]'), 'click'));
  await settle(harness);
  assert.match(root.querySelector('.grf-decide [role="alert"]').textContent, /decision window is stale/);
});

test('the performer dock lists the group and says drag-assign is not available yet', async (t) => {
  const { root } = await mountRunFocus(t);
  const agents = [...root.querySelectorAll('[data-run-dock-agent]')];
  assert.deepEqual(agents.map((a) => a.textContent.slice(1)), ['fe-lead', 'fe-dev-forms', 'fe-dev-charts']);
  for (const agent of agents) {
    assert.notEqual(agent.getAttribute('draggable'), 'true');
    assert.equal(agent.getAttribute('aria-disabled'), 'true');
  }
  assert.ok(root.querySelector('[data-run-dock-agent="agt_fe_forms"]').classList.contains('busy'));
  assert.match(root.querySelector('.grf-dock-note').textContent, /drag-assign arrives with run reassignment/);
});

test('Escape and the back button close the focus; Escape in the note does not', async (t) => {
  const { harness, root, calls } = await mountRunFocus(t);
  await harness.act(() => harness.fireEvent(root.querySelector('.grf-decide textarea'), 'keydown', { key: 'Escape' }));
  assert.equal(calls.closed, 0);
  await harness.act(() => harness.fireEvent(root.querySelector('[data-run-list-item="run_release"]'), 'keydown', { key: 'Escape' }));
  assert.equal(calls.closed, 1);
  await harness.act(() => harness.fireEvent(getByRole(root, 'button', { name: '← all runs' }), 'click'));
  assert.equal(calls.closed, 2);
});

test('a run that left the context says so', async (t) => {
  const { root } = await mountRunFocus(t, { runId: 'run_gone' });
  assert.match(root.querySelector('.grf-gone').textContent, /no longer in the process context/);
});

test('the run detail API pages events and posts an idempotent decision', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/process-context.js');
  const calls = [];
  const pages = {
    '/v1/process/runs/run%2F1/events': { events: [{ sequence: 1 }], next: 1 },
    '/v1/process/runs/run%2F1/events?after=1': { events: [{ sequence: 2 }], next: 0 },
  };
  const api = mod.createRunDetailApi({ fetchImpl: async (path, init) => {
    calls.push({ path, init });
    return { ok: true, status: 200, json: async () => pages[path] || { id: 'run/1' } };
  } });
  assert.deepEqual(await api.getRun('run/1'), { id: 'run/1' });
  assert.deepEqual(await api.listEvents('run/1'), { events: [{ sequence: 1 }, { sequence: 2 }], more: false });
  await api.decide('run/1', { nodeId: 'review', verdict: 'approved', attempt: 2, attemptKey: '11111111-2222-4333-8444-555555555555' });
  const post = calls.at(-1);
  assert.equal(post.path, '/v1/process/runs/run%2F1/decide');
  assert.equal(post.init.method, 'POST');
  assert.deepEqual(JSON.parse(post.init.body), { nodeId: 'review', verdict: 'approved', attempt: 2 });
  assert.equal(post.init.headers['Idempotency-Key'], '11111111-2222-4333-8444-555555555555');
});

test('verdict labels read as approve / request changes, anything else verbatim', async (t) => {
  const harness = await createPreactHarness(t);
  const { verdictPresentation } = await harness.importDashboardModule('js/groups-run-focus.js');
  assert.deepEqual(verdictPresentation('approved'), { label: '✓ approve', cls: 'primary' });
  assert.deepEqual(verdictPresentation('changes_requested'), { label: '↺ request changes', cls: '' });
  assert.deepEqual(verdictPresentation('escalate'), { label: 'escalate', cls: '' });
});

test('a member badge opens its run; a header chip goes back to the cards', async (t) => {
  const harness = await createPreactHarness(t);
  const [indicators, { registerFeatureState }, { createGroupsState }] = await Promise.all([
    harness.importDashboardModule('js/groups-process-indicators.js'),
    harness.importDashboardModule('js/feature-state-registry.js'),
    harness.importDashboardModule('js/groups-state.js'),
  ]);
  const prefs = new Map();
  const state = createGroupsState({
    prefs: { getItem: (k) => prefs.get(k) ?? null, setItem: (k, v) => prefs.set(k, v), removeItem: (k) => prefs.delete(k) },
    resetOffsets: () => {}, reorder: (g) => g,
    columns: { list: () => [], hidden: () => false, setHidden: () => {}, deviationCount: () => 0 },
  });
  t.after(registerFeatureState('groups', state));
  const source = fakeSource(harness);
  const on = { processes_enabled: true };
  const mounted = await harness.mount(harness.html`<div>
    <${indicators.MemberProcessCell} member=${{ agent_id: 'agt_fe_lead' }} group=${GROUPS[0]} snapshot=${on} source=${source} />
    <${indicators.GroupProcessChips} group=${GROUPS[0]} snapshot=${on} source=${source} />
  </div>`);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('.pk-run-badge'), 'click'));
  assert.equal(state.focus.value, 'processes');
  assert.deepEqual(state.focusedRun.value, { group: 'frontend-squad', runId: 'run_release' });
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-group-processes="running"]'), 'click'));
  assert.equal(state.focusedRun.value, null);
});
