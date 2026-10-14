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

async function mountFocus(t, props = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-processes.js');
  const source = props.source || fakeSource(harness, props.sourceOptions);
  const filters = [];
  const mounted = await harness.mount(harness.html`<${mod.GroupsProcessesFocus} groups=${GROUPS} source=${source}
    filter=${props.filter || 'running'} onFilter=${(value) => filters.push(value)} api=${props.api} wizard=${props.wizard || false} />`);
  const group = (name) => mounted.container.querySelector(`[data-process-group="${name}"]`);
  return { harness, mod, source, filters, mounted, root: mounted.container, group };
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

  assert.equal(fe.querySelector('.gp-strip-text').textContent, '2 busy in processes · 1 free');
  assert.deepEqual([...fe.querySelectorAll('.gp-member.busy')].map((a) => a.getAttribute('title').split(' · ')[0]), ['fe-dev-forms', 'fe-dev-charts']);
  assert.equal(group('infra-crew').querySelector('.gp-empty').textContent, 'No running processes.');
  assert.equal(group('Ungrouped'), null, 'virtual groups have no runs');
  assert.equal(root.querySelector('[data-process-filter="running"]').getAttribute('aria-pressed'), 'true');
});

test('Done, Planned and All filters pick the matching runs', async (t) => {
  for (const [filter, cards, planned] of [
    ['done', ['run_hotfix'], []],
    ['planned', [], ['cron_nightly']],
    ['all', ['run_migrate', 'run_release', 'run_reindex', 'run_hotfix'], ['cron_nightly']],
  ]) {
    await t.test(filter, async (t2) => {
      const { group } = await mountFocus(t2, { filter });
      const fe = group('frontend-squad');
      assert.deepEqual([...fe.querySelectorAll('[data-process-run-card]')].map((c) => c.dataset.processRunCard), cards);
      assert.deepEqual([...fe.querySelectorAll('[data-process-planned]')].map((c) => c.dataset.processPlanned), planned);
    });
  }
  const { harness, root, filters } = await mountFocus(t);
  await harness.act(() => harness.fireEvent(getByRole(root, 'button', { name: 'Done' }), 'click'));
  assert.deepEqual(filters, ['done']);
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
  assert.deepEqual(started, [{ templateId: 'tpl_b', group: 'frontend-squad' }]);
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
