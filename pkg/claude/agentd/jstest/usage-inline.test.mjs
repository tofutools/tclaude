import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function budgetsSnapshot(harness) {
  return harness.signals.signal({
    usage: {
      budgets: {
        profiles: [{ id: 1, name: 'standard', rules: [] }, { id: 2, name: 'strict', rules: [] }],
        global: ['standard'],
        groups: { 7: ['strict'] },
        agents: {
          agt_hot: { name: 'Lifetime budget', profile: 'standard', metric: 'total_usd', value: 17.2, budget: 20, pct: 86, rules: 2 },
          agt_cold: { name: 'Lifetime budget', metric: 'total_usd', value: 4, budget: 20, pct: 20, rules: 1 },
          agt_stop: { name: 'Lifetime budget', metric: 'total_usd', value: 31, budget: 30, pct: 103, reached: 'wrap_up', rules: 1 },
        },
      },
      usage_limits: [{ scope: 'group', group_id: 7, name: 'weekly', metric: 'total_usd', period: 'week', value: 62, budget: 100, pct: 62 }],
    },
  });
}

test('the 💰 chip shows a layer\'s usage profiles and picks one inline', async (t) => {
  const harness = await createPreactHarness(t);
  const { UsageProfileChip } = await harness.importDashboardModule('js/usage-profile-chip.js');
  const snapshot = budgetsSnapshot(harness);
  const calls = [];
  let refreshed = 0;
  const api = { assign: async (...args) => { calls.push(args); } };
  const group = { id: 7, name: 'backend' };
  const mounted = await harness.mount(harness.html`<div>
    <${UsageProfileChip} scope="global" snapshot=${snapshot} api=${api} onChanged=${async () => { refreshed += 1; }}/>
    <${UsageProfileChip} scope="group" group=${group} snapshot=${snapshot} api=${api} onChanged=${async () => { refreshed += 1; }}/>
  </div>`);
  const host = mounted.container;
  const chips = host.querySelectorAll('.usage-profile-chip');
  assert.equal(chips[0].textContent.trim(), '💰 standard');
  assert.equal(chips[1].textContent.trim(), '💰 strict');
  assert.match(chips[1].getAttribute('title'), /Group limit "weekly": \$62\.00 \/ \$100 per week \(62%\)/);
  await harness.act(() => { harness.fireEvent(chips[1], 'click'); });
  const select = host.querySelector('select[aria-label="Usage profiles for backend"]');
  assert.ok(select, 'clicking opens the picker');
  Object.defineProperty(select, 'value', { configurable: true, writable: true, value: 'standard' });
  await harness.act(async () => { harness.fireEvent(select, 'change'); await Promise.resolve(); });
  for (let i = 0; i < 4; i++) await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, [['group', 'backend', ['standard']]]);
  assert.equal(refreshed, 1);
  await mounted.unmount();
});

test('an agent row shows its budget only once it matters', async (t) => {
  const harness = await createPreactHarness(t);
  const { AgentBudgetBadge } = await harness.importDashboardModule('js/usage-profile-chip.js');
  const snapshot = budgetsSnapshot(harness);
  const mounted = await harness.mount(harness.html`<div>
    <span id="cold"><${AgentBudgetBadge} agentID="agt_cold" snapshot=${snapshot}/></span>
    <span id="hot"><${AgentBudgetBadge} agentID="agt_hot" snapshot=${snapshot}/></span>
    <span id="stop"><${AgentBudgetBadge} agentID="agt_stop" snapshot=${snapshot}/></span>
    <span id="none"><${AgentBudgetBadge} agentID="agt_none" snapshot=${snapshot}/></span>
  </div>`);
  const host = mounted.container;
  assert.equal(host.querySelector('#cold').textContent, '', 'under half a budget stays quiet');
  assert.equal(host.querySelector('#none').textContent, '');
  const hot = host.querySelector('#hot .agent-budget-badge');
  assert.equal(hot.textContent, '💰86%');
  assert.ok(hot.classList.contains('warn'));
  assert.match(hot.getAttribute('title'), /\$17\.20 \/ \$20\.00 \(86%\).*tightest of 2 rules/);
  assert.ok(host.querySelector('#stop .agent-budget-badge').classList.contains('over'));
  await mounted.unmount();
});

test('a spawn carries the picked usage profile, never on a remote spawn', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses: [{ name: 'claude' }], sandboxImpl: {}, groups: [{ name: 'crew' }] };
  const base = { group: 'crew', harness: 'claude', name: 'w', cwd: '/repo', wtRepo: '/repo' };
  assert.equal('usage_profiles' in model.buildSpawnRequest(base, context, null).body, false, 'inherited sends nothing');
  assert.deepEqual(model.buildSpawnRequest({ ...base, usageProfile: 'strict' }, context, null).body.usage_profiles, ['strict']);
  assert.equal('usage_profiles' in model.buildSpawnRequest({ ...base, usageProfile: 'strict' }, { ...context, remoteSpawn: true }, null).body, false);
});
