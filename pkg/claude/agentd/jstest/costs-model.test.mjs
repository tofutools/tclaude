import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Costs derivation projects months, filters harnesses, sorts, and builds chart segments', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/costs-model.js');
  const now = new Date(2026, 6, 10, 12);
  const days = [];
  for (let day = 1; day <= 10; day += 1) {
    const key = `2026-07-${String(day).padStart(2, '0')}`;
    days.push({ day: key, cost_usd: day === 10 ? 50 : 0 });
  }
  const agents = [
    { conv_id: 'a', day: '2026-07-10', title: 'Alpha', harness: 'claude', cost_usd: 30, last_day: '2026-07-10' },
    { conv_id: 'b', day: '2026-07-10', title: 'Beta', harness: 'codex', cost_usd: 20, last_day: '2026-07-10' },
  ];
  const payload = { from: '2026-07-01', to: '2026-07-10', first_day: '2026-07-10', total_usd: 50, days, agents };
  const projection = model.monthProjection(payload, true, false, now);
  assert.equal(projection.daysElapsed, 1);
  assert.ok(projection.leadingFill['2026-07-01'] > 0);
  assert.equal(projection.leadingFill['2026-07-04'], undefined, 'weekends stay unfilled by default');
  const withWeekends = model.monthProjection(payload, true, true, now);
  assert.ok(withWeekends.leadingFill['2026-07-04'] > 0);
  assert.equal(withWeekends.weekendsIncluded, true);

  const selected = new Set(['claude']);
  const narrowed = model.filterCostData(payload, selected);
  assert.equal(narrowed.total_usd, 30);
  assert.equal(narrowed.days.at(-1).cost_usd, 30);
  const harnesses = model.costHarnesses(agents);
  const chart = model.buildCostChart(narrowed, null, agents, selected, harnesses);
  assert.equal(chart.days.at(-1).segments.length, 1);
  assert.equal(chart.days.at(-1).segments[0].harness, 'claude');
  assert.deepEqual(model.sortCostAgents(agents, { key: 'cost', dir: 'asc' }).map((row) => row.conv_id), ['b', 'a']);
  assert.equal(model.matchesCostAgent(agents[1], 'codex'), true);
});
