import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function installHosts(harness) {
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  for (const name of ['groups', 'terminals']) {
    const button = nav.appendChild(harness.document.createElement('a'));
    button.dataset.tab = name;
    button.href = `/${name === 'groups' ? '' : name}`;
    button.textContent = name;
    button.addEventListener('click', (event) => {
      event.preventDefault();
      for (const item of nav.querySelectorAll('[data-tab]')) item.classList.toggle('active', item === button);
      for (const section of harness.document.querySelectorAll('main section')) {
        section.classList.toggle('active', section.id === `tab-${name}`);
      }
    });
  }
  nav.querySelector('[data-tab="groups"]').classList.add('active');
  const main = harness.document.body.appendChild(harness.document.createElement('main'));
  const groups = main.appendChild(harness.document.createElement('section'));
  groups.id = 'tab-groups';
  groups.classList.add('active');
  const terminals = main.appendChild(harness.document.createElement('section'));
  terminals.id = 'tab-terminals';
  const host = terminals.appendChild(harness.document.createElement('div'));
  host.id = 'terminals-root';
  const badgeHost = nav.appendChild(harness.document.createElement('span'));
  badgeHost.id = 'terminals-badge-root';
  const modalHost = harness.document.body.appendChild(harness.document.createElement('div'));
  modalHost.id = 'terminal-session-root';
  return { host, badgeHost, modalHost, terminals };
}

function fakeWidgetFactory(harness) {
  const widgets = [];
  const factory = (options) => {
    const child = harness.document.createElement('textarea');
    child.dataset.opaqueXterm = String(widgets.length + 1);
    options.host.append(child);
    let disposed = false;
    const widget = {
      options,
      child,
      disposeCount: 0,
      connectCount: 0,
      copyCount: 0,
      activeEdges: [],
      currentStatus: 'disconnected',
      connect() {
        this.connectCount += 1;
        this.currentStatus = 'connected';
        options.onStatus('connected');
        options.onReconnectChange(false);
        return Promise.resolve(true);
      },
      copy() { this.copyCount += 1; return Promise.resolve(); },
      // The reveal path drives fit/focus directly, so the fake mirrors the real
      // widget contract. Each focus records whether the Terminals tab was
      // actually revealed at that moment: a real browser drops focus attempted
      // while the pane is still display:none, and LinkeDOM would not.
      revealedAtFocus: [],
      calls: [],
      fit() { this.calls.push('fit'); },
      focus() {
        this.calls.push('focus');
        this.revealedAtFocus.push(
          harness.document.getElementById('tab-terminals')?.classList.contains('active') === true,
        );
      },
      setActive(value) { this.activeEdges.push(!!value); },
      status() { return this.currentStatus; },
      dispose() {
        if (disposed) return;
        disposed = true;
        this.disposeCount += 1;
      },
    };
    widgets.push(widget);
    return widget;
  };
  return { factory, widgets };
}

test('dashboard terminal feature owns three hosts while preserving opaque xterm descendants', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, badgeHost, modalHost, terminals } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const requests = [];
  const composed = [];
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const controller = await harness.importDashboardModule('js/terminals-tab.js');
  const { terminalTabReorderOffset } =
    await harness.importDashboardModule('js/terminal-shell-island.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    confirm: async () => true,
    fetchImpl: async (url) => { requests.push(url); return { ok: true }; },
    onComposeMessage: (seed) => composed.push(seed),
  });
  await harness.act(() => {});

  assert.equal(host.dataset.islandOwner, 'terminals');
  assert.equal(badgeHost.dataset.islandOwner, 'terminals');
  assert.equal(modalHost.dataset.islandOwner, 'terminals');
  assert.equal(harness.document.body.classList.contains('hide-terminals'), true);

  await harness.act(async () => {
    controller.openTerminalPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    await Promise.resolve();
  });
  assert.equal(harness.document.body.classList.contains('hide-terminals'), false);
  assert.equal(terminals.classList.contains('active'), true, 'opening reveals the dashboard tab');
  assert.deepEqual(fake.widgets[0].revealedAtFocus, [true],
    'the first pane focuses xterm only once the Terminals tab is revealed');
  assert.deepEqual(fake.widgets[0].calls, ['fit', 'focus'],
    'the revealed pane is refitted before it takes focus');
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '1');
  assert.equal(host.querySelectorAll('[role="tab"]').length, 1);
  assert.equal(fake.widgets.length, 1);
  const opaqueChild = fake.widgets[0].child;
  assert.equal(opaqueChild.parentElement.classList.contains('mux-pane-xterm-fit'), true);
  assert.equal(opaqueChild.parentElement.parentElement.classList.contains('mux-pane-xterm'), true,
    'the fit host fills the padded visual host content box');

  const firstActivationEdges = fake.widgets[0].activeEdges.length;
  harness.fireEvent(harness.document.querySelector('nav [data-tab="groups"]'), 'click');
  assert.equal(terminals.classList.contains('active'), false, 'fixture leaves the terminal tab before re-reveal');
  await harness.act(async () => {
    controller.openTerminalPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    await Promise.resolve();
  });
  assert.equal(fake.widgets.length, 1, 'revealing the active pane does not remount its widget');
  assert.equal(fake.widgets[0].disposeCount, 0);
  assert.equal(fake.widgets[0].activeEdges.length, firstActivationEdges + 1);
  assert.equal(fake.widgets[0].activeEdges.at(-1), true, 'revealing the active pane refocuses xterm');
  assert.deepEqual(fake.widgets[0].revealedAtFocus, [true, true],
    'revealing an existing pane refocuses xterm after the tab is visible');
  assert.equal(fake.widgets[0].child, opaqueChild);

  await harness.act(() => fake.widgets[0].options.onStatus('copied'));
  assert.equal(host.querySelector('.mux-pane-status').textContent, 'copied');
  assert.equal(fake.widgets[0].child, opaqueChild, 'a chrome rerender never reconciles xterm descendants');
  await harness.act(() => fake.widgets[0].options.onSelectionChange(true));
  const copy = getByRole(host, 'button', { name: /Copy selected terminal text/ });
  harness.fireEvent(copy, 'click');
  assert.equal(fake.widgets[0].copyCount, 1);
  const message = getByRole(host, 'button', { name: '✉ Message' });
  harness.fireEvent(message, 'click');
  fake.widgets[0].options.onComposeMessage();
  assert.deepEqual(composed.map(({ restoreFocus, ...target }) => target), [
    { ws: '/one', key: 'one', label: 'one', agent: 'agt_one' },
    { ws: '/one', key: 'one', label: 'one', agent: 'agt_one' },
  ], 'button and xterm Ctrl/Cmd+M callback open the same agent-scoped composer');
  assert.equal(typeof composed[0].restoreFocus, 'function');

  const headerChord = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperties(headerChord, {
    key: { value: 'm' }, code: { value: 'KeyM' }, ctrlKey: { value: true },
  });
  harness.document.dispatchEvent(headerChord);
  assert.equal(headerChord.defaultPrevented, true, 'page capture owns Ctrl/Cmd+M outside xterm');
  assert.equal(composed.length, 3);

  await harness.act(async () => {
    controller.openTerminalPane({ ws: '/two', key: 'two', label: 'two', agent: 'agt_two' });
    await Promise.resolve();
  });
  assert.equal(host.querySelectorAll('[role="tab"]').length, 2);
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '2');
  assert.equal(fake.widgets[0].activeEdges.at(-1), false);
  assert.equal(fake.widgets[1].activeEdges.at(-1), true);
  assert.deepEqual(fake.widgets[1].calls, [],
    'switching panes while Terminals is visible relies on the existing activation path');
  await harness.act(() => composed[0].restoreFocus());
  assert.equal(fake.widgets[0].activeEdges.at(-1), true,
    'closing the composer restores the exact originating pane');
  await harness.act(() => controller.focusTerminalForConv(['agt_one']));
  assert.equal(fake.widgets[0].activeEdges.at(-1), true);
  const activeEdges = fake.widgets[0].activeEdges.length;
  const inactiveEdges = fake.widgets[1].activeEdges.length;
  const inactiveCalls = fake.widgets[1].calls.length;
  await harness.act(() => controller.focusTerminalForConv(['agt_one']));
  assert.equal(fake.widgets[0].activeEdges.length, activeEdges + 1);
  assert.equal(fake.widgets[0].activeEdges.at(-1), true);
  assert.equal(fake.widgets[1].activeEdges.length, inactiveEdges, 'reveal does not touch inactive widgets');
  assert.equal(fake.widgets[1].calls.length, inactiveCalls,
    'reveal never fits or focuses an inactive widget');

  const tabsBeforeDrag = [...host.querySelectorAll('[role="tab"]')];
  const oneTab = tabsBeforeDrag.find((tab) => tab.querySelector('.mux-tab-label').textContent === 'one');
  const twoTab = tabsBeforeDrag.find((tab) => tab.querySelector('.mux-tab-label').textContent === 'two');
  oneTab.getBoundingClientRect = () => ({ left: 10, width: 100 });
  const transfer = {
    data: {}, effectAllowed: '', dropEffect: '',
    setData(type, value) { this.data[type] = value; },
    getData(type) { return this.data[type] || ''; },
  };
  await harness.act(() => harness.fireEvent(twoTab, 'dragstart', {
    dataTransfer: transfer,
  }));
  assert.equal(transfer.data['application/x-tclaude-terminal-tab'], 'two');
  assert.equal(transfer.data['text/plain'], undefined,
    'terminal-tab drag stays isolated from the dashboard member drag router');
  assert.ok(twoTab.classList.contains('dragging'), 'source tab shows its drag state');
  await harness.act(() => harness.fireEvent(oneTab, 'dragover', {
    dataTransfer: transfer, clientX: 20,
  }));
  assert.ok(oneTab.classList.contains('drop-before'), 'target tab shows the exact insertion edge');
  await harness.act(() => harness.fireEvent(oneTab, 'drop', {
    dataTransfer: transfer, clientX: 20,
  }));
  assert.deepEqual([...host.querySelectorAll('[role="tab"] .mux-tab-label')].map((tab) => tab.textContent),
    ['two', 'one']);
  assert.equal(host.querySelector('[role="tab"][aria-selected="true"] .mux-tab-label').textContent, 'one',
    'drag reorder preserves the active terminal');
  assert.equal(fake.widgets[0].child, opaqueChild, 'reorder keeps the keyed xterm host intact');
  assert.equal(fake.widgets[0].disposeCount, 0);
  assert.equal(fake.widgets[1].disposeCount, 0, 'reorder neither closes nor reconnects a terminal');
  assert.equal(host.querySelector('.drop-before,.drop-after,.dragging'), null,
    'drop clears every transient drag marker');

  const movedOneTab = [...host.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === 'one');
  assert.equal(movedOneTab.getAttribute('aria-keyshortcuts'),
    'Alt+Shift+ArrowLeft Alt+Shift+ArrowRight');
  assert.equal(terminalTabReorderOffset({
    type: 'keydown', key: 'ArrowLeft', altKey: true, shiftKey: true,
    target: { closest: (selector) => selector === 'button' ? {} : null },
  }), null, 'a reorder shortcut from the nested Close button is ignored');
  await harness.act(() => harness.fireEvent(movedOneTab, 'keydown', {
    key: 'ArrowLeft', altKey: true, shiftKey: true,
  }));
  assert.deepEqual([...host.querySelectorAll('[role="tab"] .mux-tab-label')].map((tab) => tab.textContent),
    ['one', 'two'], 'keyboard reordering follows the same state path');
  assert.match(host.querySelector('.mux-tab-a11y[role="status"]').textContent,
    /Moved one to position 1 of 2/);
  assert.equal(host.querySelector('[role="tab"][aria-selected="true"] .mux-tab-label').textContent, 'one');

  twoTab.getBoundingClientRect = () => ({ left: 10, width: 100 });
  await harness.act(() => harness.fireEvent(movedOneTab, 'dragstart', {
    dataTransfer: transfer,
  }));
  await harness.act(() => harness.fireEvent(twoTab, 'dragover', {
    dataTransfer: transfer, clientX: 20,
  }));
  assert.ok(twoTab.classList.contains('drop-before'));
  await harness.act(() => harness.fireEvent(twoTab, 'drop', {
    dataTransfer: transfer, clientX: 100,
  }));
  assert.deepEqual([...host.querySelectorAll('[role="tab"] .mux-tab-label')].map((tab) => tab.textContent),
    ['two', 'one'], 'drop coordinates, not a stale hover render, choose the committed edge');

  await harness.act(() => harness.fireEvent(movedOneTab, 'dragstart', {
    dataTransfer: transfer,
  }));
  await harness.act(() => harness.fireEvent(twoTab, 'dragover', {
    dataTransfer: transfer, clientX: 20,
  }));
  await harness.act(() => harness.fireEvent(movedOneTab, 'dragend', {
    dataTransfer: transfer,
  }));
  assert.deepEqual([...host.querySelectorAll('[role="tab"] .mux-tab-label')].map((tab) => tab.textContent),
    ['two', 'one'], 'a cancelled drag leaves order unchanged');
  assert.equal(host.querySelector('.drop-before,.drop-after,.dragging'), null,
    'drag cancellation clears source and target state');

  const closeOne = getByRole(host, 'button', { name: 'Close one' });
  await harness.act(() => harness.fireEvent(closeOne, 'click'));
  assert.equal(fake.widgets[0].disposeCount, 1);
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '1');

  await harness.act(() => controller.openTermModal({
    wsPath: '/modal-live', label: 'agent one', hideConv: 'agt_one',
  }));
  assert.equal(modalHost.querySelector('#term-session-title').textContent, 'Terminal — agent one');
  assert.ok(modalHost.querySelector('#term-session-detach'));
  assert.equal(fake.widgets.length, 3);
  const modalOpaque = fake.widgets[2].child;
  await harness.act(() => fake.widgets[2].options.onStatus('connected'));
  assert.equal(fake.widgets[2].child, modalOpaque);
  await harness.act(async () => {
    harness.fireEvent(modalHost.querySelector('#term-session-close'), 'click');
    await Promise.resolve();
    await Promise.resolve();
  });
  assert.equal(modalHost.childElementCount, 0);
  assert.equal(fake.widgets[2].disposeCount, 1);
  assert.deepEqual(requests, ['/api/hide/agt_one']);

  cleanup();
  cleanup();
  assert.equal(fake.widgets[1].disposeCount, 1, 'feature cleanup disposes the remaining pane once');
  assert.equal(host.childElementCount, 0);
  assert.equal(badgeHost.childElementCount, 0);
  assert.equal(modalHost.childElementCount, 0);
});

test('background pane open and focus leave the current dashboard tab visible', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, badgeHost, terminals } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const controller = await harness.importDashboardModule('js/terminals-tab.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    confirm: async () => true,
  });

  await harness.act(async () => {
    controller.openTerminalPane({
      ws: '/background-one', key: 'background-one', label: 'one', agent: 'agt_one',
    }, { reveal: false });
    await Promise.resolve();
  });
  assert.equal(terminals.classList.contains('active'), false,
    'background open leaves Groups visible');
  assert.equal(harness.document.querySelector('#tab-groups').classList.contains('active'), true);
  assert.equal(harness.document.body.classList.contains('hide-terminals'), false,
    'the Terminals tab becomes available even though it is not selected');
  assert.equal(host.querySelectorAll('[role="tab"]').length, 1,
    'background open still creates a terminal pane');
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '1');

  await harness.act(async () => {
    controller.openTerminalPane({
      ws: '/background-two', key: 'background-two', label: 'two', agent: 'agt_two',
    }, { reveal: false });
    await Promise.resolve();
  });
  assert.equal(terminals.classList.contains('active'), false);
  assert.equal(host.querySelectorAll('[role="tab"]').length, 2,
    'several panes can accumulate before visiting Terminals');
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '2');

  await harness.act(() => controller.focusTerminalForConv(['agt_one'], { reveal: false }));
  assert.equal(terminals.classList.contains('active'), false,
    'background focus selects an existing pane without revealing Terminals');
  assert.equal(host.querySelector('[role="tab"][aria-selected="true"] .mux-tab-label').textContent, 'one');

  cleanup();
});

test('terminal tab context menu supports pointer and keyboard detach and close actions', async (t) => {
  const harness = await createPreactHarness(t);
  const { host } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const requests = [];
  const opened = [];
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const controller = await harness.importDashboardModule('js/terminals-tab.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    fetchImpl: async (url) => { requests.push(url); return { ok: true }; },
    windowRef: {
      open(url, target, features) {
        const tab = { opened: [url, target, features], location: { replace: (next) => opened.push(next) } };
        opened.push(tab);
        return tab;
      },
    },
  });
  const open = async (key) => harness.act(async () => {
    controller.openTerminalPane({
      ws: `/${key}`, key, label: key, agent: `agt_${key}`, hideConv: `agt_${key}`,
    });
    await Promise.resolve();
  });
  const tab = (name) => [...host.querySelectorAll('[role="tab"]')]
    .find((candidate) => candidate.querySelector('.mux-tab-label')?.textContent === name);

  await open('one');
  await open('two');
  await open('three');

  await harness.act(() => harness.fireEvent(tab('two'), 'contextmenu', { clientX: 24, clientY: 32 }));
  const pointerMenu = getByRole(host, 'menu', { name: 'Actions for two' });
  assert.equal(tab('two').getAttribute('aria-expanded'), 'true');
  // Detach / close / close-others / close-all, plus the grouping commands the
  // ungrouped case offers: start a group from this tab (no stacks exist yet, so
  // no join items and no leave item).
  assert.deepEqual([...pointerMenu.querySelectorAll('[role="menuitem"]')].map((item) => item.textContent),
    ['New group from this tab', 'Detach tab', 'Close tab', 'Close other tabs', 'Close all tabs']);
  assert.equal(pointerMenu.querySelectorAll('[role="separator"]').length, 2);
  assert.equal(harness.document.activeElement.textContent, 'New group from this tab',
    'opening focuses the first action');
  await harness.act(() => harness.fireEvent(pointerMenu, 'keydown', { key: 'Escape' }));
  assert.equal(host.querySelector('[role="menu"]'), null);
  assert.equal(harness.document.activeElement, tab('two'), 'Escape restores focus to the invoking tab');
  await harness.act(() => harness.fireEvent(tab('two'), 'contextmenu', { clientX: 24, clientY: 32 }));
  const tabMenu = getByRole(host, 'menu', { name: 'Actions for two' });
  await harness.act(async () => {
    harness.fireEvent(tabMenu, 'keydown', { key: 'Tab' });
    await Promise.resolve();
  });
  assert.equal(host.querySelector('[role="menu"]'), null, 'Tab dismisses the floating menu');
  assert.equal(tab('two').getAttribute('aria-expanded'), 'false');
  assert.equal(harness.document.activeElement, host.querySelector('.mux-pane.active button'),
    'forward Tab moves into the active pane controls');
  await harness.act(() => harness.fireEvent(tab('two'), 'contextmenu', { clientX: 24, clientY: 32 }));
  const reverseMenu = getByRole(host, 'menu', { name: 'Actions for two' });
  await harness.act(async () => {
    harness.fireEvent(reverseMenu, 'keydown', { key: 'Tab', shiftKey: true });
    await Promise.resolve();
  });
  assert.equal(host.querySelector('[role="menu"]'), null, 'Shift+Tab dismisses the floating menu');
  assert.equal(harness.document.activeElement, tab('two'), 'reverse Tab restores the invoking tab');
  await harness.act(() => harness.fireEvent(tab('two'), 'contextmenu', { clientX: 24, clientY: 32 }));
  const reopenedMenu = getByRole(host, 'menu', { name: 'Actions for two' });
  await harness.act(() => harness.fireEvent(getByRole(reopenedMenu, 'menuitem', { name: 'Close tab' }), 'click'));
  assert.deepEqual([...host.querySelectorAll('.mux-tab-label')].map((label) => label.textContent), ['one', 'three']);
  assert.equal(tab('three').getAttribute('aria-selected'), 'true', 'closing an inactive tab preserves active selection');
  assert.equal(harness.document.activeElement, tab('three'), 'close tab focuses the surviving active tab');

  await open('detached');
  await harness.act(() => harness.fireEvent(tab('detached'), 'contextmenu', { clientX: 24, clientY: 32 }));
  const detachMenu = getByRole(host, 'menu', { name: 'Actions for detached' });
  await harness.act(async () => {
    harness.fireEvent(getByRole(detachMenu, 'menuitem', { name: 'Detach tab' }), 'click');
    await Promise.resolve();
    await Promise.resolve();
  });
  assert.deepEqual([...host.querySelectorAll('.mux-tab-label')].map((label) => label.textContent), ['one', 'three']);
  assert.equal(fake.widgets.at(-1).disposeCount, 1, 'detach disposes the dashboard widget');
  assert.match(opened.at(-1), /^\/terminals\?solo=1#open=/,
    'detach uses the same standalone terminal handoff as the pane header button');
  assert.equal(opened.at(-2).opened[2], undefined,
    'the context menu still detaches into a browser tab — window features are what would make it a window');
  assert.equal(harness.document.activeElement, tab('three'), 'detach focuses the surviving active tab');

  await open('two');
  let keyboardOpen;
  await harness.act(() => {
    keyboardOpen = harness.fireEvent(tab('three'), 'keydown', { key: 'F10', shiftKey: true });
  });
  assert.equal(keyboardOpen.defaultPrevented, true, 'Shift+F10 is the keyboard context-menu gesture');
  const keyboardMenu = getByRole(host, 'menu', { name: 'Actions for three' });
  harness.fireEvent(keyboardMenu, 'keydown', { key: 'ArrowDown' });
  assert.equal(harness.document.activeElement.textContent, 'Detach tab');
  harness.fireEvent(keyboardMenu, 'keydown', { key: 'ArrowDown' });
  assert.equal(harness.document.activeElement.textContent, 'Close tab');
  harness.fireEvent(keyboardMenu, 'keydown', { key: 'ArrowDown' });
  assert.equal(harness.document.activeElement.textContent, 'Close other tabs');
  await harness.act(() => harness.fireEvent(harness.document.activeElement, 'click'));
  assert.deepEqual([...host.querySelectorAll('.mux-tab-label')].map((label) => label.textContent), ['three']);
  assert.equal(tab('three').getAttribute('aria-selected'), 'true');
  assert.equal(harness.document.activeElement, tab('three'), 'close others focuses the kept tab');

  await open('four');
  await open('five');
  await harness.act(() => harness.fireEvent(tab('four'), 'keydown', { key: 'ContextMenu' }));
  const allMenu = getByRole(host, 'menu', { name: 'Actions for four' });
  await harness.act(() => harness.fireEvent(getByRole(allMenu, 'menuitem', { name: 'Close all tabs' }), 'click'));
  assert.equal(host.querySelectorAll('[role="tab"]').length, 0);
  assert.equal(harness.document.body.classList.contains('hide-terminals'), true);
  assert.equal(harness.document.activeElement, harness.document.querySelector('nav [data-tab="groups"]'),
    'close all moves focus to the selected Groups navigation tab');
  assert.deepEqual(requests.sort(), [
    '/api/hide/agt_detached', '/api/hide/agt_five', '/api/hide/agt_four', '/api/hide/agt_one',
    '/api/hide/agt_three', '/api/hide/agt_two', '/api/hide/agt_two',
  ]);
  cleanup();
});

test('throwaway modal omits Detach, ignores Escape, and confirms backdrop close', async (t) => {
  const harness = await createPreactHarness(t);
  const { modalHost } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const confirmations = [];
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const { openTermModal } = await harness.importDashboardModule('js/terminals-tab.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    confirm: async (options) => { confirmations.push(options); return true; },
    fetchImpl: async () => { throw new Error('throwaway close must not detach'); },
  });
  await harness.act(() => openTermModal({ wsPath: '/scratch', label: 'scratch' }));
  assert.equal(modalHost.querySelector('#term-session-detach'), null);
  const overlay = modalHost.querySelector('#term-session-modal');
  const escape = harness.fireEvent(overlay, 'keydown', { key: 'Escape' });
  assert.equal(escape.defaultPrevented, false, 'Escape remains terminal input, not a shell close key');
  assert.ok(modalHost.querySelector('#term-session-modal'));
  await harness.act(async () => {
    harness.fireEvent(overlay, 'click');
    await Promise.resolve();
    await Promise.resolve();
  });
  assert.equal(confirmations[0].okLabel, 'Close terminal');
  assert.equal(modalHost.childElementCount, 0);
  cleanup();
});

test('terminal button and shortcut route through the mounted Preact operator composer', async (t) => {
  const harness = await createPreactHarness(t);
  const { host } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const dialogHost = harness.document.body.appendChild(harness.document.createElement('div'));
  dialogHost.id = 'message-access-dialog-root';
  const [loader, terminalController, messageController] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
    harness.importDashboardModule('js/message-access-dialog-controller.js'),
  ]);
  const requests = [];
  const fetchImpl = async (url, options) => {
    requests.push({ url, options });
    return { ok: true, status: 200, text: async () => '{}' };
  };
  const messageCleanup = await loader.mountMessageAccessDialogsFeature({
    fetchImpl, confirmDiscard: async () => true,
  });
  const terminalCleanup = await loader.mountTerminalsFeature({
    widgetFactory: fake.factory,
    confirm: async () => true,
    onComposeMessage: messageController.openOperatorMessageDialog,
    composeMessageDialogKind: messageController.activeMessageAccessDialogKind,
  });
  await harness.act(async () => {
    terminalController.openTerminalPane({
      ws: '/worker', key: 'worker', label: 'Worker', agent: 'agt_worker',
    });
    await Promise.resolve();
  });
  const beforeRestore = fake.widgets[0].activeEdges.length;
  await harness.act(() => harness.fireEvent(getByRole(host, 'button', { name: '✉ Message' }), 'click'));
  assert.ok(dialogHost.querySelector('#operator-message-modal'));
  assert.equal(dialogHost.querySelector('#operator-message-to').textContent, 'Worker');
  assert.equal(dialogHost.querySelector('#operator-message-to').title, 'agt_worker');

  const held = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperties(held, {
    key: { value: 'm' }, code: { value: 'KeyM' }, ctrlKey: { value: true }, repeat: { value: true },
  });
  harness.document.dispatchEvent(held);
  assert.equal(held.defaultPrevented, true, 'an already-open composer consumes the held global shortcut');
  await harness.input(dialogHost.querySelector('#operator-message-body'), 'from terminal');
  await harness.act(async () => {
    dialogHost.querySelector('#operator-message-submit').click();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, '/api/operator-message');
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    to: 'agt_worker', subject: '', body: 'from terminal', attachment_token: '',
  });
  assert.equal(dialogHost.querySelector('#operator-message-modal'), null);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(fake.widgets[0].activeEdges.length, beforeRestore + 1,
    'composer close restores the exact terminal pane through its action boundary');
  terminalCleanup();
  messageCleanup();
});

test('dragging a terminal tab clear of the strip detaches it into its own window', async (t) => {
  const harness = await createPreactHarness(t);
  const { host } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const requests = [];
  const opened = [];
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const controller = await harness.importDashboardModule('js/terminals-tab.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    fetchImpl: async (url) => { requests.push(url); return { ok: true }; },
    windowRef: {
      screen: { availWidth: 1600, availHeight: 900, availLeft: 0, availTop: 0 },
      open: (url, target, features) => {
        opened.push({ features });
        return { location: { replace: (next) => { opened.at(-1).url = next; } } };
      },
    },
  });
  const open = async (key) => harness.act(async () => {
    controller.openTerminalPane({ ws: `/${key}`, key, label: key, hideConv: `agt_${key}` });
    await Promise.resolve();
  });
  await open('one');
  await open('two');
  const strip = host.querySelector('.mux-tabs');
  // The strip is the home region; the geometry rule is what the browser cannot
  // tell us, so the test supplies the measurement a real layout would.
  strip.getBoundingClientRect = () => ({ left: 0, top: 0, right: 400, bottom: 30, width: 400, height: 30 });
  host.querySelector('.mux-panes').getBoundingClientRect =
    () => ({ left: 0, top: 30, right: 900, bottom: 630, width: 900, height: 600 });
  const labels = () => [...host.querySelectorAll('.mux-tab-label')].map((label) => label.textContent);
  const label = (name) => [...host.querySelectorAll('.mux-tab-label')].find((el) => el.textContent === name);
  const tab = (name) => label(name).closest('[role="tab"]');
  const transfer = {
    data: {}, effectAllowed: '', dropEffect: '',
    setData(type, value) { this.data[type] = value; },
    getData(type) { return this.data[type] || ''; },
  };
  const dragOver = async (init) => {
    let event = null;
    await harness.act(() => { event = harness.fireEvent(harness.document, 'dragover', init); });
    return event;
  };

  // Registered before the drag starts, so it runs before the shell's own
  // document listener and this really exercises the defaultPrevented guard: a
  // target that claimed the event keeps its own effect, which is what leaves the
  // tab strip's reorder edges alone.
  const claim = (event) => { event.preventDefault(); event.dataTransfer.dropEffect = 'copy'; };
  harness.document.addEventListener('dragover', claim);
  await harness.act(() => harness.fireEvent(tab('two'), 'dragstart', { dataTransfer: transfer }));
  await harness.act(() => harness.fireEvent(strip, 'dragover', { clientX: 120, clientY: 10, dataTransfer: transfer }));
  assert.equal(transfer.dropEffect, 'copy', 'an already-claimed dragover is left alone');
  // Deferring on the effect must not also silence the hint. Which target claimed
  // the dragover has no bearing on the gesture — endTabDrag decides on geometry
  // and on whether the strip itself took the drop — so releasing over a claimed
  // target still detaches, and a hint that went quiet there would be lying.
  await harness.act(() => harness.fireEvent(
    harness.document, 'dragover', { clientX: 120, clientY: 260, dataTransfer: transfer },
  ));
  assert.equal(strip.classList.contains('drag-out-armed'), true,
    'a claimed dragover outside the strip still arms, because releasing there still detaches');
  assert.equal(transfer.dropEffect, 'copy', 'arming never touches an effect another target set');
  harness.document.removeEventListener('dragover', claim);
  transfer.dropEffect = '';

  const nearMiss = await dragOver({ clientX: 120, clientY: 60, dataTransfer: transfer });
  assert.equal(host.querySelector('.mux-drag-out-hint'), null,
    'a drag hovering just past the strip is still a reorder near-miss');
  assert.equal(strip.classList.contains('drag-out-armed'), false);
  // A browser draws the no-drop cursor over anything that has not accepted the
  // drag, which made a gesture that works look like one that cannot.
  assert.equal(nearMiss.defaultPrevented, true,
    'the page accepts a live terminal drag rather than showing a prohibition sign');
  assert.equal(transfer.dropEffect, 'move');
  const armedOver = await dragOver({ clientX: 120, clientY: 260, dataTransfer: transfer });
  assert.equal(armedOver.defaultPrevented, true);
  assert.match(host.querySelector('.mux-drag-out-hint').textContent, /Release anywhere/,
    'the strip states the pending detach before the user commits');
  assert.equal(strip.classList.contains('drag-out-armed'), true);
  const dropped = harness.fireEvent(harness.document, 'drop', { dataTransfer: transfer });
  assert.equal(dropped.defaultPrevented, true,
    'accepting the drag means consuming the drop it now receives');
  await harness.act(async () => {
    harness.fireEvent(tab('two'), 'dragend', {
      dataTransfer: transfer, clientX: 120, clientY: 260, screenX: 340, screenY: 260,
    });
    await Promise.resolve();
    await Promise.resolve();
  });
  assert.deepEqual(labels(), ['one'], 'the dragged terminal leaves the dashboard strip');
  assert.equal(host.querySelector('.mux-drag-out-hint'), null, 'the gesture clears its own hint');
  assert.equal(fake.widgets[1].disposeCount, 1, 'drag detach disposes the dashboard widget');
  assert.match(opened.at(-1).url, /^\/terminals\?solo=1#open=/,
    'drag detach reuses the standalone handoff, not a new session');
  // A terminal dragged OUT of the dashboard lands in its own window, sized to
  // the pane it left and placed where the drag was released.
  assert.equal(opened.at(-1).features, 'popup=yes,width=900,height=640,left=340,top=260',
    'drag detach asks for a separate window, not another tab of this one');
  assert.deepEqual(requests, ['/api/hide/agt_two']);

  await open('three');
  await harness.act(() => harness.fireEvent(tab('three'), 'dragstart', { dataTransfer: transfer }));
  await harness.act(() => harness.fireEvent(tab('one'), 'dragover', { dataTransfer: transfer, clientX: 20 }));
  await harness.act(() => harness.fireEvent(tab('one'), 'drop', { dataTransfer: transfer, clientX: 20 }));
  await harness.act(async () => {
    harness.fireEvent(tab('three'), 'dragend', {
      dataTransfer: transfer, clientX: 120, clientY: 260, screenX: 340, screenY: 260,
    });
    await Promise.resolve();
  });
  assert.deepEqual(labels(), ['one', 'three'], 'a reorder drop still reorders');
  assert.equal(opened.length, 1, 'a tab that accepted the drop is never also detached');

  await harness.act(() => harness.fireEvent(tab('three'), 'dragstart', { dataTransfer: transfer }));
  await harness.act(async () => {
    harness.fireEvent(tab('three'), 'dragend', { dataTransfer: transfer });
    await Promise.resolve();
  });
  assert.deepEqual(labels(), ['one', 'three'], 'a cancelled drag reports no end point and detaches nothing');
  assert.equal(opened.length, 1);
  cleanup();
});
