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

test('global activity omits offline agents only when their group view is hidden', async (t) => {
  const harness = await createPreactHarness(t);
  const { globalActivityView } = await harness.importDashboardModule('js/shell-model.js');
  const offline = (conv_id) => ({ conv_id, online: false, state: { status: 'idle' } });
  const crashed = (conv_id) => ({
    conv_id, online: false, state: { status: 'working', exit_reason: 'unexpected' },
  });
  const online = (conv_id) => ({ conv_id, online: true, state: { status: 'working' } });
  const snapshot = {
    groups: [
      // Collapse state is intentionally irrelevant: this visible group's
      // sleeping member remains part of the global count.
      { name: 'folded', collapsed: true, members: [offline('visible-offline')] },
      {
        name: 'scribe', scribe: true, online: 0,
        members: [offline('hidden-scribe'), crashed('hidden-crashed-scribe')],
      },
    ],
    ungrouped: [
      offline('hidden-ungrouped'), crashed('hidden-crashed-ungrouped'),
      online('live-ungrouped'),
    ],
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
  };

  const hidden = globalActivityView(snapshot, false, { scribe: false, ungrouped: false });
  assert.match(hidden.title, /1 working/,
    'a live member remains globally visible even when its virtual group is hidden');
  assert.doesNotMatch(hidden.title, /offline/,
    'offline members from hidden real and virtual groups are excluded');
  assert.doesNotMatch(hidden.title, /crashed/,
    'unexpectedly exited members do not leak through tooltip detail lines');

  const visible = globalActivityView(snapshot, false, { scribe: true, ungrouped: true });
  // Clean offline is suppressed while a live status exists, so inspect the
  // all-offline snapshot to assert the exact visible count.
  const cleanGroups = snapshot.groups.map((group) => ({
    ...group,
    members: group.members.filter((member) => member.state.exit_reason !== 'unexpected'),
  }));
  const hiddenAllOffline = globalActivityView(
    { ...snapshot, groups: cleanGroups, ungrouped: [offline('hidden-ungrouped')] },
    false,
    { scribe: false, ungrouped: false },
  );
  const allOffline = globalActivityView(
    { ...snapshot, groups: cleanGroups, ungrouped: [offline('hidden-ungrouped')] },
    false,
    { scribe: true, ungrouped: true },
  );
  assert.match(hiddenAllOffline.title, /1 offline/,
    'the collapsed but visible group remains in the count');
  assert.match(allOffline.title, /3 offline/);
  assert.match(visible.title, /1 working/);
});

test('an offline agent shared by hidden and visible groups remains counted', async (t) => {
  const harness = await createPreactHarness(t);
  const { globalActivityView } = await harness.importDashboardModule('js/shell-model.js');
  const shared = { conv_id: 'shared', online: false, state: { status: 'idle' } };
  const activity = globalActivityView({
    groups: [
      { name: 'hidden scribe', scribe: true, online: 0, members: [shared] },
      { name: 'visible group', members: [shared] },
    ],
    ungrouped: [],
  }, false, { scribe: false, ungrouped: true });
  assert.match(activity.title, /1 offline/);
  assert.doesNotMatch(activity.title, /2 offline/);
});
