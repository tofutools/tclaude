import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

test('shell island reacts to snapshots while preserving keyed usage and footer nodes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { createShellState }, island] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const feedback = createShellState();
  const snapshot = {
    version: 'v1', popup_base: 'http://127.0.0.1:9999', generated_at: '2026-07-13T10:00:00Z',
    auth_session: { minted_at: '2026-07-13T09:00:00Z' },
    messages_unread: 2, access_requests_pending: 1,
    usage: { available: true, five_hour: { pct: 17, remaining: '2h' }, seven_day: { pct: 20, remaining: '4d' } },
    groups: [], ungrouped: [],
  };

  const usage = await harness.mount(harness.html`<${island.Usage} state=${state} />`);
  const meta = await harness.mount(harness.html`<${island.FooterMeta} state=${state} />`);
  const badge = await harness.mount(harness.html`<${island.MessagesBadge} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, snapshot));
  const fiveHour = usage.container.querySelector('.uw');
  const version = meta.container.querySelector('.meta-version');
  assert.equal(badge.container.querySelector('#messages-badge').textContent, '3');
  assert.ok(badge.container.querySelector('#messages-badge').classList.contains('blink'));
  assertAbsent(meta.container.querySelector('.meta-base'), 'footer omits the dashboard URL');
  assertAbsent(meta.container.querySelector('.footer-session-toggle'), 'footer omits auth controls');

  state.beginRequest();
  await harness.act(() => state.commitRequest(2, { ...snapshot, generated_at: '2026-07-13T10:00:02Z' }));
  assertSameNode(usage.container.querySelector('.uw'), fiveHour, 'stable usage token survives a poll');
  assertSameNode(meta.container.querySelector('.meta-version'), version,
    'unchanged version remains a valid selection anchor');

  feedback.showStatus('live');
  const status = await harness.mount(harness.html`<${island.Status} feedback=${feedback} />`);
  assert.ok(status.container.querySelector('#status').classList.contains('live'));
  await Promise.all([usage.unmount(), meta.unmount(), badge.unmount(), status.unmount()]);
});

test('disconnect overlay is removed on reconnect so its compositor layers cannot linger', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { Disconnect }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${Disconnect} state=${state} />`);

  assertAbsent(mounted.container.querySelector('#disconnect-overlay'));
  await harness.act(() => state.setConnection('disconnected'));
  assert.ok(mounted.container.querySelector('#disconnect-overlay.show'));
  assert.equal(mounted.container.querySelector('#disconnect-status').textContent, 'Reconnecting…');

  await harness.act(() => state.setConnection('connected'));
  assertAbsent(mounted.container.querySelector('#disconnect-overlay'),
    'reconnect destroys the backdrop-filter and animation subtree');
  await mounted.unmount();
});

test('footer open PRs disclosure pins, filters, and closes accessibly', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenPRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_prs: {
    available: true, total: 3, updated_at: '2026-08-13T08:00:00Z',
    search_url: 'https://github.com/pulls?q=open',
    items: [
      { number: 1, url: 'https://github.com/acme/app/pull/1', title: 'Fails', repository: 'acme/app', agent_id: 'agt_1', agent_title: 'builder', checks: { total: 1, failed: 1, state: 'failing' } },
      { number: 2, url: 'https://github.com/acme/app/pull/2', title: 'Runs', repository: 'acme/app', checks: { total: 1, pending: 1, state: 'pending' } },
      { number: 3, url: 'https://github.com/acme/app/pull/3', title: 'Passes', repository: 'acme/app', agent_id: 'agt_3', checks: { total: 1, passed: 1, state: 'passing' } },
    ],
  } }));
  const trigger = getByRole(mounted.container, 'button', { name: /Open PRs/ });
  assert.equal(trigger.getAttribute('aria-expanded'), 'false');
  await harness.act(() => trigger.click());
  assert.equal(trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(mounted.container.querySelectorAll('.open-pr-row').length, 3);

  await harness.act(() => getByRole(mounted.container, 'button', { name: /Unattached 1/ }).click());
  assert.deepEqual([...mounted.container.querySelectorAll('.open-pr-title')].map((node) => node.textContent), ['Runs']);

  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assertAbsent(mounted.container.querySelector('#open-prs-popover'));
  assertSameNode(harness.document.activeElement, trigger);
  await mounted.unmount();
});

test('footer open PRs stays mounted at zero and lists recently closed PRs', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenPRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_prs: {
    available: true, always_show: true, total: 0, updated_at: '2026-08-13T08:00:00Z',
    items: [],
    recent_window_days: 3,
    recent_search_url: 'https://github.com/pulls?q=closed',
    recent: [
      { number: 9, url: 'https://github.com/acme/app/pull/9', title: 'Landed', repository: 'acme/app', state: 'merged', closed_at: '2026-08-12T10:00:00Z' },
    ],
  } }));
  const trigger = getByRole(mounted.container, 'button', { name: /Open PRs/ });
  assert.ok(mounted.container.querySelector('.open-prs.is-empty'), 'zero open PRs reads as idle, not as a live counter');

  await harness.act(() => trigger.click());
  assert.equal(mounted.container.querySelectorAll('.open-pr-row').length, 0);
  assert.match(mounted.container.querySelector('.open-prs-empty').textContent, /No open pull requests\./);

  await harness.act(() => getByRole(mounted.container, 'button', { name: /Closed 3d 1/ }).click());
  assert.deepEqual([...mounted.container.querySelectorAll('.open-pr-title')].map((node) => node.textContent), ['Landed']);
  assert.ok(mounted.container.querySelector('.open-pr-state-merged'), 'a merged PR is dotted by its terminal state');
  await mounted.unmount();
});

test('footer open PRs can be opted out of the permanent indicator', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenPRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_prs: {
    available: true, always_show: false, total: 0, items: [], recent_window_days: 3, recent: [],
  } }));
  assertAbsent(mounted.container.querySelector('.open-prs'));
  await mounted.unmount();
});

test('shell confirmation keeps capture-Escape semantics and feedback cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createShellState }, { Confirm }] = await Promise.all([
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const feedback = createShellState();
  const mounted = await harness.mount(harness.html`<${Confirm} feedback=${feedback} />`);
  let accepted;
  await harness.act(() => { accepted = feedback.confirm({ title: 'Proceed?', body: 'Careful', okLabel: 'Do it' }); });
  const ok = getByRole(mounted.container, 'button', { name: 'Do it' });
  assertSameNode(harness.document.activeElement, ok);
  let shortcut;
  await harness.act(() => {
    shortcut = harness.fireEvent(harness.document, 'keydown', { key: 'Enter', ctrlKey: true });
  });
  assert.equal(await accepted, true);
  assert.equal(shortcut.defaultPrevented, true);

  let cmdAccepted;
  await harness.act(() => { cmdAccepted = feedback.confirm({ title: 'Proceed on macOS?' }); });
  await harness.act(() => {
    shortcut = harness.fireEvent(harness.document, 'keydown', { key: 'Enter', metaKey: true });
  });
  assert.equal(await cmdAccepted, true);
  assert.equal(shortcut.defaultPrevented, true);

  let cancelled;
  await harness.act(() => { cancelled = feedback.confirm({ title: 'Again?' }); });
  let escape;
  await harness.act(() => { escape = harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }); });
  assert.equal(await cancelled, false);
  assert.equal(escape.defaultPrevented, true);

  let closed;
  await harness.act(() => {
    closed = feedback.confirm({
      title: 'Recorded details', body: 'Status: ON\nProfile: base',
      okLabel: 'Close', informational: true, preformatted: true,
    });
  });
  const body = mounted.container.querySelector('#confirm-body');
  assert.equal(body.textContent, 'Status: ON\nProfile: base');
  assert.equal(body.classList.contains('confirm-body-preformatted'), true);
  assertAbsent(mounted.container.querySelector('#confirm-cancel'));
  const close = getByRole(mounted.container, 'button', { name: 'Close' });
  assert.equal(close.classList.contains('confirm-danger'), false);
  await harness.act(() => close.click());
  assert.equal(await closed, true);
  await mounted.unmount();
});

test('global activity keeps keyed native bot identity across polls and wizard changes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { GlobalActivity }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${GlobalActivity} state=${state} />`);
  const snapshot = {
    groups: [{ name: 'alpha', members: [
      { conv_id: 'a', online: true, state: { status: 'working' } },
      { conv_id: 'b', online: true, state: { status: 'working' } },
    ] }],
    ungrouped: [],
    activity_bots: { regular: 'emoji', slop: 'sprites', wizard: 'emoji' },
  };
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, snapshot));
  const regular = mounted.container.querySelector('.ga-regular');
  const working = regular.querySelector('.actbot-working');
  const count = working.querySelector('.actbot-count');
  assert.equal(count.textContent, '2');

  state.beginRequest();
  await harness.act(() => state.commitRequest(2, {
    ...snapshot,
    groups: [{ name: 'alpha', members: snapshot.groups[0].members.concat(
      { conv_id: 'c', online: true, state: { status: 'working' } },
    ) }],
  }));
  assertSameNode(mounted.container.querySelector('.ga-regular'), regular);
  assertSameNode(regular.querySelector('.actbot-working'), working);
  assertSameNode(working.querySelector('.actbot-count'), count);
  assert.equal(count.textContent, '3');

  harness.document.body.classList.add('wizard');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:wizard', { detail: { active: true } },
  )));
  assertSameNode(mounted.container.querySelector('.ga-regular'), regular,
    'theme wording changes do not remount hidden animation rows');
  assertSameNode(regular.querySelector('.actbot-working'), working);
  assert.match(mounted.container.querySelector('#global-activity').title, /familiars channeling/);
  await mounted.unmount();
});

test('a failed aggregate shell mount aborts bootstrap instead of stranding feedback', async (t) => {
  const harness = await createPreactHarness(t);
  const hostIDs = [
    'shell-activity-root', 'shell-usage-root', 'shell-status-root',
    'shell-notify-root', 'shell-credits-root', 'shell-messages-badge-root',
    'shell-meta-root', 'shell-disconnect-root', 'shell-confirm-root',
    'shell-toast-root', 'shell-palette-button-root', 'shell-palette-modal-root',
  ];
  for (const id of hostIDs) {
    const host = harness.document.body.appendChild(harness.document.createElement('div'));
    host.id = id;
  }
  const { mountShellFeature } = await harness.importDashboardModule('js/preact-loader.js');

  await assert.rejects(
    mountShellFeature({}, {
      documentRef: harness.document,
      // A null lifecycle result is the contract for an import/render failure
      // after the island has already painted its visible error fallback.
      mount: async () => null,
    }),
    /Dashboard shell failed to mount/,
  );
});

test('a blocking confirmation stays up, disabled and spinning, until its action settles', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createShellState }, { Confirm }] = await Promise.all([
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const feedback = createShellState();
  const mounted = await harness.mount(harness.html`<${Confirm} feedback=${feedback} />`);

  let release;
  const work = new Promise((resolve) => { release = resolve; });
  let answered;
  await harness.act(() => {
    answered = feedback.confirm({
      title: 'Shutdown?', okLabel: 'Shut down 3 agents', busyLabel: 'Shutting down…',
      action: () => work,
    });
  });
  await harness.act(() => { getByRole(mounted.container, 'button', { name: 'Shut down 3 agents' }).click(); });

  const ok = mounted.container.querySelector('#confirm-ok');
  const cancel = mounted.container.querySelector('#confirm-cancel');
  assert.equal(ok.disabled, true, 'the primary is disabled while the work runs');
  assert.equal(cancel.disabled, true, 'cancel cannot abandon work already in flight');
  assert.equal(ok.getAttribute('aria-busy'), 'true');
  assert.match(ok.textContent, /Shutting down…/, 'the primary swaps to its busy label');
  assert.ok(ok.querySelector('.btn-spinner'), 'the shared in-button spinner marks the wait');

  // Escape must not dismiss a busy dialog — hiding it would only lose sight of
  // the request, not stop it.
  await harness.act(() => { harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }); });
  assert.ok(mounted.container.querySelector('#confirm-ok'), 'a busy dialog ignores Escape');

  await harness.act(async () => { release('ok'); await answered; });
  assert.equal(await answered, 'ok');
  assert.equal(mounted.container.querySelector('#confirm-modal').classList.contains('show'), false,
    'the dialog comes down once the action settles');
  await mounted.unmount();
});
