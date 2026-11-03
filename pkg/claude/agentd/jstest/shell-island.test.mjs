import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
import { createPreactHarness, getByLabelText, getByRole } from './preact-harness.mjs';

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

test('Copilot header selector switches percentage, what-if cost, and AIC', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { dashPrefs }, { Usage }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const pref = 'tclaude.dash.headerUsageMode.copilot';
  dashPrefs.removeItem(pref);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${Usage} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { usage: {
    copilot: { available: true, monthly: {
      pct: 38, used_units: 114, limit_units: 300, remaining: '18d',
    } },
    what_if_enabled: true,
    what_if_costs: [{ provider: 'github', total_cost_usd: 1.75 }],
  } }));

  const label = mounted.container.querySelector('.usrc');
  assert.equal(label.tagName, 'SPAN', 'provider label is descriptive text, not a second toggle');
  const selectorSlot = mounted.container.querySelector('.usage-unit-switch-slot');
  assert.equal(selectorSlot.nextElementSibling, label,
    'display-mode selector occupies the aligned gutter immediately before the provider label');
  assert.equal(mounted.container.querySelector('.usage-source').lastElementChild, label,
    'selector and provider remain one fixed source region');
  assert.ok(mounted.container.querySelector('.usage-values').contains(mounted.container.querySelector('.uw')),
    'mode-specific tokens remain inside the fixed data region');
  const buttons = () => [...mounted.container.querySelectorAll('.usage-unit-switch button')];
  assert.match(mounted.container.textContent, /38%/);
  assert.equal(mounted.container.querySelector('.uwhatif'), null, 'usage mode is not an estimate');
  await harness.act(() => buttons()[1].click());
  const marker = mounted.container.querySelector('.usrc .uwhatif');
  assert.equal(marker?.textContent, '?', 'cost mode marks the row WHAT-IF compactly');
  assert.match(marker.getAttribute('title'), /WHAT-IF estimate/, 'the tooltip explains the estimate');
  assert.equal(buttons()[1].textContent, '≈$', 'the selector identifies WHAT-IF dollar mode');
  assert.equal(mounted.container.querySelector('.ucost-amt').textContent, '$1.75',
    'the value does not repeat the selector approximation mark');
  assert.equal(dashPrefs.getItem(pref), 'cost');
  await harness.act(() => buttons()[2].click());
  assert.match(mounted.container.textContent, /114\/ 300 AIC/);
  assert.equal(dashPrefs.getItem(pref), 'units');
  await harness.act(() => buttons()[0].click());
  assert.match(mounted.container.textContent, /38%/);
  assert.equal(dashPrefs.getItem(pref), 'usage');

  dashPrefs.removeItem(pref);
  await mounted.unmount();
});

test('a fused view picks whose quota the header shows; peers come from their snapshots', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { dashPrefs }, { Usage }, { createFleetState }, { USAGE_NODE_PREF }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/shell-island.js'),
    harness.importDashboardModule('js/fleet-state.js'),
    harness.importDashboardModule('js/usage-nodes.js'),
  ]);
  dashPrefs.removeItem(USAGE_NODE_PREF);
  const fleet = createFleetState({ search: '?nodes=all' });
  const peer = (id, label) => ({ instance_id: id, label, trusted: true, online: true, level: 'restricted' });
  fleet.setStatus({ enabled: true, instance_id: 'inst_self', name: 'desk', hub: { state: 'connected' }, peers: [peer('inst_forge', 'forge'), peer('inst_mac0', 'mac')] });
  const fetched = [];
  const fetchImpl = async (path) => {
    fetched.push(path);
    if (path.includes('inst_mac0')) return { ok: true, json: async () => ({ usage: { available: false }, peer_view: { level: 'restricted', included: [], omitted: [{ feature: 'costs', requires: 'costs.read' }] } }) };
    return { ok: true, json: async () => ({ usage: { available: true, five_hour: { pct: 62, remaining: '2h' }, seven_day: { pct: 31, remaining: '4d' } }, peer_view: { level: 'unrestricted' } }) };
  };
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${Usage} state=${state} fleet=${fleet} fetchImpl=${fetchImpl} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { usage: { available: true, five_hour: { pct: 4, remaining: '4h' }, seven_day: { pct: 5, remaining: '4d9h' } } }));
  const c = mounted.container;
  const pill = c.querySelector('.usage-node-pill');
  assert.match(pill.textContent, /⌂ desk/, 'this node by default');
  assert.ok(c.querySelector('#usage.multiline'));
  assert.equal(c.querySelector('#usage').contains(pill), false, 'beside the two-row readout, not a row above it');
  assert.equal(pill.closest('.usage-with-node')?.lastElementChild?.id, 'usage');
  assert.match(c.textContent, /5%/);
  assert.deepEqual(fetched, [], 'no peer read until one is asked for');
  await harness.act(() => pill.click());
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  const items = () => [...c.querySelectorAll('.usage-node-item')];
  assert.deepEqual(items().map((i) => i.textContent.trim().replace(/\s+/g, ' ')), ['⌂ deskpeak 5%', 'forgepeak 62%', 'mac🔒']);
  assert.equal(items()[2].disabled, true, 'a peer that does not share costs cannot be picked');
  assert.match(items()[2].querySelector('.lock-why').dataset.why, /mac does not share usage or costs with you/);
  await harness.act(() => items()[1].click());
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  assert.match(c.querySelector('.usage-node-pill').textContent, /forge/);
  assert.match(c.textContent, /62%/, "forge's own quota, not a sum");
  assert.equal(dashPrefs.getItem(USAGE_NODE_PREF), 'inst_forge');
  dashPrefs.removeItem(USAGE_NODE_PREF);
  await mounted.unmount(); fleet.dispose();
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
    'reconnect destroys the overlay and animation subtree');
  await mounted.unmount();
});

test('footer open PRs disclosure pins, filters, and closes accessibly', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { dashPrefs }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
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
      { number: 3, url: 'https://github.com/acme/app/pull/3', title: 'Passes', repository: 'acme/app', agent_id: 'agt_3', draft: true, checks: { total: 1, passed: 1, state: 'passing' } },
    ],
  } }));
  const trigger = getByRole(mounted.container, 'button', { name: /Open PRs/ });
  assert.ok(mounted.container.querySelector('.open-prs.has-attention'),
    'the counter is highlighted while at least one PR needs attention');
  assert.equal(trigger.getAttribute('aria-expanded'), 'false');
  await harness.act(() => trigger.click());
  assert.equal(trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(mounted.container.querySelectorAll('.open-pr-row').length, 3);
  assert.ok(mounted.container.querySelector('.open-pr-row.is-draft .open-pr-state-draft'));
  assert.equal(mounted.container.querySelector('.open-pr-draft-label').textContent, 'Draft');
  assert.match(getByRole(mounted.container, 'button', { name: /Needs attention 1/ }).textContent, /Needs attention 1/);

  assert.equal(mounted.container.querySelector('.open-prs-include-drafts'), null,
    'the draft preference stays contextual to the attention view');
  await harness.act(() => getByRole(mounted.container, 'button', { name: /Needs attention 1/ }).click());
  const includeDrafts = getByLabelText(mounted.container, 'Include drafts');
  assert.equal(includeDrafts.hasAttribute('checked'), false);
  includeDrafts.checked = true;
  await harness.act(() => harness.fireEvent(includeDrafts, 'change'));
  assert.match(getByRole(mounted.container, 'button', { name: /Needs attention 2/ }).textContent, /Needs attention 2/);
  assert.equal(dashPrefs.getItem('tclaude.dash.open-prs.include-drafts-attention'), '1');

  await harness.act(() => getByRole(mounted.container, 'button', { name: /Unattached 1/ }).click());
  assert.deepEqual([...mounted.container.querySelectorAll('.open-pr-title')].map((node) => node.textContent), ['Runs']);

  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assertAbsent(mounted.container.querySelector('#open-prs-popover'));
  assertSameNode(harness.document.activeElement, trigger);
  await mounted.unmount();
});

test('footer open PR counter is neutral while only clean CI is running', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenPRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_prs: {
    available: true, total: 1, items: [
      { number: 1, url: 'https://github.com/acme/app/pull/1', checks: { total: 2, pending: 2, state: 'pending' } },
    ],
  } }));
  assert.ok(mounted.container.querySelector('.open-prs'));
  assertAbsent(mounted.container.querySelector('.open-prs.has-attention'),
    'an open PR with clean ongoing CI keeps the counter grey');
  await mounted.unmount();
});

test('footer open PRs gives the pointer the shared hover grace to reach the popover', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenPRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_prs: {
    available: true, total: 1, items: [
      { number: 1, url: 'https://github.com/acme/app/pull/1', title: 'Open', repository: 'acme/app' },
    ],
  } }));
  const root = mounted.container.querySelector('.open-prs');

  await harness.act(() => harness.fireEvent(root, 'mouseenter'));
  assert.ok(mounted.container.querySelector('#open-prs-popover'));
  await harness.act(() => harness.fireEvent(root, 'mouseleave'));
  assert.ok(mounted.container.querySelector('#open-prs-popover'),
    'leaving does not close the popover immediately');

  // Returning before the grace period expires cancels that close. A second
  // leave starts a fresh, complete grace period: 80% of it after that second
  // leave, the original timer would already have fired if it survived.
  const { HOVER_CLOSE_MS } = await harness.importDashboardModule('js/hover-popover.js');
  const wait = (ms) => harness.act(() => new Promise((resolve) => setTimeout(resolve, ms)));
  await wait(HOVER_CLOSE_MS * 0.4);
  await harness.act(() => harness.fireEvent(root, 'mouseenter'));
  await harness.act(() => harness.fireEvent(root, 'mouseleave'));
  await wait(HOVER_CLOSE_MS * 0.8);
  assert.ok(mounted.container.querySelector('#open-prs-popover'),
    're-entry cancels the old timer and the final leave gets a full grace period');
  await wait(HOVER_CLOSE_MS * 0.4);
  assertAbsent(mounted.container.querySelector('#open-prs-popover'));

  const trigger = getByRole(mounted.container, 'button', { name: /Open PRs/ });
  await harness.act(() => harness.fireEvent(root, 'mouseenter'));
  await harness.act(() => trigger.click());
  await harness.act(() => harness.fireEvent(root, 'mouseleave'));
  await harness.act(() => harness.fireEvent(harness.document.body, 'pointerdown'));
  assertAbsent(mounted.container.querySelector('#open-prs-popover'),
    'an explicit outside dismissal does not inherit the hover grace period');
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
  assert.equal(close.classList.contains('ui-btn-danger'), false);
  await harness.act(() => close.click());
  assert.equal(await closed, true);
  await mounted.unmount();
});

test('a confirmation starts focus on its action (so a plain Enter confirms) and Escape cancels', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createShellState }, { Confirm }] = await Promise.all([
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const feedback = createShellState();
  const mounted = await harness.mount(harness.html`<${Confirm} feedback=${feedback} />`);
  let answered;
  await harness.act(() => { answered = feedback.confirm({ title: 'Move?', okLabel: 'Move agent' }); });
  assertSameNode(harness.document.activeElement, mounted.container.querySelector('#confirm-ok'));
  await harness.act(() => { harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }); });
  assert.equal(await answered, false);
  await harness.act(() => { answered = feedback.confirm({ title: 'Rotate?', okLabel: 'Rotate identity' }); });
  assertSameNode(harness.document.activeElement, mounted.container.querySelector('#confirm-ok'));
  await harness.act(() => mounted.container.querySelector('#confirm-ok').click());
  assert.equal(await answered, true);
  await mounted.unmount();
});

test('a confirmation renders its checkboxes and hands the choices back on the caller\'s objects', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createShellState }, { Confirm }] = await Promise.all([
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const feedback = createShellState();
  const mounted = await harness.mount(harness.html`<${Confirm} feedback=${feedback} />`);
  const checks = [{ label: 'Apply sandbox', checked: true }, { label: 'Apply environment', checked: true, hint: 'next restart' }];
  let answered;
  await harness.act(() => { answered = feedback.confirm({ title: 'Move?', checks }); });
  const boxes = [...mounted.container.querySelectorAll('.confirm-check input')];
  assert.equal(boxes.length, 2);
  assert.match(mounted.container.querySelector('.confirm-checks').textContent, /next restart/);
  boxes[0].focus();
  await harness.act(() => { boxes[0].checked = false; boxes[0].dispatchEvent(new harness.window.Event('change', { bubbles: true })); });
  assertSameNode(harness.document.activeElement, boxes[0], 'toggling keeps focus on the checkbox, not the action');
  await harness.act(() => mounted.container.querySelector('#confirm-ok').click());
  assert.equal(await answered, true);
  assert.deepEqual(checks.map((check) => check.checked), [false, true]);
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
  assert.match(mounted.container.querySelector('#global-activity').title, /heroes channeling/);
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

test('Codex snapshot usage renders with a saved cost mode and switches to percentage', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { dashPrefs }, { Usage }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const pref = 'tclaude.dash.headerUsageMode.codex';
  dashPrefs.setItem(pref, 'cost');
  t.after(() => dashPrefs.removeItem(pref));
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${Usage} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { usage: {
    codex: { available: true, five_hour: { pct: 27, remaining: '2h' } },
    what_if_enabled: true,
    what_if_costs: [{ provider: 'openai', total_cost_usd: 2.75 }],
  } }));
  assert.match(mounted.container.textContent, /Codex:/);
  assert.equal(mounted.container.querySelector('.ucost-amt').textContent, '$2.75');
  const percent = [...mounted.container.querySelectorAll('.usage-unit-switch button')]
    .find((button) => button.textContent === '%');
  await harness.act(() => percent.click());
  assert.match(mounted.container.textContent, /27%/);
  assert.equal(dashPrefs.getItem(pref), 'usage');
  await mounted.unmount();
});

test('footer open PRs ignores a PR after confirming, and brings it back', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { dashPrefs }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  dashPrefs.removeItem('tclaude.dash.open-prs.ignored');
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenPRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_prs: {
    available: true, total: 2, items: [
      { number: 1, url: 'https://github.com/acme/app/pull/1', title: 'Keep', repository: 'acme/app' },
      { number: 2, url: 'https://github.com/acme/app/pull/2', title: 'Stale idea', repository: 'acme/app' },
    ],
  } }));
  const c = mounted.container;
  const trigger = getByRole(c, 'button', { name: /Open PRs/ });
  await harness.act(() => trigger.click());
  const titles = () => [...c.querySelectorAll('.open-pr-title')].map((node) => node.textContent);

  const ignoreButton = getByRole(c, 'button', { name: /Ignore pull request #2/ });
  ignoreButton.focus();
  await harness.act(() => ignoreButton.click());
  const dialog = c.querySelector('[role="alertdialog"]');
  assert.ok(dialog, 'ignoring asks first');
  assert.match(dialog.textContent, /Stale idea/);
  await harness.act(() => harness.fireEvent(dialog, 'keydown', { key: 'Escape' }));
  assertAbsent(c.querySelector('[role="alertdialog"]'));
  assert.ok(c.querySelector('#open-prs-popover'), 'Escape cancels the dialog, not the popover');
  assert.equal(harness.document.activeElement?.getAttribute('aria-label'), 'Ignore pull request #2…',
    'focus returns to the button that opened the dialog');
  assert.deepEqual(titles(), ['Keep', 'Stale idea']);

  await harness.act(() => getByRole(c, 'button', { name: /Ignore pull request #2/ }).click());
  const forever = getByLabelText(c, 'Permanently');
  forever.checked = true;
  await harness.act(() => harness.fireEvent(forever, 'change'));
  await harness.act(() => getByRole(c, 'button', { name: /^Ignore$/ }).click());
  assertAbsent(c.querySelector('[role="alertdialog"]'));
  assert.deepEqual(titles(), ['Keep']);
  assert.match(c.querySelector('.open-prs-count').textContent, /^1$/);
  const stored = JSON.parse(dashPrefs.getItem('tclaude.dash.open-prs.ignored'));
  assert.equal(stored['acme/app#2'].until, 0, 'the choice is persisted as permanent');

  await harness.act(() => getByRole(c, 'button', { name: /Ignored 1/ }).click());
  assert.deepEqual(titles(), ['Stale idea']);
  assert.match(c.querySelector('.open-pr-meta').textContent, /ignored permanently/);
  await harness.act(() => getByRole(c, 'button', { name: /Bring back pull request #2/ }).click());
  assert.equal(dashPrefs.getItem('tclaude.dash.open-prs.ignored'), null);
  await harness.act(() => getByRole(c, 'button', { name: /Open 2/ }).click());
  assert.deepEqual(titles(), ['Keep', 'Stale idea']);
  await mounted.unmount();
});

test('footer open PR titles open the PR details card unless turned off', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { dashPrefs }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  dashPrefs.removeItem('tclaude.dash.open-prs.title-details');
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenPRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_prs: {
    available: true, total: 1, items: [
      { number: 4, url: 'https://github.com/acme/app/pull/4', title: 'Hover me', repository: 'acme/app' },
    ],
  } }));
  const c = mounted.container;
  await harness.act(() => getByRole(c, 'button', { name: /Open PRs/ }).click());
  const hoverRoot = () => c.querySelector('.open-pr-main > .prd-hover');
  assert.ok(hoverRoot(), 'on by default');
  await harness.act(() => harness.fireEvent(hoverRoot(), 'mouseenter'));
  const card = c.querySelector('.prd-panel');
  assert.ok(card, 'the shared PR details card opens on the title');
  assert.match(card.querySelector('.prd-title').textContent, /Hover me\s+#4/);
  assert.equal(card.querySelector('.prd-repo').textContent.trim(), 'acme/app');
  // mouseleave does not bubble: this leaves the card, not the open popover.
  await harness.act(() => hoverRoot().dispatchEvent(new (hoverRoot().ownerDocument.defaultView.Event)('mouseleave')));
  // The card lingers briefly after the pointer leaves; wait that out so the
  // Escape below reaches a title with no card open.
  const { HOVER_CLOSE_MS } = await harness.importDashboardModule('js/pr-checks-hover.js');
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, HOVER_CLOSE_MS + 20)));
  const title = c.querySelector('a.open-pr-title');
  await harness.act(() => harness.fireEvent(title, 'keydown', { key: 'Escape' }));
  assertAbsent(c.querySelector('#open-prs-popover'), 'Escape on a title with no card open closes the popover');
  await harness.act(() => getByRole(c, 'button', { name: /Open PRs/ }).click());

  const toggle = getByLabelText(c, 'Hover details');
  toggle.checked = false;
  await harness.act(() => harness.fireEvent(toggle, 'change'));
  assertAbsent(hoverRoot());
  assert.equal(c.querySelector('a.open-pr-title').textContent, 'Hover me', 'a plain link again');
  assert.equal(dashPrefs.getItem('tclaude.dash.open-prs.title-details'), '0');
  dashPrefs.removeItem('tclaude.dash.open-prs.title-details');
  await mounted.unmount();
});

test('footer open PRs tracks colleagues in their own tabs', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { dashPrefs }, { OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  dashPrefs.removeItem('tclaude.dash.open-prs.ignored');
  const saved = globalThis.fetch;
  const posts = [];
  let authors = ['alice', 'bob'];
  globalThis.fetch = async (url, init) => {
    if (String(url) !== '/api/open-prs/tracked-authors') return { ok: true, json: async () => ({}) };
    const body = JSON.parse(init.body);
    posts.push([String(url), body]);
    authors = body.action === 'add' ? [...authors, body.login] : authors.filter((a) => a !== body.login);
    return { ok: true, json: async () => ({ authors }) };
  };
  t.after(() => { globalThis.fetch = saved; });
  const pr = (n, repo, extra = {}) => ({ number: n, url: `https://github.com/${repo}/pull/${n}`, title: `PR ${n}`, repository: repo, ...extra });
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenPRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, {
    authored_open_prs: { available: true, total: 1, items: [pr(1, 'me/app')] },
    tracked_pr_authors: [
      { login: 'alice', available: true, total: 2, items: [pr(7, 'acme/infra', { agent_id: '' }), pr(8, 'acme/api')],
        recent_window_days: 3, recent: [pr(5, 'acme/api', { state: 'merged', closed_at: new Date().toISOString() })] },
      { login: 'bob', available: false, items: [], recent: [] },
    ],
  }));
  const c = mounted.container;
  const trigger = getByRole(c, 'button', { name: /Open PRs/ });
  assert.match(trigger.textContent, /Open PRs\s*1/, 'the trigger counts your PRs only');
  await harness.act(() => trigger.click());
  const titles = () => [...c.querySelectorAll('.open-pr-title')].map((node) => node.textContent);
  const tabs = () => [...c.querySelectorAll('[role="tab"]')].map((node) => node.textContent.replace(/\s+/g, ' ').trim());
  assert.deepEqual(tabs(), ['You 1', 'alice 2', 'bob …']);

  await harness.act(() => getByRole(c, 'tab', { name: /alice/ }).click());
  assert.match(c.querySelector('.open-prs-head strong').textContent, /alice’s pull requests/);
  assert.deepEqual(titles(), ['PR 7', 'PR 8']);
  assertAbsent([...c.querySelectorAll('.open-prs-filters button')].find((b) => /Unattached/.test(b.textContent)) || null,
    'a colleague tab has no Unattached filter');
  assert.ok([...c.querySelectorAll('.open-prs-filters button')].some((b) => /Closed 3d 1/.test(b.textContent)));

  await harness.act(() => getByRole(c, 'button', { name: /Ignore pull request #8/ }).click());
  await harness.act(() => getByRole(c, 'button', { name: /^Ignore$/ }).click());
  assert.deepEqual(titles(), ['PR 7']);
  assert.deepEqual(tabs(), ['You 1', 'alice 1', 'bob …'], 'ignoring works in a colleague tab and its count follows');
  await harness.act(() => getByRole(c, 'tab', { name: /You/ }).click());
  assertAbsent([...c.querySelectorAll('.open-prs-filters button')].find((b) => /Ignored/.test(b.textContent)) || null,
    "a colleague's ignore stays out of your Ignored list");

  await harness.act(() => getByRole(c, 'tab', { name: /bob/ }).click());
  assert.match(c.querySelector('.open-prs-empty').textContent, /Searching GitHub for bob’s pull requests/);

  await harness.act(() => getByRole(c, 'button', { name: /Add person/ }).click());
  const field = c.querySelector('input[aria-label="GitHub username"]');
  await harness.input(field, '@carol');
  await harness.act(async () => { harness.fireEvent(c.querySelector('.open-prs-add-form'), 'submit'); });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(posts.at(-1), ['/api/open-prs/tracked-authors', { action: 'add', login: 'carol' }]);
  assert.deepEqual(tabs(), ['You 1', 'alice 1', 'bob …', 'carol …'], 'the new tab shows before the snapshot has it');
  assert.equal(c.querySelector('[role="tab"][aria-selected="true"]').textContent.trim().startsWith('carol'), true);

  await harness.act(async () => { getByRole(c, 'button', { name: 'Stop tracking alice' }).click(); });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(posts.at(-1)[1], { action: 'remove', login: 'alice' });
  assert.deepEqual(tabs(), ['You 1', 'bob …', 'carol …']);
  dashPrefs.removeItem('tclaude.dash.open-prs.ignored');
  await mounted.unmount();
});

test('the header\'s global notification hint opens the quick reader on the first unread notification', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, island] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${island.GlobalActivity} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, {
    groups: [{ name: 'g', members: [{ conv_id: 'c', online: true, state: { status: 'working' } }] }],
    ungrouped: [],
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
    messages_unread: 1,
    // A sender-less daemon notice is newest, but the reader cannot key on it.
    messages: [
      { id: 10, from_title: 'Fleet health', read: false },
      { id: 9, from_agent: 'agt_x', from_conv: 'conv-x', from_title: 'x', read: false },
    ],
  }));
  const hint = mounted.container.querySelector('button.human-notification-hint');
  assert.ok(hint, 'the global hint is its own button');
  assertAbsent(mounted.container.querySelector('.activity-hover-trigger .human-notification-hint'),
    'the hint is not nested inside the activity trigger button');
  const opened = [];
  const listen = (event) => opened.push(event.detail);
  harness.document.addEventListener('tclaude:open-human-notification', listen);
  t.after(() => harness.document.removeEventListener('tclaude:open-human-notification', listen));
  harness.fireEvent(hint, 'click');
  assert.equal(opened.length, 1);
  assert.equal(opened[0].messageId, 9);
  assert.equal(opened[0].sender.agent, 'agt_x');
  await mounted.unmount();
});

test('footer open MRs lists GitLab merge requests next to open PRs', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenMRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenMRs} state=${state} />`);
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_prs: { available: true, total: 0, items: [] } }));
  assert.equal(mounted.container.querySelector('.open-mrs'), null, 'absent until the daemon has a GitLab list');

  const base = 'https://gitlab.example.test:8929/acme/app/-/merge_requests/';
  state.beginRequest();
  await harness.act(() => state.commitRequest(2, { authored_open_mrs: {
    available: true, total: 2, updated_at: '2026-11-02T10:00:00Z',
    items: [
      { number: 7, url: `${base}7`, title: 'Ready one', repository: 'acme/app', agent_title: 'builder', agent_id: 'agt_1',
        checks: { total: 1, passed: 1, state: 'passing' }, reviews: { approved: 2 } },
      { number: 8, url: `${base}8`, title: 'Stuck one', repository: 'acme/app', conflicts: true,
        checks: { total: 1, pending: 1, state: 'pending' }, reviews: { approved: 0 } },
    ],
    instances: [
      { host: 'gitlab.example.test:8929', url: 'https://gitlab.example.test:8929/dashboard/merge_requests?state=opened&author_username=jdoe' },
      { host: 'other.example.test', error: 'GitLab applied a rate limit; retry in 2m0s' },
    ],
  } }));
  const trigger = getByRole(mounted.container, 'button', { name: /Open MRs/ });
  assert.ok(mounted.container.querySelector('.open-mrs.has-attention'));
  await harness.act(() => trigger.click());
  assert.equal(trigger.getAttribute('aria-expanded'), 'true');
  const rows = [...mounted.container.querySelectorAll('.open-pr-row')];
  assert.equal(rows.length, 2);
  assert.match(rows[0].textContent, /acme\/app · !7 · builder/);
  assert.doesNotMatch(rows[0].textContent, /approval/, 'the review pill carries the approvals, not the meta line');
  assert.match(rows[1].textContent, /!8 · conflict · no active agent/);
  assert.equal(rows[0].querySelector('.ci-count').textContent, 'pipeline', 'one head pipeline, not a check count');
  const pill = rows[0].querySelector('.rv-badge');
  assert.ok(pill?.classList.contains('rv-approved'), 'a merge request has the same review pill as a pull request');
  assert.equal(pill.textContent.trim(), '2');
  assert.match(pill.getAttribute('aria-label'), /Reviews for !7 .*Opens the merge request on GitLab/);
  assert.ok(rows[0].querySelector('.prd-hover .open-pr-title'), 'the title opens the merge request\'s details card');
  const link = getByRole(mounted.container, 'link', { name: /Open all on gitlab\.example\.test:8929/ });
  assert.equal(link.getAttribute('href'), 'https://gitlab.example.test:8929/dashboard/merge_requests?state=opened&author_username=jdoe');
  assert.match(mounted.container.querySelector('.open-prs-stale').textContent, /other\.example\.test: GitLab applied a rate limit/);

  await harness.act(() => getByRole(mounted.container, 'button', { name: /Ready to merge 1/ }).click());
  assert.deepEqual([...mounted.container.querySelectorAll('.open-pr-title')].map((node) => node.textContent), ['Ready one']);
  await harness.act(() => getByRole(mounted.container, 'button', { name: /Unattached 1/ }).click());
  assert.deepEqual([...mounted.container.querySelectorAll('.open-pr-title')].map((node) => node.textContent), ['Stuck one']);

  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assertAbsent(mounted.container.querySelector('#open-mrs-popover'));
  assertSameNode(harness.document.activeElement, trigger);
  await mounted.unmount();
});

test('footer open MRs shows mine and all, and a tab per project for everyone\'s', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenMRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenMRs} state=${state} />`);
  const base = 'https://gitlab.example.test:8929';
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_mrs: {
    available: true, total: 0, items: [], everyone_total: 2,
    instances: [{ host: 'gitlab.example.test:8929', url: `${base}/dashboard/merge_requests?state=opened&author_username=jdoe`,
      everyone_error: 'GitLab applied a rate limit; retry in 1m30s' }],
    projects: [{ host: 'gitlab.example.test:8929', project: 'contoso/player', url: `${base}/contoso/player/-/merge_requests`,
      total: 2, agents: true, items: [
        { number: 249, url: `${base}/contoso/player/-/merge_requests/249`, title: 'Merge 1st', repository: 'contoso/player',
          author: 'martin', agent_title: 'builder', agent_id: 'agt_1', checks: { total: 1, passed: 1, state: 'passing' }, reviews: { approved: 1 } },
        { number: 250, url: `${base}/contoso/player/-/merge_requests/250`, title: 'Merge 2nd', repository: 'contoso/player',
          author: 'agent-bot', conflicts: true, checks: { total: 1, pending: 1, state: 'pending' } },
      ] }],
  } }));
  const trigger = getByRole(mounted.container, 'button', { name: /Open MRs/ });
  assert.match(trigger.textContent, /Open MRs\s*mine\s*0\s*all\s*2/, 'two numbers, each named');
  assert.ok(mounted.container.querySelector('.open-mrs.is-empty'), 'nothing of yours: the dot stays grey');
  assert.equal(mounted.container.querySelector('.open-mrs-all-count').textContent, '2');

  await harness.act(() => trigger.click());
  assert.match(mounted.container.querySelector('.open-prs-empty').textContent, /No open merge requests/);
  assert.equal(mounted.container.querySelector('.open-prs-stale'), null,
    'a failed project-wide read is not an instance failure on Mine');
  const tab = getByRole(mounted.container, 'tab', { name: /player 2/ });
  assert.equal(getByRole(mounted.container, 'tab', { name: /Mine 0/ }).getAttribute('aria-selected'), 'true');
  await harness.act(() => tab.click());
  assert.equal(tab.getAttribute('aria-selected'), 'true');
  assert.match(mounted.container.querySelector('.open-prs-head').textContent, /Open merge requests in contoso\/player/);
  assert.match(mounted.container.querySelector('.open-prs-stale').textContent,
    /Everyone's merge requests on gitlab\.example\.test:8929 are from an earlier read: GitLab applied a rate limit/);
  const rows = [...mounted.container.querySelectorAll('.open-pr-row')];
  assert.equal(rows.length, 2);
  assert.match(rows[0].textContent, /!249 · by martin · builder/);
  assert.equal(rows[0].querySelector('.rv-badge').textContent.trim(), '1', 'a project tab row has the review pill too');
  assert.match(rows[1].textContent, /!250 · by agent-bot · conflict · no active agent/);
  const link = getByRole(mounted.container, 'link', { name: /Open in GitLab/ });
  assert.equal(link.getAttribute('href'), `${base}/contoso/player/-/merge_requests`);
  await harness.act(() => getByRole(mounted.container, 'button', { name: /Ready to merge 1/ }).click());
  assert.deepEqual([...mounted.container.querySelectorAll('.open-pr-title')].map((node) => node.textContent), ['Merge 1st']);
  await mounted.unmount();
});

test('hover details is one setting across Open PRs and Open MRs', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenMRs, OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<div><${OpenPRs} state=${state} /><${OpenMRs} state=${state} /></div>`);
  const mr = 'https://gitlab.example.test:8929/acme/app/-/merge_requests/';
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, {
    authored_open_prs: { available: true, total: 1, items: [
      { number: 1, url: 'https://github.com/acme/app/pull/1', title: 'A PR', repository: 'acme/app', checks: { total: 1, passed: 1, state: 'passing' } },
    ] },
    authored_open_mrs: { available: true, total: 1, items: [
      { number: 2, url: `${mr}2`, title: 'An MR', repository: 'acme/app', checks: { total: 1, passed: 1, state: 'passing' } },
    ] },
  }));
  const prs = mounted.container.querySelector('.open-prs:not(.open-mrs)');
  const mrs = mounted.container.querySelector('.open-mrs');
  await harness.act(() => getByRole(mrs, 'button', { name: /Open MRs/ }).click());
  assert.ok(mrs.querySelector('.open-pr-row .prd-hover'), 'on by default');
  const box = getByLabelText(mrs, 'Hover details');
  box.checked = false;
  await harness.act(() => harness.fireEvent(box, 'change'));
  assert.equal(mrs.querySelector('.open-pr-row .prd-hover'), null, 'off: the title is a plain link');
  await harness.act(() => getByRole(prs, 'button', { name: /Open PRs/ }).click());
  assert.ok(!getByLabelText(prs, 'Hover details').checked, 'turning it off in Open MRs turns it off in Open PRs');
  assert.equal(prs.querySelector('.open-pr-row .prd-hover'), null);
  await mounted.unmount();
});

test('include drafts is one setting across Open PRs and Open MRs', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenMRs, OpenPRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<div><${OpenPRs} state=${state} /><${OpenMRs} state=${state} /></div>`);
  const mr = 'https://gitlab.example.test:8929/acme/app/-/merge_requests/';
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, {
    authored_open_prs: { available: true, total: 1, items: [
      { number: 1, url: 'https://github.com/acme/app/pull/1', title: 'Draft PR', repository: 'acme/app', draft: true, checks: { total: 1, failed: 1, state: 'failing' } },
    ] },
    authored_open_mrs: { available: true, total: 1, items: [
      { number: 2, url: `${mr}2`, title: 'Draft MR', repository: 'acme/app', draft: true, checks: { total: 1, failed: 1, state: 'failing' } },
    ] },
  }));
  const prs = mounted.container.querySelector('.open-prs:not(.open-mrs)');
  const mrs = mounted.container.querySelector('.open-mrs');
  await harness.act(() => getByRole(prs, 'button', { name: /Open PRs/ }).click());
  await harness.act(() => getByRole(prs, 'button', { name: /Needs attention 0/ }).click());
  const box = getByLabelText(prs, 'Include drafts');
  box.checked = true;
  await harness.act(() => harness.fireEvent(box, 'change'));
  assert.ok(mrs.classList.contains('has-attention'), 'ticking it in Open PRs counts the draft MR in Open MRs too');
  await mounted.unmount();
});

test('a project-wide read that never answered says so on Mine, and all reads ?', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, { OpenMRs }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/shell-island.js'),
  ]);
  const state = createDashboardState();
  const mounted = await harness.mount(harness.html`<${OpenMRs} state=${state} />`);
  const base = 'https://gitlab.example.test:8929';
  state.beginRequest();
  await harness.act(() => state.commitRequest(1, { authored_open_mrs: {
    available: true, total: 1, everyone_total: 0, projects: [],
    items: [{ number: 7, url: `${base}/acme/app/-/merge_requests/7`, title: 'Mine', repository: 'acme/app',
      checks: { total: 1, passed: 1, state: 'passing' } }],
    instances: [{ host: 'gitlab.example.test:8929', url: `${base}/dashboard/merge_requests?state=opened&author_username=jdoe`,
      everyone_error: 'context deadline exceeded' }],
  } }));
  const trigger = getByRole(mounted.container, 'button', { name: /Open MRs/ });
  assert.equal(mounted.container.querySelector('.open-mrs-all-count').textContent, '?', 'unknown, not zero');
  await harness.act(() => trigger.click());
  assert.match(mounted.container.querySelector('.open-prs-stale').textContent,
    /Everyone's merge requests on gitlab\.example\.test:8929 could not be read: context deadline exceeded/);
  assert.match(mounted.container.querySelector('.ci-badge').getAttribute('aria-label'), /Opens the pipeline on GitLab/,
    'a merge request badge names GitLab');
  await mounted.unmount();
});
