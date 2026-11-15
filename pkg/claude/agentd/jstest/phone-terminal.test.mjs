// js/phone-terminal-island.js: the phone's docked terminal (one at a time,
// live only while on screen), its key strip, keyboard and message sheet, and
// its place in the location (Back closes it, a link opens an agent's).
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';
// A screen 39 columns wide: a URL soft-wrapped across two rows (the first
// full, as a soft wrap is), a path alone on its line.
const SCREEN = [
  ['● Done. See https://github.com/acme/web', false],
  ['shop/pull/1482 for the PR', true],
  ['/home/dev/git/webshop/notes.md', false],
  ['ok  2.1s', false],
];
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
      // The screen: SCREEN's rows, a cell per character, and a cell per
      // pixel (x a column, y a row) for cellAt.
      screen: () => SCREEN.map(([text, wrapped], row) => ({ row, cells: [...text.padEnd(39)], wrapped })),
      cellAt: (x, y) => ({ row: Math.floor(y), col: Math.floor(x) }),
      link: (raw) => (raw.startsWith('/remote/') ? null : { kind: raw.startsWith('http') ? 'http' : 'file', target: raw }),
      followLink(raw) { this.calls.push(['followLink', raw]); return true; },
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

  assert.deepEqual([...host.querySelectorAll('.phone-term-key')].map((b) => b.textContent), ['1', '2', '3', '↑', '↓', '⏎', 'Esc', '^C', 'Copy']);
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
  assert.equal(made.at(-1).options.swipeWheel, 'claude', 'Claude Code\'s own wheel model');
  await harness.act(() => tab.openWebWindowPane('agt_00000000000000000000000000000002', 'codex-one', { harness: 'codex' }));
  await harness.act(() => {});
  assert.equal(made.at(-1).options.wheelCopyMode, true, 'Codex: tmux copy-mode would take keys');
  assert.equal(made.at(-1).options.swipeWheel, 'tmux', 'Codex: five lines a notch');
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

test('swipeWheel: Claude Code\'s model only for a harness known to be Claude Code\'s', async (t) => {
  const harness = await createPreactHarness(t);
  const { swipeWheel } = await harness.importDashboardModule('js/phone-terminal-island.js');
  const agent = (id) => (id === 'agt_cc' ? { harness: 'claude' } : id === 'agt_codex' ? { harness: 'codex' } : null);
  assert.equal(swipeWheel({ ws: '/api/open-window-ws/x', agent: 'agt_x', harness: 'claude' }, { agent }), 'claude');
  assert.equal(swipeWheel({ ws: '/api/open-window-ws/agt_cc', agent: 'agt_cc' }, { agent }), 'claude', 'the agent\'s harness');
  assert.equal(swipeWheel({ ws: '/api/open-window-ws/agt_codex', agent: 'agt_codex' }, { agent }), 'tmux');
  assert.equal(swipeWheel({ ws: '/api/open-window-ws/agt_gone', agent: 'agt_gone' }, { agent }), 'tmux',
    'not known yet (a deep link before the snapshot): five lines a notch, which at worst lags');
  assert.equal(swipeWheel({ ws: '/api/group-term-ws/g' }, { agent }), 'tmux');
});

test('the bar names the agent and its group, wears its state, and ⋯ opens its sheet', async (t) => {
  const row = { key: AGENT, agent: AGENT, name: 'checkout-ui', group: 'webshop', variant: 'asking', online: true };
  const menus = [];
  const ctx = await setup(t, {
    agentRow: (id) => (id === AGENT ? { row, group: { key: 'g:webshop', name: 'webshop' } } : null),
    agentMenu: (r) => menus.push(r.key),
  });
  const { harness, tab, host, button, tap } = ctx;
  // A link opened before the snapshot came knows only the agent's id: the
  // bar takes the name from the agent's row.
  await harness.act(() => tab.openWebWindowPane(AGENT, AGENT, { harness: 'claude' }));
  const title = host.querySelector('.phone-term-bar .phone-bar-title');
  assert.equal(title.textContent, 'checkout-uiwebshop');
  assert.equal(title.querySelector('small').textContent, 'webshop');
  const pill = host.querySelector('.phone-term-bar .phone-pill');
  assert.equal(pill.textContent.trim(), 'asking');
  assert.match(pill.className, /phone-pill-ask/);
  assert.equal(host.querySelector('.phone-bar-live'), null, 'the state pill stands in for the live dot');
  await tap(button('checkout-ui: more'));
  assert.deepEqual(menus, [AGENT]);
  // The widget's own message key (Ctrl+M) takes the bar's name too.
  const { composed, made } = ctx;
  made[0].options.onComposeMessage();
  assert.deepEqual(composed.at(-1), { agent: AGENT, label: 'checkout-ui' });
  assert.match(host.querySelector('.phone-term-compose').textContent, /✉ Message checkout-ui…/);
});

test('a terminal with no agent row keeps the live dot and has no ⋯', async (t) => {
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

// touch dispatches a touch event at (x, y) on the terminal's host: a cell
// per pixel, as the fake widget's cellAt reads it.
function touch(harness, el, type, x = 0, y = 0, fingers = 1) {
  return harness.act(() => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    const list = type === 'touchend' || type === 'touchcancel' ? [] : Array.from({ length: fingers }, () => ({ clientX: x, clientY: y }));
    Object.assign(event, { touches: list });
    el.dispatchEvent(event);
  });
}

async function openTerminal(t) {
  const env = await setup(t, { copy: async (text, what) => { env.copied.push([text, what]); } });
  env.copied = [];
  const sheet = await env.harness.importDashboardModule('js/phone-sheet.js');
  const sheetHost = env.harness.document.createElement('div');
  env.harness.document.body.append(sheetHost);
  await env.harness.act(() => env.harness.preact.render(env.harness.html`<${sheet.PhoneSheetHost} />`, sheetHost));
  t.after(() => sheet.closePhoneSheet());
  await env.harness.act(() => env.tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  await env.harness.act(() => {});
  return { ...env, sheet, sheetHost, surface: env.host.querySelector('.phone-term-host') };
}

test('a tap on a link opens its sheet: where it goes, Open or Download, Copy; a tap elsewhere is a tap', async (t) => {
  const { harness, made, surface, sheetHost, copied } = await openTerminal(t);
  const [widget] = made;
  const tapAt = async (x, y) => { await touch(harness, surface, 'touchstart', x, y); await touch(harness, surface, 'touchend'); };
  const items = () => [...sheetHost.querySelectorAll('.phone-sheet .phone-menu-item')].map((b) => b.textContent.trim());
  // The URL's second row, wrapped: the whole URL.
  await tapAt(3, 1);
  assert.equal(sheetHost.querySelector('.phone-term-link-target').textContent, 'https://github.com/acme/webshop/pull/1482');
  assert.deepEqual(items(), ['Open link ›', 'Copy link']);
  await harness.act(() => sheetHost.querySelector('.phone-sheet .phone-menu-item').click());
  assert.deepEqual(widget.calls.at(-1), ['followLink', 'https://github.com/acme/webshop/pull/1482']);
  assert.equal(sheetHost.querySelector('.phone-sheet'), null, 'it closes');

  await tapAt(10, 2);
  assert.deepEqual(items(), ['Download notes.md', 'Copy path']);
  await harness.act(() => [...sheetHost.querySelectorAll('.phone-sheet .phone-menu-item')][1].click());
  assert.deepEqual(copied.at(-1), ['/home/dev/git/webshop/notes.md', 'the path']);

  const calls = widget.calls.length;
  await tapAt(2, 3);
  assert.equal(sheetHost.querySelector('.phone-sheet'), null, 'no link there');
  // A swipe is not a tap.
  await touch(harness, surface, 'touchstart', 15, 0);
  await touch(harness, surface, 'touchmove', 15, 40);
  await touch(harness, surface, 'touchend');
  assert.equal(sheetHost.querySelector('.phone-sheet'), null, 'a swipe on a link is a scroll');
  assert.equal(widget.calls.length, calls);
});

test('a still hold opens Select with the word under the finger picked; Copy on the strip opens it too', async (t) => {
  const { harness, host, surface, button, tap, copied } = await openTerminal(t);
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const { HOLD_MS } = await harness.importDashboardModule('js/phone-terminal-text.js');
  // Moved first: no Select.
  await touch(harness, surface, 'touchstart', 2, 3);
  await touch(harness, surface, 'touchmove', 2, 30);
  await harness.act(() => t.mock.timers.tick(HOLD_MS));
  assert.equal(host.querySelector('.phone-term-text'), null);
  await touch(harness, surface, 'touchend');
  // Two fingers are a pinch.
  await touch(harness, surface, 'touchstart', 2, 3, 2);
  await harness.act(() => t.mock.timers.tick(HOLD_MS));
  assert.equal(host.querySelector('.phone-term-text'), null);
  await touch(harness, surface, 'touchend');

  await touch(harness, surface, 'touchstart', 1, 3);
  await harness.act(() => t.mock.timers.tick(HOLD_MS));
  await touch(harness, surface, 'touchend');
  const page = host.querySelector('.phone-term-text[role=dialog]');
  assert.equal(page.getAttribute('aria-label'), 'Select text');
  assert.deepEqual([...page.querySelectorAll('.phone-term-text-line')].map((l) => l.textContent),
    ['● Done. See https://github.com/acme/webshop/pull/1482 for the PR', '/home/dev/git/webshop/notes.md', 'ok  2.1s'],
    'soft-wrapped rows are one line');
  assert.deepEqual([...page.querySelectorAll('.phone-term-link')].map((l) => l.dataset.link), ['http', 'file']);
  // The word held is selected, and Copy selection copies it: linkedom has no
  // Selection, so the phone-terminal-select smoke checks those in a browser.
  await tap(button('Copy screen'));
  assert.deepEqual(copied.at(-1), ['● Done. See https://github.com/acme/webshop/pull/1482 for the PR\n/home/dev/git/webshop/notes.md\nok  2.1s', 'the screen']);
  await tap(button('Done'));
  assert.equal(host.querySelector('.phone-term-text'), null);

  await tap(button('Select and copy text'));
  assert.ok(host.querySelector('.phone-term-text'), 'the strip\'s Copy');
  assert.equal(host.querySelector('.phone-term'), host.querySelector('.phone-term-text').parentElement, 'over the terminal, which stays');
});

test('a link this view may not follow offers Copy only, and says why', async (t) => {
  const harness = await createPreactHarness(t);
  const text = await harness.importDashboardModule('js/phone-terminal-text.js');
  const copied = [];
  const widget = { link: () => null, followLink: () => assert.fail('not followed') };
  const mounted = await harness.mount(harness.html`<${text.LinkSheet} link=${{ text: '/remote/x.log', kind: 'file' }} widget=${widget}
    copy=${(v, w) => copied.push([v, w])} close=${() => {}} />`);
  assert.deepEqual([...mounted.container.querySelectorAll('.phone-menu-item')].map((b) => b.textContent.trim()), ['Copy path']);
  assert.match(mounted.container.textContent, /cannot download/);
});

test('the text model: wrapped rows join, links by the desktop\'s rules, a cell maps into its line', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/phone-terminal-text.js');
  const rows = SCREEN.map(([s, wrapped], row) => ({ row: row + 100, cells: [...s.padEnd(39)], wrapped }));
  const lines = m.screenLines(rows);
  assert.equal(lines.length, 3);
  assert.deepEqual(m.indexAt(lines, { row: 101, col: 3 }), { line: 0, index: 42 });
  assert.equal(m.indexAt(lines, { row: 103, col: 30 }), null, 'past the line\'s end');
  assert.deepEqual(m.linksIn('see https://a.example/x). and https://u:p@evil.example/ too').map((l) => l.text), ['https://a.example/x'],
    'trailing punctuation off; credentials refused');
  assert.deepEqual(m.linksIn('edit ./go.mod and /etc/hosts now').map((l) => l.text), [], 'relative paths, and a path mid-prose, stay plain (as on the desktop)');
  assert.deepEqual(m.linksIn('wrote /tmp/out.txt').map((l) => [l.kind, l.text]), [['file', '/tmp/out.txt']]);
  assert.deepEqual(m.linksIn('https://x.example/a/b').map((l) => l.kind), ['http'], 'a URL\'s path is not a file too');
  assert.deepEqual(m.wordAt('go test ./db/...', 4), [3, 7]);
  assert.equal(m.wordAt('a b', 1), null);
});
