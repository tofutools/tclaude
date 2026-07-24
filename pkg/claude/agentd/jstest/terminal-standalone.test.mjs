import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function encodedSeed(seed) {
  return '#open=' + encodeURIComponent(JSON.stringify(seed));
}

function messageEvent(windowRef, { origin, source, data }) {
  const event = new windowRef.Event('message');
  Object.defineProperties(event, {
    origin: { value: origin }, source: { value: source }, data: { value: data },
  });
  return event;
}

function fakeWidgetFactory(harness) {
  const widgets = [];
  const factory = (options) => {
    const child = harness.document.createElement('textarea');
    options.host.append(child);
    let disposed = false;
    const widget = {
      child,
      options,
      connectCount: 0,
      disposeCount: 0,
      activeEdges: [],
      connect() {
        this.connectCount += 1;
        options.onStatus('connected');
        return Promise.resolve(true);
      },
      copy() { return Promise.resolve(); },
      setActive(value) { this.activeEdges.push(!!value); },
      status() { return 'connected'; },
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

test('standalone lifecycle preserves hash, auth return, beacon, and exact-once cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const seed = {
    ws: '/api/open-window-ws/agt_one', label: 'one', key: 'one',
    hideConv: 'agt_one', wizard: true,
  };
  const locationRef = {
    pathname: '/terminals', search: '?solo=1', hash: encodedSeed(seed),
  };
  const calls = [];
  const beacons = [];
  let resolvePrefs;
  let resolveDialogs;
  const prefs = new Promise((resolve) => { resolvePrefs = resolve; });
  const dialogs = new Promise((resolve) => { resolveDialogs = resolve; });
  const mountShell = ({ state, actions }) => {
    calls.push('mount');
    return () => {
      calls.push('cleanup');
      actions.dispose();
      state.dispose();
    };
  };
  const mountMessageDialogs = ({ confirmDiscard, notify, refresh }) => {
    calls.push('dialogs');
    assert.equal(typeof confirmDiscard, 'function');
    assert.equal(typeof notify, 'function');
    assert.equal(typeof refresh, 'function');
    return dialogs;
  };
  const historyRef = {
    replaceState(_state, _unused, url) {
      calls.push(`replace:${url}`);
      locationRef.hash = '';
    },
  };
  const { createStandaloneTerminalsPage, decodeTerminalOpenHash } =
    await harness.importDashboardModule('js/terminal-standalone.js');
  assert.deepEqual(decodeTerminalOpenHash(encodedSeed(seed)), seed);
  assert.equal(decodeTerminalOpenHash('#open=%E0%A4%A'), null);

  const page = createStandaloneTerminalsPage({
    host,
    locationRef,
    historyRef,
    navigatorRef: { sendBeacon: (url) => { beacons.push(url); return true; } },
    windowRef: harness.window,
    documentRef: harness.document,
    initPrefs: () => { calls.push('prefs'); return prefs; },
    initThemeSync: () => calls.push('theme'),
    mountShell,
    mountMessageDialogs,
  });

  const starting = page.start();
  harness.window.dispatchEvent(new harness.window.Event('hashchange'));
  assert.deepEqual(calls, ['prefs'], 'hash is not consumed before preferences hydrate');
  resolvePrefs();
  assert.equal(await starting, true);
  assert.deepEqual(calls, [
    'prefs', 'theme', 'mount', 'replace:/terminals?solo=1', 'dialogs',
  ]);
  resolveDialogs(() => calls.push('dialogs-cleanup'));
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(page.state.panes.value.length, 1);
  assert.equal(page.state.panes.value[0].seed.ws, seed.ws);
  assert.equal(harness.document.body.classList.contains('wizard'), true);

  const auth = new harness.window.CustomEvent('tclaude:auth-expired', { detail: {} });
  harness.window.dispatchEvent(auth);
  assert.equal(auth.detail.returnTo, '/terminals?solo=1' + encodedSeed(seed));

  harness.window.dispatchEvent(new harness.window.Event('pagehide'));
  assert.deepEqual(beacons, ['/api/hide/agt_one']);
  harness.window.dispatchEvent(new harness.window.Event('unload'));
  page.dispose();
  await Promise.resolve();
  assert.equal(calls.filter((call) => call === 'cleanup').length, 1);
  assert.equal(calls.filter((call) => call === 'dialogs-cleanup').length, 1);
  assert.equal(page.state.panes.value.length, 0);

  const lateAuth = new harness.window.CustomEvent('tclaude:auth-expired', { detail: {} });
  harness.window.dispatchEvent(lateAuth);
  assert.equal(lateAuth.detail.returnTo, undefined, 'disposed lifecycle removes auth writer');
});

test('standalone lifecycle connects before the real composer loads and sends through its fetch seam', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'terminals-root';
  const dialogHost = harness.document.body.appendChild(harness.document.createElement('div'));
  dialogHost.id = 'message-access-dialog-root';
  const fake = fakeWidgetFactory(harness);
  const seed = {
    ws: '/solo', key: 'solo', label: 'solo terminal', agent: 'agt_solo',
  };
  const locationRef = {
    pathname: '/terminals', search: '?solo=1', hash: encodedSeed(seed),
  };
  const requests = [];
  const fetchImpl = async (url, options) => {
    requests.push({ url, options });
    return {
      ok: true,
      status: 202,
      text: async () => JSON.stringify({ id: 17, queued: true }),
    };
  };
  harness.window.confirm = () => true;
  const { createStandaloneTerminalsPage } =
    await harness.importDashboardModule('js/terminal-standalone.js');
  const page = createStandaloneTerminalsPage({
    host,
    widgetFactory: fake.factory,
    fetchImpl,
    initPrefs: async () => {},
    initThemeSync: () => {},
    windowRef: harness.window,
    documentRef: harness.document,
    locationRef,
    historyRef: { replaceState: () => { locationRef.hash = ''; } },
    navigatorRef: { sendBeacon: () => true },
  });

  let started;
  await harness.act(async () => { started = await page.start(); });
  assert.equal(started, true);
  assert.equal(fake.widgets.length, 1,
    'the terminal shell mounts without awaiting the composer module graph');
  assert.equal(fake.widgets[0].connectCount, 1);

  for (let i = 0; i < 20 && dialogHost.dataset.islandOwner !== 'message-access-dialogs'; i++) {
    await harness.act(() => new Promise((resolve) => setImmediate(resolve)));
  }
  assert.equal(dialogHost.dataset.islandOwner, 'message-access-dialogs');

  const chord = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperties(chord, {
    key: { value: 'm' }, code: { value: 'KeyM' }, ctrlKey: { value: true },
  });
  await harness.act(async () => {
    host.querySelector('.mux-pane-header').dispatchEvent(chord);
    await Promise.resolve();
  });
  assert.equal(chord.defaultPrevented, true);
  for (let i = 0; i < 5 && !dialogHost.querySelector('#operator-message-modal'); i++) {
    await harness.act(() => new Promise((resolve) => setImmediate(resolve)));
  }
  assert.ok(dialogHost.querySelector('#operator-message-modal'));

  await harness.input(dialogHost.querySelector('#operator-message-body'), 'from detached mode');
  await harness.act(async () => {
    dialogHost.querySelector('#operator-message-submit').click();
    for (let i = 0; i < 5 && dialogHost.querySelector('#operator-message-modal'); i++) {
      await new Promise((resolve) => setImmediate(resolve));
    }
  });
  assert.equal(dialogHost.querySelector('#operator-message-modal'), null);
  assert.deepEqual(requests.map(({ url }) => url), ['/api/operator-message']);
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    to: 'agt_solo',
    subject: '',
    body: 'from detached mode',
    attachment_token: '',
  });

  page.dispose();
  assert.equal(dialogHost.dataset.islandOwner, undefined);
  assert.equal(dialogHost.childElementCount, 0);
  assert.equal(fake.widgets[0].disposeCount, 1);
});

test('standalone disposal before preference hydration prevents a late mount', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  let resolvePrefs;
  let mounts = 0;
  const prefs = new Promise((resolve) => { resolvePrefs = resolve; });
  const { createStandaloneTerminalsPage } =
    await harness.importDashboardModule('js/terminal-standalone.js');
  const page = createStandaloneTerminalsPage({
    host,
    initPrefs: () => prefs,
    initThemeSync: () => assert.fail('disposed page must not start theme sync'),
    mountShell: () => { mounts += 1; return () => {}; },
    windowRef: harness.window,
    documentRef: harness.document,
    locationRef: { pathname: '/terminals', search: '?solo=1', hash: '' },
    historyRef: { replaceState() {} },
    navigatorRef: { sendBeacon() { return true; } },
  });
  const starting = page.start();
  page.dispose();
  page.dispose();
  resolvePrefs();
  assert.equal(await starting, false);
  assert.equal(mounts, 0);
});

test('standalone reattach disarms its beacon, hands off to its exact opener, and closes', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const origin = 'https://dashboard.test';
  const seed = {
    ws: '/api/open-window-ws/agt_one', label: 'one', key: 'one',
    hideConv: 'agt_one', agent: 'agt_one',
  };
  const locationRef = {
    origin, protocol: 'https:', host: 'dashboard.test',
    pathname: '/terminals', search: '?solo=1', hash: encodedSeed(seed),
    replace: (url) => assert.fail(`acknowledged handoff must not use fallback ${url}`),
  };
  const requests = [];
  const beacons = [];
  const posted = [];
  let focused = 0;
  let closed = 0;
  const [{ createStandaloneTerminalsPage }, handoff] = await Promise.all([
    harness.importDashboardModule('js/terminal-standalone.js'),
    harness.importDashboardModule('js/terminal-handoff.js'),
  ]);
  const opener = {
    closed: false,
    focus: () => { focused += 1; },
    postMessage(data, targetOrigin) {
      posted.push({ data, targetOrigin });
      queueMicrotask(() => harness.window.dispatchEvent(messageEvent(harness.window, {
        origin, source: opener,
        data: { type: handoff.TERMINAL_REATTACH_ACK, id: data.id, accepted: true },
      })));
    },
  };
  Object.defineProperty(harness.window, 'opener', { value: opener, configurable: true });
  harness.window.close = () => { closed += 1; };
  const page = createStandaloneTerminalsPage({
    host,
    initPrefs: async () => {},
    initThemeSync: () => {},
    mountShell: () => () => {},
    fetchImpl: async (url) => { requests.push(url); return { ok: true }; },
    windowRef: harness.window,
    documentRef: harness.document,
    locationRef,
    historyRef: { replaceState: () => { locationRef.hash = ''; } },
    navigatorRef: { sendBeacon: (url) => { beacons.push(url); return true; } },
  });
  await page.start();
  const pane = page.state.panes.value[0];
  assert.equal(await page.actions.reattachPane(pane.key), true);
  assert.deepEqual(requests, ['/api/hide/agt_one']);
  assert.equal(posted.length, 1);
  assert.equal(posted[0].targetOrigin, origin);
  assert.equal(posted[0].data.seed.initialRetry, true);
  assert.equal(focused, 1);
  assert.equal(closed, 1);
  assert.equal(page.state.panes.value.length, 0);
  harness.window.dispatchEvent(new harness.window.Event('pagehide'));
  assert.deepEqual(beacons, [], 'reattach must not detach the newly-created dashboard client on pagehide');
  page.dispose();
});

test('standalone reattach becomes the dashboard when its opener is gone', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const seed = { ws: '/api/term-ws/agt_one', label: 'one', key: 'one' };
  let replaced = '';
  const locationRef = {
    origin: 'https://dashboard.test', pathname: '/terminals', search: '?solo=1',
    hash: encodedSeed(seed), replace: (url) => { replaced = url; },
  };
  Object.defineProperty(harness.window, 'opener', { value: null, configurable: true });
  const [{ createStandaloneTerminalsPage }, { decodeTerminalOpenHash }] = await Promise.all([
    harness.importDashboardModule('js/terminal-standalone.js'),
    harness.importDashboardModule('js/terminal-handoff.js'),
  ]);
  const page = createStandaloneTerminalsPage({
    host,
    initPrefs: async () => {},
    initThemeSync: () => {},
    mountShell: () => () => {},
    windowRef: harness.window,
    documentRef: harness.document,
    locationRef,
    historyRef: { replaceState: () => { locationRef.hash = ''; } },
    navigatorRef: { sendBeacon: () => true },
  });
  await page.start();
  await page.actions.reattachPane(page.state.panes.value[0].key);
  assert.match(replaced, /^\/terminals#open=/);
  const fallbackSeed = decodeTerminalOpenHash(replaced.slice('/terminals'.length));
  assert.equal(fallbackSeed.ws, seed.ws);
  assert.equal(fallbackSeed.initialRetry, true);
  page.dispose();
});

test('standalone Preact shell renders solo chrome around an opaque active widget', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'terminals-root';
  const fake = fakeWidgetFactory(harness);
  const [{ createTerminalShellState }, { createTerminalShellActions }, { mountStandaloneTerminalShell }] =
    await Promise.all([
      harness.importDashboardModule('js/terminal-shell-state.js'),
      harness.importDashboardModule('js/terminal-shell-actions.js'),
      harness.importDashboardModule('js/terminal-shell-island.js'),
    ]);
  const state = createTerminalShellState();
  const actions = createTerminalShellActions({
    state,
    windowRef: harness.window,
    documentRef: harness.document,
    fetchImpl: async () => ({ ok: true }),
  });
  const composed = [];
  let dialogKind = '';
  const cleanup = mountStandaloneTerminalShell({
    host, state, actions, widgetFactory: fake.factory,
    onComposeMessage: (seed) => composed.push(seed),
    composeMessageDialogKind: () => dialogKind,
  });
  await harness.act(() => {});
  assert.ok(host.querySelector('#mux-empty'));

  await harness.act(async () => {
    actions.openPane({
      ws: '/solo', key: 'solo', label: 'solo terminal', agent: 'agt_solo',
    });
    await Promise.resolve();
  });
  assert.equal(host.querySelector('[role="tablist"]'), null);
  assert.equal(host.querySelector('[title="Move this terminal to its own browser tab"]'), null);
  assert.ok(host.querySelector('[title="Move this terminal back to its dashboard tab"]'));
  assert.equal(host.querySelector('#mux-empty'), null);
  assert.equal(fake.widgets.length, 1);
  assert.equal(fake.widgets[0].child.parentElement.classList.contains('mux-pane-xterm-fit'), true);
  assert.equal(fake.widgets[0].child.parentElement.parentElement.classList.contains('mux-pane-xterm'), true,
    'standalone xterm uses the same inner fit host inside the padded visual shell');
  assert.equal(fake.widgets[0].activeEdges.at(-1), true);
  assert.equal(fake.widgets[0].connectCount, 1);
  assert.equal(harness.document.title, 'solo terminal — tclaude terminals');

  const message = getByRole(host, 'button', { name: '✉ Message' });
  harness.fireEvent(message, 'click');
  const headerChord = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperties(headerChord, {
    key: { value: 'm' }, code: { value: 'KeyM' }, metaKey: { value: true },
  });
  host.querySelector('.mux-pane-header').dispatchEvent(headerChord);
  assert.equal(headerChord.defaultPrevented, true,
    'the standalone shell captures Cmd+M above xterm and header controls');
  assert.deepEqual(composed.map(({ restoreFocus, ...target }) => target), [
    { ws: '/solo', key: 'solo', label: 'solo terminal', agent: 'agt_solo' },
    { ws: '/solo', key: 'solo', label: 'solo terminal', agent: 'agt_solo' },
  ]);
  assert.equal(typeof composed[0].restoreFocus, 'function');

  dialogKind = 'operator-message';
  const held = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperties(held, {
    key: { value: 'm' }, code: { value: 'KeyM' }, ctrlKey: { value: true },
  });
  harness.document.dispatchEvent(held);
  assert.equal(held.defaultPrevented, true,
    'the open standalone composer consumes repeated Ctrl/Cmd+M');
  assert.equal(composed.length, 2, 'the held shortcut does not reopen or retarget the composer');

  const beforeUnload = new harness.window.Event('beforeunload', { cancelable: true });
  harness.window.dispatchEvent(beforeUnload);
  assert.equal(beforeUnload.defaultPrevented, true, 'open solo terminal guards accidental unload');
  const auth = new harness.window.CustomEvent('tclaude:auth-expired', { detail: {} });
  harness.window.dispatchEvent(auth);
  const afterAuth = new harness.window.Event('beforeunload', { cancelable: true });
  harness.window.dispatchEvent(afterAuth);
  assert.equal(afterAuth.defaultPrevented, false, 'auth recovery disarms unload prompt');

  getByRole(host, 'button', { name: /Copy terminal text/ });
  cleanup();
  cleanup();
  assert.equal(fake.widgets[0].disposeCount, 1);
  assert.equal(host.childElementCount, 0);
});

test('dragging the solo header title off the header sends the terminal back to the dashboard', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const fake = fakeWidgetFactory(harness);
  const [{ createTerminalShellState }, { createTerminalShellActions }, { mountStandaloneTerminalShell }] =
    await Promise.all([
      harness.importDashboardModule('js/terminal-shell-state.js'),
      harness.importDashboardModule('js/terminal-shell-actions.js'),
      harness.importDashboardModule('js/terminal-shell-island.js'),
    ]);
  const state = createTerminalShellState({ persistOrder: false });
  const reattached = [];
  const actions = createTerminalShellActions({
    state,
    windowRef: harness.window,
    documentRef: harness.document,
    fetchImpl: async () => ({ ok: true }),
    onReattachPane: (pane) => { reattached.push(pane.key); return true; },
  });
  const cleanup = mountStandaloneTerminalShell({ host, state, actions, widgetFactory: fake.factory });
  await harness.act(async () => {
    actions.openPane({ ws: '/solo', key: 'solo', label: 'solo terminal' });
    await Promise.resolve();
  });

  const header = host.querySelector('.mux-pane-header');
  // The solo pop-out has no tab strip, so its header is the home region the
  // terminal is dragged out of. Supply the measurement a real layout would.
  header.getBoundingClientRect = () => ({ left: 0, top: 0, right: 900, bottom: 34, width: 900, height: 34 });
  const title = host.querySelector('.mux-pane-title');
  assert.equal(title.classList.contains('mux-pane-title-drag'), true, 'the solo title is the drag handle');
  const transfer = {
    data: {}, effectAllowed: '', dropEffect: '',
    setData(type, value) { this.data[type] = value; },
    getData(type) { return this.data[type] || ''; },
  };
  const dragOver = (init) => harness.act(() => harness.fireEvent(harness.document, 'dragover', init));

  await harness.act(() => harness.fireEvent(title, 'dragstart', { dataTransfer: transfer }));
  assert.equal(transfer.data['application/x-tclaude-terminal-tab'], 'solo');
  await dragOver({ clientX: 300, clientY: 60 });
  assert.equal(host.querySelector('.mux-drag-out-hint'), null, 'a drag still near the header is a near-miss');
  await dragOver({ clientX: 300, clientY: 400 });
  assert.match(host.querySelector('.mux-drag-out-hint').textContent, /back to the dashboard/);
  assert.equal(header.classList.contains('drag-out-armed'), true);
  await harness.act(async () => {
    harness.fireEvent(title, 'dragend', { dataTransfer: transfer, clientX: 300, clientY: 400 });
    await Promise.resolve();
  });
  assert.deepEqual(reattached, ['solo'],
    'the drag runs the same reattach path as the ↩ dashboard button');

  await harness.act(() => harness.fireEvent(title, 'dragstart', { dataTransfer: transfer }));
  await harness.act(async () => {
    harness.fireEvent(title, 'dragend', { dataTransfer: transfer });
    await Promise.resolve();
  });
  assert.deepEqual(reattached, ['solo'], 'a cancelled drag leaves the terminal in its pop-out');
  cleanup();
});
