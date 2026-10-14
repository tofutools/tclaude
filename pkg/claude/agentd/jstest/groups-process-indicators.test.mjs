import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

const FIXTURE = JSON.parse(readFileSync(new URL('./process-context-fixture.json', import.meta.url), 'utf8'));
const ON = { processes_enabled: true };

function fakeSource(harness, { status = 'ready', context = FIXTURE } = {}) {
  return {
    context: harness.signals.signal(context), status: harness.signals.signal(status), error: harness.signals.signal(''),
    refresh: () => Promise.resolve(),
  };
}

async function setup(t) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-process-indicators.js');
  return { harness, mod };
}

test('agentRunIndex maps each agent to its runs once per context', async (t) => {
  const { mod } = await setup(t);
  const index = mod.agentRunIndex(FIXTURE);
  assert.equal(mod.agentRunIndex(FIXTURE), index, 'cached per context object');
  assert.deepEqual(index.get('agt_fe_forms').map((e) => [e.runId, e.nodeId, e.run.name]), [
    ['run_release', 'checkout', 'release-2.4'], ['run_migrate', 'verify', 'migrate-orders'],
  ]);
  assert.equal(index.get('agt_unknown'), undefined);
  assert.equal(mod.agentRunIndex(null).size, 0);
  assert.equal(mod.agentRunIndex({ agents: [{ agentId: 'a', runs: [{ runId: 'gone' }] }], runs: {} }).size, 0,
    'entries whose run is not in the map are dropped');
});

test('group header chips summarise running, needs-you and planned; a quiet group says "no processes"', async (t) => {
  const { harness, mod } = await setup(t);
  const source = fakeSource(harness);
  const mounted = await harness.mount(harness.html`<div>
    <div id="fe"><${mod.GroupProcessChips} group=${{ name: 'frontend-squad' }} snapshot=${ON} source=${source} /></div>
    <div id="infra"><${mod.GroupProcessChips} group=${{ name: 'infra-crew' }} snapshot=${ON} source=${source} /></div>
    <div id="unknown"><${mod.GroupProcessChips} group=${{ name: 'nowhere' }} snapshot=${ON} source=${source} /></div>
    <div id="off"><${mod.GroupProcessChips} group=${{ name: 'frontend-squad' }} snapshot=${{}} source=${source} /></div>
  </div>`);
  const chips = [...mounted.container.querySelectorAll('#fe .gpi-chip')].map((c) => [c.dataset.groupProcesses, c.textContent.trim()]);
  assert.deepEqual(chips, [['running', '● 3 running'], ['needs-you', '● 2 needs you'], ['planned', '◷ 1 planned']]);
  assert.ok(mounted.container.querySelector('#fe .gpi-chip.pk-state-waiting'), 'needs-you is amber');
  assert.equal(mounted.container.querySelector('#infra').textContent, 'no processes');
  assert.equal(mounted.container.querySelector('#unknown').textContent, 'no processes');
  assert.equal(mounted.container.querySelector('#off').textContent, '', 'nothing while processes are off');
});

test('chips stay hidden until the context has loaded', async (t) => {
  const { harness, mod } = await setup(t);
  const mounted = await harness.mount(harness.html`<${mod.GroupProcessChips} group=${{ name: 'frontend-squad' }} snapshot=${ON}
    source=${fakeSource(harness, { status: 'unavailable', context: null })} />`);
  assert.equal(mounted.container.textContent, '');
});

test('a header chip opens the Processes focus on its filter without toggling the group', async (t) => {
  const { harness, mod } = await setup(t);
  const [{ registerFeatureState }, { createGroupsState }] = await Promise.all([
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
  const navigated = [];
  harness.document.addEventListener('tclaude:navigated', (event) => navigated.push(event.detail.location));
  let toggles = 0;
  const mounted = await harness.mount(harness.html`<details onClick=${() => { toggles++; }}><summary>
    <${mod.GroupProcessChips} group=${{ name: 'frontend-squad' }} snapshot=${ON} source=${fakeSource(harness)} />
  </summary></details>`);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-group-processes="planned"]'), 'click'));
  assert.equal(toggles, 0);
  assert.equal(state.focus.value, 'processes');
  assert.equal(state.processFilter.value, 'planned');
  assert.deepEqual(navigated, [{ tab: 'groups', subtab: 'processes' }]);
});

test('member rows get a run › node badge per run: running, waiting on a human, and queued (dashed) stack', async (t) => {
  const { harness, mod } = await setup(t);
  const source = fakeSource(harness);
  const cell = (agent) => harness.html`<div data-agent=${agent}><${mod.MemberProcessCell} member=${{ agent_id: agent }} group=${{ name: 'frontend-squad' }} snapshot=${ON} source=${source} /></div>`;
  const mounted = await harness.mount(harness.html`<div>${cell('agt_fe_forms')}${cell('agt_fe_lead')}${cell('agt_infra_lead')}</div>`);
  const badges = (agent) => [...mounted.container.querySelectorAll(`[data-agent="${agent}"] .pk-run-badge`)];

  const forms = badges('agt_fe_forms');
  assert.deepEqual(forms.map((b) => [b.querySelector('.pk-run-name').textContent, b.querySelector('.pk-run-node').textContent]), [
    ['release-2.4', 'Implement checkout'], ['migrate-orders', 'Verify counts'],
  ]);
  assert.ok(forms[0].classList.contains('pk-state-running'));
  assert.ok(forms[1].classList.contains('pk-state-planned'), 'a queued node reads as planned (dashed)');
  assert.ok(forms[0].querySelector('.pk-steps'), 'each badge carries its step strip');

  const lead = badges('agt_fe_lead');
  assert.equal(lead.length, 1);
  assert.equal(lead[0].querySelector('.pk-run-node').textContent, 'Review gate', 'the agent index names the node');
  assert.ok(lead[0].classList.contains('pk-state-waiting'), 'a human gate is amber');

  assert.equal(mounted.container.querySelector('[data-agent="agt_infra_lead"]').textContent, '—');
});
