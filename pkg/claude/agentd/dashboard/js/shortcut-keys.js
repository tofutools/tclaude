// shortcut-keys.js -- renders shortcuts from the keybindings registry: the
// <Keys> chip row used wherever markup shows a chord, and Config › Keyboard,
// which lists every shortcut and rebinds the ones that can be.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  KEYBINDINGS_LIST, SHORTCUTS, SHORTCUT_SCOPES, chordFromEvent, chordParts, chordText, isRebindable, rebindProblems,
  setShortcutRecording, shortcut, shortcutChords, shortcutDefault, shortcutPlatform,
} from './keybindings.js';

const html = htm.bind(h);

// Keys shows one chord as <kbd> chips: Ctrl , → [Ctrl] [,].
export function Keys({ id, chord, os }) {
  const parts = chordParts(chord || shortcut(id).chord, os);
  return html`<span class="shortcut-keys">${parts.map((part, i) => html`<kbd key=${i}>${part}</kbd>`)}</span>`;
}

function ChordList({ chords, os }) {
  return chords.map((chord, i) => html`${i > 0 && html`<span class="shortcut-or"> or </span>`}<${Keys} key=${chord} chord=${chord} os=${os} />`);
}

const TERMINAL_COPY = {
  true: 'yes',
  false: 'no',
  setting: 'with a setting',
};


const draftMap = (drafts) => Object.fromEntries((drafts || []).map(({ id, chord }) => [id, chord]));

// Recorder takes the next key combination pressed while it has focus and
// shows what it would collide with. No shortcut fires while it records.
function Recorder({ entry, drafts, onUse, onCancel }) {
  const [chord, setChord] = useState(null);
  const ref = useRef(null);
  // Shortcuts are held off only while the recorder has focus: clicking away,
  // to another section or tab, gives them back even with the recorder open.
  useEffect(() => {
    ref.current?.focus();
    return () => setShortcutRecording(false);
  }, []);
  const onKeyDown = (event) => {
    if (event.isComposing) return;
    event.preventDefault();
    event.stopPropagation();
    if (event.key === 'Escape' && !event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey) { onCancel(); return; }
    const next = chordFromEvent(event);
    if (next) setChord(next);
  };
  const problems = chord ? rebindProblems(entry.id, chord, draftMap(drafts)) : [];
  const blocked = problems.some(problem => problem.blocking);
  return html`<div class="shortcut-recorder" data-shortcut-recorder=${entry.id}>
    <button type="button" ref=${ref} class="ui-btn shortcut-recorder-keys" onKeyDown=${onKeyDown}
      onFocus=${() => setShortcutRecording(true)} onBlur=${() => setShortcutRecording(false)}
      aria-label=${`New keys for ${entry.label}. Press them now; Escape cancels.`}>
      ${chord ? html`<${Keys} chord=${chord} />` : 'Press the new keys…'}
    </button>
    <span class="shortcut-recorder-hint ui-hint">${chord ? 'Press again to change, or' : 'Escape cancels.'}</span>
    ${chord && html`<button type="button" class="ui-btn ui-btn-primary" disabled=${blocked}
      onClick=${() => onUse(chord)}>Use ${chordText(chord)}</button>`}
    <button type="button" class="ui-btn ui-btn-ghost" onClick=${onCancel}>Cancel</button>
    ${problems.length > 0 && html`<ul class="shortcut-problems" role="status">
      ${problems.map((problem, i) => html`<li key=${i} class=${problem.blocking ? 'is-blocking' : 'is-warning'}>
        ${problem.blocking ? '⛔ ' : '⚠ '}${problem.text}</li>`)}
    </ul>`}
  </div>`;
}

// KeyboardShortcutList is Config › Keyboard: every registered shortcut by
// scope, with its keys on Windows / Linux and on macOS (the column for this
// browser's platform is marked) and whether it fires inside a web terminal.
// Rebindable shortcuts take new keys and reset to their default; the drafts
// are saved with the rest of the form.
export function KeyboardShortcutList({ Setting, drafts = [], onChange = () => {} }) {
  const [recording, setRecording] = useState(null);
  const here = shortcutPlatform() === 'macos' ? 'macos' : 'pc';
  const draft = draftMap(drafts);
  const setDraft = (id, chord) => {
    const rest = (drafts || []).filter(item => item.id !== id);
    onChange(chord && chord !== shortcutDefault(id).chord ? [...rest, { id, chord }] : rest);
  };
  const chordsOf = (id) => (draft[id] ? [draft[id]] : shortcutChords(id, { defaults: true }));
  return html`<div id=${KEYBINDINGS_LIST}>
    ${(drafts || []).length > 0 && html`<div class="shortcut-reset-all">
      <span>${drafts.length} shortcut${drafts.length === 1 ? '' : 's'} changed from the default.</span>
      <button type="button" class="ui-btn" onClick=${() => { setRecording(null); onChange([]); }}>Reset all to defaults</button>
    </div>`}
    ${SHORTCUT_SCOPES.map(scope => {
      const rows = SHORTCUTS.filter(entry => entry.scope === scope.id);
      if (!rows.length) return null;
      // Only dashboard-wide keys can collide with a terminal's own.
      const terminalColumn = scope.id === 'global' || scope.id === 'terminal';
      const editable = rows.some(entry => isRebindable(entry.id));
      const columns = 3 + (terminalColumn ? 1 : 0) + (editable ? 1 : 0);
      return html`<div class="cfg-section" key=${scope.id} data-shortcut-scope=${scope.id}>
        <h3 class="ui-eyebrow ui-eyebrow-section">${scope.title}</h3>
        <${Setting} title=${`${scope.title}: ${rows.length} shortcut${rows.length === 1 ? '' : 's'}`} wide summary=${scope.summary}>
          <table class=${`cfg-shortcut-list${editable ? ' is-editable' : ''}`}>
            <thead><tr>
              <th scope="col">Action</th>
              <th scope="col" class=${here === 'pc' ? 'is-here' : ''}>Windows & Linux${here === 'pc' ? ' (this browser)' : ''}</th>
              <th scope="col" class=${here === 'macos' ? 'is-here' : ''}>macOS${here === 'macos' ? ' (this browser)' : ''}</th>
              ${terminalColumn && html`<th scope="col">In a terminal</th>`}
              ${editable && html`<th scope="col" aria-label="Change"></th>`}
            </tr></thead>
            <tbody>${rows.map(entry => html`
              <tr key=${entry.id} data-shortcut=${entry.id} class=${draft[entry.id] ? 'is-changed' : ''}>
                <td>${entry.label}${draft[entry.id] && html` <span class="ui-tag ui-tag-accent shortcut-changed" title=${`Default: ${chordText(shortcutDefault(entry.id).chord)}`}>changed</span>`}</td>
                <td class=${here === 'pc' ? 'is-here' : ''}><${ChordList} chords=${chordsOf(entry.id)} os="linux" /></td>
                <td class=${here === 'macos' ? 'is-here' : ''}><${ChordList} chords=${chordsOf(entry.id)} os="macos" /></td>
                ${terminalColumn && html`<td>${TERMINAL_COPY[String(entry.terminal)]}</td>`}
                ${editable && html`<td class="shortcut-actions">${isRebindable(entry.id) ? html`
                  <button type="button" class="ui-btn ui-btn-ghost" data-shortcut-change=${entry.id}
                    aria-label=${`Change the keys for ${entry.label}`} onClick=${() => setRecording(entry.id)}>Change</button>
                  ${draft[entry.id] && html`<button type="button" class="ui-btn ui-btn-ghost" data-shortcut-reset=${entry.id}
                    aria-label=${`Reset ${entry.label} to ${chordText(shortcutDefault(entry.id).chord)}`}
                    onClick=${() => setDraft(entry.id, null)}>Reset</button>`}` : html`<span class="ui-tag shortcut-fixed" title="Owned by the browser or the editor it belongs to">fixed</span>`}</td>`}
              </tr>
              ${recording === entry.id && html`<tr key=${`${entry.id}-rec`} class="shortcut-recording-row"><td colspan=${columns}>
                <${Recorder} entry=${entry} drafts=${drafts}
                  onUse=${(chord) => { setDraft(entry.id, chord); setRecording(null); }}
                  onCancel=${() => setRecording(null)} />
              </td></tr>`}`)}</tbody>
          </table>
        </${Setting}>
      </div>`;
    })}
  </div>`;
}
