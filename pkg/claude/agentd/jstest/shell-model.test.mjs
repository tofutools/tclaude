import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('shell models preserve usage layouts, badge urgency, footer, and activity deduplication', async (t) => {
  const harness = await createPreactHarness(t);
  const { usageView, messagesBadgeView, footerMetaView, globalActivityView } =
    await harness.importDashboardModule('js/shell-model.js');

  assert.equal(usageView(null).text, 'usage: n/a');
  const claude = usageView({
    available: true,
    five_hour: { pct: 17, remaining: '2h' },
    seven_day: { pct: 80, remaining: '5d' },
    total_cost_usd: 0.42,
    today_cost_usd: 0.12,
  });
  assert.equal(claude.multiline, false);
  assert.deepEqual(claude.lines[0].tokens.map((token) => token.key), ['claude-5h', 'claude-7d', 'api-cost']);
  assert.equal(claude.lines[0].tokens[0].filled, 1);
  assert.equal(claude.lines[0].tokens[1].color, '#f85149');

  const mixed = usageView({
    available: true,
    five_hour: { pct: 1 }, seven_day: { pct: 2 },
    codex: { available: true, seven_day: { pct: 33, remaining: '4d' } },
  });
  assert.equal(mixed.multiline, true);
  assert.deepEqual(mixed.lines.map((line) => line.label), ['Claude:', 'Codex:']);
  assert.equal(mixed.lines[1].tokens[0].hidden, true, 'missing Codex 5h retains its geometry');

  assert.deepEqual(messagesBadgeView({ messages_unread: 98, access_requests_pending: 3 }),
    { text: '99+', hidden: false, blink: true });
  assert.equal(footerMetaView({ version: 'v1', popup_base: 'http://x', generated_at: 'now' }).base, 'http://x');

  const member = { conv_id: 'same', online: true, state: { status: 'working' } };
  const activity = globalActivityView({
    groups: [{ name: 'alpha', members: [member] }, { name: 'beta', members: [member] }],
    ungrouped: [],
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'emoji' },
  });
  assert.match(activity.title, /1 working/);
  assert.doesNotMatch(activity.title, /2 working/);
  assert.match(activity.markup, /ga-regular/);
});
