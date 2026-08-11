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
// "$26,222", never "26 222 $".
test('USD formatting groups thousands the currency\'s way', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/costs-model.js');

  assert.equal(model.fmtUSD(0.42), '$0.42');
  assert.equal(model.fmtUSD(99.994), '$99.99');
  assert.equal(model.fmtUSD(1000), '$1,000');
  assert.equal(model.fmtUSD(26222.375), '$26,222');
  assert.equal(model.fmtUSD(1234567.891), '$1,234,568');
  // From $100 up the cents stop being worth their two digits.
  assert.equal(model.fmtUSD(99.99), '$99.99');
  assert.equal(model.fmtUSD(100), '$100');
  assert.equal(model.fmtUSD(845.88), '$846');
  // The written figure decides the branch: $99.999 is "$100.00" to the cent,
  // so it prints "$100" rather than wearing cents in a column of whole
  // dollars. $99.99 is still $99.99 and keeps them.
  assert.equal(model.fmtUSD(99.999), '$100');
  assert.equal(model.fmtUSD(99.5), '$99.50');
  // Halves round away from zero here and in pkg/claude/common/money alike.
  assert.equal(model.fmtUSD(102.5), '$103');
  // Real spend that would round to $0.00 must not read as free.
  assert.equal(model.fmtUSD(0.004), '<1¢');
  assert.equal(model.fmtUSD(0), '$0.00');

  // The tooltip keeps the cents a three-figure cell rounds off, and spells
  // out more only below one cent, where two decimals would read as $0.00.
  assert.equal(model.fmtExactUSD(26222.375), '$26,222.38');
  assert.equal(model.fmtExactUSD(845.88), '$845.88');
  assert.equal(model.fmtExactUSD(1.5), '$1.50');
  assert.equal(model.fmtExactUSD(0.0042), '$0.0042');
  assert.equal(model.fmtExactUSD(0), '$0.00');

  assert.equal(model.fmtCredits(1234), '1,234 credits');
  assert.equal(model.fmtCredits(1234.5), '1,234.50 credits');

  // Axis ticks stay compact instead of grouped — the gutter is narrow.
  assert.equal(model.fmtAxisUSD(26222.38), '$26.2k');
  assert.equal(model.fmtAxisUSD(2600000), '$2.6M');
  assert.equal(model.fmtAxisUSD(12.5), '$12.50');
});

// The model an agent runs on reaches the dashboard as the harness's own
// display name, which for an extended-window model carries the window with it
// ("Opus 5 (1M context)"). Whether a given agent launched on that window is an
// accident of its launch flags, not a difference in model — so the Costs tab
// must reduce both spellings to ONE value, or the same model sorts apart, and
// a filter for either finds only half of what it cost.
test('One model is one value: the Costs label peels the window, keeps every other qualifier', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/costs-model.js');

  assert.equal(model.costModelLabel({ model: 'Opus 5 (1M context)' }), 'Opus 5');
  assert.equal(model.costModelLabel({ model: 'Opus 5' }), 'Opus 5',
    'the plain spelling is already canonical');
  assert.equal(model.costModelLabel({ model: 'claude-opus-5[1m]' }), 'claude-opus-5',
    'an operator-typed launch alias loses only its window suffix');
  assert.equal(model.costModelLabel({ model: 'Opus 4.8 (fast)' }), 'Opus 4.8 (fast)',
    'a qualifier that is NOT a context window names a different model and must survive');
  assert.equal(model.costModelLabel({ model: '' }), '(unknown)',
    'an unattributed amount still gets a name, so spend cannot go missing from the table');
  assert.equal(model.costModelLabel({}), '(unknown)');

  // The Groups roster peels an OpenCode provider prefix to keep its token
  // narrow, and keeps the provider in the row tooltip. This tab must NOT: the
  // same model reached through two providers bills at two different rates, it
  // has no tooltip to fall back on, and merging them would hide a real price
  // difference — the same defect this label exists to prevent, inverted.
  assert.equal(model.costModelLabel({ model: 'anthropic/claude-sonnet-4-5', harness: 'opencode' }),
    'anthropic/claude-sonnet-4-5');
  assert.equal(model.costModelLabel({ model: 'openrouter/claude-sonnet-4-5', harness: 'opencode' }),
    'openrouter/claude-sonnet-4-5', 'two providers of one model stay two separately priced rows');

  // Sorting and filtering go through the same label, so the two spellings are
  // adjacent under the sort and both answer to one query.
  const rows = [
    { conv_id: 'a', title: 'wide', harness: 'claude', model: 'Opus 5 (1M context)', cost_usd: 8 },
    { conv_id: 'b', title: 'plain', harness: 'claude', model: 'Opus 5', cost_usd: 2 },
    { conv_id: 'c', title: 'small', harness: 'claude', model: 'Sonnet 4.6', cost_usd: 1 },
  ];
  const sorted = model.sortCostAgents(rows, { key: 'model', dir: 'asc' });
  assert.deepEqual(sorted.map((row) => row.conv_id), ['a', 'b', 'c'],
    'both Opus 5 spellings sort together, ahead of Sonnet');
  assert.deepEqual(rows.filter((row) => model.matchesCostAgent(row, 'opus 5')).map((row) => row.conv_id),
    ['a', 'b'], 'one query finds the whole model, not the half that skipped the extended window');
});

// The per-agent rows answer "what did this agent cost" and the chart stacks by
// harness; neither answers "how much of this was Opus", which is the question
// that matters when the models in one fleet differ several fold in price.
test('The per-model rollup merges spellings, shares to 100%, and marks hypothetical spend', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/costs-model.js');

  const rows = [
    { conv_id: 'a', model: 'Opus 5 (1M context)', cost_usd: 8 },
    { conv_id: 'b', model: 'Opus 5', cost_usd: 1 },
    // Same conversation across two days: one agent, counted once.
    { conv_id: 'b', model: 'Opus 5', cost_usd: 1 },
    { conv_id: 'c', model: 'Sonnet 4.6', cost_usd: 6, what_if_cost_usd: 6 },
    { conv_id: 'd', model: '', cost_usd: 4 },
  ];
  const rollup = model.modelRollup(rows);
  assert.deepEqual(rollup.map((entry) => entry.model), ['Opus 5', 'Sonnet 4.6', '(unknown)'],
    'biggest spender first; a row with no recorded model is named, not dropped');
  assert.equal(rollup[0].cost, 10, 'both Opus 5 spellings total into one entry');
  assert.equal(rollup[0].agents, 2, 'a conversation spanning two days is ONE agent');
  assert.equal(rollup[0].share, 0.5);
  assert.equal(rollup[2].model, '(unknown)');
  assert.equal(rollup.reduce((sum, entry) => sum + entry.share, 0), 1,
    'the shares partition the listed spend exactly');
  assert.equal(rollup[1].whatIf, 6,
    'the hypothetical subset is tracked so a WHAT-IF-only model can be marked as an estimate');
  assert.equal(rollup[0].whatIf, 0);

  // A model that spent nothing is dropped once anything else has: its 0% cell
  // says only "this model appears in the table", and one such row was enough
  // to force a breakdown of what is really a single-model fleet.
  const withFreeloader = model.modelRollup([
    { conv_id: 'a', model: 'Opus 5', cost_usd: 4 },
    { conv_id: 'b', model: 'Sonnet 4.6', cost_usd: 0 },
  ]);
  assert.deepEqual(withFreeloader.map((entry) => entry.model), ['Opus 5'],
    'a zero-cost model neither shows a 0% cell nor pushes the strip past its threshold');

  // A span where NOTHING spent must not divide by zero, and keeps its models:
  // every share is 0, not NaN, and the entries are not emptied.
  const free = model.modelRollup([{ conv_id: 'a', model: 'Opus 5', cost_usd: 0 }]);
  assert.equal(free.length, 1);
  assert.equal(free[0].share, 0);
  assert.deepEqual(model.modelRollup([]), []);
  assert.deepEqual(model.modelRollup(undefined), []);
});
