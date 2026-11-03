// overlay-window-controls.js -- an overlay window's title bar buttons. The
// standard look, the default, is plain symbols on the far right, as the rest of
// the dashboard has: − minimize, ⇄ open in the Collab tab, × close. Where a
// window cannot go to Collab, the middle one maximizes, so there are always
// three. The OS looks are opt-in and draw the native minimize, maximize and
// close of the browser's OS; macOS leads with close, the others end on it.
import { h } from 'preact';
import htm from 'htm';
import { browserOS } from './helpers.js';
import { withShortcut } from './keybindings.js';

// The words for a move to the Collab tab, the same for every kind of tab
// (overlay-tab-moves.js says them from here, which keeps this file's imports light).
export const TO_COLLAB_LABEL = 'Open in the Collab tab';

const html = htm.bind(h);

const LOOKS = Object.freeze(['standard', 'macos', 'windows', 'linux', 'classic']);

// windowControlsLook resolves the dashboard.window_controls setting: "os" follows
// the browser's OS, and anything but a named look, such as "auto" or a snapshot
// from before the setting, is the standard look.
export function windowControlsLook(setting, os = browserOS()) {
  if (setting === 'os') return os;
  return LOOKS.includes(setting) ? setting : 'standard';
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

// StandardControls are plain symbols, borderless with the house hover: the
// operator asked for title-bar symbols, so they are exempt from the bordered
// button rule (data-ui-exempt).
function StandardControls({ maximized, closeLabel, closeTitle, onMinimize, onMaximize, onCollab, onClose }) {
  const sizeLabel = maximized ? 'Restore' : 'Maximize';
  return html`<div class="overlay-window-controls overlay-wc-standard" role="group" aria-label="Window">
    ${onMinimize && html`<button data-ui-exempt type="button" class="overlay-wc-minimize" aria-label=${withShortcut('Minimize', 'steward')} title=${withShortcut('Minimize to dock', 'steward')}
      onClick=${onMinimize}>−</button>`}
    ${onCollab
    ? html`<button data-ui-exempt type="button" class="overlay-wc-collab" aria-label=${TO_COLLAB_LABEL} title=${TO_COLLAB_LABEL} onClick=${onCollab}>⇄</button>`
    : html`<button data-ui-exempt type="button" class="overlay-wc-maximize" aria-label=${sizeLabel} title=${sizeLabel} onClick=${onMaximize}>${maximized ? '❐' : '□'}</button>`}
    <button data-ui-exempt type="button" class="overlay-wc-close" aria-label=${closeLabel} title=${closeTitle} onClick=${onClose}>×</button>
  </div>`;
}

export function WindowControls({ look, maximized, closeLabel, closeTitle, onMinimize, onMaximize, onCollab = null, onClose }) {
  if (look === 'standard') return html`<${StandardControls} ...${{ maximized, closeLabel, closeTitle, onMinimize, onMaximize, onCollab, onClose }} />`;
  const glyphs = GLYPHS[look];
  const sizeLabel = maximized ? 'Restore' : 'Maximize';
  const minimize = onMinimize && html`<button data-ui-exempt type="button" class="overlay-wc-minimize" aria-label=${withShortcut('Minimize', 'steward')} title=${withShortcut('Minimize', 'steward')}
    onClick=${onMinimize}><${Glyph} path=${glyphs.minimize} /></button>`;
  const size = html`<button data-ui-exempt type="button" class="overlay-wc-maximize" aria-label=${sizeLabel} title=${sizeLabel}
    onClick=${onMaximize}><${Glyph} path=${maximized ? glyphs.restore : glyphs.maximize} /></button>`;
  const close = html`<button data-ui-exempt type="button" class="overlay-wc-close" aria-label=${closeLabel} title=${closeTitle}
    onClick=${onClose}><${Glyph} path=${glyphs.close} /></button>`;
  return html`<div class="overlay-window-controls" role="group" aria-label="Window">
    ${look === 'macos' ? [close, minimize, size] : [minimize, size, close]}
  </div>`;
}
