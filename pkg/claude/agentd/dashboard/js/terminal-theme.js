// terminal-theme.js — shared xterm palettes + the persisted wizard-palette
// preference used by every browser terminal surface.
//
// The preference is global rather than per-pane: every open terminal repaints
// together, and terminals opened later inherit the same choice. dashPrefs is
// SQLite-backed through /api/dashboard/prefs, so the choice survives agentd's
// random dashboard port, daemon restarts, browser profiles, and separate tabs.

import { dashPrefs } from './prefs.js';

export const ARCANE_PALETTE_PREF = 'tclaude.dash.terminals.arcanePalette';

export const TERMINAL_THEME = Object.freeze({
  background: '#0d1117', foreground: '#c9d1d9', cursor: '#c9d1d9',
  selectionBackground: 'rgba(255,255,255,0.2)',
});

// Keep the standard ANSI meanings recognisable: errors stay red, success stays
// green, warnings stay yellow. The wizard skin shifts the surrounding neutrals
// and accents toward the dashboard's ink-purple / parchment / gold palette
// without rewriting explicit 24-bit colours emitted by terminal applications.
export const ARCANE_TERMINAL_THEME = Object.freeze({
  background: '#120c24',
  foreground: '#e7d9f5',
  cursor: '#f0d066',
  cursorAccent: '#120c24',
  selectionBackground: 'rgba(169,123,214,0.38)',
  selectionInactiveBackground: 'rgba(122,93,176,0.24)',
  black: '#1a1330',
  red: '#d96f78',
  green: '#8fc780',
  yellow: '#d9b45a',
  blue: '#86aee0',
  magenta: '#b184d1',
  cyan: '#70bdb5',
  white: '#d8ccea',
  brightBlack: '#796a91',
  brightRed: '#ef8b94',
  brightGreen: '#a9dc9b',
  brightYellow: '#f0d066',
  brightBlue: '#a8c8ef',
  brightMagenta: '#d2a8ef',
  brightCyan: '#99dcd4',
  brightWhite: '#f3e6c0',
});

// Missing means enabled: wizard mode is meant to arrive fully themed, while a
// human who wants the terminal's neutral palette can opt out once and retain
// that choice. Only the explicit persisted "0" disables it.
export function arcanePaletteEnabled(prefs = dashPrefs) {
  return prefs.getItem(ARCANE_PALETTE_PREF) !== '0';
}

export function terminalThemeFor(wizard, enabled = arcanePaletteEnabled()) {
  return wizard && enabled ? ARCANE_TERMINAL_THEME : TERMINAL_THEME;
}

// setArcanePaletteEnabled updates the synchronous dashPrefs mirror first, then
// broadcasts so every mounted mux and the fallback singleton modal repaint in
// the same turn. The debounced server write follows through prefs.js.
export function setArcanePaletteEnabled(enabled, prefs = dashPrefs, target = document) {
  prefs.setItem(ARCANE_PALETTE_PREF, enabled ? '1' : '0');
  target.dispatchEvent(new CustomEvent('tclaude:terminal-palette', {
    detail: { enabled: !!enabled },
  }));
}
