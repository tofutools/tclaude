import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const CAP = { name: 'fleet cap', metric: 'total_usd', period: 'month', budget: 100,
  steps: [{ at: 50, action: 'notify', notify: ['operator'] }, { at: 100, action: 'block_spawn' }] };

function fleetFixture() {
  return {
    limits: [{ id: 4, target: 'pool:p1', target_label: 'pool rigs', include_self: true, enabled: true, rule: CAP }],
    statuses: [{ fleet_id: 4, rule_key: 'fleet:4', scope: 'fleet', name: 'fleet cap', metric: 'total_usd', period: 'month',
      value: 70, budget: 100, pct: 70, counted: 2, reached: 'notify', members: [
        { node: 'self', label: 'this node', value: 30, state: 'self', counted: true, enforce: 'self' },
        { node: 'inst_ann', label: 'ann', value: 40, state: 'ok', counted: true, enforce: 'granted', applied: { block: false } },
        { node: 'inst_ben', label: 'ben', value: 0, state: 'not_shared', counted: false, enforce: 'not_granted' },
      ] }],
    self: { grants: [], totals: { day: { total_usd: 3 }, week: { total_usd: 10 }, month: { total_usd: 30 } } },
    nodes: [
      { node: 'inst_ann', label: 'ann', pools: ['rigs'], summary: { grants: ['usage.read', 'usage-rules.manage', 'usage-rules.enforce'],
        totals: { day: { total_usd: 4 }, week: { total_usd: 12 }, month: { total_usd: 40 } }, limits: [{ name: 'local cap', metric: 'total_usd', value: 90, budget: 100, pct: 90 }] } },
      { node: 'inst_ben', label: 'ben', pools: [], summary: { grants: [] } },
    ],
    pools: [{ id: 'p1', name: 'rigs', members: ['inst_ann'] }],
    controls: [{ peer: 'inst_lead', peer_label: 'lead', rule_key: 'fleet:1', rule_name: 'org cap', fleet: 'all nodes', pct: 120, block: true, stop: false, until: new Date(Date.now() + 60000).toISOString() }],
  };
}

async function mount(t, descriptor = { kind: 'usage-rules', tab: 'fleet' }) {
  const harness = await createPreactHarness(t);
  const { UsageRulesDialog } = await harness.importDashboardModule('js/usage-rules.js');
  const calls = [];
  const record = (name) => async (...args) => { calls.push([name, ...args]); return {}; };
  const api = {
    overview: async () => ({ profiles: [], limits: [], limit_statuses: [], assignments: [], overrides: [], statuses: [], groups: [], spawn_profiles: [], agents: [], windows: [] }),
    fleet: async () => fleetFixture(),
    createFleetLimit: record('createFleetLimit'), updateFleetLimit: record('updateFleetLimit'), deleteFleetLimit: record('deleteFleetLimit'),
  };
  const mounted = await harness.mount(harness.html`<${UsageRulesDialog} descriptor=${descriptor} api=${api}
    onClose=${() => {}} confirmDiscard=${async () => true} confirm=${async () => true}/>`);
  // The Fleet tab mounts once the overview arrives, and its load effect runs
  // after paint: give it real event-loop turns.
  const flush = async () => {
    for (let turn = 0; turn < 4; turn++) {
      await new Promise((resolve) => setTimeout(resolve, 0));
      for (let i = 0; i < 8; i++) await harness.act(() => Promise.resolve());
    }
  };
  const host = mounted.container;
  for (let i = 0; i < 100 && !host.querySelector('.usage-fleet-tab, #usage-rules-title .usage-rules-node'); i++) {
    await new Promise((resolve) => setTimeout(resolve, 10));
    await flush();
  }
  const click = async (el) => { assert.ok(el, 'missing element'); await harness.act(() => { harness.fireEvent(el, 'click'); }); await flush(); };
  return { harness, host, mounted, calls, click, flush };
}

test('fleet tab shows readings per node, what each node shares, and peers\' controls', async (t) => {
  const { host, mounted, calls, click, harness, flush } = await mount(t);
  const card = host.querySelector('[data-fleet-limit-id="4"]');
  assert.match(card.textContent, /pool rigs \+ this node/);
  assert.match(card.textContent, /\$70\.00 \/ \$100 · 70% · 2 nodes counted/);
  assert.match(card.querySelector('[data-fleet-member="inst_ben"]').textContent, /shares no usage/);
  const ann = host.querySelector('[data-fleet-node="inst_ann"]');
  assert.match(ann.textContent, /\$40\.00/);
  assert.match(ann.textContent, /local cap 90%/);
  assert.equal(ann.querySelectorAll('.usage-grant-chip.on').length, 3);
  assert.ok([...ann.querySelectorAll('button')].some((b) => b.textContent.includes('manage its rules')));
  const ben = host.querySelector('[data-fleet-node="inst_ben"]');
  assert.match(ben.textContent, /shares no usage with you/);
  assert.ok(![...ben.querySelectorAll('button')].some((b) => b.textContent.includes('manage')));
  assert.match(host.textContent, /lead's "org cap" \(all nodes\) at 120%: blocks spawns/);

  const enabled = card.querySelector('.usage-rule-enabled input');
  await harness.act(() => { harness.fireEvent(enabled, 'change'); });
  await flush();
  assert.deepEqual(calls.at(-1), ['updateFleetLimit', 4, { enabled: false }]);

  await click([...host.querySelectorAll('button')].find((b) => b.textContent.includes('Blank fleet limit')));
  const editor = host.querySelector('#usage-fleet-limit-editor');
  assert.ok(editor);
  const options = [...editor.querySelectorAll('[aria-label="Fleet limit target"] option')].map((o) => o.textContent);
  assert.deepEqual(options, ['All trusted nodes', 'Node pool rigs (1 node)', 'Node ann', 'Node ben']);
  assert.ok(![...editor.querySelectorAll('[aria-label="Metric"] option')].some((o) => o.value === 'window_pct'), 'no quota windows for a fleet');
  assert.ok(![...editor.querySelectorAll('[aria-label="Period"] option')].some((o) => o.value === 'lifetime'));
  const name = editor.querySelector('[aria-label="Rule name"]');
  name.value = 'org';
  await harness.act(() => { harness.fireEvent(name, 'input'); });
  await flush();
  await click(host.querySelector('#usage-fleet-limit-save'));
  const [, body] = calls.find(([n]) => n === 'createFleetLimit');
  assert.equal(body.target, 'all');
  assert.equal(body.include_self, true);
  assert.equal(body.rule.name, 'org');
  await mounted.unmount();
});

test('a peer\'s manager names the node and has no Fleet tab', async (t) => {
  const { host, mounted } = await mount(t, { kind: 'usage-rules', node: 'inst_ann', nodeLabel: 'ann' });
  assert.match(host.querySelector('#usage-rules-title').textContent, /on ann/);
  assert.equal(host.querySelector('[data-usage-rules-tab="fleet"]'), null);
  await mounted.unmount();
});

test('the usage API reaches a peer through the proxy', async (t) => {
  const harness = await createPreactHarness(t);
  const { usageRulesApiFor } = await harness.importDashboardModule('js/usage-rules-actions.js');
  const seen = [];
  const api = usageRulesApiFor('inst_ann', async (url, init) => { seen.push([url, init.method || 'GET']); return { ok: true, status: 200, json: async () => ({}) }; });
  await api.overview();
  await api.assign('global', '', ['a']);
  assert.deepEqual(seen, [['/api/peer/inst_ann/usage/rules', 'GET'], ['/api/peer/inst_ann/usage/assignments', 'PUT']]);
});
