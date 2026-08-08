import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const storage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
function payload(title = 'Alpha') {
  return {
    from: '2026-07-01', to: '2026-07-10', first_day: '2026-07-01', total_usd: 5,
    real_total_usd: 3, what_if_total_usd: 2, cost_kind: 'mixed',
    days: [{ day: '2026-07-10', cost_usd: 5, real_cost_usd: 3, what_if_cost_usd: 2, cost_kind: 'mixed' }],
    agents: [
      { agent_id: 'agt_alpha', conv_id: 'conv-a', day: '2026-07-10', title, harness: 'claude', model: 'opus', cost_usd: 3, real_cost_usd: 3, cost_kind: 'real' },
      { agent_id: 'agt_beta', conv_id: 'conv-b', day: '2026-07-10', title: 'Beta', harness: 'codex', model: 'gpt', cost_usd: 2, what_if_cost_usd: 2, cost_kind: 'what_if' },
    ],
  };
}

test('Costs island renders controls and preserves keyed table focus/selection across refreshes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { CostsApp }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-island.js'),
  ]);
  const snapshot = harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: false });
  const activeTab = harness.signals.signal('costs');
  const state = createCostsState({ snapshot, activeTab, prefs: storage, now: () => new Date(2026, 6, 10, 12) });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, payload());
  const calls = [];
  const actions = { load: async () => calls.push('load'), loadFactor: async () => calls.push('factor'), saveFactor: async () => calls.push('save') };
  const mounted = await harness.mount(harness.html`<${CostsApp} state=${state} actions=${actions} />`);
  const chartColumn = mounted.container.querySelector('.cost-col[data-tip]');
  harness.fireEvent(chartColumn, 'mousemove', { clientX: 20, clientY: 30 });
  const tooltip = harness.document.body.querySelector('.cost-tip');
  assert.ok(tooltip, 'chart hover opens its tooltip');
  await harness.act(() => {
    snapshot.value = { cost_tab_visible: true, cost_tab_whatif: false, generated_at: '2026-07-10T12:00:02Z' };
  });
  assertSameNode(mounted.container.querySelector('.cost-col[data-tip]'), chartColumn, 'snapshot refresh preserves the imperative chart');
  assertSameNode(harness.document.body.querySelector('.cost-tip'), tooltip, 'snapshot refresh preserves the open chart tooltip');
  await harness.act(() => { activeTab.value = 'groups'; });
  assertAbsent(harness.document.body.querySelector('.cost-tip'), 'leaving Costs removes its body-level tooltip');
  await harness.act(() => { activeTab.value = 'costs'; });

  const row = mounted.container.querySelector('tr[data-key="cost-conv-a-2026-07-10"]');
  const id = row.querySelector('.id');
  id.tabIndex = 0;
  id.focus();
  const text = row.querySelector('.rowname').firstChild;
  await harness.act(() => { state.beginRequest(2); state.commitRequest(2, payload()); });
  assertSameNode(mounted.container.querySelector('tr[data-key="cost-conv-a-2026-07-10"]'), row);
  assertSameNode(row.querySelector('.rowname').firstChild, text);
  assertSameNode(harness.document.activeElement, id);

  const filter = getByRole(mounted.container, 'textbox', { name: 'Filter cost agents' });
  await harness.input(filter, 'gpt');
  assert.equal(mounted.container.querySelectorAll('tbody tr[data-key]').length, 1);
  assert.equal(mounted.container.querySelector('#filter-costs-count').textContent, '1 / 2');
  const last7 = [...mounted.container.querySelectorAll('#costs-spans button')].find((button) => button.textContent === 'Last 7d');
  await harness.act(() => harness.fireEvent(last7, 'click'));
  assert.equal(state.span.value, '7d');
  assert.ok(calls.includes('load'));
  await mounted.unmount();
});

test('Costs island exposes loading/error/what-if visibility and production cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { CostsApp }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-island.js'),
  ]);
  const snapshot = harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: true });
  const activeTab = harness.signals.signal('groups');
  const state = createCostsState({ snapshot, activeTab, prefs: storage });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, payload());
  const actions = { load: async () => {}, loadFactor: async () => {}, saveFactor: async () => {} };
  const mounted = await harness.mount(harness.html`<${CostsApp} state=${state} actions=${actions} />`);
  assert.match(mounted.container.textContent, /WHAT-IF/);
  assert.match(mounted.container.textContent, /mixes real billed spend/);
  // Scoped to the summary on purpose: the ≈ left the rows, not the header, so
  // an unscoped match here would keep passing off the summary while reading
  // like a row assertion.
  assert.match(mounted.container.querySelector('#costs-summary').textContent, /≈\$2\.00/,
    'the header summary keeps its real/estimate split');

  // A row states its amount once and defers the caveat to the banner: the
  // hypothetical row gets a single ⚠ linking there, not a ≈ prefix, a repeated
  // WHAT-IF label or (on a mixed row) an inline real+estimate split.
  const realAmount = mounted.container.querySelector('tr[data-key="cost-conv-a-2026-07-10"] .cost-amt');
  const whatIfAmount = mounted.container.querySelector('tr[data-key="cost-conv-b-2026-07-10"] .cost-amt');
  assert.match(realAmount.textContent.replace(/\s+/g, ''), /^\$3\.00$/, 'a real row is the bare amount');
  assertAbsent(realAmount.querySelector('.cost-whatif-mark'), 'a real row carries no WHAT-IF marker');
  assert.match(whatIfAmount.textContent.replace(/\s+/g, ''), /^\$2\.00⚠︎$/,
    'a hypothetical row is the bare amount plus one text-presentation ⚠ marker');
  const mark = whatIfAmount.querySelector('.cost-whatif-mark');
  assert.equal(mark.getAttribute('href'), '#costs-whatif-banner', 'the marker points at the banner');
  assert.match(mark.title, /not a real charge/, 'the marker documents itself on hover');

  await harness.act(() => state.beginRequest(2));
  await harness.act(() => state.failRequest(2, new Error('offline')));
  assert.match(getByRole(mounted.container, 'alert').textContent, /offline/);
  await mounted.unmount();

  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'costs-root';
  const { mountCostsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const cleanup = await mountCostsFeature({ fetchImpl: async () => { throw new Error('should remain inactive'); } });
  assert.equal(typeof cleanup, 'function');
  assert.ok(host.querySelector('#costs-spans'));
  cleanup();
  assert.equal(host.childElementCount, 0);
});

// htm strips the whitespace around a newline in a static chunk, so every
// text/expression boundary that spans lines needs an explicit ${' '} or the
// words run together. These are the three places in the island where the gap
// is load-bearing; all three regressed at some point.
test('Costs island keeps its cross-line word gaps and leads with the WHAT-IF caveat', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { CostsApp }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-island.js'),
  ]);
  const chained = payload();
  chained.agents.push({
    agent_id: 'agt_alpha2', conv_id: 'conv-a', day: '2026-07-09', title: 'Alpha', harness: 'claude',
    model: 'opus', cost_usd: 1, real_cost_usd: 1, cost_kind: 'real', continued: true,
  });
  const snapshot = harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: true });
  const activeTab = harness.signals.signal('costs');
  const state = createCostsState({ snapshot, activeTab, prefs: storage, now: () => new Date(2026, 6, 10, 12) });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, chained);
  const actions = { load: async () => {}, loadFactor: async () => {}, saveFactor: async () => {} };
  const mounted = await harness.mount(harness.html`<${CostsApp} state=${state} actions=${actions} />`);

  const banner = mounted.container.querySelector('#costs-whatif-banner');
  const controls = mounted.container.querySelector('#costs-spans');
  assert.ok(banner && controls, 'banner and controls both render');
  assert.ok(banner.compareDocumentPosition(controls) & 4 /* DOCUMENT_POSITION_FOLLOWING */,
    'the WHAT-IF caveat renders above the controls it qualifies');
  assert.match(banner.textContent, /subscription estimates\. WHAT-IF values estimate/,
    'the banner keeps a space between its two sentences');

  assert.match(mounted.container.querySelector('.cost-proj').textContent, /: ~\$8/,
    'the projection keeps a space after its label');
  assert.match(mounted.container.querySelector('tr[data-key="cost-conv-a-2026-07-10"] td').textContent, /↳ Alpha/,
    'the chain marker keeps a space before the agent name');
  await mounted.unmount();
});

// A row's ⚠ is a pointer to the banner, not a repeat of it, so what it says and
// what clicking it does are the whole contract. The mixed row is the case worth
// pinning: it holds both kinds of money, so it is exactly where a single
// "this row is hypothetical" boolean silently relabels real spend as an
// estimate. The zero-cost row guards the other direction — kind "" is what the
// server returns when both subtotals are 0, and it is not an estimate.
test('Costs rows carry an accurate, banner-linked WHAT-IF marker', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { CostsApp }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-island.js'),
  ]);
  const mixed = payload();
  mixed.agents.push({
    agent_id: 'agt_gamma', conv_id: 'conv-c', day: '2026-07-10', title: 'Gamma', harness: 'claude',
    model: 'opus', cost_usd: 1.5, real_cost_usd: 1, what_if_cost_usd: 0.5, cost_kind: 'mixed',
  });
  mixed.agents.push({
    agent_id: 'agt_delta', conv_id: 'conv-d', day: '2026-07-10', title: 'Delta', harness: 'claude',
    model: 'opus', cost_usd: 0, cost_kind: '',
  });
  const snapshot = harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: true });
  const activeTab = harness.signals.signal('costs');
  const state = createCostsState({ snapshot, activeTab, prefs: storage, now: () => new Date(2026, 6, 10, 12) });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, mixed);
  const actions = { load: async () => {}, loadFactor: async () => {}, saveFactor: async () => {} };
  const mounted = await harness.mount(harness.html`<${CostsApp} state=${state} actions=${actions} />`);
  const cell = (conv) => mounted.container.querySelector(`tr[data-key="cost-${conv}-2026-07-10"] .cost-amt`);

  // A mixed row shows its total once, and neither tooltip may call that total
  // hypothetical — $1.00 of it is billed.
  const mixedCell = cell('conv-c');
  assert.match(mixedCell.textContent.replace(/\s+/g, ''), /^\$1\.50⚠︎$/, 'a mixed row shows one total plus the marker');
  assert.equal(mixedCell.title, '$1.5000 total — $1.0000 real spend + $0.5000 estimated (WHAT-IF)',
    'the amount tooltip names both parts instead of labelling the whole total an estimate');
  assert.match(mixedCell.querySelector('.cost-whatif-mark').title, /\$1\.00 real \+ \$0\.50 WHAT-IF estimate/,
    'the marker tooltip carries the split the cell no longer shows inline');

  assert.equal(cell('conv-a').title, '$3.0000 real spend', 'a real row is named real');
  assert.equal(cell('conv-b').title, '$2.0000 estimated (WHAT-IF)', 'a hypothetical row is named an estimate');
  assertAbsent(cell('conv-d').querySelector('.cost-whatif-mark'), 'a zero-cost row (kind "") is not marked as an estimate');

  // Clicking through. A modified click belongs to the browser — the marker is a
  // real anchor and ⌘/Ctrl-click must still open it — while a plain click is
  // handled in place and has to land the reader somewhere they can perceive.
  const banner = mounted.container.querySelector('#costs-whatif-banner');
  const mark = cell('conv-b').querySelector('.cost-whatif-mark');
  assert.equal(mark.getAttribute('href'), '#costs-whatif-banner', 'the marker points at the banner');
  assert.equal(mark.getAttribute('aria-label'), 'About this WHAT-IF estimate',
    'its accessible name is short — every row carries one on a subscription');

  let event;
  await harness.act(() => { event = harness.fireEvent(mark, 'click', { button: 0, ctrlKey: true }); });
  assert.equal(event.defaultPrevented, false, 'a modified click is left to the browser to open');
  assert.equal(banner.classList.contains('cost-whatif-flash'), false, 'and does not scroll/flash in place');

  await harness.act(() => { event = harness.fireEvent(mark, 'click', { button: 0 }); });
  assert.equal(event.defaultPrevented, true, 'a plain click is handled in place, without a history entry');
  assert.ok(banner.classList.contains('cost-whatif-flash'), 'the banner flashes so the jump visibly lands');
  assertSameNode(harness.document.activeElement, banner,
    'and takes focus, so a keyboard or screen-reader visitor arrives too');
  await mounted.unmount();
});

// The banner is what every row's ⚠ points at, so it must be shown whenever any
// row is marked. A payload can name a row's kind without carrying its split
// fields — costs-model's chart walk defends against that same shape — and
// keying the banner off the hypothetical subtotal alone would hide it there,
// leaving every marker on the page a dead link.
test('Costs banner shows for row kinds that carry no split fields', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { CostsApp }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-island.js'),
  ]);
  const legacy = {
    from: '2026-07-01', to: '2026-07-10', first_day: '2026-07-01', total_usd: 2,
    days: [{ day: '2026-07-10', cost_usd: 2, cost_kind: 'what_if' }],
    agents: [{
      agent_id: 'agt_beta', conv_id: 'conv-b', day: '2026-07-10', title: 'Beta',
      harness: 'codex', model: 'gpt', cost_usd: 2, cost_kind: 'what_if',
    }],
  };
  const snapshot = harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: true });
  const activeTab = harness.signals.signal('costs');
  const state = createCostsState({ snapshot, activeTab, prefs: storage, now: () => new Date(2026, 6, 10, 12) });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, legacy);
  const actions = { load: async () => {}, loadFactor: async () => {}, saveFactor: async () => {} };
  const mounted = await harness.mount(harness.html`<${CostsApp} state=${state} actions=${actions} />`);
  const cell = mounted.container.querySelector('tr[data-key="cost-conv-b-2026-07-10"] .cost-amt');
  assert.ok(cell.querySelector('.cost-whatif-mark'), 'the row is marked from its kind alone');
  assert.equal(mounted.container.querySelector('#costs-whatif-banner').hidden, false,
    'so the banner it points at is shown');
  await mounted.unmount();
});

test('Copilot WHAT-IF tooltips identify native credits and subscription value', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { CostsApp }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-island.js'),
  ]);
  const copilot = payload();
  copilot.total_usd = 0.43;
  copilot.real_total_usd = 0;
  copilot.what_if_total_usd = 0.43;
  copilot.cost_kind = 'what_if';
  copilot.days = [{ day: '2026-07-10', cost_usd: 0.43, what_if_cost_usd: 0.43,
    virtual_cost_credits: 43, cost_kind: 'what_if' }];
  copilot.agents = [{ agent_id: 'agt_copilot', conv_id: 'conv-copilot', day: '2026-07-10',
    title: 'Copilot', harness: 'copilot', model: 'gpt-5', cost_usd: 0.43,
    what_if_cost_usd: 0.43, virtual_cost_credits: 43, cost_kind: 'what_if' }];
  const snapshot = harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: true });
  const activeTab = harness.signals.signal('costs');
  const state = createCostsState({ snapshot, activeTab, prefs: storage });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, copilot);
  const actions = { load: async () => {}, loadFactor: async () => {}, saveFactor: async () => {} };
  const mounted = await harness.mount(harness.html`<${CostsApp} state=${state} actions=${actions} />`);
  const cell = mounted.container.querySelector('tr[data-key="cost-conv-copilot-2026-07-10"] .cost-amt');
  assert.equal(cell.title, '43 credits — $0.4300 subscription value');
  assert.match(cell.querySelector('.cost-whatif-mark').title,
    /43 credits — \$0\.43 subscription value/);

  const chartColumn = mounted.container.querySelector('.cost-col[data-tip]');
  await harness.act(() => harness.fireEvent(chartColumn, 'mousemove', { clientX: 20, clientY: 30 }));
  assert.match(harness.document.body.querySelector('.cost-tip').textContent,
    /43 credits — \$0\.43 subscription value/);
  await mounted.unmount();
});
