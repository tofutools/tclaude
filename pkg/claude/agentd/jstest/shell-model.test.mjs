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
  assert.deepEqual(activity.modes.map((mode) => mode.key), ['regular', 'wizard']);
  assert.equal(activity.modes[0].className, 'ga-regular');
  assert.equal(activity.modes[0].bots[0].key, 'working');
  assert.equal(activity.details.total, 1);
  assert.equal(activity.details.groups[0].name, 'alpha');
  assert.equal(activity.details.groups[0].states[0].members[0].name, 'same');
});

test('global activity details preserve exact states, dedup shared members, and include suppressed offline workers', async (t) => {
  const harness = await createPreactHarness(t);
  const { globalActivityView, activityMemberTitle } = await harness.importDashboardModule('js/shell-model.js');
  const shared = { conv_id: 'shared', title: 'Shared Worker', online: true, state: { status: 'working' } };
  const activity = globalActivityView({
    groups: [
      {
        name: 'builders',
        members: [
          shared,
          { conv_id: 'ask', title: 'Needs Access', online: true, state: { status: 'awaiting_permission' } },
          { conv_id: 'sleep', title: 'Sleeping Worker', online: false, state: { status: 'idle' } },
        ],
      },
      {
        name: 'reviewers',
        members: [
          shared,
          { conv_id: 'stuck', title: 'Stuck Worker', online: true, state: { status: 'error', status_detail: 'tool failed' } },
        ],
      },
    ],
    ungrouped: [
      { conv_id: 'background', title: 'Background Worker', online: true, state: { status: 'main_agent_idle' } },
    ],
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
  });

  assert.equal(activity.details.total, 5, 'the shared conv_id is listed once');
  assert.equal(activity.details.suppressedOffline, 1, 'offline is explicit when its bot is suppressed');
  assert.deepEqual(activity.details.groups.map((group) => group.name), ['builders', 'reviewers', 'Ungrouped']);
  const builders = activity.details.groups[0];
  assert.deepEqual(builders.states.map((state) => state.key), [
    'awaiting_permission', 'working', 'offline',
  ]);
  assert.equal(builders.states[0].label, 'Awaiting permission');
  assert.equal(builders.states[0].members[0].name, 'Needs Access');
  assert.equal(builders.states[2].members[0].name, 'Sleeping Worker');
  assert.equal(activity.details.groups[1].states[0].label, 'Error / stuck');
  assert.equal(activity.details.groups[2].states[0].label, 'Idle + background work');

  assert.equal(activityMemberTitle({ title: '<img src=x>', agent_id: 'agt_safe' }), 'agt_safe');
  assert.equal(activityMemberTitle({ title: 'a'.repeat(65), conv_id: 'conv_safe' }), 'conv_safe');
  assert.equal(activityMemberTitle({ title: 'line\tbreak', conv_id: 'conv_tab' }), 'conv_tab');
});

test('activity hover opens a text-only worker panel without changing the shared bot renderer', async (t) => {
  const harness = await createPreactHarness(t);
  const { ActivityHover } = await harness.importDashboardModule('js/activity-hover.js');
  const details = {
    total: 2,
    suppressedOffline: 1,
    groups: [{
      key: 'build', name: 'build', states: [
        { key: 'working', label: 'Working', wizardLabel: 'Channeling', members: [{ key: 'a', name: 'Alice', detail: '' }] },
        { key: 'offline', label: 'Offline', wizardLabel: 'Departed', members: [{ key: 'b', name: 'Bob', detail: '' }] },
      ],
    }],
  };
  const mounted = await harness.mount(harness.html`
    <${ActivityHover} id="global-activity" className="global-activity"
      label="Activity" title="Activity" details=${details}>
      <span>bot</span>
    </${ActivityHover}>
  `);
  const root = mounted.container.querySelector('.activity-hover');
  const trigger = mounted.container.querySelector('button');
  assert.equal(root.classList.contains('is-open'), false);
  await harness.act(() => harness.fireEvent(trigger, 'click'));
  assert.equal(root.classList.contains('is-open'), true);
  assert.match(root.textContent, /Alice/);
  assert.match(root.textContent, /Bob/);
  await harness.act(() => harness.fireEvent(trigger, 'keydown', { key: 'Escape' }));
  assert.equal(root.classList.contains('is-open'), false);
  await mounted.unmount();
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
