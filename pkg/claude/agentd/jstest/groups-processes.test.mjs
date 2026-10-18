import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const FIXTURE = JSON.parse(readFileSync(new URL('./process-context-fixture.json', import.meta.url), 'utf8'));

const GROUPS = [
  { name: 'frontend-squad', members: [
    { conv_id: 'c1', agent_id: 'agt_fe_lead', title: 'fe-lead', online: true, state: { status: 'working' } },
    { conv_id: 'c2', agent_id: 'agt_fe_forms', title: 'fe-dev-forms' },
    { conv_id: 'c3', agent_id: 'agt_fe_charts', title: 'fe-dev-charts' },
  ] },
  { name: 'infra-crew', members: [{ conv_id: 'c4', agent_id: 'agt_infra_lead', title: 'infra-lead' }] },
  { name: 'Ungrouped', virtual: true, members: [] },
];

function fakeSource(harness, { status = 'ready', context = FIXTURE, error = '' } = {}) {
  const source = {
    context: harness.signals.signal(context), status: harness.signals.signal(status), error: harness.signals.signal(error),
    refreshes: 0,
  };
  source.refresh = () => { source.refreshes++; return Promise.resolve(); };
  return source;
}

async function settle(harness) {
  for (let i = 0; i < 3; i++) await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
}

// mountFocus renders the Processes perspective as the Groups tab does: the
// shared groups tree (groups-list.js) inside the focus frame.
async function mountFocus(t, props = {}) {
  const harness = await createPreactHarness(t);
  const [mod, list, interactions] = await Promise.all([
    harness.importDashboardModule('js/groups-processes.js'),
    harness.importDashboardModule('js/groups-list.js'),
    harness.importDashboardModule('js/groups-interactions.js'),
  ]);
  const source = props.source || fakeSource(harness, props.sourceOptions);
  const groups = props.groups || GROUPS;
  const allGroups = props.allGroups || groups;
  const snapshot = { processes_enabled: true, groups: allGroups.filter((group) => !group.virtual) };
  const mounted = await harness.mount(harness.html`<${interactions.GroupsInteractionProvider}>
    <${mod.GroupsProcessesFocus} groups=${groups} allGroups=${allGroups} source=${source}
      filter=${props.filter || 'running'} api=${props.api} wizard=${props.wizard || false}
      focusedRun=${props.focusedRun || null} onFocusRun=${props.onFocusRun || (() => {})} fused=${props.fused || false}>
      <${list.GroupsNativeList} groups=${groups} snapshot=${snapshot} actions=${{}} />
    <//>
  <//>`);
  const group = (name) => mounted.container.querySelector(`[data-process-group="${name}"]`);
  const details = (name) => mounted.container.querySelector(`details[data-group-key="${name}"]`);
  return { harness, mod, source, mounted, root: mounted.container, group, details };
}

test('Running shows each group\'s live runs as graph cards, needs-you first, and an empty group says so', async (t) => {
  const { root, group, source } = await mountFocus(t);
  assert.ok(source.refreshes >= 1, 'mounting the lens loads the context');
  const fe = group('frontend-squad');
  assert.deepEqual([...fe.querySelectorAll('[data-process-run-card]')].map((c) => c.dataset.processRunCard), ['run_migrate', 'run_release', 'run_reindex']);
  assert.equal(fe.querySelectorAll('.gp-needs-you').length, 2);
  const release = fe.querySelector('[data-process-run-card="run_release"]');
  assert.equal(release.querySelector('.gp-card-name').textContent, 'release-2.4');
  assert.match(release.querySelector('.pk-chip.pk-state-waiting').textContent, /^blocked on human · /);
  assert.equal(release.querySelector('.pk-scope').textContent, '⌂ group scope · frontend-squad');
  assert.match(release.querySelector('.gp-card-meta').textContent, /template release-train v7 · owner fe-lead/);
  assert.ok(release.querySelector('.pk-mini-graph [data-process-node="qa"].pk-kind-subprocess'), 'composite nodes render stacked');
  const avatar = release.querySelector('[data-process-node="checkout"] .pk-avatar');
  assert.equal(avatar.textContent, 'F');
  assert.equal(avatar.getAttribute('title'), 'fe-dev-forms');
  const reindex = fe.querySelector('[data-process-run-card="run_reindex"]');
  assert.equal(reindex.querySelector('.pk-scope').textContent, '◉ agent scope · fe-dev-charts');
  const migrate = fe.querySelector('[data-process-run-card="run_migrate"]');
  assert.ok(migrate.querySelector('[data-process-node="apply"].pk-state-waiting'), 'a blocked node reads as blocked on human');

  assert.equal(fe.querySelector('.gp-strip-text').textContent, '2 in processes · 1 working · 0 idle');
  assert.equal(fe.querySelector('.gp-member.busy').getAttribute('title'), '2 performing a process step — fe-dev-forms, fe-dev-charts');
  assert.equal(fe.querySelector('.gp-member.busy .gp-member-count').textContent, '2');
  assert.equal(fe.querySelector('.gp-member.is-working .gp-member-count'), null, 'a lone member gets no count');
  // Processes run elsewhere, so the empty group folds Get started to a line.
  assert.ok(group('infra-crew').querySelector('[data-quickstart-folded="infra-crew"]'), 'an empty group folds Get started once processes exist');
  assert.equal(group('infra-crew').querySelector('[data-quickstart]'), null);
  assert.equal(group('Ungrouped').querySelector('.gp-empty').textContent, 'No ungrouped agent runs a process.');
  assert.equal(root.querySelector('.gp-toolbar'), null, 'no filter row of its own: the checklist lives on the toolbar icon');
});

test('the ticked run kinds pick the matching runs', async (t) => {
  for (const [filter, cards, planned] of [
    ['done', ['run_hotfix'], []],
    ['planned', [], ['cron_nightly']],
    ['running,planned', ['run_migrate', 'run_release', 'run_reindex'], ['cron_nightly']],
    ['planned,done', ['run_hotfix'], ['cron_nightly']],
    ['running,planned,done', ['run_migrate', 'run_release', 'run_reindex', 'run_hotfix'], ['cron_nightly']],
    ['all', ['run_migrate', 'run_release', 'run_reindex', 'run_hotfix'], ['cron_nightly']],
  ]) {
    await t.test(filter, async (t2) => {
      const { group } = await mountFocus(t2, { filter });
      const fe = group('frontend-squad');
      assert.deepEqual([...fe.querySelectorAll('[data-process-run-card]')].map((c) => c.dataset.processRunCard), cards);
      assert.deepEqual([...fe.querySelectorAll('[data-process-planned]')].map((c) => c.dataset.processPlanned), planned);
    });
  }
  // A group with runs, none of the ticked kinds, says so; a group with no
  // runs at all offers Get started instead.
  const onlyRunning = { ...FIXTURE, groups: FIXTURE.groups.map((entry) => (entry.group === 'frontend-squad' ? { ...entry, done: [], planned: [] } : entry)) };
  const { group } = await mountFocus(t, { filter: 'planned,done', sourceOptions: { context: onlyRunning } });
  assert.equal(group('frontend-squad').querySelector('.gp-empty:not(.gq-folded)').textContent, 'No planned or done processes.');
  assert.ok(group('infra-crew').querySelector('[data-quickstart-folded]'));
});

test('an agentd without the read model shows the placeholder', async (t) => {
  const missing = await mountFocus(t, { sourceOptions: { status: 'unavailable', context: null } });
  assert.ok(missing.root.querySelector('[data-groups-processes-placeholder]'));
  assert.equal(missing.root.querySelector('[data-process-group]'), null);
});

test('a failed first load shows the error', async (t) => {
  const failed = await mountFocus(t, { sourceOptions: { status: 'error', context: null, error: '403 forbidden' } });
  assert.match(failed.root.querySelector('[role="alert"]').textContent, /403 forbidden/);
});

test('+ start process starts a run scoped to the group and refreshes', async (t) => {
  const started = [];
  let fail = false;
  const api = {
    listTemplates: async () => [{ id: 'tpl_a', name: 'Alpha' }, { id: 'tpl_b', name: 'Beta' }],
    startGroupRun: async (request) => { if (fail) throw new Error('template needs params'); started.push(request); return { runId: 'run_new' }; },
  };
  const { harness, group, source } = await mountFocus(t, { api });
  const fe = group('frontend-squad');
  await harness.act(() => harness.fireEvent(fe.querySelector('[data-process-start="frontend-squad"]'), 'click'));
  await settle(harness);
  const form = fe.querySelector('form.gp-start-form');
  assert.equal(form.getAttribute('aria-label'), 'Start a process in frontend-squad');
  assert.match(form.textContent, /group scope · frontend-squad/);
  const select = form.querySelector('select[aria-label="Template"]');
  assert.deepEqual([...select.options].map((o) => o.textContent), ['Alpha', 'Beta']);
  Object.defineProperty(select, 'value', { configurable: true, writable: true, value: 'tpl_b' });
  await harness.act(() => harness.fireEvent(select, 'change'));

  fail = true;
  await harness.act(() => harness.fireEvent(form, 'submit'));
  await settle(harness);
  assert.match(form.querySelector('[role="alert"]').textContent, /template needs params/);
  assert.deepEqual(started, []);

  fail = false;
  const before = source.refreshes;
  await harness.act(() => harness.fireEvent(form, 'submit'));
  await settle(harness);
  assert.deepEqual(started, [{ templateId: 'tpl_b', group: 'frontend-squad', params: {}, authorizeProgramProfiles: [] }]);
  assert.equal(source.refreshes, before + 1);
  assert.equal(fe.querySelector('form'), null, 'the form closes after a start');
});

test('the context source maps 404 to unavailable and keeps the last good context on errors', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/process-context.js');
  const replies = [
    { status: 404, ok: false, json: async () => ({}) },
    { status: 200, ok: true, json: async () => FIXTURE },
    { status: 500, ok: false, statusText: 'Internal', json: async () => ({ error: 'boom' }) },
  ];
  const calls = [];
  const source = mod.createProcessContextSource({ fetchImpl: async (path) => { calls.push(path); return replies.shift(); } });
  await source.refresh();
  assert.equal(source.status.value, 'unavailable');
  await source.refresh();
  assert.equal(source.status.value, 'ready');
  assert.equal(source.context.value.runs.run_release.name, 'release-2.4');
  await source.refresh();
  assert.equal(source.status.value, 'ready', 'a later failure keeps the last good runs on screen');
  assert.equal(source.error.value, 'boom');
  assert.deepEqual(calls, ['/v1/process/context', '/v1/process/context', '/v1/process/context']);

  const now = Date.parse('2026-10-14T12:04:00Z');
  assert.equal(mod.formatRunDuration('2026-10-14T10:00:00Z', now), '2h 04m');
  assert.equal(mod.formatRunDuration('2026-10-14T11:23:00Z', now), '41m');
  assert.equal(mod.formatRunDuration('2026-10-14T12:03:50Z', now), '<1m');
  assert.equal(mod.formatRunDuration('2026-10-12T10:00:00Z', now), '2d 2h');
  assert.equal(mod.formatRunDuration('', now), '');
});

test('a Groups search narrows the groups shown, not the busy strip or performer names', async (t) => {
  const filtered = [{ name: 'frontend-squad', members: [GROUPS[0].members[0]] }];
  const { group } = await mountFocus(t, { groups: filtered, allGroups: GROUPS });
  const fe = group('frontend-squad');
  assert.equal(fe.querySelector('.gp-strip-text').textContent, '2 in processes · 1 working · 0 idle');
  assert.equal(fe.querySelector('[data-process-node="checkout"] .pk-avatar').getAttribute('title'), 'fe-dev-forms');
});

test('starting a run posts the group with idempotency headers', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/process-context.js');
  const calls = [];
  const api = mod.createProcessRunApi({ fetchImpl: async (path, init) => { calls.push({ path, init }); return { ok: true, status: 201, json: async () => ({ runId: 'run_new' }) }; } });
  assert.deepEqual(await api.startGroupRun({ templateId: 'tpl_a', group: 'frontend-squad', attemptKey: '11111111-2222-4333-8444-555555555555' }), { runId: 'run_new' });
  const [{ path, init }] = calls;
  assert.equal(path, '/v1/process/runs');
  assert.equal(init.method, 'POST');
  assert.deepEqual(JSON.parse(init.body), { templateId: 'tpl_a', group: 'frontend-squad', authorizeProgramProfiles: [] });
  assert.equal(init.headers['Idempotency-Key'], '11111111-2222-4333-8444-555555555555');
  assert.match(init.headers['X-Tclaude-Request-Digest'], /^[0-9a-f]{64}$/);
});

test('the run api saves, publishes and pins templates with group metadata outside the template', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/process-context.js');
  const calls = [];
  const api = mod.createProcessRunApi({ fetchImpl: async (path, init = {}) => { calls.push({ path, init }); return { ok: true, status: 200, json: async () => ({ id: 'tpl_new' }) }; } });
  const view = { template: { id: 'draft', name: 'triage', nodes: {} }, edges: [], layout: { nodes: {} } };
  await api.createTemplate(view, { listed: false, pinGroup: 'web shop' });
  await api.createTemplate(view);
  await api.saveTemplate('tpl_a', view, { sourceHash: 'h1' });
  await api.publishTemplate('tpl_a');
  await api.pinProcess('web shop', 'tpl_a');
  await api.unpinProcess('web shop', 'tpl_a');
  await api.getTemplate('tpl_a');
  const bodies = calls.map(({ init }) => (init.body ? JSON.parse(init.body) : null));
  assert.deepEqual(calls.map(({ path, init }) => `${init.method || 'GET'} ${path}`), [
    'POST /v1/process/templates', 'POST /v1/process/templates', 'POST /v1/process/templates/tpl_a',
    'POST /v1/process/templates/tpl_a/listing', 'POST /v1/process/groups/web%20shop/processes',
    'DELETE /v1/process/groups/web%20shop/processes/tpl_a', 'GET /v1/process/templates/tpl_a',
  ]);
  assert.deepEqual(bodies[0], { template: { name: 'triage', nodes: {} }, edges: [], layout: { nodes: {} }, listed: false, pinGroup: 'web shop' });
  assert.deepEqual(bodies[1], { template: { name: 'triage', nodes: {} }, edges: [], layout: { nodes: {} } }, 'no metadata unless asked');
  assert.deepEqual(bodies[2], { template: { id: 'tpl_a', name: 'triage', nodes: {} }, edges: [], layout: { nodes: {} }, sourceHash: 'h1' });
  assert.deepEqual(bodies[3], { listed: true });
  assert.deepEqual(bodies[4], { templateId: 'tpl_a' });
  for (const i of [0, 2, 3, 4]) assert.ok(calls[i].init.headers['Idempotency-Key'], 'creating calls carry an idempotency key');
});

test('the start form asks for params and explicit program authorization', async (t) => {
  const started = [];
  let refuse = true;
  const api = {
    listTemplates: async () => [{ id: 'tpl_a', name: 'Alpha' }],
    templateRequirements: async () => ({
      params: [
        { name: 'issue', type: 'string', required: true, default: '', hasDefault: false, description: 'Tracker issue' },
        { name: 'branch', type: 'string', required: false, default: 'main', hasDefault: true, description: '' },
      ],
      programProfiles: ['ci'],
    }),
    startGroupRun: async (request) => {
      if (refuse) { refuse = false; throw new Error('program profiles require explicit authorization: ci, deploy'); }
      started.push(request);
      return { runId: 'run_new' };
    },
  };
  const { harness, group } = await mountFocus(t, { api });
  const fe = group('frontend-squad');
  await harness.act(() => harness.fireEvent(fe.querySelector('[data-process-start="frontend-squad"]'), 'click'));
  await settle(harness);
  const form = fe.querySelector('form.gp-start-form');
  const submit = form.querySelector('button[type="submit"]');
  assert.ok(submit.disabled, 'a required param blocks the start');
  assert.equal(form.querySelector('[data-start-param="branch"]').value, 'main', 'a default starts out filled in');
  const input = form.querySelector('[data-start-param="issue"]');
  Object.defineProperty(input, 'value', { configurable: true, writable: true, value: 'TCL-1' });
  await harness.act(() => harness.fireEvent(input, 'input'));
  const ci = form.querySelector('[data-start-profile="ci"]');
  assert.ok(!ci.checked, 'program authorization is never pre-ticked');
  await harness.act(() => harness.fireEvent(form, 'submit'));
  await settle(harness);
  assert.ok(form.querySelector('[data-start-profile="deploy"]'), 'a refusal naming more profiles lists them');
  for (const profile of ['ci', 'deploy']) {
    const box = form.querySelector(`[data-start-profile="${profile}"]`);
    Object.defineProperty(box, 'checked', { configurable: true, writable: true, value: true });
    await harness.act(() => harness.fireEvent(box, 'change'));
  }
  await harness.act(() => harness.fireEvent(form, 'submit'));
  await settle(harness);
  assert.deepEqual(started, [{ templateId: 'tpl_a', group: 'frontend-squad', params: { branch: 'main', issue: 'TCL-1' }, authorizeProgramProfiles: ['ci', 'deploy'] }]);
});

test('template start requirements walk stages and inline bodies', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/process-context.js');
  const needs = mod.templateStartRequirements({
    params: { b: { type: 'string', default: 'x' }, a: { type: 'number', required: true } },
    nodes: {
      t: { type: 'task', performer: { kind: 'program', run: 'make', profile: 'ci' }, checks: [{ performer: { kind: 'program', run: 'lint' } }] },
      q: { type: 'subprocess', subprocess: { nodes: { u: { type: 'task', performer: { kind: 'program', run: 'go test', profile: 'test' } } } } },
      h: { type: 'decision', performer: { kind: 'human' } },
    },
  });
  assert.deepEqual(needs.params.map((p) => [p.name, p.required, p.hasDefault]), [['a', true, false], ['b', false, true]]);
  assert.deepEqual(needs.programProfiles, ['', 'ci', 'test']);
  assert.deepEqual(mod.unauthorizedProfiles('program profiles require explicit authorization: <empty>, ci'), ['', 'ci']);
});

test('the Processes perspective is the Agents tree: nesting, summary counts, a subgroup total, and runs instead of members', async (t) => {
  // reindex is agent-scoped: the read model indexes it under its agent, not
  // the group, so it belongs on the member-runs line.
  const context = structuredClone(FIXTURE);
  context.groups[0].active = context.groups[0].active.filter((id) => id !== 'run_reindex');
  context.groups[0].activeCount = 2;
  context.agents.push({ agentId: 'agt_doc', runs: [{ runId: 'run_docs', nodeId: 'fix', index: 2, total: 3, state: 'running' }] });
  context.runs.run_docs = { ...structuredClone(context.runs.run_reindex), runId: 'run_docs', name: 'doc-sweep', scope: { kind: 'agent', agentId: 'agt_doc', state: 'bound' } };
  const groups = [
    { name: 'platform', members: [] },
    { ...GROUPS[0], parent: 'platform' },
    GROUPS[1],
    { name: 'Ungrouped', key: ' ungrouped-virtual', virtual: true, members: [{ conv_id: 'c9', agent_id: 'agt_doc', title: 'doc-bot' }] },
  ];
  const focused = [];
  const { harness, root, group, details } = await mountFocus(t, {
    groups, sourceOptions: { context }, onFocusRun: (value) => focused.push(value),
  });

  assert.ok(details('platform').querySelector('.group-subgroups details[data-group-key="frontend-squad"]'), 'subgroups nest as in Agents');
  assert.equal(root.querySelector('table'), null, 'no member tables');
  assert.equal(root.querySelector('[data-act="spawn-agent"]'), null, 'no agent actions');
  const fe = details('frontend-squad');
  const graphs = [...fe.querySelector(':scope > summary').querySelectorAll('.gpi-graph')].map((graph) => graph.getAttribute('aria-label'));
  assert.deepEqual(graphs, ['2 needs you', '2 running', '1 planned'], 'the summary row counts runs');
  assert.equal(details('infra-crew').querySelector(':scope > summary .gpi-graphs'), null, 'a quiet group shows no graphs');

  const platform = details('platform');
  assert.equal(platform.querySelector(':scope > summary .gpi-graphs'), null, 'no roll-up into the parent');
  assert.equal(platform.querySelector(':scope > summary .gp-subtree-total').textContent, '↳ in subgroups: 2 running · 2 needs you · 1 planned');
  assert.ok(platform.querySelector(':scope > .subtable > [data-fused-section="subgroups"] + .gp-group'), 'the parent\'s own runs follow its subgroups');

  const memberRuns = group('frontend-squad').querySelector('[data-process-member-runs]');
  assert.equal(memberRuns.querySelector('.gp-member-runs-label').textContent, 'Member runs');
  assert.deepEqual([...memberRuns.querySelectorAll('[data-process-member-run]')].map((el) => el.dataset.processMemberRun), ['run_reindex']);
  await harness.act(() => harness.fireEvent(memberRuns.querySelector('button[data-process-run]'), 'click'));
  assert.deepEqual(focused.pop(), { group: 'frontend-squad', runId: 'run_reindex' });

  const ungrouped = details(' ungrouped-virtual');
  assert.equal(ungrouped.querySelector(':scope > summary .gpi-graph').getAttribute('aria-label'), '1 running');
  const agentRuns = group(' ungrouped-virtual').querySelector('[data-process-member-runs]');
  assert.equal(agentRuns.querySelector('.gp-member-runs-label').textContent, 'Agent runs');
  await harness.act(() => harness.fireEvent(agentRuns.querySelector('button[data-process-run]'), 'click'));
  assert.deepEqual(focused.pop(), { group: ' ungrouped-virtual', runId: 'run_docs' });
});

test('an Ungrouped agent run opens in the single-run focus', async (t) => {
  const context = structuredClone(FIXTURE);
  context.agents.push({ agentId: 'agt_doc', runs: [{ runId: 'run_docs', nodeId: 'fix', index: 2, total: 3, state: 'running' }] });
  context.runs.run_docs = { ...structuredClone(context.runs.run_reindex), runId: 'run_docs', name: 'doc-sweep', scope: { kind: 'agent', agentId: 'agt_doc', state: 'bound' } };
  const groups = [GROUPS[0], { name: 'Ungrouped', key: ' ungrouped-virtual', virtual: true, members: [{ conv_id: 'c9', agent_id: 'agt_doc', title: 'doc-bot' }] }];
  const closed = [];
  const { root } = await mountFocus(t, {
    groups, sourceOptions: { context }, focusedRun: { group: ' ungrouped-virtual', runId: 'run_docs' }, onFocusRun: (value) => closed.push(value),
  });
  assert.ok(root.querySelector('[data-run-focus="run_docs"]'), 'the agent run opens');
  assert.equal(root.querySelector('details[data-group-key]'), null, 'the run focus replaces the tree');
  assert.deepEqual(closed, [], 'the run is not treated as stranded');
});

// Kept last: dismissing writes the shared dashPrefs key that folds the panel.
test('Get started shows in full only before the first process, folds when dismissed, and expands on demand', async (t) => {
  const empty = { ...FIXTURE, runs: {}, groups: (FIXTURE.groups || []).map((entry) => ({ ...entry, active: [], needsYou: [], done: [], planned: [] })) };
  const api = { listTemplates: async () => [] };
  const { harness, group } = await mountFocus(t, { sourceOptions: { context: empty }, api });
  await settle(harness);
  const infra = () => group('infra-crew');
  assert.ok(infra().querySelector('[data-quickstart="infra-crew"]'), 'no process anywhere yet: the full panel');
  assert.equal(infra().querySelector('[data-quickstart-folded]'), null);
  await harness.act(async () => { infra().querySelector('[data-quickstart-dismiss]').click(); });
  assert.equal(infra().querySelector('[data-quickstart]'), null, 'dismissed: folded');
  assert.ok(infra().querySelector('[data-quickstart-folded="infra-crew"]'));
  await harness.act(async () => { infra().querySelector('[data-quickstart-expand]').click(); });
  await settle(harness);
  assert.ok(infra().querySelector('[data-quickstart="infra-crew"]'), 'Get started ▸ opens the panel again');
});

test('the avatar strip reads each member\'s real status, not just process work', async (t) => {
  const { group, mod } = await mountFocus(t);
  const { memberAvailability } = mod;
  const busy = new Set(['agt_p']);
  const of = (member) => memberAvailability({ agent_id: 'agt_x', ...member }, busy);
  assert.equal(of({ agent_id: 'agt_p', online: true, state: { status: 'idle' } }), 'process');
  assert.equal(of({ online: true, state: { status: 'working' } }), 'working');
  assert.equal(of({ online: true, state: { status: 'main_agent_idle' } }), 'working');
  assert.equal(of({ online: true, state: { status: 'awaiting_permission' } }), 'waiting');
  assert.equal(of({ online: true, state: { status: 'idle' } }), 'idle');
  assert.equal(of({ online: false, state: { status: 'idle' } }), 'offline');

  const infra = group('infra-crew');
  assert.equal(infra.querySelector('.gp-strip-text').textContent, '0 idle · 1 offline');
  assert.equal(group('frontend-squad').querySelector('.gp-member.is-working').getAttribute('title'), '1 working on other tasks (not in a process) — fe-lead');
});

test('the group row mirrors the Agents row: agent process, group process, resume all, pause all', async (t) => {
  const started = [];
  const api = {
    listTemplates: async () => [{ id: 'tpl_a', name: 'Alpha' }],
    startGroupRun: async (request) => { started.push(request); return { runId: 'run_new' }; },
  };
  const calls = [];
  const lists = [];
  const runApi = {
    pause: async (runId) => { calls.push(['pause', runId]); if (runId === 'run_migrate') throw new Error('refused'); },
    resume: async (runId) => { calls.push(['resume', runId]); },
    // The full list holds a run the capped context did not carry.
    listLiveRuns: async (filters) => {
      lists.push(filters);
      return [
        { runId: 'run_release', paused: { by: 'human' } }, { runId: 'run_migrate' }, { runId: 'run_reindex' }, { runId: 'run_extra' },
      ];
    },
  };
  const context = { ...FIXTURE, runs: { ...FIXTURE.runs, run_release: { ...FIXTURE.runs.run_release, paused: { by: 'human' } } } };
  const notes = [];
  const harness = await createPreactHarness(t);
  const [mod, list, interactions] = await Promise.all([
    harness.importDashboardModule('js/groups-processes.js'),
    harness.importDashboardModule('js/groups-list.js'),
    harness.importDashboardModule('js/groups-interactions.js'),
  ]);
  const source = fakeSource(harness, { context });
  const snapshot = { processes_enabled: true, groups: GROUPS.filter((group) => !group.virtual) };
  const { container } = await harness.mount(harness.html`<${interactions.GroupsInteractionProvider}>
    <${mod.GroupsProcessesFocus} groups=${GROUPS} allGroups=${GROUPS} source=${source} filter="done" api=${api} runApi=${runApi}
      notify=${(message, isError) => notes.push([message, isError])} onFocusRun=${() => {}}>
      <${list.GroupsNativeList} groups=${GROUPS} snapshot=${snapshot} actions=${{}} />
    <//>
  <//>`);
  const fe = container.querySelector('[data-process-group="frontend-squad"]');
  const pause = fe.querySelector('[data-process-pause-all]');
  const resume = fe.querySelector('[data-process-resume-all]');
  // The Done filter hides live runs, but the controls still count them.
  assert.match(pause.title, /^Pause 2 running processes/);
  assert.match(resume.title, /^Resume 1 paused process:/);
  const infra = container.querySelector('[data-process-group="infra-crew"]');
  assert.equal(infra.querySelector('[data-process-pause-all]').disabled, true);
  assert.equal(infra.querySelector('[data-process-resume-all]').disabled, true);

  await harness.act(async () => { pause.click(); await new Promise((resolve) => setTimeout(resolve, 0)); });
  assert.deepEqual(lists[0], [{ group: 'frontend-squad' }, { agent: 'agt_fe_lead' }, { agent: 'agt_fe_forms' }, { agent: 'agt_fe_charts' }]);
  assert.deepEqual(calls.map(([kind, id]) => `${kind}:${id}`).sort(), ['pause:run_extra', 'pause:run_migrate', 'pause:run_reindex']);
  assert.equal(notes.length, 1);
  assert.match(notes[0][0], /^Could not pause 1 of 3 processes: .*refused/);
  await harness.act(async () => { fe.querySelector('[data-process-resume-all]').click(); await new Promise((resolve) => setTimeout(resolve, 0)); });
  assert.deepEqual(calls.at(-1), ['resume', 'run_release']);

  await harness.act(() => harness.fireEvent(fe.querySelector('[data-process-start-agent="frontend-squad"]'), 'click'));
  await settle(harness);
  const form = fe.querySelector('form.gp-start-form[data-start-scope="agent"]');
  assert.equal(form.getAttribute('aria-label'), 'Start a process for one agent in frontend-squad');
  const agent = form.querySelector('select[data-start-agent]');
  assert.deepEqual([...agent.options].map((o) => o.textContent), ['fe-lead', 'fe-dev-forms', 'fe-dev-charts']);
  Object.defineProperty(agent, 'value', { configurable: true, writable: true, value: 'agt_fe_charts' });
  await harness.act(() => harness.fireEvent(agent, 'change'));
  await harness.act(() => harness.fireEvent(form, 'submit'));
  await settle(harness);
  assert.equal(started.length, 1);
  assert.equal(started[0].agent, 'agt_fe_charts');
  assert.equal(started[0].group, undefined, 'an agent-scoped run names no group');
});

test('Both shows each group\'s members and runs as sections, each with its controls heading its first column', async (t) => {
  const { harness, root, details } = await mountFocus(t, { fused: true });
  assert.ok(root.querySelector('[data-groups-fused]'));
  assert.equal(root.querySelector('[data-groups-processes]'), null);
  const fe = details('frontend-squad');
  const section = (el, name) => el.querySelector(`:scope > .subtable > [data-fused-section="${name}"]`);
  const head = (el, name) => section(el, name).querySelector(':scope > .gf-section-head');
  const lead = section(fe, 'agents').querySelector('table > thead > tr > th:first-child');
  for (const act of ['spawn-agent', 'create-subgroup', 'power-on-group', 'shutdown-group']) {
    assert.ok(lead.querySelector(`.gf-table-actions [data-act="${act}"]`), `${act} heads the member table's first column`);
    assert.equal(head(fe, 'agents').querySelector(`[data-act="${act}"]`), null, `${act} left the open Agents line`);
  }
  const titleRow = section(fe, 'processes').querySelector('[data-process-group] > .gf-title-row');
  assert.ok(titleRow.querySelector('[data-process-start-agent="frontend-squad"]'), 'process controls head the open runs');
  assert.ok(titleRow.querySelector('[data-process-pause-all] [data-run-glyph="pause"]'), 'pause never wears the shutdown glyph');
  assert.equal(head(fe, 'processes').querySelector('[data-process-start-agent]'), null, 'process controls left the open Processes line');
  assert.equal(fe.querySelector(':scope > .subtable > .group-header-actions'), null, 'no separate action row');

  assert.ok(section(fe, 'agents').querySelector('table'), 'the member table shows');
  const runs = section(fe, 'processes');
  assert.ok(runs.classList.contains('open'), 'a group with runs shows them');
  assert.ok(runs.querySelector('[data-process-run-card="run_release"]'));
  assert.equal(root.querySelector('.gf-summary'), null, 'section lines carry no counts: the group header shows them');

  const infra = details('infra-crew');
  const quiet = section(infra, 'processes');
  assert.equal(quiet.classList.contains('open'), false, 'a quiet group folds its runs to one line');
  assert.equal(quiet.querySelector('[data-process-group]'), null);
  assert.ok(quiet.querySelector('.gf-section-actions [data-process-start="infra-crew"]'), 'a folded section keeps its controls');
  await harness.act(() => harness.fireEvent(quiet.querySelector('.gf-section-toggle'), 'click'));
  assert.ok(section(infra, 'processes').querySelector('[data-quickstart-folded="infra-crew"]'), 'opened, Get started stays a line');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.processes.infra-crew'), '1', 'the choice is remembered');
  await harness.act(() => harness.fireEvent(section(fe, 'agents').querySelector('.gf-section-fill'), 'click'));
  assert.equal(section(fe, 'agents').querySelector('table'), null, 'Agents folds too');
  assert.ok(head(fe, 'agents').querySelector('.gf-section-actions [data-act="spawn-agent"]'), 'folded Agents keeps its controls on the line');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.agents.frontend-squad'), '0');
  // A control on the folded line unfolds its section, so what it spawns shows.
  // The button must still be in the page when the click reaches the
  // document, where the row actions are delegated.
  const reached = [];
  const spy = (event) => reached.push(event.target.isConnected);
  document.addEventListener('click', spy);
  await harness.act(() => harness.fireEvent(head(fe, 'agents').querySelector('[data-act="spawn-agent"]'), 'click'));
  document.removeEventListener('click', spy);
  assert.deepEqual(reached, [true], 'the clicked spawn button reaches the delegated handler');
  await settle(harness);
  assert.ok(section(fe, 'agents').querySelector('table'), 'spawning from the folded line unfolds Agents');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.agents.frontend-squad'), '1');
  await harness.act(() => harness.fireEvent(section(infra, 'processes').querySelector('.gf-section-toggle'), 'click'));
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.processes.infra-crew'), '0', 'Processes folded again');
  await harness.act(() => harness.fireEvent(section(infra, 'processes').querySelector('.gf-section-actions [data-process-start="infra-crew"]'), 'click'));
  await settle(harness);
  assert.ok(section(infra, 'processes').classList.contains('open'), 'a process control on the folded line unfolds Processes');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.processes.infra-crew'), '1');
});

for (const status of ['idle', 'unavailable']) {
  test(`Both shows the member tree while runs are ${status}`, async (t) => {
    const { root, details } = await mountFocus(t, { fused: true, sourceOptions: { status, context: null } });
    assert.ok(details('frontend-squad').querySelector('table'), 'members show');
    assert.ok(details('frontend-squad').querySelector('[data-act="spawn-agent"]'));
    assert.equal(root.querySelector('[data-fused-section]'), null, 'no sections without runs');
    assert.equal(root.querySelector('.groups-processes-placeholder'), null);
  });
}

test('Both links an agent to its steps, a step to its performers, and a run card to all of its agents, on hover only', async (t) => {
  const { harness, root, details } = await mountFocus(t, { fused: true });
  const wait = (ms) => harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, ms)); });
  const lines = () => [...root.querySelectorAll('.gf-links path.gf-link')].length;
  const hover = (el) => harness.act(() => el.dispatchEvent(new harness.window.Event('pointerover', { bubbles: true })));
  const fe = details('frontend-squad');
  assert.equal(root.querySelector('.gf-links'), null, 'nothing at rest');

  const forms = fe.querySelector('tr[data-dnd-agent="agt_fe_forms"]');
  await hover(forms.querySelector('td'));
  assert.equal(lines(), 0, 'the first hover waits a moment');
  await wait(160);
  assert.equal(lines(), 2, 'fe-dev-forms performs checkout (release) and verify (migrate)');
  assert.ok(forms.classList.contains('gf-link-hot'));
  const release = fe.querySelector('[data-process-run-card="run_release"]');
  assert.ok(release.querySelector('[data-process-node="checkout"]').classList.contains('gf-link-hot'));
  assert.ok(release.querySelector('[data-process-node="plan"]').classList.contains('gf-link-dim'), 'the rest of the card dims');

  // jsdom lays nothing out, so every end reads as off screen and gets a tag;
  // with a tag showing, moving on to another link waits a short dwell.
  assert.ok(root.querySelector('.gf-link-tag'), 'an off-screen end becomes a tag');
  await hover(release.querySelector('.gp-card-head'));
  await wait(40);
  assert.equal(lines(), 2, 'with a tag showing, a passing hover does not switch at once');
  await wait(300);
  assert.equal(lines(), 3, 'a run card links each agent-performed step: plan, checkout, charts');
  assert.equal(forms.classList.contains('gf-link-dim'), false);

  await hover(release.querySelector('[data-process-node="charts"]'));
  await wait(340);
  assert.equal(lines(), 1, 'a step links only its performer');
  assert.ok(fe.querySelector('tr[data-dnd-agent="agt_fe_charts"]').classList.contains('gf-link-hot'));
  assert.equal(forms.classList.contains('gf-link-hot'), false, 'the previous highlights clear');

  await harness.act(() => root.querySelector('[data-groups-fused]').dispatchEvent(new harness.window.Event('pointerleave')));
  await wait(260);
  assert.equal(root.querySelector('.gf-links'), null, 'leaving the frame clears the links');
  assert.equal(fe.querySelectorAll('.gf-link-hot, .gf-link-dim').length, 0);
});
