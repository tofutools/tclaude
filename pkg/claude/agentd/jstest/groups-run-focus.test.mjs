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
  assert.equal(harness.document.activeElement, mounted.container.querySelector('[data-process-open="run_release"]'),
    'focus returns to the card that opened the run');

  await harness.act(() => { focused.value = { group: 'gone-squad', runId: 'run_x' }; });
  await settle(harness);
  assert.equal(focused.value, null, 'a run whose group is gone closes instead of lingering');
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

test('a program step shows where its attempt ran', async (t) => {
  const { harness, root } = await mountRunFocus(t, { runId: 'run_reindex' });
  await harness.act(() => harness.fireEvent(root.querySelector('[data-process-node="snapshot"]'), 'click'));
  const bounds = root.querySelector('[data-run-inspector="snapshot"] [data-run-bounds]');
  assert.equal(bounds.textContent, 'in webshop-charts · 🛡 sandbox dev-net');
  assert.equal(bounds.getAttribute('title'), '/home/dev/git/webshop-charts');
  await harness.act(() => harness.fireEvent(root.querySelector('[data-process-node="verify"]'), 'click'));
  assert.equal(root.querySelector('[data-run-inspector="verify"] [data-run-bounds]'), null, 'not planned yet: nothing recorded');
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
  assert.ok([...root.querySelectorAll('[data-run-verdict]')].every((b) => b.disabled), 'the gate stays shut until the view drops the decision');
  assert.match(root.querySelector('.grf-decide [role="status"]').textContent, /Recorded: ↺ request changes/);
  await harness.act(() => harness.fireEvent(root.querySelector('[data-run-verdict="approved"]'), 'click'));
  await settle(harness);
  assert.equal(api.decisions.length, 1, 'no second decide');
});

test('polls during a slow read queue one follow-up instead of discarding it, and long histories catch up', async (t) => {
  const api = fakeApi();
  const gates = [];
  api.getRun = (runId) => new Promise((resolve) => gates.push(() => resolve(VIEWS[runId])));
  let pages = 0;
  api.listEvents = async (runId, after = 0) => {
    api.eventReads.push([runId, after]);
    pages++;
    return pages === 1 ? { events: EVENTS.slice(0, 2), more: true } : { events: EVENTS.filter((e) => e.sequence > after), more: false };
  };
  const { harness, root } = await mountRunFocus(t, { api });
  for (let i = 0; i < 3; i++) await harness.act(() => harness.document.dispatchEvent(new harness.window.Event('tclaude:snapshot')));
  assert.equal(gates.length, 1, 'one read in flight however many polls land');
  await harness.act(async () => { gates.shift()(); });
  await settle(harness);
  assert.match(root.textContent, /Reading the rest of this run's history/);
  assert.equal(gates.length, 1, 'catch-up (and the queued poll) start exactly one more read');
  await harness.act(async () => { gates.shift()(); });
  await settle(harness);
  assert.deepEqual(api.eventReads.map(([, after]) => after), [0, 2]);
  assert.doesNotMatch(root.textContent, /Reading the rest/);
  assert.equal(root.querySelector('[data-run-inspector]').dataset.runInspector, 'review');
});

test('a refused verdict stays on screen as an error', async (t) => {
  const { harness, root } = await mountRunFocus(t, { api: fakeApi({ fail: 'decision window is stale' }) });
  await harness.act(() => harness.fireEvent(root.querySelector('[data-run-verdict="approved"]'), 'click'));
  await settle(harness);
  assert.match(root.querySelector('.grf-decide [role="alert"]').textContent, /decision window is stale/);
});

// withDispatch gives run_release's running checkout node an outstanding
// attempt delivered to fe-dev-forms, as performer dispatch reports it.
function withDispatch(dispatch = { nodeId: 'checkout', agentId: 'agt_fe_forms', attempt: 1, state: 'delivered' }) {
  const context = structuredClone(FIXTURE);
  context.runs.run_release.nodes = context.runs.run_release.nodes.map((node) => (node.id === 'checkout' ? { ...node, dispatch } : node));
  return context;
}

function assigningApi({ fail = null } = {}) {
  const api = fakeApi();
  api.assignments = [];
  api.assign = async (runId, request) => {
    if (fail) throw new Error(fail);
    api.assignments.push([runId, request.nodeId, request.agent]);
    return { runId };
  };
  return api;
}

async function mountWithContext(t, context, api) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-run-focus.js');
  const source = fakeSource(harness);
  const mounted = await harness.mount(harness.html`<${mod.GroupsRunFocus} group=${GROUPS[0]} roster=${GROUPS[0]} context=${context}
    runId="run_release" source=${source} api=${api} onSelectRun=${() => {}} onClose=${() => {}} />`);
  await settle(harness);
  return { harness, mod, source, root: mounted.container };
}

function dragEvent(harness, type, data) {
  const store = new Map(Object.entries(data));
  return { dataTransfer: { types: [...store.keys()], getData: (key) => store.get(key) || '', setData: (key, value) => store.set(key, value), dropEffect: '', effectAllowed: '' } };
}

test('the performer dock lists the group as draggable performers', async (t) => {
  const { root } = await mountRunFocus(t);
  const agents = [...root.querySelectorAll('[data-run-dock-agent]')];
  assert.deepEqual(agents.map((a) => a.textContent.slice(1)), ['fe-lead', 'fe-dev-forms', 'fe-dev-charts']);
  for (const agent of agents) {
    assert.ok(agent.hasAttribute('draggable') && agent.getAttribute('draggable') !== 'false', 'dock agents are draggable');
    assert.equal(agent.getAttribute('aria-disabled'), null);
  }
  assert.ok(root.querySelector('[data-run-dock-agent="agt_fe_forms"]').classList.contains('busy'));
  assert.match(root.querySelector('.grf-dock-note').textContent, /drag onto an agent node to reassign/);
});

test('dropping a dock agent onto an outstanding agent node reassigns it', async (t) => {
  const api = assigningApi();
  const { harness, mod, source, root } = await mountWithContext(t, withDispatch(), api);
  const lead = root.querySelector('[data-run-dock-agent="agt_fe_lead"]');
  const started = dragEvent(harness, 'dragstart', {});
  harness.fireEvent(lead, 'dragstart', started);
  assert.equal(started.dataTransfer.getData(mod.DOCK_AGENT_TYPE), 'agt_fe_lead');

  const checkout = root.querySelector('[data-process-node="checkout"]');
  const over = harness.fireEvent(checkout, 'dragover', dragEvent(harness, 'dragover', { [mod.DOCK_AGENT_TYPE]: 'agt_fe_lead' }));
  assert.ok(over.defaultPrevented, 'an outstanding agent node accepts the drop');
  const done = root.querySelector('[data-process-node="charts"]');
  const refused = harness.fireEvent(done, 'dragover', dragEvent(harness, 'dragover', { [mod.DOCK_AGENT_TYPE]: 'agt_fe_lead' }));
  assert.ok(!refused.defaultPrevented, 'a node with nothing outstanding refuses it');

  await harness.act(async () => { harness.fireEvent(checkout, 'drop', dragEvent(harness, 'drop', { [mod.DOCK_AGENT_TYPE]: 'agt_fe_lead' })); });
  await settle(harness);
  assert.deepEqual(api.assignments, [['run_release', 'checkout', 'agt_fe_lead']]);
  assert.ok(source.refreshes > 0, 'the context refreshes after a reassign');
  assert.equal(root.querySelector('[data-run-inspector]').dataset.runInspector, 'checkout', 'the dropped-on node is selected');

  // Dropping the current performer back on its node is a no-op.
  await harness.act(async () => { harness.fireEvent(checkout, 'drop', dragEvent(harness, 'drop', { [mod.DOCK_AGENT_TYPE]: 'agt_fe_forms' })); });
  await settle(harness);
  assert.equal(api.assignments.length, 1);
});

test('the inspector reassigns by keyboard and addresses the stage', async (t) => {
  const api = assigningApi();
  const { harness, root } = await mountWithContext(t, withDispatch({ nodeId: 'checkout.review', agentId: 'agt_fe_forms', attempt: 2, state: 'delivered' }), api);
  await harness.act(() => harness.fireEvent(root.querySelector('[data-process-node="checkout"]'), 'click'));
  const form = root.querySelector('[data-run-reassign="checkout"]');
  assert.ok(form);
  const select = form.querySelector('select');
  assert.deepEqual([...select.querySelectorAll('option')].map((o) => o.value), ['agt_fe_lead', 'agt_fe_charts'], 'the current performer is not offered');
  for (const option of select.querySelectorAll('option')) option.selected = option.value === 'agt_fe_charts';
  await harness.act(() => harness.fireEvent(select, 'change'));
  await harness.act(async () => { harness.fireEvent(form.querySelector('button'), 'click'); });
  await settle(harness);
  assert.deepEqual(api.assignments, [['run_release', 'checkout.review', 'agt_fe_charts']]);
});

test('a refused reassign is shown, and a node with nothing outstanding offers none', async (t) => {
  const refusing = assigningApi({ fail: 'the current performer is still being spawned' });
  const second = await mountWithContext(t, withDispatch(), refusing);
  await second.harness.act(() => second.harness.fireEvent(second.root.querySelector('[data-process-node="checkout"]'), 'click'));
  await second.harness.act(async () => { second.harness.fireEvent(second.root.querySelector('[data-run-reassign] button'), 'click'); });
  await settle(second.harness);
  assert.match(second.root.querySelector('[data-run-reassign] [role="alert"]').textContent, /still being spawned/);

});

test('without an outstanding attempt the inspector offers no reassign', async (t) => {
  const none = await mountWithContext(t, FIXTURE, assigningApi());
  await none.harness.act(() => none.harness.fireEvent(none.root.querySelector('[data-process-node="checkout"]'), 'click'));
  assert.equal(none.root.querySelector('[data-run-reassign]'), null);
});

test('Escape and the back button close the focus; Escape in the note does not', async (t) => {
  const { harness, root, calls } = await mountRunFocus(t);
  assert.equal(harness.document.activeElement, root.querySelector('[data-run-focus-close]'), 'opening lands focus on the back button');
  await harness.act(() => harness.fireEvent(root.querySelector('.grf-decide textarea'), 'keydown', { key: 'Escape' }));
  assert.equal(calls.closed, 0);
  await harness.act(() => harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' }));
  assert.equal(calls.closed, 1, 'Esc works with focus anywhere outside a text field');
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
  await api.assign('run/1', { nodeId: 'work.do', agent: 'agt_fe_lead', attemptKey: '11111111-2222-4333-8444-666666666666' });
  const assign = calls.at(-1);
  assert.equal(assign.path, '/v1/process/runs/run%2F1/nodes/work.do/assign');
  assert.deepEqual(JSON.parse(assign.init.body), { agent: 'agt_fe_lead' });
  assert.equal(assign.init.headers['Idempotency-Key'], '11111111-2222-4333-8444-666666666666');
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

test('the focus header pauses a live run', async (t) => {
  const api = fakeApi();
  api.paused = [];
  api.pause = async (runId) => { api.paused.push(['pause', runId]); return {}; };
  api.resume = async (runId) => { api.paused.push(['resume', runId]); return {}; };
  const { harness, source, root } = await mountWithContext(t, structuredClone(FIXTURE), api);
  assert.ok(!root.querySelector('[data-process-paused]'), 'a live run carries no paused chip');
  const pause = [...root.querySelectorAll('.grf-run-actions button')].find((b) => /pause run/.test(b.textContent));
  assert.ok(pause && !pause.disabled);
  const before = source.refreshes;
  await harness.act(async () => { harness.fireEvent(pause, 'click'); });
  await settle(harness);
  assert.deepEqual(api.paused, [['pause', 'run_release']]);
  assert.ok(source.refreshes > before, 'the context refreshes after a pause');
});

test('a paused run shows its chip and resumes from the focus header', async (t) => {
  const api = fakeApi();
  api.paused = [];
  api.resume = async (runId) => { api.paused.push(['resume', runId]); return {}; };
  const paused = structuredClone(FIXTURE);
  paused.runs.run_release.paused = { at: '2026-10-14T12:05:00Z', by: 'human:operator' };
  const held = await mountWithContext(t, paused, api);
  assert.match(held.root.querySelector('[data-process-paused]').textContent, /paused/);
  const resume = [...held.root.querySelectorAll('.grf-run-actions button')].find((b) => /resume run/.test(b.textContent));
  await held.harness.act(async () => { held.harness.fireEvent(resume, 'click'); });
  await settle(held.harness);
  assert.deepEqual(api.paused.at(-1), ['resume', 'run_release']);
});

test('the run detail API posts idempotent pause and resume', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/process-context.js');
  const calls = [];
  const api = mod.createRunDetailApi({ fetchImpl: async (path, init) => {
    calls.push({ path, init });
    return { ok: true, status: 200, json: async () => ({}) };
  } });
  await api.pause('run/1', { attemptKey: '11111111-2222-4333-8444-777777777777' });
  assert.equal(calls.at(-1).path, '/v1/process/runs/run%2F1/pause');
  assert.equal(calls.at(-1).init.method, 'POST');
  assert.equal(calls.at(-1).init.body, '{}');
  assert.equal(calls.at(-1).init.headers['Idempotency-Key'], '11111111-2222-4333-8444-777777777777');
  await api.resume('run/1');
  assert.equal(calls.at(-1).path, '/v1/process/runs/run%2F1/resume');
});

test('the run graph scales to fit its pane, down to a readable floor', async (t) => {
  const harness = await createPreactHarness(t);
  const { graphFitScale } = await harness.importDashboardModule('js/groups-run-focus.js');
  assert.equal(graphFitScale({ width: 400, height: 200 }, { width: 900, height: 500 }), 1, 'a small graph is never enlarged');
  assert.equal(graphFitScale({ width: 1000, height: 200 }, { width: 948, height: 500 }), 0.9);
  assert.equal(graphFitScale({ width: 4000, height: 200 }, { width: 948, height: 500 }), 0.8, 'past the floor the pane scrolls');
  assert.equal(graphFitScale(null, { width: 900, height: 500 }), 1);
});
