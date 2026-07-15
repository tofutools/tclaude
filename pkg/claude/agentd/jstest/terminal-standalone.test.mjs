import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function encodedSeed(seed) {
  return '#open=' + encodeURIComponent(JSON.stringify(seed));
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
  const prefs = new Promise((resolve) => { resolvePrefs = resolve; });
  const mountShell = ({ state, actions }) => {
    calls.push('mount');
    return () => {
      calls.push('cleanup');
      actions.dispose();
      state.dispose();
    };
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
  });

  const starting = page.start();
  harness.window.dispatchEvent(new harness.window.Event('hashchange'));
  assert.deepEqual(calls, ['prefs'], 'hash is not consumed before preferences hydrate');
  resolvePrefs();
  assert.equal(await starting, true);
  assert.deepEqual(calls, [
    'prefs', 'theme', 'mount', 'replace:/terminals?solo=1',
  ]);
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
  assert.equal(calls.filter((call) => call === 'cleanup').length, 1);
  assert.equal(page.state.panes.value.length, 0);

  const lateAuth = new harness.window.CustomEvent('tclaude:auth-expired', { detail: {} });
  harness.window.dispatchEvent(lateAuth);
  assert.equal(lateAuth.detail.returnTo, undefined, 'disposed lifecycle removes auth writer');
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
  const cleanup = mountStandaloneTerminalShell({
    host, state, actions, widgetFactory: fake.factory,
  });
  await harness.act(() => {});
  assert.ok(host.querySelector('#mux-empty'));

  await harness.act(async () => {
    actions.openPane({ ws: '/solo', key: 'solo', label: 'solo terminal' });
    await Promise.resolve();
  });
  assert.equal(host.querySelector('[role="tablist"]'), null);
  assert.equal(host.querySelector('[title="Move this terminal to its own browser tab"]'), null);
  assert.equal(host.querySelector('#mux-empty'), null);
  assert.equal(fake.widgets.length, 1);
  assert.equal(fake.widgets[0].child.parentElement.classList.contains('mux-pane-xterm'), true);
  assert.equal(fake.widgets[0].activeEdges.at(-1), true);
  assert.equal(fake.widgets[0].connectCount, 1);
  assert.equal(harness.document.title, 'solo terminal — tclaude terminals');

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
