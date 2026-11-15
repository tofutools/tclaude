// js/phone-terminal-island.js: the phone's docked terminal (one at a time,
// live only while on screen), its key strip, keyboard and message sheet, and
// its place in the location (Back closes it, a link opens an agent's).
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';
const CATALOG = [{ name: 'claude', tmux_scrollback: false }, { name: 'codex', tmux_scrollback: true }, { name: 'shell', tmux_scrollback: true }];

function fakeWidgets() {
  const made = [];
  const factory = (options) => {
    const widget = {
      options, calls: [], disposed: false,
      connect() { this.calls.push(['connect']); return Promise.resolve(); },
      key(name) { this.calls.push(['key', name]); return true; },
      setSoftKeyboard(on) { this.calls.push(['keyboard', on]); },
      toLive() { this.calls.push(['toLive']); options.onScrollBackChange?.(false); },
      zoomBy(step) { this.calls.push(['zoomBy', step]); },
      dispose() { this.disposed = true; },
    };
    made.push(widget);
    options.onStatus?.('connected');
    return widget;
  };
  return { made, factory };
}

async function setup(t, extra = {}) {
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
    // The snapshot's harness catalog: Claude Code scrolls its own history,
    // Codex scrolls tmux copy-mode.
    harnesses: () => CATALOG,
    onComposeMessage: (seed) => composed.push(seed),
    agent: (id) => (id === AGENT ? { label: 'checkout-ui', harness: 'claude' } : null),
    prefs: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    documentRef: harness.document,
    windowRef: {},
    ...extra,
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

test('a swipe back through history shows ↓ Live, which comes back to live', async (t) => {
  const { harness, tab, made, host, button, tap } = await setup(t);
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', {}));
  await harness.act(() => {});
  const [widget] = made;
  assert.equal(widget.options.touchScroll, true);
  assert.equal(host.querySelector('.phone-term-live'), null);
  await harness.act(() => widget.options.onScrollBackChange(true));
  await tap(button('Back to live'));
  assert.deepEqual(widget.calls.at(-1), ['toLive']);
  assert.equal(host.querySelector('.phone-term-live'), null);
});

test('↓ Live is for a session whose wheel scrolls tmux copy-mode, not one that scrolls its own history', async (t) => {
  const { harness, tab, made } = await setup(t);
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  await harness.act(() => {});
  assert.equal(made.at(-1).options.touchScroll, true);
  assert.equal(made.at(-1).options.wheelCopyMode, false, 'Claude Code: as on the desktop');
  await harness.act(() => tab.openWebWindowPane('agt_00000000000000000000000000000002', 'codex-one', { harness: 'codex' }));
  await harness.act(() => {});
  assert.equal(made.at(-1).options.wheelCopyMode, true, 'Codex: tmux copy-mode would take keys');
});

test('wheelCopyMode: an agent terminal without a harness takes the agent\'s, else Claude Code\'s; a shell may scroll copy-mode', async (t) => {
  const harness = await createPreactHarness(t);
  const { wheelCopyMode } = await harness.importDashboardModule('js/phone-terminal-island.js');
  const agent = (id) => (id === 'agt_codex' ? { label: 'c', harness: 'codex' } : id === 'agt_bare' ? { label: 'b', harness: '' } : null);
  const deps = { harnesses: CATALOG, agent };
  assert.equal(wheelCopyMode({ ws: '/api/open-window-ws/x', agent: 'agt_x', harness: 'claude' }, deps), false);
  assert.equal(wheelCopyMode({ ws: '/api/open-window-ws/x', agent: 'agt_x', harness: 'codex' }, deps), true);
  // Opened without a harness (phone mail, Get help): the agent's, from the snapshot.
  assert.equal(wheelCopyMode({ ws: '/api/open-window-ws/agt_codex', agent: 'agt_codex' }, deps), true);
  assert.equal(wheelCopyMode({ ws: '/api/open-window-ws/agt_bare', agent: 'agt_bare' }, deps), false, 'no harness known: Claude Code');
  assert.equal(wheelCopyMode({ ws: '/api/open-window-ws/agt_gone', agent: 'agt_gone' }, deps), false);
  // A peer's agent: its row's harness, else Claude Code.
  assert.equal(wheelCopyMode({ ws: '/api/federation/x', remote: { peer: 'p', agent: 'agt_r' } }, deps), false);
  assert.equal(wheelCopyMode({ ws: '/api/federation/x', harness: 'codex', remote: { peer: 'p', agent: 'agt_r' } }, deps), true);
  // No agent at all: a shell or a group terminal.
  assert.equal(wheelCopyMode({ ws: '/api/group-term-ws/g' }, deps), true);
  // A harness the catalog does not list may scroll copy-mode.
  assert.equal(wheelCopyMode({ ws: '/api/open-window-ws/x', agent: 'agt_x', harness: 'newharness' }, deps), true);
  assert.equal(wheelCopyMode({ ws: '/api/open-window-ws/x', agent: 'agt_x', harness: 'claude' }, { agent }), true, 'no catalog yet: keep ↓ Live');
});

test('the bar names the agent and its group, wears its state, and ⋯ opens its sheet', async (t) => {
  const row = { key: AGENT, agent: AGENT, name: 'checkout-ui', group: 'webshop', variant: 'asking', online: true };
  const menus = [];
  const { harness, tab, host, button, tap } = await setup(t, {
    agentRow: (id) => (id === AGENT ? { row, group: { key: 'g:webshop', name: 'webshop' } } : null),
    agentMenu: (r) => menus.push(r.key),
  });
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  const title = host.querySelector('.phone-term-bar .phone-bar-title');
  assert.equal(title.textContent, 'checkout-uiwebshop');
  assert.equal(title.querySelector('small').textContent, 'webshop');
  const pill = host.querySelector('.phone-term-bar .phone-pill');
  assert.equal(pill.textContent.trim(), 'asking');
  assert.match(pill.className, /phone-pill-ask/);
  assert.equal(host.querySelector('.phone-bar-live'), null, 'the state pill stands in for the live dot');
  await tap(button('checkout-ui: more'));
  assert.deepEqual(menus, [AGENT]);
  assert.match(host.querySelector('.phone-term-compose').textContent, /✉ Message checkout-ui…/);
});

test('a terminal of no agent keeps the live dot and has only ⌨', async (t) => {
  const { harness, tab, host } = await setup(t, { agentRow: () => null, agentMenu: () => {} });
  await harness.act(() => tab.openWebWindowPane('agt_unknown', 'shell', { harness: 'shell' }));
  assert.ok(host.querySelector('.phone-bar-live'));
  assert.equal(host.querySelector('.phone-term-bar .phone-ib'), null, 'no ⋯ without an agent row');
});

test('the pinch chip shows the size against the phone\'s own, and a tap resets it', async (t) => {
  const { harness, tab, made, host, tap } = await setup(t);
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  assert.equal(host.querySelector('.phone-term-zoom'), null, 'no chip until the terminal reports its size');
  const [widget] = made;
  await harness.act(() => widget.options.onFontChange({ size: 11, zoomed: true }));
  const chip = () => host.querySelector('.phone-term-zoom');
  assert.equal(chip().textContent, 'pinch · 100%');
  assert.equal(chip().disabled, true, 'nothing to reset at 100%');
  await harness.act(() => widget.options.onFontChange({ size: 15, zoomed: true }));
  assert.equal(chip().textContent, 'pinch · 136%');
  await tap(chip());
  assert.deepEqual(widget.calls.at(-1), ['zoomBy', -4]);
});
