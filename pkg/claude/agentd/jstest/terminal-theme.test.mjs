import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ARCANE_PALETTE_PREF,
  ARCANE_TERMINAL_THEME,
  TERMINAL_THEME,
  arcanePaletteEnabled,
  initTerminalThemeSync,
  setArcanePaletteEnabled,
  terminalThemeFor,
} from '../dashboard/js/terminal-theme.js';

function prefs(value) {
  return { getItem: key => key === ARCANE_PALETTE_PREF ? value : null };
}

test('arcane terminal palette defaults on and only explicit zero disables it', () => {
  assert.equal(arcanePaletteEnabled(prefs(null)), true);
  assert.equal(arcanePaletteEnabled(prefs('1')), true);
  assert.equal(arcanePaletteEnabled(prefs('0')), false);
});

test('arcane palette is gated by wizard mode and the persisted preference', () => {
  assert.equal(terminalThemeFor(false, true), TERMINAL_THEME);
  assert.equal(terminalThemeFor(true, false), TERMINAL_THEME);
  assert.equal(terminalThemeFor(true, true), ARCANE_TERMINAL_THEME);
});

test('arcane palette preserves semantic ANSI families', () => {
  for (const colour of ['red', 'green', 'yellow', 'blue', 'magenta', 'cyan']) {
    assert.ok(ARCANE_TERMINAL_THEME[colour], `missing ${colour}`);
    const bright = `bright${colour[0].toUpperCase()}${colour.slice(1)}`;
    assert.ok(ARCANE_TERMINAL_THEME[bright], `missing ${bright}`);
  }
});

test('palette changes synchronize across same-origin terminal documents', () => {
  const writes = [];
  const events = [];
  const localPrefs = {
    getItem: () => null,
    setItem: (key, value) => writes.push([key, value]),
  };
  const target = { dispatchEvent: event => events.push(event.detail) };
  class FakeChannel {
    constructor(name) { this.name = name; this.posts = []; FakeChannel.instance = this; }
    addEventListener(type, listener) { if (type === 'message') this.onmessage = listener; }
    postMessage(message) { this.posts.push(message); }
  }

  initTerminalThemeSync(localPrefs, target, FakeChannel);
  setArcanePaletteEnabled(false, localPrefs, target);
  assert.deepEqual(writes, [[ARCANE_PALETTE_PREF, '0']]);
  assert.deepEqual(FakeChannel.instance.posts, [{ type: 'arcane-palette', enabled: false }]);
  assert.deepEqual(events.at(-1), { enabled: false });

  // Simulate a choice arriving from the dashboard/pop-out peer. The receiver
  // persists it too and emits only locally, so no broadcast loop is possible.
  // init is intentionally singleton per document; the listener retains the
  // prefs passed during initialization, matching production module lifetime.
  FakeChannel.instance.onmessage({ data: { type: 'arcane-palette', enabled: true } });
  assert.deepEqual(writes.at(-1), [ARCANE_PALETTE_PREF, '1']);
  assert.deepEqual(events.at(-1), { enabled: true, remote: true });
});
