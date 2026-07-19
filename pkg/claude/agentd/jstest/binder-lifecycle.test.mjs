import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const dashboardStub = `
  export const lastSnapshot = { groups: [], ungrouped: [], paging: {} };
  export function sudoBadge() { return ''; }
  export function setLastSnapshot() {}
  export function webTerminalDefault() { return false; }
`;

test('tab installers are idempotent and stale cleanup cannot tear down a reinstall', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', dashboardStub);
  const { bindTabs, bindTabHotkeys } = await harness.importDashboardModule('js/refresh.js');
  harness.document.body.innerHTML = `
    <nav><a href="/groups" data-tab="groups" class="active">Groups</a><a href="/jobs" data-tab="jobs">Jobs</a></nav>
    <main><section id="tab-groups" class="active"></section><section id="tab-jobs"></section></main>`;
  const tabs = [...harness.document.querySelectorAll('nav [data-tab]')];
  for (const tab of tabs) Object.defineProperty(tab, 'offsetParent', { configurable: true, get: () => harness.document.body });
  let reselected = 0;
  harness.document.addEventListener('tclaude:tab-reselected', () => { reselected++; });

  const first = bindTabs();
  assert.equal(bindTabs(), first, 'a second install reuses the active cleanup');
  await harness.act(() => harness.fireEvent(tabs[1], 'click', { button: 0 }));
  assert.equal(reselected, 0, 'one click is handled exactly once');
  assert.equal(harness.document.querySelector('#tab-jobs').classList.contains('active'), true);

  first();
  const second = bindTabs();
  assert.notEqual(second, first);
  first();
  await harness.act(() => harness.fireEvent(tabs[0], 'click', { button: 0 }));
  assert.equal(harness.document.querySelector('#tab-groups').classList.contains('active'), true,
    'stale cleanup does not remove the new click listener');

  const hotkeysFirst = bindTabHotkeys();
  assert.equal(bindTabHotkeys(), hotkeysFirst);
  await harness.act(() => harness.fireEvent(harness.document.body, 'keydown', { key: ']' }));
  assert.equal(harness.document.querySelector('#tab-jobs').classList.contains('active'), true,
    'one hotkey advances exactly one visible tab');
  hotkeysFirst();
  const hotkeysSecond = bindTabHotkeys();
  hotkeysFirst();
  await harness.act(() => harness.fireEvent(harness.document.body, 'keydown', { key: '[' }));
  assert.equal(harness.document.querySelector('#tab-groups').classList.contains('active'), true,
    'stale hotkey cleanup does not remove the new listener');
  hotkeysSecond();
  second();
});

test('row root delegation installs once, cleans every listener, and survives stale cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', dashboardStub);
  await harness.replaceDashboardModule('js/row-action-handler.js', `
    export const handledActions = [];
    export function handleRowAction(action) { handledActions.push(action); }
  `);
  const { handledActions } = await harness.importDashboardModule('js/row-action-handler.js');
  const {
    actionDescriptor, bindRowActions, liveActionSource,
  } = await harness.importDashboardModule('js/row-actions.js');
  const producer = harness.document.body.appendChild(harness.document.createElement('button'));
  producer.id = 'frozen-producer';
  producer.dataset.act = 'documented-cross-feature-route';
  producer.dataset.conv = 'conv-before';
  assert.equal(liveActionSource({ target: producer }), producer);
  const descriptor = actionDescriptor(producer);
  producer.dataset.conv = 'conv-after';
  assert.deepEqual(descriptor, {
    producerId: 'frozen-producer',
    openInBackground: false,
    data: { act: 'documented-cross-feature-route', conv: 'conv-before' },
  }, 'delegation freezes plain data instead of retaining a live DOM producer');
  for (const modifier of ['ctrlKey', 'metaKey']) {
    assert.equal(actionDescriptor(producer, { [modifier]: true }).openInBackground, true,
      `${modifier} marks a delegated click as a background terminal request`);
  }
  assert.equal(Object.isFrozen(descriptor), true);
  assert.equal(Object.isFrozen(descriptor.data), true);
  producer.remove();
  assert.equal(liveActionSource({ target: producer }), null,
    'a detached producer cannot dispatch an operation after Preact replacement');
  const added = [];
  const removed = [];
  const add = harness.document.addEventListener.bind(harness.document);
  const remove = harness.document.removeEventListener.bind(harness.document);
  harness.document.addEventListener = (type, listener, options) => {
    added.push([type, listener]);
    add(type, listener, options);
  };
  harness.document.removeEventListener = (type, listener, options) => {
    removed.push([type, listener]);
    remove(type, listener, options);
  };

  const first = bindRowActions();
  assert.equal(bindRowActions(), first);
  assert.equal(added.filter(([type]) => type === 'click').length, 1);
  assert.equal(added.filter(([type]) => type === 'contextmenu').length, 1);
  assert.equal(added.filter(([type]) => type === 'keydown').length, 1);
  first();
  for (const [type, listener] of added.slice(0, 3)) {
    assert.ok(removed.some(([removedType, removedListener]) =>
      removedType === type && removedListener === listener));
  }

  const second = bindRowActions();
  first();
  const terminalAction = harness.document.body.appendChild(harness.document.createElement('button'));
  terminalAction.dataset.act = 'web-open-window';
  terminalAction.dataset.agent = 'agt_background';
  terminalAction.dataset.label = 'background';
  const macControlClick = harness.fireEvent(terminalAction, 'contextmenu', { ctrlKey: true });
  assert.equal(macControlClick.defaultPrevented, true,
    'macOS Control-click context gestures dispatch the terminal action instead of opening a menu');
  assert.equal(handledActions.length, 1);
  assert.equal(handledActions[0].openInBackground, true);
  harness.fireEvent(terminalAction, 'click', { ctrlKey: true });
  assert.equal(handledActions.length, 1,
    'WebKit follow-up click is suppressed after the context gesture dispatches the action');
  const ordinaryContextMenu = harness.fireEvent(terminalAction, 'contextmenu');
  assert.equal(ordinaryContextMenu.defaultPrevented, false,
    'ordinary terminal context menus remain available');
  const unrelated = harness.document.body.appendChild(harness.document.createElement('button'));
  unrelated.dataset.act = 'documented-cross-feature-route';
  const unrelatedModifiedMenu = harness.fireEvent(unrelated, 'contextmenu', { ctrlKey: true });
  assert.equal(unrelatedModifiedMenu.defaultPrevented, false,
    'modified context gestures do not activate unrelated row actions');
  const chip = harness.document.body.appendChild(harness.document.createElement('span'));
  chip.dataset.act = 'documented-cross-feature-route';
  chip.setAttribute('role', 'button');
  let clicks = 0;
  chip.click = () => { clicks++; };
  await harness.act(() => harness.fireEvent(chip, 'keydown', { key: 'Enter' }));
  assert.equal(clicks, 1, 'the reinstalled chip activation listener remains live');
  second();
});

test('static toolbar menu owns its persistent nodes across idempotent reinstalls', async (t) => {
  const harness = await createPreactHarness(t);
  harness.document.body.innerHTML = `<div class="filter-bar-cog">
    <button class="cog-btn" aria-expanded="false">menu</button>
    <div class="action-menu"><button id="toolbar-item">item</button></div>
  </div><button id="outside">outside</button>`;
  const { bindToolbarActionsMenu } = await harness.importDashboardModule('js/toolbar-actions-menu.js');
  const cog = harness.document.querySelector('.cog-btn');
  const menu = harness.document.querySelector('.action-menu');

  const first = bindToolbarActionsMenu();
  assert.equal(bindToolbarActionsMenu(), first);
  harness.fireEvent(cog, 'click', { button: 0 });
  assert.equal(menu.classList.contains('open'), true);
  assert.equal(cog.getAttribute('aria-expanded'), 'true');
  const item = harness.document.querySelector('#toolbar-item');
  item.focus();
  harness.fireEvent(harness.document.querySelector('#outside'), 'click', { button: 0 });
  assert.equal(menu.classList.contains('open'), false, 'light dismissal is menu-owned');
  assert.equal(harness.document.activeElement, cog,
    'click-away returns focus when it was still inside the dismissed menu');

  harness.fireEvent(cog, 'click', { button: 0 });
  item.focus();
  harness.fireEvent(item, 'click', { button: 0 });
  assert.equal(menu.classList.contains('open'), false, 'menu items dismiss their menu');
  assert.equal(harness.document.activeElement, cog,
    'menu-item dismissal returns focus from the hidden menu to its cog');

  first();
  const second = bindToolbarActionsMenu();
  first();
  harness.fireEvent(cog, 'click', { button: 0 });
  assert.equal(menu.classList.contains('open'), true,
    'stale cleanup does not remove the newer direct cog listener');
  second();
});

test('group disclosure binders scope to the stable host and no-op harmlessly when absent', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', dashboardStub);
  const [{ bindDetailsPersistence, bindGroupTitleToggle }, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/refresh.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  assert.doesNotThrow(() => bindDetailsPersistence()());
  assert.doesNotThrow(() => bindGroupTitleToggle()());

  harness.document.body.innerHTML = `<div id="groups-list"><details data-group-key="lifecycle" open><summary><strong class="group-name">lifecycle</strong></summary></details></div>`;
  const firstDetails = bindDetailsPersistence();
  const firstTitle = bindGroupTitleToggle();
  assert.equal(bindDetailsPersistence(), firstDetails);
  assert.equal(bindGroupTitleToggle(), firstTitle);
  firstDetails();
  firstTitle();
  const secondDetails = bindDetailsPersistence();
  const secondTitle = bindGroupTitleToggle();
  firstDetails();
  firstTitle();

  const details = harness.document.querySelector('details');
  harness.fireEvent(details.querySelector('.group-name'), 'click', { detail: 1 });
  details.open = false;
  harness.fireEvent(details, 'toggle');
  assert.equal(dashPrefs.getItem('tclaude.dash.group.lifecycle'), '0',
    'stale cleanup leaves the newer host-scoped listeners installed');
  secondTitle();
  secondDetails();
});
