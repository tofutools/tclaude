import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('shell models preserve usage layouts, badge urgency, footer, and activity deduplication', async (t) => {
  const harness = await createPreactHarness(t);
  const { usageView, messagesBadgeView, footerMetaView, authoredOpenPRsView, globalActivityView } =
    await harness.importDashboardModule('js/shell-model.js');

  assert.equal(usageView(null).text, 'usage: n/a');
  const claude = usageView({
    available: true,
    five_hour: { pct: 17, remaining: '2h' },
    seven_day: { pct: 80, remaining: '5d' },
    total_cost_usd: 0.42,
    today_cost_usd: 0.12,
    api_costs: [{ provider: 'anthropic', total_cost_usd: 0.42, today_cost_usd: 0.12 }],
  });
  assert.equal(claude.multiline, true);
  assert.deepEqual(claude.lines.map((line) => line.label), ['Claude:', 'Anthropic API:']);
  assert.deepEqual(claude.lines[0].tokens.map((token) => token.key), ['claude-5h', 'claude-7d']);
  assert.equal(claude.lines[1].tokens[0].key, 'api-cost-anthropic');
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

  const copilot = usageView({
    copilot: { available: true, monthly: { pct: 58.2, remaining: '18d' } },
  });
  assert.deepEqual(copilot.lines.map((line) => line.label), ['Copilot:']);
  assert.equal(copilot.lines[0].tokens[0].label, '', 'monthly word stays hidden so bars align');
  assert.equal(copilot.lines[0].tokens[0].pct, 58);

  assert.deepEqual(messagesBadgeView({ messages_unread: 98, access_requests_pending: 3 }),
    { text: '99+', hidden: false, blink: true });
  assert.deepEqual(
    footerMetaView({ version: 'v1', popup_base: 'http://x', generated_at: 'now', auth_session: { minted_at: 'then' } }),
    { version: 'v1', generatedAt: 'now' },
    'footer excludes the dashboard URL and auth session metadata',
  );
  const prs = {
    available: true, total: 5, updated_at: 'now', search_url: 'https://github.com/pulls?q=x',
    items: [
      { number: 1, url: 'https://github.com/acme/app/pull/1', agent_id: 'agt_1', checks: { total: 2, failed: 1, pending: 1, state: 'failing' } },
      { number: 2, url: 'https://github.com/acme/app/pull/2', checks: { total: 2, pending: 2, state: 'pending' } },
      { number: 3, url: 'https://github.com/acme/app/pull/3', checks: { total: 2, passed: 2, state: 'passing' } },
      { number: 4, url: 'https://github.com/acme/app/pull/4' },
      { number: 5, url: 'https://github.com/acme/app/pull/5', checks: { total: 0, state: 'none' } },
    ],
  };
  assert.deepEqual(
    authoredOpenPRsView({ authored_open_prs: prs }, 'attention').items.map((pr) => pr.number),
    [1, 3, 4, 5],
    'failed, completed, and no-CI PRs need attention; clean CI still running does not',
  );
  assert.deepEqual(authoredOpenPRsView({ authored_open_prs: prs }, 'unattached').items.map((pr) => pr.number), [2, 3, 4, 5]);
  assert.equal(authoredOpenPRsView({ authored_open_prs: prs }).attention, 4);
  assert.equal(authoredOpenPRsView({ authored_open_prs: { ...prs, search_url: 'javascript:alert(1)' } }).searchURL, '');

  // Recently closed PRs live in their own filter: they must not reach the open
  // list, the open count, or the attention/unattached tallies.
  const withRecent = {
    ...prs,
    always_show: true,
    recent_window_days: 3,
    recent_search_url: 'https://github.com/pulls?q=closed',
    recent: [
      { number: 9, url: 'https://github.com/acme/app/pull/9', state: 'merged', closed_at: '2026-08-12T10:00:00Z' },
      { number: 8, url: 'not-a-pr-url', state: 'closed', closed_at: '2026-08-11T10:00:00Z' },
    ],
  };
  const openView = authoredOpenPRsView({ authored_open_prs: withRecent });
  assert.deepEqual(openView.items.map((pr) => pr.number), [1, 2, 3, 4, 5]);
  assert.equal(openView.recentCount, 1, 'a malformed recent URL is dropped');
  assert.equal(openView.attention, 4, 'recent PRs never inflate the open tallies');
  assert.equal(openView.alwaysShow, true);
  const recentView = authoredOpenPRsView({ authored_open_prs: withRecent }, 'recent');
  assert.deepEqual(recentView.items.map((pr) => pr.number), [9]);
  assert.equal(recentView.showingRecent, true);
  assert.equal(recentView.searchURL, 'https://github.com/pulls?q=closed');
  assert.equal(recentView.total, 5, 'the trigger count stays the OPEN count');
  assert.equal(recentView.truncated, false);
  assert.equal(
    authoredOpenPRsView({ authored_open_prs: { ...withRecent, truncated: true, recent_truncated: true } }, 'recent').truncated,
    true, 'a capped recent page is disclosed, not presented as complete');
  assert.equal(
    authoredOpenPRsView({ authored_open_prs: { ...withRecent, recent_truncated: true } }).truncated,
    false, 'the open list keeps its own truncation flag');
  // Window 0 disables the filter; a stale "recent" selection falls back to open.
  const off = authoredOpenPRsView({ authored_open_prs: { ...withRecent, recent_window_days: 0 } }, 'recent');
  assert.equal(off.showingRecent, false);
  assert.equal(off.recentCount, 0);
  assert.deepEqual(off.items.map((pr) => pr.number), [1, 2, 3, 4, 5]);

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

test('usage model trims only placeholder columns that no source occupies', async (t) => {
  const harness = await createPreactHarness(t);
  const { usageView } = await harness.importDashboardModule('js/shell-model.js');
  const window = (pct = 10) => ({ pct, remaining: '1h' });
  const codex = (windows) => ({ codex: { available: true, ...windows } });
  const cost = { total_cost_usd: 1, api_costs: [{ provider: 'openai', total_cost_usd: 1 }] };

  const cases = [
    {
      name: 'weekly-only Codex is compact on its own',
      usage: codex({ seven_day: window() }),
      want: { codex: ['codex-7d'] },
    },
    {
      name: '5h-only Codex is compact on its own',
      usage: codex({ five_hour: window() }),
      want: { codex: ['codex-5h'] },
    },
    {
      name: 'both Codex windows remain visible',
      usage: codex({ five_hour: window(), seven_day: window() }),
      want: { codex: ['codex-5h', 'codex-7d'] },
    },
    {
      name: 'Claude 5h keeps the missing Codex 5h alignment slot',
      usage: {
        available: true, five_hour: window(), seven_day: window(),
        ...codex({ seven_day: window() }),
      },
      want: { claude: ['claude-5h', 'claude-7d'], codex: ['(codex-5h)', 'codex-7d'] },
    },
    {
      name: 'API cost keeps the missing Codex 5h alignment slot',
      usage: { ...codex({ seven_day: window() }), ...cost },
      want: { codex: ['(codex-5h)', 'codex-7d'], 'cost-openai': ['api-cost-openai'] },
    },
    {
      name: 'Copilot keeps the missing Codex 5h alignment slot',
      usage: {
        ...codex({ seven_day: window() }),
        copilot: { available: true, monthly: window() },
      },
      want: { codex: ['(codex-5h)', 'codex-7d'], copilot: ['copilot-monthly'] },
    },
    {
      name: 'a zero cost row does not reserve a column',
      usage: {
        ...codex({ seven_day: window() }),
        total_cost_usd: 0,
        api_costs: [{ provider: 'openai', total_cost_usd: 0 }],
      },
      want: { codex: ['codex-7d'] },
    },
    {
      name: 'unreported Codex windows do not create an empty row',
      usage: codex({}),
      want: {},
      na: true,
    },
  ];

  for (const tc of cases) {
    const view = usageView(tc.usage);
    assert.equal(view.na, !!tc.na, tc.name);
    const got = Object.fromEntries(view.lines.map((line) => [
      line.key,
      line.tokens.map((token) => token.hidden ? `(${token.key})` : token.key),
    ]));
    assert.deepEqual(got, tc.want, tc.name);
  }
});

test('global activity details mirror pulse buckets, dedup members, and annotate finer states', async (t) => {
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
    'asking', 'working', 'offline',
  ]);
  assert.equal(builders.states[0].label, 'Awaiting permission or input');
  assert.equal(builders.states[0].members[0].name, 'Needs Access');
  assert.equal(builders.states[2].members[0].name, 'Sleeping Worker');
  assert.equal(activity.details.groups[1].states[0].label, 'Error / stuck');
  assert.equal(activity.details.groups[2].states[0].label, 'Working');
  assert.equal(activity.details.groups[2].states[0].members[0].annotation, 'background activity still running');
  assert.equal(builders.states[0].members[0].detail, '');
  assert.equal(activity.details.groups[1].states[0].members[0].detail, 'tool failed');

  const lifecycle = globalActivityView({
    groups: [{
      name: 'lifecycle',
      members: [
        { conv_id: 'recovered-ask', title: 'Recovered Ask', online: true,
          state: { status: 'awaiting_permission', recovery_status: 'recovered' } },
        { conv_id: 'backoff-work', title: 'Backoff Work', online: true,
          state: { status: 'working', recovery_status: 'backoff' } },
        { conv_id: 'restarting', title: 'Restarting Worker', online: true,
          state: { status: 'working', recovery_status: 'restarting' } },
        { conv_id: 'suppressed', title: 'Suppressed Worker', online: true,
          state: { status: 'idle', recovery_status: 'suppressed' } },
        { conv_id: 'crashed-pending', title: 'Pending Crash', online: false,
          state: { status: 'working', recovery_status: 'crashed' } },
        { conv_id: 'exited', title: 'Exited Worker', online: true,
          state: { status: 'exited' } },
        { conv_id: 'unknown', title: 'Unknown Worker', online: true,
          state: { status: 'mystery' } },
        { conv_id: 'blank', title: 'Blank Status Worker', online: true,
          state: {} },
        { conv_id: 'waking', title: 'Waking Worker', online: false, waking: true,
          state: { status: 'working' } },
      ],
    }],
    ungrouped: [],
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
  });
  const lifecycleStates = lifecycle.details.groups[0].states;
  assert.deepEqual(lifecycleStates.map((state) => state.key), ['asking', 'working', 'idle', 'offline']);
  const membersByName = new Map(lifecycleStates.flatMap((state) => state.members.map((member) => [member.name, member])));
  assert.equal(membersByName.get('Recovered Ask').state, 'asking');
  assert.match(membersByName.get('Recovered Ask').annotation, /recovered/);
  assert.equal(membersByName.get('Backoff Work').state, 'working');
  assert.equal(membersByName.get('Backoff Work').annotation, 'crash loop / backoff');
  assert.equal(membersByName.get('Restarting Worker').state, 'working');
  assert.equal(membersByName.get('Restarting Worker').annotation, 'restarting');
  assert.equal(membersByName.get('Suppressed Worker').state, 'idle');
  assert.equal(membersByName.get('Suppressed Worker').annotation, 'recovery suppressed');
  assert.equal(membersByName.get('Pending Crash').state, 'offline');
  assert.equal(membersByName.get('Pending Crash').annotation, 'crashed — recovery pending');
  assert.equal(membersByName.get('Exited Worker').annotation, 'exited');
  assert.equal(membersByName.get('Unknown Worker').annotation, 'status unavailable');
  assert.equal(membersByName.get('Blank Status Worker').state, 'idle');
  assert.equal(membersByName.get('Blank Status Worker').annotation, '');
  assert.equal(membersByName.get('Waking Worker').state, 'offline');
  assert.equal(membersByName.get('Waking Worker').annotation, 'starting up');
  assert.equal(lifecycle.details.suppressedOffline, 2);

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

  // A pinned panel must close on a second click even though the pointer is
  // still over the trigger; otherwise hovered would immediately reopen it.
  await harness.act(() => harness.fireEvent(trigger, 'mouseenter'));
  await harness.act(() => harness.fireEvent(trigger, 'click'));
  assert.equal(root.classList.contains('is-open'), true);
  assert.equal(trigger.getAttribute('aria-expanded'), 'true');
  await harness.act(() => harness.fireEvent(trigger, 'click'));
  assert.equal(root.classList.contains('is-open'), false);
  assert.equal(trigger.getAttribute('aria-expanded'), 'false');

  // Outside pointerdown also clears focus, not only the pin, so a focused
  // trigger cannot keep the panel open after an outside dismissal.
  await harness.act(() => harness.fireEvent(trigger, 'focusin'));
  await harness.act(() => harness.fireEvent(trigger, 'click'));
  await harness.act(() => harness.fireEvent(harness.document.body, 'pointerdown'));
  assert.equal(root.classList.contains('is-open'), false);
  assert.equal(trigger.getAttribute('aria-expanded'), 'false');

  // The trigger is the focus target, while the panel is a sibling inside the
  // wrapper. Test the browser's focusin/focusout path used by the Preact
  // onFocusIn/onFocusOut handlers rather than relying on click coverage.
  await harness.act(() => harness.fireEvent(trigger, 'focusin'));
  assert.equal(root.classList.contains('is-open'), true);
  await harness.act(() => harness.fireEvent(trigger, 'focusout', { relatedTarget: harness.document.body }));
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
