// js/phone-shell-island.js: the phone shell's bottom nav, own views and More
// sheet, driven through what the operator taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';

async function setup(t, { startOnNow = true, available = () => true, processes = null, fleetView = null } = {}) {
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
  // The Fleet island's state the node chip reads: this node alone unless a
  // test links peers.
  const fleet = { view: signal(fleetView || { fleet: null, node: { self: { id: 'inst_self', name: 'dev@box', color: '#58a6ff', local: true } } }) };
  state.beginRequest();
  state.commitRequest(1, {
    groups: [{ name: 'webshop', members: [{ agent_id: AGENT, conv_id: 'c1', title: 'checkout-ui', online: true, state: { status: 'awaiting_input' } }] }],
    messages_unread: 3, access_requests_pending: 0,
    messages: [{ id: 7, from_agent: AGENT, from_title: 'checkout-ui', group: 'webshop', subject: 'Checkout build is green', body: 'All checks pass.' }],
  });
  const mounted = await harness.mount(harness.html`<${island.PhoneShell} shell=${shell} state=${state} actions=${actions}
    boardSource=${boardSource} processSource=${processSource} fleet=${fleet} fetchImpl=${async () => ({ ok: true })} />`);
  const root = harness.document.documentElement;
  const navItem = (label) => [...mounted.container.querySelectorAll('.phone-nav-item')].find((b) => b.textContent.includes(label));
  const tap = (element) => harness.act(() => element.click());
  const bar = () => mounted.container.querySelector('.phone-bar');
  const sheet = () => mounted.container.querySelector('[role="dialog"]');
  return { harness, state, shell, calls, mounted, root, navItem, tap, processSource, bar, sheet };
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
  // Every nav icon is one colour: Agents' is drawn in currentColor, not the
  // 🤖 emoji, and no glyph is left to an emoji presentation.
  assert.equal(navItem('Agents').querySelector('.phone-nav-icon svg').getAttribute('stroke'), 'currentColor');
  for (const item of items) assert.doesNotMatch(item.querySelector('.phone-nav-icon').textContent, /\p{Extended_Pictographic}(?!\uFE0E)/u);
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
  const task = mounted.container.querySelector('.phone-board-card .phone-board-open');
  assert.ok(task.textContent.includes('Ship it'));
  const before = calls.length;
  await tap(task);
  // The task opens on its phone page over the Board: never the desktop's
  // task form, never another tab (B1, B2).
  assert.equal(calls.length, before, 'no navigation, no tab change');
  assert.equal(root.dataset.phoneView, 'board');
  assert.equal(mounted.container.querySelector('.phone-bar-title').textContent, 'webshop');
  assert.equal(mounted.container.querySelector('.phone-task-page-title').textContent, 'Ship it');
  assert.equal(navItem('Board').getAttribute('aria-current'), 'page');
  assert.equal(harness.document.activeElement?.tagName === 'TEXTAREA' || harness.document.activeElement?.tagName === 'INPUT', false,
    'nothing takes the focus, so a phone keeps its keyboard down');
  await tap(mounted.container.querySelector('.phone-bar-back'));
  assert.ok(mounted.container.querySelector('.phone-board-card'), 'Back comes back to the board');
  assert.equal(mounted.container.querySelector('.phone-bar-title').textContent.startsWith('Board'), true);
  // Opened again, it is its page again, with its bar.
  await tap(mounted.container.querySelector('.phone-board-card .phone-board-open'));
  assert.equal(mounted.container.querySelector('.phone-task-page-title')?.textContent, 'Ship it');
  assert.equal(mounted.container.querySelector('.phone-bar-title').textContent, 'webshop', 'the reopened page has its bar');
  await tap(mounted.container.querySelector('.phone-bar-back'));
  assert.ok(mounted.container.querySelector('.phone-board-card'));

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

test('landsOnNow: the bare dashboard, with or without a look param, lands on Now; a deep link does not', async (t) => {
  const harness = await createPreactHarness(t);
  const { landsOnNow } = await harness.importDashboardModule('js/phone-shell-island.js');
  const at = (pathname, search = '') => landsOnNow({ pathname, search });
  assert.equal(at('/'), true);
  assert.equal(at('/dashboard'), true);
  assert.equal(at('/', '?wizard=1'), true, 'the wizard link is the bare dashboard in its skin');
  assert.equal(at('/', '?slop=1'), true);
  assert.equal(at('/', '?layout=phone&wizard=1'), true);
  assert.equal(at('/agents'), false);
  assert.equal(at('/', '?node=inst_peer'), false, 'another node is a deep link');
  assert.equal(at('/', '?wizard=1&agent=agt_1'), false);
});

test('More lists the tabs the dashboard shows with their scope, notifications and the Desktop site switch', async (t) => {
  const { harness, calls, mounted, navItem, tap } = await setup(t, { available: (tab) => tab !== 'plugins' });
  await tap(navItem('More'));
  const sheet = mounted.container.querySelector('[role="dialog"]');
  assert.ok(sheet, 'the sheet opens');
  const label = (b) => b.querySelector('.phone-menu-label').textContent.trim();
  const items = [...sheet.querySelectorAll('.phone-menu-item')];
  const labels = items.map(label);
  assert.deepEqual(labels.slice(0, 7), ['Open sessions', 'Usage', 'Automations', 'Access', 'Diagnostics', 'Settings', 'Fleet'],
    'Collab is the open sessions list, and a hidden tab (Plugins) is left out');
  const scope = (name) => items.find((b) => label(b) === name).querySelector('.phone-scope');
  assert.equal(scope('Usage').textContent, 'read-only');
  assert.ok(scope('Usage').classList.contains('phone-scope-ro'));
  assert.equal(scope('Access').textContent, 'approve, revoke');
  assert.ok(scope('Access').classList.contains('phone-scope-simp'));
  assert.equal(scope('Fleet').textContent, 'nodes, read-only');
  const end = (name) => items.find((b) => label(b) === name).querySelector('.phone-menu-note')?.textContent;
  assert.equal(end('Notifications'), 'off', 'the snapshot says notifications are off');
  assert.equal(end('Desktop site'), 'this device');

  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(mounted.container.querySelector('[role="dialog"]'), null, 'Escape closes it');

  await tap(navItem('More'));
  const item = (name) => [...mounted.container.querySelectorAll('.phone-menu-item')].find((b) => label(b) === name);
  await tap(item('Settings'));
  assert.deepEqual(calls.at(-1), ['navigate', '/config']);
  assert.equal(mounted.container.querySelector('[role="dialog"]'), null, 'picking an entry closes the sheet');
  // Fleet's anchor is always hidden: it is offered anyway, and opened through it.
  await tap(navItem('More'));
  await tap(item('Fleet'));
  assert.deepEqual(calls.at(-1), ['showTab', 'fleet-admin']);
  await tap(navItem('More'));
  await tap(item('Notifications'));
  assert.deepEqual(calls.at(-1), ['navigate', '/config/notifications']);
  await tap(navItem('More'));
  await tap(item('Desktop site'));
  assert.deepEqual(calls.at(-1), ['desktopSite']);
});

test('More: Diagnostics shows the Doctor\'s counts, and Notifications says on', async (t) => {
  const harness = await createPreactHarness(t);
  const { MoreMenu } = await harness.importDashboardModule('js/phone-more.js');
  const { signal } = harness.signals;
  const counts = signal({ decision: 1, blocking: 0 });
  const state = { snapshot: signal({ notifications_enabled: true }) };
  const actions = { tabAvailable: () => true, navigate: () => {}, showTab: () => {} };
  const mounted = await harness.mount(harness.html`<${MoreMenu} state=${state} actions=${actions} close=${() => {}} doctor=${{ counts }} />`);
  const entry = (name) => [...mounted.container.querySelectorAll('.phone-menu-item')]
    .find((b) => b.querySelector('.phone-menu-label').textContent === name);
  const badges = () => [...entry('Diagnostics').querySelectorAll('.phone-more-count')].map((b) => [b.textContent, b.classList.contains('ui-badge-danger')]);
  assert.deepEqual(badges(), [['1', false]], 'a decision waiting is a blue count');
  await harness.act(() => { counts.value = { decision: 0, blocking: 2 }; });
  assert.deepEqual(badges(), [['2', true]], 'blocking problems are a red count');
  await harness.act(() => { counts.value = { decision: 0, blocking: 0 }; });
  assert.deepEqual(badges(), []);
  assert.equal(entry('Notifications').querySelector('.phone-menu-note').textContent, 'on');
});

test('an asking agent on Now is a card that opens its terminal', async (t) => {
  const { harness, calls, mounted, tap, shell } = await setup(t);
  const card = harness.getByRole(mounted.container, 'group', { name: 'checkout-ui is asking' });
  await tap(harness.getByRole(card, 'button', { name: 'Open' }));
  assert.equal(calls.at(-1)[0], 'openTerminal');
  assert.equal(calls.at(-1)[1].agentId, AGENT);
  assert.equal(shell.view.value, 'now', 'the terminal docks over Now; Now stays');
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

test('the More sheet sits above the nav: More lit, the rest of the shell inert, More again closes it', async (t) => {
  const { harness, mounted, navItem, tap } = await setup(t);
  const more = navItem('More');
  more.focus();
  await tap(more);
  const nav = mounted.container.querySelector('.phone-nav');
  const sheet = () => mounted.container.querySelector('.phone-sheet');
  assert.ok(sheet().classList.contains('phone-sheet-over-nav'));
  assert.notEqual(nav.inert, true, 'the bottom nav stays in reach');
  assert.equal(mounted.container.querySelector('.phone-view').inert, true);
  assert.equal(mounted.container.querySelector('.phone-bar').inert, true);
  assert.equal(navItem('More').getAttribute('aria-current'), 'page', 'More is lit while its sheet is up');
  assert.equal(navItem('Now').getAttribute('aria-current'), null);
  assert.equal(navItem('More').getAttribute('aria-expanded'), 'true');
  assert.equal(harness.document.documentElement.dataset.phoneSheet, 'over-nav', 'the nav stays on top of it');
  await tap(navItem('More'));
  assert.equal(sheet(), null, 'More again closes it');
  assert.equal(harness.document.documentElement.dataset.phoneSheet, undefined);
  assert.equal(navItem('Now').getAttribute('aria-current'), 'page');
  assert.equal(navItem('More').getAttribute('aria-expanded'), 'false');
  await tap(more);
  assert.equal(sheet().getAttribute('aria-modal'), null);
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(sheet(), null);
  assert.equal(harness.document.activeElement, more, 'focus goes back to More');
  // Another slot closes More and goes there, keeping the focus.
  await tap(more);
  assert.equal(sheet().getAttribute('aria-modal'), null, 'not modal: the nav stays in reach for a screen reader too');
  navItem('Board').focus();
  await tap(navItem('Board'));
  assert.equal(sheet(), null);
  assert.equal(navItem('Board').getAttribute('aria-current'), 'page');
  assert.equal(harness.document.activeElement, navItem('Board'), 'focus stays on the slot tapped');
});

test('a sheet from the top bar is modal: the nav is inert under it', async (t) => {
  const { harness, mounted, navItem, tap } = await setup(t);
  await tap(navItem('Agents'));
  await tap(mounted.container.querySelector('.phone-bar [aria-label="Search"]'));
  const nav = mounted.container.querySelector('.phone-nav');
  assert.equal(nav.inert, true);
  assert.equal(mounted.container.querySelector('.phone-sheet').classList.contains('phone-sheet-over-nav'), false);
  assert.equal(mounted.container.querySelector('.phone-sheet').getAttribute('aria-modal'), 'true');
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(nav.inert, false);
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
  await harness.act(() => state.setActiveTab('vegas'));
  assert.equal(root.dataset.phoneView, 'desktop-only', 'the desktop content is hidden under it');
  assert.equal(mounted.container.querySelector('.phone-bar-title').textContent, 'Vegas');
  assert.match(mounted.container.querySelector('.phone-desktop-page').textContent, /Vegas is desktop only/);
  assert.equal(mounted.container.querySelector('.phone-bar-back'), null, 'no phone view under it to go back to');
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
  calls.length = 0;
  await tap(mounted.container.querySelector('.phone-bar .phone-bar-back'));
  assert.deepEqual(calls.at(-1), ['navigate', '/'], 'the bar\'s ‹ goes back to the list too, as in the mock');
  await tap(navItem('Agents'));
  assert.deepEqual(calls.at(-1), ['navigate', '/'], 'not a no-op showTab of the active tab');
  await harness.act(() => { subview.value = 'members'; });
  assert.equal(mounted.container.querySelector('.phone-desktop-page'), null);
});

test('More → Fleet over Now shows Fleet even when it is the tab underneath', async (t) => {
  const { harness, shell, calls, mounted, navItem, tap, state } = await setup(t);
  await harness.act(() => state.setActiveTab('fleet-admin'));
  await harness.act(() => { shell.view.value = 'now'; });
  await tap(navItem('More'));
  const fleet = [...mounted.container.querySelectorAll('.phone-menu-item')].find((b) => b.querySelector('.phone-menu-label').textContent === 'Fleet');
  await tap(fleet);
  assert.deepEqual(calls.at(-1), ['showTab', 'fleet-admin']);
  assert.equal(shell.view.value, null, 'Now steps aside for the tab');
});

test('each root view has its own top-bar actions: node chip and 🔍 on Now, 🔍 on Agents, and no ⏻', async (t) => {
  const { harness, bar, navItem, tap } = await setup(t);
  const named = (name) => harness.getByRole(bar(), 'button', { name });
  assert.ok(named('Search'), 'Now has search');
  assert.equal(bar().querySelector('.phone-node-chip').textContent.trim(), '⌂dev@box', 'and names the node on screen');
  assert.equal(bar().querySelector('[aria-label="Power"]'), null);
  await tap(navItem('Agents'));
  assert.equal(bar().querySelector('[aria-label="Power"]'), null, 'powering on or off is in ☰ More');
  assert.ok(named('Search'));
  assert.equal(bar().querySelector('.phone-node-chip'), null, 'the node chip is on Now and More only');
  await tap(navItem('Board'));
  assert.ok(named('Add a task'), 'Board adds its ＋');
  assert.ok(named('Board webshop, pick a group'), 'and the group in its title');
  assert.equal(bar().querySelector('[aria-label="Power"]'), null);
});

test('☰ More has the power actions', async (t) => {
  const { harness, calls, navItem, sheet, tap } = await setup(t);
  await tap(navItem('More'));
  assert.ok(harness.getByRole(sheet(), 'button', { name: /Shut down all…$/ }));
  await tap(harness.getByRole(sheet(), 'button', { name: /Power on all$/ }));
  assert.deepEqual(calls.at(-1), ['powerOnAll']);
  assert.equal(sheet(), null);
});

test('a tab view adds bar actions and a title menu while it is the one on screen', async (t) => {
  const { harness, bar, navItem, tap, state } = await setup(t, { startOnNow: false });
  const { phoneBar } = await harness.importDashboardModule('js/phone-page.js');
  const calls = [];
  await harness.act(() => state.setActiveTab('messages'));
  await harness.act(() => {
    phoneBar.value = { tab: 'messages', menu: { sub: 'Human', label: 'Pick a mailbox', onClick: () => calls.push('menu') },
      actions: [{ key: 'read', icon: '✓✓', label: 'Mark all read', onClick: () => calls.push('read') }] };
  });
  await tap(harness.getByRole(bar(), 'button', { name: 'Mark all read' }));
  await tap(harness.getByRole(bar(), 'button', { name: 'Mail Human, Pick a mailbox' }));
  assert.deepEqual(calls, ['read', 'menu']);
  assert.match(bar().querySelector('.phone-bar-title').textContent, /Mail\s*Human ▾/);
  assert.equal(bar().querySelector('[aria-label="Search"]'), null, "Mail's 🔍 is its own, searching the open mailbox");
  await harness.act(() => state.setActiveTab('groups'));
  assert.equal(bar().querySelector('[aria-label="Mark all read"]'), null, 'another tab does not show it');
  await tap(navItem('Board'));
  await harness.act(() => state.setActiveTab('messages'));
  assert.ok(bar().querySelector('[aria-label="Mark all read"]'), 'its tab shows it again');
});

test('🔍 finds agents, groups, mail and tasks, and opens what is tapped', async (t) => {
  const { harness, calls, bar, sheet, tap, shell } = await setup(t);
  await tap(harness.getByRole(bar(), 'button', { name: 'Search' }));
  const field = harness.getByRole(sheet(), 'textbox', { name: 'Search' });
  assert.equal(harness.document.activeElement, field, 'the field has focus, so the keyboard comes up');
  await harness.input(field, 'checkout');
  const kinds = [...sheet().querySelectorAll('.phone-section')].map((s) => s.getAttribute('aria-label'));
  assert.deepEqual(kinds, ['Agents', 'Mail']);
  await tap(sheet().querySelector('[aria-label="Agents"] .phone-search-hit'));
  assert.equal(calls.at(-1)[0], 'openTerminal');
  assert.equal(calls.at(-1)[1].agentId, AGENT);
  assert.equal(sheet(), null, 'a pick closes the sheet');

  await tap(harness.getByRole(bar(), 'button', { name: 'Search' }));
  await harness.input(harness.getByRole(sheet(), 'textbox', { name: 'Search' }), 'ship');
  await tap(sheet().querySelector('[aria-label="Tasks"] .phone-search-hit'));
  assert.equal(shell.view.value, 'board', 'it opens the task over the Board');
  assert.equal(bar().querySelector('.phone-bar-title').textContent, 'webshop');
  assert.ok(bar().querySelector('.phone-bar-back'), "the task's page, with its back button");
});

test('a search for a group opens Agents on that group', async (t) => {
  const { harness, calls, bar, sheet, tap } = await setup(t);
  const { agentsFilter } = await harness.importDashboardModule('js/phone-agents-model.js');
  t.after(() => { agentsFilter.value = []; });
  await tap(harness.getByRole(bar(), 'button', { name: 'Search' }));
  await harness.input(harness.getByRole(sheet(), 'textbox', { name: 'Search' }), 'webshop');
  await tap(sheet().querySelector('[aria-label="Groups"] .phone-search-hit'));
  assert.deepEqual(calls.at(-1), ['showTab', 'groups']);
  assert.deepEqual(agentsFilter.value, ['g:webshop']);
});

test('with linked nodes the node chip opens a picker, on Now and atop More', async (t) => {
  const self = { id: 'inst_self', name: 'dev@box', color: '#58a6ff', local: true, online: true };
  const peers = [
    { id: 'inst_lap', name: 'laptop', color: '#a371f7', local: false, online: true, level: 'restricted' },
    { id: 'inst_nas', name: 'nas', color: '#3fb950', local: false, online: false, level: 'restricted' },
  ];
  const { harness, bar, sheet, navItem, tap } = await setup(t, { fleetView: { fleet: { self, peers }, node: { self } } });
  await tap(harness.getByRole(bar(), 'button', { name: 'Node: dev@box. Pick a node' }));
  assert.equal(sheet().getAttribute('aria-label'), 'Nodes');
  const rows = [...sheet().querySelectorAll('.phone-node-row')];
  assert.deepEqual(rows.map((r) => r.querySelector('.phone-node-name').textContent), ['dev@box', 'laptop', 'nas']);
  assert.equal(rows[0].getAttribute('aria-current'), 'page');
  assert.equal(rows[2].disabled, true, 'an offline peer cannot be opened');
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));

  await tap(navItem('More'));
  assert.ok(sheet().querySelector('.phone-more-node .phone-node-chip'), 'More names the node too');
});

test('Agents\' Board › brings the Board up over the tab, on that group', async (t) => {
  const { harness, shell, mounted, navItem, tap, root } = await setup(t);
  await tap(navItem('Agents'));
  assert.equal(shell.view.value, null);
  const board = await harness.importDashboardModule('js/phone-board.js');
  await harness.act(() => board.openBoardGroup('webshop'));
  assert.equal(shell.view.value, 'board');
  assert.equal(root.dataset.phoneView, 'board');
  assert.ok(mounted.container.querySelector('.phone-bar-title').textContent.includes('webshop'));
});

test('Performance is desktop only: leaving it, Diagnostics opens on the Doctor again', async (t) => {
  const { harness, state } = await setup(t, { startOnNow: false });
  const { activitySubtab } = await harness.importDashboardModule('js/activity-state.js');
  await harness.act(() => { activitySubtab.value = 'performance'; state.setActiveTab('diagnostics'); });
  assert.ok(harness.document.querySelector('.phone-desktop-page'), 'the desktop-only page');
  await harness.act(() => state.setActiveTab('groups'));
  assert.equal(activitySubtab.value, 'doctor');
  await harness.act(() => state.setActiveTab('diagnostics'));
  assert.equal(harness.document.querySelector('.phone-desktop-page'), null, 'back on Diagnostics, not the desktop-only page');
});

// js/phone-nav-toggle.js: a terminal docked over the dashboard shows the
// nav under it unless the operator hid it there (▾/▴) or the keyboard is
// up; a view picked in it closes the terminal first. The toggle carries the
// nav's count while the nav is hidden.
test('a terminal keeps the nav under it unless hidden or the keyboard is up; a slot closes it', async (t) => {
  const { harness, mounted, root, navItem, tap, calls } = await setup(t);
  const toggles = await harness.importDashboardModule('js/phone-nav-toggle.js');
  const prefs = new Map();
  const store = { getItem: (k) => prefs.get(k) ?? null, setItem: (k, v) => prefs.set(k, v), removeItem: (k) => prefs.delete(k) };
  toggles.resetNavToggleForTest();
  t.after(() => toggles.resetNavToggleForTest());
  const nav = () => mounted.container.querySelector('.phone-nav');
  let closed = 0;
  await harness.act(() => { toggles.terminalDock.value = { keyboard: false, close: () => { closed += 1; toggles.terminalDock.value = null; } }; });
  assert.ok(nav(), 'shown by default under a terminal');
  assert.equal(root.dataset.phoneTerminalNav, '', 'the terminal leaves the nav its room');
  assert.equal(toggles.navBadge.value, 4, 'the nav\'s count: 1 asking + 3 unread');

  await harness.act(() => toggles.setNavShown('terminal', false, store));
  assert.equal(nav(), null, 'hidden by the toggle');
  assert.equal(root.dataset.phoneTerminalNav, undefined);
  assert.equal(prefs.get('tclaude.dash.phone.nav.terminal'), 'hidden', 'remembered');
  assert.equal(toggles.navShown('mail', store), true, 'mail keeps its own choice');

  await harness.act(() => toggles.setNavShown('terminal', true, store));
  assert.equal(prefs.has('tclaude.dash.phone.nav.terminal'), false);
  await harness.act(() => { toggles.terminalDock.value = { ...toggles.terminalDock.value, keyboard: true }; });
  assert.equal(nav(), null, 'never over a raised keyboard');
  await harness.act(() => { toggles.terminalDock.value = { ...toggles.terminalDock.value, keyboard: false }; });

  await tap(navItem('Mail'));
  assert.equal(closed, 1, 'picking a view closes the terminal');
  assert.deepEqual(calls.at(-1), ['showTab', 'messages']);
  assert.equal(root.dataset.phoneTerminalNav, undefined);
});

test('the toggle: ▾ while the nav shows, ▴ with the nav\'s count while hidden', async (t) => {
  const harness = await createPreactHarness(t);
  const toggles = await harness.importDashboardModule('js/phone-nav-toggle.js');
  toggles.resetNavToggleForTest();
  t.after(() => toggles.resetNavToggleForTest());
  const prefs = new Map([['tclaude.dash.phone.nav.mail', 'hidden']]);
  const store = { getItem: (k) => prefs.get(k) ?? null, setItem: (k, v) => prefs.set(k, v), removeItem: (k) => prefs.delete(k) };
  toggles.navBadge.value = 7;
  const mounted = await harness.mount(harness.html`<div>
    <${toggles.NavToggle} kind="terminal" prefs=${store} />
    <${toggles.NavToggle} kind="mail" prefs=${store} />
  </div>`);
  const [term, mail] = mounted.container.querySelectorAll('.phone-nav-toggle');
  assert.equal(term.textContent.trim(), '▾');
  assert.equal(term.getAttribute('aria-label'), 'Hide page selector');
  assert.equal(term.getAttribute('aria-expanded'), 'true');
  assert.equal(term.querySelector('.ui-badge'), null, 'no count while the nav shows it');
  assert.ok(mail.classList.contains('off'), 'mail was hidden last time');
  assert.equal(mail.getAttribute('aria-expanded'), 'false');
  assert.equal(mail.querySelector('.ui-badge').textContent, '7');
  assert.equal(mail.getAttribute('aria-label'), 'Show page selector (7 waiting)');
  await harness.act(() => { toggles.navBadge.value = 0; });
  assert.equal(mail.querySelector('.ui-badge'), null, 'nothing waiting, no badge');
  await harness.act(() => mail.click());
  assert.equal(mail.getAttribute('aria-expanded'), 'true');
  assert.equal(prefs.has('tclaude.dash.phone.nav.mail'), false);

  // Prefs that throw: the choice still holds for the session.
  toggles.resetNavToggleForTest();
  const broken = { getItem: () => { throw new Error('no'); }, setItem: () => { throw new Error('no'); }, removeItem: () => { throw new Error('no'); } };
  assert.equal(toggles.navShown('terminal', broken), true);
  toggles.setNavShown('terminal', false, broken);
  assert.equal(toggles.navShown('terminal', broken), false);
});

test('the Steward\'s ✦ button floats over Now and Agents only', async (t) => {
  const { mounted, navItem, tap, shell } = await setup(t);
  const fab = () => mounted.container.querySelector('.phone-steward-fab');
  assert.ok(fab(), 'on Now');
  await tap(navItem('Agents'));
  assert.ok(fab(), 'on Agents');
  await tap(navItem('Board'));
  assert.equal(fab(), null, 'not on Board');
  shell.view.value = 'now';
});

test('a floating terminal keeps the view and its nav, and a nav pick leaves it open', async (t) => {
  const { harness, mounted, navItem, tap } = await setup(t);
  const nav = await harness.importDashboardModule('js/phone-nav-toggle.js');
  let closed = 0;
  await harness.act(() => { nav.terminalDock.value = { keyboard: false, float: true, close: () => { closed += 1; } }; });
  t.after(() => { nav.terminalDock.value = null; });
  assert.ok(mounted.container.querySelector('.phone-nav'), 'the nav stays');
  assert.equal(harness.document.documentElement.dataset.phoneTerminalNav, undefined, 'not the docked terminal\'s nav');
  await tap(navItem('Agents'));
  assert.equal(closed, 0, 'the floating terminal stays open');
  await tap(navItem('Board'));
  assert.equal(closed, 1, 'another view takes a floating one down: it would cover its controls');
  await harness.act(() => { nav.terminalDock.value = { keyboard: false, float: false, close: () => { closed += 1; } }; });
  await tap(navItem('Now'));
  assert.equal(closed, 2, 'a docked one closes');
});
