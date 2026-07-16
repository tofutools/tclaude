import test from 'node:test';
import assert from 'node:assert/strict';
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
  const baseText = meta.container.querySelector('.meta-base').firstChild;
  assert.equal(badge.container.querySelector('#messages-badge').textContent, '3');
  assert.ok(badge.container.querySelector('#messages-badge').classList.contains('blink'));

  state.beginRequest();
  await harness.act(() => state.commitRequest(2, { ...snapshot, generated_at: '2026-07-13T10:00:02Z' }));
  assert.equal(usage.container.querySelector('.uw'), fiveHour, 'stable usage token survives a poll');
  assert.equal(meta.container.querySelector('.meta-base').firstChild, baseText,
    'unchanged base URL remains a valid selection anchor');

  feedback.showStatus('live');
  const status = await harness.mount(harness.html`<${island.Status} feedback=${feedback} />`);
  assert.ok(status.container.querySelector('#status').classList.contains('live'));
  await Promise.all([usage.unmount(), meta.unmount(), badge.unmount(), status.unmount()]);
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
  assert.equal(harness.document.activeElement, ok);
  await harness.act(() => harness.fireEvent(ok, 'click'));
  assert.equal(await accepted, true);

  let cancelled;
  await harness.act(() => { cancelled = feedback.confirm({ title: 'Again?' }); });
  let escape;
  await harness.act(() => { escape = harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }); });
  assert.equal(await cancelled, false);
  assert.equal(escape.defaultPrevented, true);
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
  assert.equal(mounted.container.querySelector('.ga-regular'), regular);
  assert.equal(regular.querySelector('.actbot-working'), working);
  assert.equal(working.querySelector('.actbot-count'), count);
  assert.equal(count.textContent, '3');

  harness.document.body.classList.add('wizard');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:wizard', { detail: { active: true } },
  )));
  assert.equal(mounted.container.querySelector('.ga-regular'), regular,
    'theme wording changes do not remount hidden animation rows');
  assert.equal(regular.querySelector('.actbot-working'), working);
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
