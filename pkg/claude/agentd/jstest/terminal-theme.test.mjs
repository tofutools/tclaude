import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ARCANE_PALETTE_PREF,
  ARCANE_TERMINAL_THEME,
  TERMINAL_THEME,
  arcanePaletteEnabled,
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
