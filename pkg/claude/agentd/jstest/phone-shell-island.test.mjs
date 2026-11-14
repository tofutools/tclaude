// js/phone-shell-island.js: the phone shell's bottom nav, own views and More
// sheet, driven through what the operator taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';

async function setup(t, { startOnNow = true, available = () => true, processes = null } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, island] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-shell-island.js'),
  ]);
  const state = createDashboardState();
  const shell = island.createPhoneShellState({ startOnNow });
  const calls = [];
  const record = (name) => (...args) => { calls.push([name, ...args]); };
  const actions = {
    notify: record('notify'), navigate: record('navigate'), showTab: record('showTab'),
    tabAvailable: available, openTerminal: record('openTerminal'), message: record('message'),
    openAccessRequest: record('openAccessRequest'), powerOnAll: record('powerOnAll'),
    shutdownAll: record('shutdownAll'), desktopSite: record('desktopSite'),
  };
  const { signal } = harness.signals;
  const boardSource = {
    attach: () => () => {},
    status: signal('ready'),
    error: signal(''),
    context: signal({ groups: [{ group: 'webshop', counts: { live: 1 }, tasks: [{ id: 'tk_1', title: 'Ship it', status: 'open', stage: 'next' }] }] }),
  };
  // The process read model Now reads its gates from; no runs unless a test
  // gives some.
  const processSource = {
    context: signal(processes), status: signal('ready'), error: signal(''), refreshes: 0,
    refresh() { this.refreshes += 1; return Promise.resolve(); },
  };
  state.beginRequest();
  state.commitRequest(1, {
    groups: [{ name: 'webshop', members: [{ agent_id: AGENT, conv_id: 'c1', title: 'checkout-ui', online: true, state: { status: 'awaiting_input' } }] }],
    messages_unread: 3, access_requests_pending: 0,
  });
  const mounted = await harness.mount(harness.html`<${island.PhoneShell} shell=${shell} state=${state} actions=${actions}
    boardSource=${boardSource} processSource=${processSource} fetchImpl=${async () => ({ ok: true })} />`);
  const root = harness.document.documentElement;
  const navItem = (label) => [...mounted.container.querySelectorAll('.phone-nav-item')].find((b) => b.textContent.includes(label));
  const tap = (element) => harness.act(() => element.click());
  return { harness, state, shell, calls, mounted, root, navItem, tap, processSource };
}

test('a fresh load lands on Now, with the counts on the nav', async (t) => {
  const { mounted, root, navItem } = await setup(t);
  const items = [...mounted.container.querySelectorAll('.phone-nav-item')];
  assert.deepEqual(items.map((b) => b.querySelector('.phone-nav-icon').nextSibling.textContent), ['Now', 'Agents', 'Mail', 'Board', 'More']);
  assert.equal(navItem('Now').getAttribute('aria-current'), 'page');
  assert.equal(root.dataset.phoneView, 'now', 'main is hidden under the own view');
  assert.equal(mounted.container.querySelector('.phone-bar-title').textContent, 'Now');
  assert.equal(navItem('Now').querySelector('.ui-badge').textContent, '1', 'one agent is asking');
  assert.equal(navItem('Mail').querySelector('.ui-badge').textContent, '3');
  assert.ok(mounted.container.querySelector('[aria-label="Needs you"]').textContent.includes('checkout-ui'));
});

test('Agents and Mail show their tab; Now and Board come back over it', async (t) => {
  const { harness, shell, calls, mounted, root, navItem, tap, state } = await setup(t);
  await tap(navItem('Mail'));
  assert.deepEqual(calls.at(-1), ['showTab', 'messages']);
  assert.equal(shell.view.value, null);
  assert.equal(root.dataset.phoneView, undefined, 'the tab shows');

  await tap(navItem('Board'));
  assert.equal(root.dataset.phoneView, 'board');
  assert.equal(navItem('Board').getAttribute('aria-current'), 'page');
  const task = mounted.container.querySelector('.phone-task');
  assert.ok(task.textContent.includes('Ship it'));
  await tap(task);
  assert.deepEqual(calls.at(-1), ['navigate', '/?task=tk_1%3Awebshop']);

  // A tab activation from anywhere (a deep link, Back, the task's dialog)
  // clears the own view.
  await tap(navItem('Now'));
  assert.equal(root.dataset.phoneView, 'now');
  await harness.act(() => state.setActiveTab('config'));
  assert.equal(shell.view.value, null);
  assert.equal(mounted.container.querySelector('.phone-bar-title').textContent, 'Settings');
  assert.equal(navItem('More').getAttribute('aria-current'), 'page', 'a More tab lights More');
});

test('a deep link starts on its tab, not Now', async (t) => {
  const { root, navItem } = await setup(t, { startOnNow: false });
  assert.equal(root.dataset.phoneView, undefined);
  assert.equal(navItem('Agents').getAttribute('aria-current'), 'page', 'the default tab is Groups');
});

test('More lists the tabs the dashboard shows, and the Desktop site switch', async (t) => {
  const { harness, calls, mounted, navItem, tap } = await setup(t, { available: (tab) => tab !== 'plugins' });
  await tap(navItem('More'));
  const sheet = mounted.container.querySelector('[role="dialog"]');
  assert.ok(sheet, 'the sheet opens');
  const labels = [...sheet.querySelectorAll('.phone-menu-item')].map((b) => b.textContent.trim());
  assert.ok(labels.some((l) => l.endsWith('Collab')));
  assert.ok(!labels.some((l) => l.endsWith('Plugins')), 'a hidden tab is left out');
  assert.ok(labels.some((l) => l.endsWith('Desktop site')));

  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(mounted.container.querySelector('[role="dialog"]'), null, 'Escape closes it');

  await tap(navItem('More'));
  const item = (label) => [...mounted.container.querySelectorAll('.phone-menu-item')].find((b) => b.textContent.trim().endsWith(label));
  await tap(item('Settings'));
  assert.deepEqual(calls.at(-1), ['navigate', '/config']);
  assert.equal(mounted.container.querySelector('[role="dialog"]'), null, 'picking an entry closes the sheet');
  await tap(navItem('More'));
  await tap(item('Desktop site'));
  assert.deepEqual(calls.at(-1), ['desktopSite']);
});

test('an asking agent on Now opens its terminal or a message', async (t) => {
  const { calls, mounted, tap, shell } = await setup(t);
  const row = mounted.container.querySelector('.phone-agent');
  await tap([...row.querySelectorAll('button')].find((b) => b.textContent === 'Open'));
  assert.equal(calls.at(-1)[0], 'openTerminal');
  assert.equal(calls.at(-1)[1].agentId, AGENT);
  assert.equal(shell.view.value, 'now', 'the terminal docks over Now; Now stays');
  await tap(row.querySelector('[aria-label="Message checkout-ui"]'));
  assert.equal(calls.at(-1)[0], 'message');
});

test('leaving Now for the tab already under it still shows that tab', async (t) => {
  // A fresh load lands on Now with Groups as the active tab: navigating to
  // Groups changes no tab, so the shell must leave the own view itself.
  const { shell, calls, mounted, root, tap } = await setup(t);
  const allAgents = [...mounted.container.querySelectorAll('.phone-link')].find((b) => b.textContent.includes('All agents'));
  await tap(allAgents);
  assert.deepEqual(calls.at(-1), ['navigate', '/']);
  assert.equal(shell.view.value, null);
  assert.equal(root.dataset.phoneView, undefined);
});

test('the More sheet is modal: the shell behind it is inert until it closes', async (t) => {
  const { harness, mounted, navItem, tap } = await setup(t);
  const more = navItem('More');
  more.focus();
  await tap(more);
  const nav = mounted.container.querySelector('.phone-nav');
  assert.equal(nav.inert, true, 'the bottom nav is inert under the sheet');
  assert.equal(mounted.container.querySelector('.phone-view').inert, true);
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(nav.inert, false);
  assert.equal(harness.document.activeElement, more, 'focus goes back to More');
});

test('a tab\'s pushed page takes the top bar and the bottom edge until it goes', async (t) => {
  const { harness, mounted, root, navItem, tap, state } = await setup(t);
  const { phonePage } = await harness.importDashboardModule('js/phone-page.js');
  await tap(navItem('Mail'));
  const calls = [];
  await harness.act(() => {
    phonePage.value = { title: 'Re: build', hideNav: true, onBack: () => calls.push('back'),
      actions: [{ key: 'term', icon: 'Terminal ›', label: 'Open terminal', onClick: () => calls.push('act') }] };
  });
  const bar = mounted.container.querySelector('.phone-bar');
  assert.ok(bar.classList.contains('phone-bar-page'));
  assert.equal(bar.querySelector('.phone-bar-title').textContent, 'Re: build');
  assert.equal(mounted.container.querySelector('.phone-nav'), null, 'the nav is hidden under the page');
  assert.equal(root.dataset.phonePage, 'full');
  await tap(bar.querySelector('.phone-bar-act'));
  await tap(bar.querySelector('.phone-bar-back'));
  assert.deepEqual(calls, ['act', 'back']);

  await harness.act(() => { phonePage.value = null; });
  assert.ok(mounted.container.querySelector('.phone-nav'), 'the nav comes back');
  assert.equal(root.dataset.phonePage, undefined);

  // An own view covers the page without dropping it, and picking the tab it
  // belongs to again shows it as it was: nothing else would put it back.
  await harness.act(() => state.setActiveTab('messages'));
  const page = { title: 'Re: build', actions: [] };
  await harness.act(() => { phonePage.value = page; });
  await tap(navItem('Board'));
  assert.equal(mounted.container.querySelector('.phone-bar-back'), null, 'Board has its own bar');
  await tap(navItem('Mail'));
  assert.equal(phonePage.value, page);
  assert.ok(mounted.container.querySelector('.phone-bar-back'), 'the page is back with its back button');

  // Another tab drops it, so it never outlives its tab.
  await tap(navItem('Agents'));
  assert.equal(phonePage.value, null);
  assert.equal(mounted.container.querySelector('.phone-bar-back'), null);
});

test('a process run waiting on the operator is a gate card on Now and counts as needing you', async (t) => {
  const processes = {
    runs: { run_1: { runId: 'run_1', name: 'Release train', status: 'running', needsYou: true, action: 'awaiting_decision', nodes: [
      { id: 'gate', label: 'Checks pass?', kind: 'decision', state: 'awaiting', performer: { kind: 'human' } },
    ], edges: [] } },
    groups: [{ group: 'webshop', needsYou: ['run_1'], active: ['run_1'], done: [] }],
  };
  const { mounted, navItem, processSource } = await setup(t, { processes });
  const needs = mounted.container.querySelector('[aria-label="Needs you"]');
  const card = needs.querySelector('.phone-gate-card');
  assert.ok(card, 'the gate has a card');
  assert.match(card.textContent, /Release train/);
  assert.match(card.textContent, /webshop/);
  assert.equal(navItem('Now').querySelector('.ui-badge').textContent, '2', 'the asking agent and the gate');
  assert.ok(processSource.refreshes >= 1, 'Now reads the runs while it shows');
});

test('a tab a phone does not offer gets a desktop-only page in its place', async (t) => {
  const { harness, state, calls, mounted, root, navItem, tap } = await setup(t, { startOnNow: false });
  await harness.act(() => state.setActiveTab('fleet-admin'));
  assert.equal(root.dataset.phoneView, 'desktop-only', 'the desktop content is hidden under it');
  assert.equal(mounted.container.querySelector('.phone-bar-title').textContent, 'Fleet admin');
  assert.match(mounted.container.querySelector('.phone-desktop-page').textContent, /Fleet admin is desktop only/);
  assert.equal(navItem('More').getAttribute('aria-current'), 'page');
  await tap(harness.getByRole(mounted.container, 'button', { name: 'Desktop site' }));
  assert.deepEqual(calls.at(-1), ['desktopSite']);

  await tap(navItem('Now'));
  assert.equal(root.dataset.phoneView, 'now', 'an own view still comes up over it');
  assert.equal(mounted.container.querySelector('.phone-desktop-page'), null);
  await harness.act(() => state.setActiveTab('access'));
  assert.equal(root.dataset.phoneView, undefined, 'a tab with a phone view shows');
});

test('a deep link left Groups on the route map: Agents and Show agents both go back to the list', async (t) => {
  const { harness, state, calls, mounted, navItem, tap } = await setup(t, { startOnNow: false });
  const { registerFeatureState } = await harness.importDashboardModule('js/feature-state-registry.js');
  const subview = harness.signals.signal('routes');
  t.after(registerFeatureState('groups', { subview }));
  await harness.act(() => state.setActiveTab('config'));
  await harness.act(() => state.setActiveTab('groups'));
  assert.match(mounted.container.querySelector('.phone-desktop-page').textContent, /Route map is desktop only/);
  await tap(harness.getByRole(mounted.container, 'button', { name: 'Show agents' }));
  assert.deepEqual(calls.at(-1), ['navigate', '/']);
  await tap(navItem('Agents'));
  assert.deepEqual(calls.at(-1), ['navigate', '/'], 'not a no-op showTab of the active tab');
  await harness.act(() => { subview.value = 'members'; });
  assert.equal(mounted.container.querySelector('.phone-desktop-page'), null);
});
