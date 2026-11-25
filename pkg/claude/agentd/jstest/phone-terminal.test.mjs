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
const CATALOG = [{ name: 'claude', tmux_scrollback: false }, { name: 'codex', tmux_scrollback: true }, { name: 'shell', tmux_scrollback: true, command_input: true }];

function fakeWidgets() {
  const made = [];
  const factory = (options) => {
    const widget = {
      options, calls: [], disposed: false,
      connect() { this.calls.push(['connect']); return Promise.resolve(); },
      key(name) { this.calls.push(['key', name]); return true; },
      paste(text) { this.calls.push(['paste', text]); return this.connected !== false; },
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

  // The ▾/▴ toggle for the bottom nav sits left of ✉ Message, and the
  // terminal tells the shell it is docked (js/phone-nav-toggle.js).
  const toggles = await harness.importDashboardModule('js/phone-nav-toggle.js');
  const input = host.querySelector('.phone-term-input');
  assert.equal(input.firstElementChild.className, 'phone-nav-toggle');
  assert.equal(input.firstElementChild.getAttribute('aria-label'), 'Hide page selector');
  assert.equal(toggles.terminalDock.value?.keyboard, false);

  await tap(button('Select and copy text'));
  assert.equal(toggles.terminalDock.value?.keyboard, true, 'Select covers the terminal: the nav keeps off its Copy buttons');
  await tap(button('Done'));
  assert.equal(toggles.terminalDock.value?.keyboard, false);

  await tap(button('Keyboard'));
  assert.equal(toggles.terminalDock.value?.keyboard, true, 'the shell keeps the nav off the raised keyboard');
  assert.equal(host.querySelector('.phone-nav-toggle'), null, 'no toggle while typing raw');
  assert.deepEqual(widget.calls.at(-1), ['keyboard', true]);
  assert.equal(button('Keyboard').getAttribute('aria-pressed'), 'true');
  assert.equal(host.querySelector('.phone-term-compose'), null, 'raw typing takes the message button\'s place');

  await tap(button('Back'));
  assert.equal(host.querySelector('.phone-term'), null);
  assert.equal(widget.disposed, true, 'closing drops the socket');
  assert.deepEqual(overlays.at(-1), { closed: true, replace: false }, 'closing steps back');
  assert.equal(harness.document.documentElement.dataset.phoneTerminal, undefined);
  assert.equal(toggles.terminalDock.value, null, 'closed, no terminal is docked');
});

// A shell's input: the shell key strip, and a sheet that types into it with
// no message header, with or without Enter. Group shells and agents of a
// command-input harness alike; any other agent keeps ✉ and 1 2 3.
test('a shell terminal types straight into the shell and has the command-line keys', async (t) => {
  const { harness, tab, made, composed, host, button, tap } = await setup(t, {
    agent: (id) => (id === AGENT ? { label: 'checkout-ui', harness: 'claude' } : id === 'agt_sh' ? { label: 'ops-shell', harness: 'shell' } : null),
  });
  const sheets = await harness.importDashboardModule('js/phone-sheet.js');
  const SHELL_KEYS = ['←', '→', '↑', '↓', '⇥', '␣', '⏎', 'Esc', '^C', 'Copy'];
  const keyLabels = () => [...host.querySelectorAll('.phone-term-key')].map((b) => b.textContent);

  await harness.act(() => tab.openShellResource({ id: 'sh_1', group: 'webshop', ws: '/api/shell-ws/sh_1' }, 'webshop shell'));
  await harness.act(() => {});
  const widget = made.at(-1);
  assert.deepEqual(keyLabels(), SHELL_KEYS);
  assert.equal(host.querySelector('.phone-term-keys').style.getPropertyValue('--phone-term-keys'), '10');
  for (const spoken of ['Left', 'Right', 'Tab', 'Space']) await tap(button(spoken));
  assert.deepEqual(widget.calls.slice(1), [['key', 'left'], ['key', 'right'], ['key', 'tab'], ['key', 'space']]);
  assert.equal(widget.options.onComposeMessage, null, 'no message key');
  assert.equal(host.querySelector('.phone-term-compose:not(.phone-term-type)'), null, 'no ✉ Message');

  const typeButton = host.querySelector('.phone-term-type');
  assert.equal(typeButton.textContent, '› Type into webshop shell…');
  await tap(typeButton);
  const sheet = sheets.phoneSheet.value;
  assert.equal(sheet.label, 'Type into webshop shell');
  let closed = 0;
  const view = await harness.mount(harness.html`<${sheet.Body} ...${sheet.props} close=${() => { closed += 1; }} />`);
  const field = view.container.querySelector('textarea[name="terminal-input"]');
  const press = (label) => harness.act(() => [...view.container.querySelectorAll('button')].find((b) => b.textContent === label).click());
  await press('Type');
  assert.equal(view.container.querySelector('[role=alert]').textContent, 'Write something to type.');
  await harness.act(() => { field.value = 'git status'; field.dispatchEvent(new harness.window.Event('input', { bubbles: true })); });
  widget.calls.length = 0;
  await press('Type + ⏎');
  assert.deepEqual(widget.calls, [['paste', 'git status'], ['key', 'enter']], 'the text as typed, no header, then Enter');
  assert.equal(closed, 1);
  widget.calls.length = 0;
  await press('Type');
  assert.deepEqual(widget.calls, [['paste', 'git status']], 'Type leaves the line for more');
  widget.connected = false;
  widget.calls.length = 0;
  await press('Type');
  assert.equal(view.container.querySelector('[role=alert]').textContent, 'The terminal took no input (not connected, or view only), so nothing was typed.');
  assert.equal(closed, 2, 'a send the terminal refused keeps the sheet open');

  // An agent of a command-input harness (--harness shell) is a shell too.
  await harness.act(() => tab.openWebWindowPane('agt_sh', 'ops-shell', { harness: 'shell' }));
  await harness.act(() => {});
  assert.deepEqual(keyLabels(), SHELL_KEYS);
  assert.ok(host.querySelector('.phone-term-type'));
  made.at(-1).options.onComposeMessage();
  assert.equal(composed.length, 0, 'its Ctrl+M messages nobody');

  // An agent's directory shell is a shell, named for itself, whatever runs in the agent.
  await harness.act(() => tab.openShellResource({ id: `term:${AGENT}:current:webshop`, agent: AGENT, which: 'current' }, 'checkout-ui shell'));
  await harness.act(() => {});
  assert.deepEqual(keyLabels(), SHELL_KEYS);
  assert.equal(host.querySelector('.phone-term-type').textContent, '› Type into checkout-ui shell…');
  assert.equal(made.at(-1).options.onComposeMessage, null);

  // Every other harness keeps the message sheet and 1 2 3.
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  await harness.act(() => {});
  assert.deepEqual(keyLabels(), ['1', '2', '3', '↑', '↓', '⏎', 'Esc', '^C', 'Copy']);
  assert.equal(host.querySelector('.phone-term-type'), null);
  await tap(host.querySelector('.phone-term-compose'));
  assert.deepEqual(composed.at(-1), { agent: AGENT, label: 'checkout-ui' });
});

test('terminalKind: no agent or a command-input harness is a shell; any other or unknown harness an agent', async (t) => {
  const harness = await createPreactHarness(t);
  const { terminalKind } = await harness.importDashboardModule('js/phone-terminal-island.js');
  const harnesses = CATALOG;
  const win = '/api/open-window-ws/agt_a';
  assert.equal(terminalKind({ ws: '/api/group-term-ws/webshop' }, { harnesses }), 'shell');
  assert.equal(terminalKind({ ws: '/api/term-ws/agt_a?which=current', agent: 'agt_a', harness: 'claude' }, { harnesses }), 'shell', 'an agent\'s directory shell');
  assert.equal(terminalKind({ ws: win, agent: 'agt_a', harness: 'shell' }, { harnesses }), 'shell');
  assert.equal(terminalKind({ ws: win, agent: 'agt_a', harness: 'codex' }, { harnesses }), 'agent');
  assert.equal(terminalKind({ ws: win, agent: 'agt_a' }, { harnesses, agent: () => ({ harness: 'shell' }) }), 'shell', 'the harness from the agent');
  assert.equal(terminalKind({ ws: win, agent: 'agt_a', harness: 'shell' }, { harnesses: [] }), 'agent', 'a catalog not yet loaded is an agent\'s');
  const peer = '/api/federation/terminal?';
  assert.equal(terminalKind({ ws: `${peer}peer=n1&agent=agt_r&mode=watch`, remote: { agent: 'agt_r' } }, { harnesses }), 'agent');
  assert.equal(terminalKind({ ws: `${peer}peer=n1&group=g&shell=current&agent=agt_r&mode=interactive`, remote: { agent: 'agt_r' } }, { harnesses }), 'shell', 'a peer agent\'s directory shell');
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
  assert.equal(made.at(-1).options.wheelCopyMode(), false, 'Claude Code: as on the desktop');
  assert.equal(made.at(-1).options.swipeWheel(), 'claude', 'Claude Code\'s own wheel model');
  await harness.act(() => tab.openWebWindowPane('agt_00000000000000000000000000000002', 'codex-one', { harness: 'codex' }));
  await harness.act(() => {});
  assert.equal(made.at(-1).options.wheelCopyMode(), true, 'Codex: tmux copy-mode would take keys');
  assert.equal(made.at(-1).options.swipeWheel(), 'tmux', 'Codex: five lines a notch');
});

test('a terminal opened before the page knows its agent takes Claude Code\'s wheel and history once the snapshot names it', async (t) => {
  // A cold link (/?terminal=window:<agent>): the widget is made before the
  // first snapshot, so neither the agent nor the harness catalog is known.
  let known = null;
  let catalog = [];
  const { harness, tab, made } = await setup(t, { agent: (id) => (id === AGENT ? known : null), harnesses: () => catalog });
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', {}));
  await harness.act(() => {});
  const { swipeWheel, wheelCopyMode } = made.at(-1).options;
  assert.equal(swipeWheel(), 'tmux', 'unknown yet: the safe mapping');
  assert.equal(wheelCopyMode(), true, 'no catalog yet: keep ↓ Live');
  known = { label: 'checkout-ui', harness: 'claude' };
  catalog = CATALOG;
  assert.equal(swipeWheel(), 'claude', 'read again at the next notch');
  assert.equal(wheelCopyMode(), false, 'Claude Code scrolls its own history: no ↓ Live');
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

test('the bar names the agent after its full group, wears its state, and ⋯ opens its sheet', async (t) => {
  const row = { key: AGENT, agent: AGENT, name: 'checkout-ui', group: 'webshop', variant: 'asking', online: true };
  const menus = [];
  const ctx = await setup(t, {
    agentRow: (id) => (id === AGENT ? { row, group: { key: 'g:webshop', name: 'webshop' } } : null),
    agentMenu: (r) => menus.push(r.key),
    groupMark: (id, openedFrom) => (id === AGENT && openedFrom === 'webshop' ? { group: 'webshop', groups: ['webshop'] } : null),
  });
  const { harness, tab, host, button, tap } = ctx;
  // A link opened before the snapshot came knows only the agent's id: the
  // bar takes the name from the agent's row.
  await harness.act(() => tab.openWebWindowPane(AGENT, AGENT, { harness: 'claude' }));
  const title = host.querySelector('.phone-term-bar .phone-bar-title');
  assert.equal(title.textContent, 'webshop/checkout-ui');
  assert.equal(title.querySelector('.agent-group-prefix').textContent, 'webshop');
  assert.equal(title.getAttribute('title'), 'webshop/checkout-ui');
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
  let event;
  return harness.act(() => {
    event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    const list = type === 'touchend' || type === 'touchcancel' ? [] : Array.from({ length: fingers }, () => ({ clientX: x, clientY: y }));
    Object.assign(event, { touches: list });
    el.dispatchEvent(event);
  }).then(() => event);
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
  const tapAt = async (x, y) => { await touch(harness, surface, 'touchstart', x, y); return touch(harness, surface, 'touchend'); };
  const items = () => [...sheetHost.querySelectorAll('.phone-sheet .phone-menu-item')].map((b) => b.textContent.trim());
  // The URL's second row, wrapped: the whole URL. The tap's click is
  // cancelled: it would land on the sheet just opened under the finger.
  assert.equal((await tapAt(3, 1)).defaultPrevented, true);
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
  assert.equal((await tapAt(2, 3)).defaultPrevented, false, 'a plain tap keeps its click');
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

test('screenRows: a line cut by the screen\'s edges comes whole, so its tail is not a link of its own', async (t) => {
  const harness = await createPreactHarness(t);
  const [interactions, m] = await Promise.all([
    harness.importDashboardModule('js/terminal-interactions.js'),
    harness.importDashboardModule('js/phone-terminal-text.js'),
  ]);
  const cell = (ch) => ({ getWidth: () => 1, getChars: () => ch, isInvisible: () => false });
  const line = (text, isWrapped) => ({ isWrapped, getCell: (x) => cell(text[x] ?? ' ') });
  // Rows 0-1: a path wrapped over the top edge; 2: on screen; 3-4: a URL
  // wrapped past the bottom edge. The screen is rows 1-3.
  const buffer = ['saved /home/u/proj/b', 'uild/out.tar', 'ok', 'see https://x.exampl', 'e/a/b'].map((t2, i) => line(t2.padEnd(20), [false, true, false, false, true][i]));
  const term = { rows: 3, cols: 20, buffer: { active: { viewportY: 1, getLine: (y) => buffer[y] } } };
  const lines = m.screenLines(interactions.screenRows(term));
  assert.deepEqual(lines.map((l) => l.text), ['saved /home/u/proj/build/out.tar', 'ok', 'see https://x.example/a/b']);
  assert.deepEqual(lines.flatMap((l) => m.linksIn(l.text).map((k) => k.text)), ['/home/u/proj/build/out.tar', 'https://x.example/a/b']);
  // A wrap flag below a row that does not fill the width joins nothing.
  buffer[3] = line('wrote /tmp/notes.md'.padEnd(20), false);
  assert.deepEqual(m.screenLines(interactions.screenRows(term)).at(-1).text, 'wrote /tmp/notes.md');
});

test('an agent\'s directory shell is titled a shell, with the directory under it, and wears no agent state', async (t) => {
  const row = {
    key: AGENT, agent: AGENT, name: 'checkout-ui', group: 'webshop', variant: 'asking', online: true,
    member: { current_dir: '/home/dev/git/webshop-wt', startup_dir: '/home/dev/git/webshop' },
  };
  const ctx = await setup(t, { agentRow: (id) => (id === AGENT ? { row, group: { key: 'g:webshop', name: 'webshop' } } : null) });
  const { harness, tab, host } = ctx;
  await harness.act(() => tab.openWebTermPane(AGENT, 'shell · checkout-ui', 'worktree', { group: 'webshop' }));
  await harness.act(() => {});
  const title = host.querySelector('.phone-term-bar .phone-bar-title');
  assert.equal(title.firstChild.textContent, '›_ Shell · checkout-ui\'s dir');
  assert.equal(title.querySelector('small').textContent, '/home/dev/git/webshop-wt');
  assert.equal(host.querySelector('.phone-term-bar .phone-pill'), null, 'no agent state on a shell');
  assert.ok(host.querySelector('.phone-term-bar .phone-bar-live'), 'the connection dot, as any shell');
  assert.equal(host.querySelector('.phone-term').getAttribute('aria-label'), '›_ Shell · checkout-ui\'s dir');

  // The launch directory, and the current one, whose path the daemon keeps.
  const { dirShellTitle } = await harness.importDashboardModule('js/phone-terminal-island.js');
  const seed = (which) => ({ ws: `/api/term-ws/${AGENT}?which=${which}`, key: `term:${AGENT}:${which}:webshop`, agent: AGENT, label: 'shell · checkout-ui' });
  assert.deepEqual(dirShellTitle(seed('start'), row), { label: '›_ Shell · checkout-ui\'s dir', sub: '/home/dev/git/webshop' });
  assert.deepEqual(dirShellTitle(seed('current'), row), { label: '›_ Shell · checkout-ui\'s dir', sub: 'where it last edited' });
  assert.deepEqual(dirShellTitle(seed('worktree'), null), { label: '›_ Shell · checkout-ui\'s dir', sub: 'its worktree' }, 'before the snapshot: the label names it');
  // Not a directory shell: the agent's own terminal, a group shell.
  assert.equal(dirShellTitle({ ws: `/api/open-window-ws/${AGENT}`, agent: AGENT }, row), null);
  assert.equal(dirShellTitle({ ws: '/api/group-term-ws/webshop' }, null), null);
});

// tk_1e2337c3: a keyboard raised for a sheet's own field (✉ Message, Type
// into) over the terminal does not shrink the terminal, so it is not refitted
// and the agent's screen is not resized. The terminal's own keyboard still
// shrinks it, and when the sheet lets go the terminal follows the screen once
// it has grown back (or after a while), in one step.
test('a compose sheet\'s keyboard leaves the terminal its size; the terminal\'s own keyboard still shrinks it', async (t) => {
  const listeners = { resize: new Set(), scroll: new Set() };
  const viewport = {
    height: 800, width: 390, offsetTop: 0, scale: 1,
    addEventListener: (type, fn) => listeners[type].add(fn),
    removeEventListener: (type, fn) => listeners[type].delete(fn),
  };
  const timers = [];
  const windowRef = {
    visualViewport: viewport, innerHeight: 800,
    setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
    clearTimeout: (id) => { if (timers[id - 1]) timers[id - 1].fn = null; },
  };
  const { harness, tab, host } = await setup(t, { windowRef });
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  await harness.act(() => {});
  const height = () => host.querySelector('.phone-term').getAttribute('style');
  const resize = (h) => harness.act(() => { viewport.height = h; for (const fn of listeners.resize) fn(); });
  const focusIn = (el) => harness.act(() => { el.focus(); harness.document.dispatchEvent(new harness.window.Event('focusin')); });
  const focusOut = (el) => harness.act(() => { el.blur(); harness.document.dispatchEvent(new harness.window.Event('focusout')); });
  assert.match(height(), /height:800px/);

  // The ✉ Message sheet's field takes the keyboard: the terminal keeps 800.
  const sheet = harness.document.createElement('textarea');
  harness.document.body.append(sheet);
  await focusIn(sheet);
  await resize(500);
  assert.match(height(), /height:800px/, 'no refit behind the sheet');
  // It closes: the keyboard goes down in steps; one refit when it is back.
  await focusOut(sheet);
  await resize(650);
  assert.match(height(), /height:800px/, 'not on the way down');
  await resize(800);
  assert.match(height(), /height:800px/);
  assert.equal(timers.filter((timer) => timer.fn).length, 0, 'settled: nothing left waiting');

  // Hopping between two fields of one sheet keeps the hold.
  const other = harness.document.createElement('input');
  harness.document.body.append(other);
  await focusIn(sheet);
  await resize(500);
  await focusOut(sheet);
  await focusIn(other);
  assert.match(height(), /height:800px/);
  assert.equal(timers.filter((timer) => timer.fn).length, 0, 'the hop cancelled the wait');
  // Rotated while composing: a new screen, the box follows it.
  await harness.act(() => { viewport.width = 844; viewport.height = 300; for (const fn of listeners.resize) fn(); });
  assert.match(height(), /height:300px/, 'not the portrait height on a landscape screen');
  await harness.act(() => { viewport.width = 390; viewport.height = 800; for (const fn of listeners.resize) fn(); });
  await focusOut(other);
  await resize(800);
  assert.match(height(), /height:800px/);
  // A checkbox raises no keyboard: no hold.
  const box = harness.document.createElement('input');
  box.setAttribute('type', 'checkbox');
  harness.document.body.append(box);
  await focusIn(box);
  await resize(600);
  assert.match(height(), /height:600px/);
  await focusOut(box);
  await resize(800);
  other.remove();
  box.remove();

  // The terminal's own input: the keyboard shrinks it so the line stays in sight.
  const own = harness.document.createElement('textarea');
  host.querySelector('.phone-term-host').append(own);
  await focusIn(own);
  await resize(500);
  assert.match(height(), /height:500px/, 'the terminal\'s keyboard still resizes it');
  await focusOut(own);
  await resize(800);
  assert.match(height(), /height:800px/);

  // A sheet that closes with the keyboard left up: after a while the
  // terminal follows the screen anyway.
  await focusIn(sheet);
  await resize(500);
  await focusOut(sheet);
  assert.match(height(), /height:800px/);
  const settle = timers.at(-1);
  assert.equal(settle.ms > 0, true);
  await harness.act(() => settle.fn());
  assert.match(height(), /height:500px/);
  sheet.remove();
});

test('the Steward\'s terminal floats over the page and docks back; the keyboard docks it; no other agent\'s floats', async (t) => {
  const { harness, island, tab, host, button, tap } = await setup(t, { isSteward: (id) => id === AGENT });
  const nav = await harness.importDashboardModule('js/phone-nav-toggle.js');
  t.after(() => { island.phoneTermFloat.value = false; });
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  await harness.act(() => {});
  const term = () => host.querySelector('.phone-term');
  const root = harness.document.documentElement;
  assert.equal(button('Float over the page').getAttribute('aria-pressed'), 'false');
  assert.equal(term().getAttribute('aria-modal'), 'true');

  await tap(button('Float over the page'));
  assert.ok(term().classList.contains('phone-term-float'));
  assert.equal(term().getAttribute('aria-modal'), 'false', 'the page around it stays usable');
  assert.equal(root.dataset.phoneTerminal, 'float', 'the page scrolls under a floating terminal');
  assert.equal(nav.terminalDock.value.float, true, 'the shell keeps its view and nav');
  assert.equal(button('Full screen').getAttribute('aria-pressed'), 'true');

  await tap(button('Full screen'));
  assert.ok(!term().classList.contains('phone-term-float'));
  assert.equal(root.dataset.phoneTerminal, 'open');

  await tap(button('Float over the page'));
  await tap(button('Keyboard'));
  assert.ok(!term().classList.contains('phone-term-float'), 'typing docks it full screen');
  assert.equal(island.phoneTermFloat.value, false);
});

test('an agent that is not the Steward has no float button', async (t) => {
  const { harness, tab, button } = await setup(t, { isSteward: () => false });
  await harness.act(() => tab.openWebWindowPane(AGENT, 'checkout-ui', { harness: 'claude' }));
  await harness.act(() => {});
  assert.equal(button('Float over the page'), undefined);
});
