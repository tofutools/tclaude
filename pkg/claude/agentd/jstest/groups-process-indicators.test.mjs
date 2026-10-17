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

test('group header graphs show one glyph per state, loudest first; a quiet group shows nothing', async (t) => {
  const { harness, mod } = await setup(t);
  const source = fakeSource(harness);
  const mounted = await harness.mount(harness.html`<div>
    <div id="fe"><${mod.GroupProcessGraphs} group=${{ name: 'frontend-squad' }} snapshot=${ON} source=${source} /></div>
    <div id="infra"><${mod.GroupProcessGraphs} group=${{ name: 'infra-crew' }} snapshot=${ON} source=${source} /></div>
    <div id="unknown"><${mod.GroupProcessGraphs} group=${{ name: 'nowhere' }} snapshot=${ON} source=${source} /></div>
    <div id="off"><${mod.GroupProcessGraphs} group=${{ name: 'frontend-squad' }} snapshot=${{}} source=${source} /></div>
  </div>`);
  const graphs = [...mounted.container.querySelectorAll('#fe .gpi-graph')].map((g) => [g.dataset.groupProcesses, g.getAttribute('aria-label'), g.textContent]);
  assert.deepEqual(graphs, [['needs-you', '2 needs you', '2'], ['running', '3 running', '3'], ['planned', '1 planned', '']],
    'a count badge only when more than one run shares the state');
  assert.ok(mounted.container.querySelector('#fe .gpi-graphs.level-waiting'), 'needs-you leads the row\'s glow');
  assert.ok(mounted.container.querySelector('#fe .gpi-graph.gpi-state-waiting svg.gpi-flow'), 'each state is a flow-graph glyph');
  assert.equal(mounted.container.querySelector('#infra').innerHTML, '');
  assert.equal(mounted.container.querySelector('#unknown').innerHTML, '');
  assert.equal(mounted.container.querySelector('#off').innerHTML, '', 'nothing while processes are off');
});

test('graphs stay hidden until the context has loaded', async (t) => {
  const { harness, mod } = await setup(t);
  const mounted = await harness.mount(harness.html`<${mod.GroupProcessGraphs} group=${{ name: 'frontend-squad' }} snapshot=${ON}
    source=${fakeSource(harness, { status: 'unavailable', context: null })} />`);
  assert.equal(mounted.container.textContent, '');
});

async function mountGraphs(t) {
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
  const counter = { toggles: 0 };
  const mounted = await harness.mount(harness.html`<details onClick=${() => { counter.toggles++; }}><summary>
    <${mod.GroupProcessGraphs} group=${{ name: 'frontend-squad' }} snapshot=${ON} source=${fakeSource(harness)} />
  </summary></details>`);
  const click = () => harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-group-processes="planned"]'), 'click'));
  return { harness, state, navigated, counter, click };
}

test('from Agents, a header graph opens the Processes focus on its filter without toggling the group', async (t) => {
  const { state, navigated, counter, click } = await mountGraphs(t);
  state.setFocus('agents');
  await click();
  assert.equal(counter.toggles, 0);
  assert.equal(state.focus.value, 'processes');
  state.setProcessFilter('done');
  await click();
  assert.equal(state.processFilter.value, 'planned,done', 'the clicked kind is ticked without unticking the others');
  assert.deepEqual(navigated, [{ tab: 'groups', subtab: 'processes' }]);
});

test('in Both, a header graph stays in Both and opens the group\'s Processes section', async (t) => {
  const { harness, state, navigated, counter, click } = await mountGraphs(t);
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.groups.fused.processes.frontend-squad', '0');
  await click();
  assert.equal(counter.toggles, 0);
  assert.equal(state.focus.value, 'both');
  assert.deepEqual(navigated, [], 'no lens change, no history entry');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.processes.frontend-squad'), '1', 'a folded section is opened');
  assert.equal(state.processFilter.value, 'running,planned');
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
