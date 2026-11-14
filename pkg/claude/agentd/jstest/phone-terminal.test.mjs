// js/phone-terminal-island.js: the phone's docked terminal (one at a time,
// live only while on screen), its key strip, keyboard and message sheet, and
// its place in the location (Back closes it, a link opens an agent's).
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';

function fakeWidgets() {
  const made = [];
  const factory = (options) => {
    const widget = {
      options, calls: [], disposed: false,
      connect() { this.calls.push(['connect']); return Promise.resolve(); },
      key(name) { this.calls.push(['key', name]); return true; },
      setSoftKeyboard(on) { this.calls.push(['keyboard', on]); },
      dispose() { this.disposed = true; },
    };
    made.push(widget);
    options.onStatus?.('connected');
    return widget;
  };
  return { made, factory };
}

async function setup(t) {
  // Unmount before the harness takes its DOM globals away.
  const cleanups = [];
  t.after(() => cleanups.forEach((fn) => fn()));
  const harness = await createPreactHarness(t);
  const [island, routes, tab] = await Promise.all([
    harness.importDashboardModule('js/phone-terminal-island.js'),
    harness.importDashboardModule('js/overlay-routes.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
  ]);
  const { made, factory } = fakeWidgets();
  const composed = [];
  const overlays = [];
  harness.document.addEventListener(routes.OVERLAY_EVENT, (event) => overlays.push(event.detail));
  const deps = {
    widgetFactory: factory,
    widgetOptions: () => ({}),
    onComposeMessage: (seed) => composed.push(seed),
    agent: (id) => (id === AGENT ? { label: 'checkout-ui', harness: 'claude' } : null),
    prefs: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    documentRef: harness.document,
    windowRef: {},
  };
  const host = harness.document.createElement('div');
  harness.document.body.append(host);
  await harness.act(() => island.mountPhoneTerminalIsland({
    host, deps, register: tab.setPhoneTerminals, registerCleanup: (fn) => cleanups.push(fn),
  }));
  const button = (label) => [...host.querySelectorAll('button')].find((b) => b.getAttribute('aria-label') === label || b.textContent.trim() === label);
  const tap = (element) => harness.act(() => element.click());
  return { harness, island, routes, tab, made, composed, overlays, host, button, tap };
}

test('an agent terminal opens docked, live, with the key strip and its message sheet', async (t) => {
  const { harness, tab, made, composed, overlays, host, button, tap } = await setup(t);
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  await harness.act(() => {});
  const dialog = host.querySelector('.phone-term[role=dialog]');
  assert.equal(dialog.getAttribute('aria-label'), 'Terminal: checkout-ui');
  assert.equal(host.querySelector('.phone-bar-title').textContent, 'checkout-ui');
  assert.equal(made.length, 1, 'one widget');
  const [widget] = made;
  assert.equal(widget.options.wsPath, `/api/open-window-ws/${AGENT}`);
  assert.deepEqual([widget.options.touchZoom, widget.options.softKeyboard, widget.options.initialFontSize], [true, false, 11]);
  assert.deepEqual(widget.calls[0], ['connect']);
  assert.deepEqual(overlays.at(-1), { closed: false, replace: false }, 'opening is a history step');
  assert.equal(harness.document.documentElement.dataset.phoneTerminal, 'open');

  assert.deepEqual([...host.querySelectorAll('.phone-term-key')].map((b) => b.textContent), ['1', '2', '3', '↑', '↓', '⏎', 'Esc', '^C']);
  await tap(button('2'));
  await tap(button('Up'));
  await tap(button('Control C'));
  assert.deepEqual(widget.calls.slice(1), [['key', '2'], ['key', 'up'], ['key', 'ctrl-c']]);

  await tap(host.querySelector('.phone-term-compose'));
  assert.deepEqual(composed, [{ agent: AGENT, label: 'checkout-ui' }]);

  await tap(button('Keyboard'));
  assert.deepEqual(widget.calls.at(-1), ['keyboard', true]);
  assert.equal(button('Keyboard').getAttribute('aria-pressed'), 'true');
  assert.equal(host.querySelector('.phone-term-compose'), null, 'raw typing takes the message button\'s place');

  await tap(button('Back'));
  assert.equal(host.querySelector('.phone-term'), null);
  assert.equal(widget.disposed, true, 'closing drops the socket');
  assert.deepEqual(overlays.at(-1), { closed: true, replace: false }, 'closing steps back');
  assert.equal(harness.document.documentElement.dataset.phoneTerminal, undefined);
});

test('only the terminal on screen is live: a hidden page drops it, coming back redials', async (t) => {
  const { harness, tab, made } = await setup(t);
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', {}));
  await harness.act(() => {});
  const visibility = (state) => harness.act(() => {
    Object.defineProperty(harness.document, 'visibilityState', { value: state, configurable: true });
    harness.document.dispatchEvent(new harness.window.Event('visibilitychange'));
  });
  await visibility('hidden');
  assert.equal(made[0].disposed, true);
  await visibility('visible');
  assert.equal(made.length, 2);
  assert.equal(made[1].disposed, false);
  assert.deepEqual(made[1].calls[0], ['connect']);
});

test('the location: Back closes it, Forward or a link reopens an agent\'s terminal', async (t) => {
  const { harness, routes, tab, made, host } = await setup(t);
  await harness.act(() => routes.restoreOverlays({ terminal: `window:${AGENT}` }));
  assert.equal(host.querySelector('.phone-bar-title').textContent, 'checkout-ui', 'named from the snapshot');
  assert.equal(made.at(-1).options.wsPath, `/api/open-window-ws/${AGENT}`);
  assert.equal(routes.overlayParams().terminal, `window:${AGENT}`);
  await harness.act(() => routes.restoreOverlays({}));
  assert.equal(host.querySelector('.phone-term'), null);
  assert.equal(made.at(-1).disposed, true);

  // A shell this page opened comes back on Forward; a key nothing knows does not.
  await harness.act(() => tab.openShellResource({ id: 'sh_1', group: 'webshop', ws: '/api/shell-ws/sh_1' }, 'webshop shell'));
  await harness.act(() => {});
  const shellKey = routes.overlayParams().terminal;
  await harness.act(() => routes.restoreOverlays({}));
  await harness.act(() => routes.restoreOverlays({ terminal: shellKey }));
  assert.equal(host.querySelector('.phone-bar-title').textContent, 'webshop shell');
  await harness.act(() => routes.restoreOverlays({ terminal: 'term:agt_x:shell:' }));
  assert.equal(host.querySelector('.phone-term'), null);
});

test('on the desktop a phone\'s terminal link opens the agent\'s pane and leaves the location', async (t) => {
  const harness = await createPreactHarness(t);
  const [routes, tab] = await Promise.all([
    harness.importDashboardModule('js/overlay-routes.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
  ]);
  const opened = [];
  const unregister = tab.registerTerminalShellController({ openPane: (seed) => { opened.push(seed); return { key: seed.key }; } });
  t.after(unregister);
  const overlays = [];
  harness.document.addEventListener(routes.OVERLAY_EVENT, (event) => overlays.push(event.detail));
  // The link arrives before the route registers (the router restores early).
  routes.restoreOverlays({ terminal: `window:${AGENT}` });
  const unroute = tab.registerDesktopTerminalRoute((agent) => (agent === AGENT ? 'checkout-ui' : agent));
  t.after(unroute);
  await harness.act(async () => {});
  assert.deepEqual(opened.map((seed) => [seed.ws, seed.label]), [[`/api/open-window-ws/${AGENT}`, 'checkout-ui']]);
  assert.equal(routes.overlayParams().terminal, '', 'no later location carries it');
  assert.deepEqual(overlays.at(-1), { closed: false, replace: true });
});
