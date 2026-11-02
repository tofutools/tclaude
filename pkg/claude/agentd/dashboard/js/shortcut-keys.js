// shortcut-keys.js -- renders shortcuts from the keybindings registry: the
// <Keys> chip row used wherever markup shows a chord, and the read-only list
// in Config › Keyboard.

import { h } from 'preact';
import htm from 'htm';
import { dashboardState } from './snapshot-store.js';
import { SHORTCUTS, SHORTCUT_SCOPES, chordParts, shortcut, shortcutChords, shortcutPlatform } from './keybindings.js';

const html = htm.bind(h);

// Keys shows one chord as <kbd> chips: Ctrl , → [Ctrl] [,].
export function Keys({ id, chord, os }) {
  const parts = chordParts(chord || shortcut(id).chord, os);
  return html`<span class="shortcut-keys">${parts.map((part, i) => html`<kbd key=${i}>${part}</kbd>`)}</span>`;
}

function AllKeys({ id, os }) {
  const chords = shortcutChords(id);
  return chords.map((chord, i) => html`${i > 0 && html`<span class="shortcut-or"> or </span>`}<${Keys} key=${chord} chord=${chord} os=${os} />`);
}

const TERMINAL_COPY = {
  true: 'yes',
  false: 'no',
  setting: 'with a setting',
};

// KeyboardShortcutList is Config › Keyboard: every registered shortcut by
// scope, with its keys on Windows / Linux and on macOS (the column for this
// browser's platform is marked), and whether it fires inside a web terminal.
export function KeyboardShortcutList({ Setting, collab = dashboardState.snapshot.value?.collab_enabled === true }) {
  const here = shortcutPlatform() === 'macos' ? 'macos' : 'pc';
  return SHORTCUT_SCOPES.map(scope => {
    // Collab-only features have no shortcuts while Collab is off.
    const rows = SHORTCUTS.filter(entry => entry.scope === scope.id && (collab || !entry.collab));
    if (!rows.length) return null;
    // Only dashboard-wide keys can collide with a terminal's own.
    const terminalColumn = scope.id === 'global' || scope.id === 'terminal';
    return html`<div class="cfg-section" key=${scope.id} data-shortcut-scope=${scope.id}>
      <h3>${scope.title}</h3>
      <${Setting} title=${`${scope.title}: ${rows.length} shortcut${rows.length === 1 ? '' : 's'}`} wide summary=${scope.summary}>
        <table class="cfg-tool-list cfg-shortcut-list">
          <thead><tr>
            <th scope="col">Action</th>
            <th scope="col" class=${here === 'pc' ? 'is-here' : ''}>Windows & Linux${here === 'pc' ? ' (this browser)' : ''}</th>
            <th scope="col" class=${here === 'macos' ? 'is-here' : ''}>macOS${here === 'macos' ? ' (this browser)' : ''}</th>
            ${terminalColumn && html`<th scope="col">In a terminal</th>`}
          </tr></thead>
          <tbody>${rows.map(entry => html`<tr key=${entry.id} data-shortcut=${entry.id}>
            <td>${entry.label}</td>
            <td class=${here === 'pc' ? 'is-here' : ''}><${AllKeys} id=${entry.id} os="linux" /></td>
            <td class=${here === 'macos' ? 'is-here' : ''}><${AllKeys} id=${entry.id} os="macos" /></td>
            ${terminalColumn && html`<td>${TERMINAL_COPY[String(entry.terminal)]}</td>`}
          </tr>`)}</tbody>
        </table>
      </${Setting}>
    </div>`;
  });
}
