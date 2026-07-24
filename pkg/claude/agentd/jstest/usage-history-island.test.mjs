import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const prefs = { getItem: () => null, setItem: () => {} };

test('Usage island keeps graphs available and renders provider-aware OpenCode coverage warnings', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createUsageHistoryState }, { UsageHistoryApp }] = await Promise.all([
    harness.importDashboardModule('js/usage-history-state.js'),
    harness.importDashboardModule('js/usage-history-island.js'),
  ]);
  const state = createUsageHistoryState({
    snapshot: harness.signals.signal({ usage_tab_visible: true }),
    activeTab: harness.signals.signal('usage'),
    prefs,
  });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, {
    from: '2026-07-23T00:00:00Z',
    generated_at: '2026-07-24T00:00:00Z',
    coverage_warnings: [{
      provider: 'openai', native_source: 'openai', models: ['gpt-5.6-terra'],
      activity_from: '2026-07-23T12:00:00Z', activity_to: '2026-07-23T12:30:00Z',
    }],
    series: [{
      provider: 'openai', window_name: 'five_hour', from: '2026-07-23T00:00:00Z',
      duration_seconds: 18000, points: [], resets: [], reset_count: 0,
      forecast: { status: 'insufficient', sample_count: 0 },
    }],
  });
  let loadCount = 0;
  const actions = { load: async () => { loadCount += 1; }, setPointExcluded: async () => {} };
  const mounted = await harness.mount(harness.html`<${UsageHistoryApp} state=${state} actions=${actions} />`);
  assert.match(mounted.container.textContent, /OpenCode does not export provider-account usage-limit history/);
  assert.match(mounted.container.textContent, /may be incomplete or stale/);
  assert.ok(mounted.container.querySelector('.usage-series-card'),
    'available native graph card remains visible beside the warning');

  await harness.act(() => {
    state.beginRequest(2);
    state.commitRequest(2, {
      from: '2026-07-23T00:00:00Z', generated_at: '2026-07-24T00:00:00Z',
      coverage_warnings: [], series: [],
    });
  });
  assert.doesNotMatch(mounted.container.textContent, /incomplete or stale/,
    'warning disappears when the server reports qualifying native coverage');
  const ninetyDays = [...mounted.container.querySelectorAll('.usage-empty-range button')]
    .find((button) => button.textContent === '90d');
  assert.ok(ninetyDays, 'an activity-only tab can widen the otherwise empty history range');
  const loadsBeforeClick = loadCount;
  await harness.act(() => ninetyDays.click());
  assert.equal(state.view.value.defaultHours, 2160);
  assert.ok(loadCount > loadsBeforeClick, 'changing the activity-only range reloads coverage');
  await mounted.unmount();
});
