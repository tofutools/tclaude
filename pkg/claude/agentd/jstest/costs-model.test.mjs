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
  assert.equal(model.monthProjectionLabel({ fillEmpty: false, includesWhatIf: false }, true),
    'Projected month total', 'real-only projection retains the normal label');
  assert.equal(model.monthProjectionLabel({ fillEmpty: false, includesWhatIf: true }, false),
    'WHAT-IF Projected month total', 'WHAT-IF-only projection remains wholly hypothetical');
  assert.equal(model.monthProjectionLabel({ fillEmpty: false, includesWhatIf: true }, true),
    'Projected month total (includes WHAT-IF)', 'mixed projection does not label real spend as hypothetical');
  assert.equal(model.monthProjectionLabel({ fillEmpty: true, includesWhatIf: true }, true),
    'Projected avg month total (includes WHAT-IF)', 'mixed full-month fill uses the same qualified label');

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

test('Copilot cost segments retain native credits beside gross subscription dollars', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/costs-model.js');
  const agents = [
    { conv_id: 'copilot', day: '2026-07-10', title: 'Copilot', harness: 'copilot',
      cost_usd: 0.43, what_if_cost_usd: 0.43, virtual_cost_credits: 43, cost_kind: 'what_if' },
    { conv_id: 'claude', day: '2026-07-10', title: 'Claude', harness: 'claude',
      cost_usd: 0.20, real_cost_usd: 0.20, cost_kind: 'real' },
  ];
  const payload = {
    from: '2026-07-10', to: '2026-07-10', total_usd: 0.63,
    days: [{ day: '2026-07-10', cost_usd: 0.63, cost_kind: 'mixed' }], agents,
  };
  const selected = new Set(['copilot']);
  const filtered = model.filterCostData(payload, selected);
  assert.equal(filtered.virtual_cost_credits, 43);
  assert.equal(filtered.days[0].virtual_cost_credits, 43);
  assert.equal(model.fmtCredits(43), '43 credits');
  assert.equal(model.fmtCredits(0.004), '<0.01 credits');

  const chart = model.buildCostChart(filtered, null, agents, selected, model.costHarnesses(agents));
  const copilotSegment = chart.days[0].segments.find((segment) => segment.harness === 'copilot');
  assert.equal(copilotSegment.credits, 43);
  assert.equal(copilotSegment.kind, 'what_if');
});

// Dollar figures are USD wherever they are read, so the separators follow the
// currency and not the viewer's locale: a four-figure month total reads
// "$26,222.38", never "26 222,38 $".
test('USD formatting groups thousands the currency\'s way', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/costs-model.js');

  assert.equal(model.fmtUSD(0.42), '$0.42');
  assert.equal(model.fmtUSD(1000), '$1,000.00');
  assert.equal(model.fmtUSD(26222.375), '$26,222.38');
  assert.equal(model.fmtUSD(1234567.891), '$1,234,567.89');
  // Real spend that would round to $0.00 must not read as free.
  assert.equal(model.fmtUSD(0.004), '<1¢');
  assert.equal(model.fmtUSD(0), '$0.00');

  assert.equal(model.fmtExactUSD(0.0042), '$0.0042');
  assert.equal(model.fmtExactUSD(26222.375), '$26,222.3750');
  assert.equal(model.fmtExactUSD(0), '$0.0000');

  assert.equal(model.fmtCredits(1234.5), '1,234.5 credits');

  // Axis ticks stay compact instead of grouped — the gutter is narrow.
  assert.equal(model.fmtAxisUSD(26222.38), '$26.2k');
  assert.equal(model.fmtAxisUSD(2600000), '$2.6M');
  assert.equal(model.fmtAxisUSD(12.5), '$12.50');
});
