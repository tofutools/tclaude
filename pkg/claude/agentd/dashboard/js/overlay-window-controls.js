// overlay-window-controls.js -- an overlay window's minimize, maximize and close
// buttons, drawn like the native ones of the browser's OS. The look
// decides the order too: macOS leads with close, the others end on it.
import { h } from 'preact';
import htm from 'htm';
import { browserOS } from './helpers.js';

const html = htm.bind(h);

const LOOKS = Object.freeze(['macos', 'windows', 'linux', 'classic']);

// windowControlsLook resolves the dashboard.window_controls setting: anything
// but a named look, such as "auto" or a snapshot from before the setting,
// follows the OS.
export function windowControlsLook(setting, os = browserOS()) {
  return LOOKS.includes(setting) ? setting : os;
}

const SQUARE = 'M1.5 .5h7a1 1 0 0 1 1 1v7a1 1 0 0 1-1 1h-7a1 1 0 0 1-1-1v-7a1 1 0 0 1 1-1z';
const STACKED = 'M.5 3.5h6v6h-6zM3.5 3.5v-3h6v6h-3';
const GLYPHS = Object.freeze({
  macos: { minimize: 'M1.5 5h7', maximize: 'M2 5.5v2.5h2.5M8 4.5v-2.5h-2.5', restore: 'M4.5 8v-2.5h-2.5M5.5 2v2.5h2.5', close: 'M2 2l6 6M8 2l-6 6' },
  windows: { minimize: 'M0 5h10', maximize: SQUARE, restore: STACKED, close: 'M.5 .5l9 9M9.5 .5l-9 9' },
  linux: { minimize: 'M1 8h8', maximize: SQUARE, restore: STACKED, close: 'M1 1l8 8M9 1l-8 8' },
});

function Glyph({ path }) {
  return html`<svg viewBox="0 0 10 10" aria-hidden="true"><path d=${path} /></svg>`;
}

export function WindowControls({ look, maximized, closeLabel, closeTitle, onMinimize, onMaximize, onClose }) {
  const glyphs = GLYPHS[look];
  const sizeLabel = maximized ? 'Restore' : 'Maximize';
  const minimize = onMinimize && html`<button type="button" class="overlay-wc-minimize" aria-label="Minimize (Ctrl+.)" title="Minimize (Ctrl+.)"
    onClick=${onMinimize}><${Glyph} path=${glyphs.minimize} /></button>`;
  const size = html`<button type="button" class="overlay-wc-maximize" aria-label=${sizeLabel} title=${sizeLabel}
    onClick=${onMaximize}><${Glyph} path=${maximized ? glyphs.restore : glyphs.maximize} /></button>`;
  const close = html`<button type="button" class="overlay-wc-close" aria-label=${closeLabel} title=${closeTitle}
    onClick=${onClose}><${Glyph} path=${glyphs.close} /></button>`;
  return html`<div class="overlay-window-controls" role="group" aria-label="Window">
    ${look === 'macos' ? [close, minimize, size] : [minimize, size, close]}
  </div>`;
}
