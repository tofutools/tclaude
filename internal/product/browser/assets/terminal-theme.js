'use strict';
// Palettes retained from the legacy dashboard; explicit terminal RGB output remains unchanged.
const TERMINAL_THEME = Object.freeze({
  background: '#0d1117', foreground: '#c9d1d9', cursor: '#c9d1d9',
  selectionBackground: 'rgba(255,255,255,0.2)',
});

// Keep the standard ANSI meanings recognisable: errors stay red, success stays
// green, warnings stay yellow. The wizard skin shifts the surrounding neutrals
// and accents toward the dashboard's ink-purple / parchment / gold palette
// without rewriting explicit 24-bit colours emitted by terminal applications.
const ARCANE_TERMINAL_THEME = Object.freeze({
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


function terminalThemeFor(wizard, enabled=true){return wizard&&enabled?ARCANE_TERMINAL_THEME:TERMINAL_THEME}
