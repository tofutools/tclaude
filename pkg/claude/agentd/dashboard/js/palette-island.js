import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { isWizardActive } from './slop.js';
import { COMMAND_PALETTE_OPEN_EVENT } from './command-registry.js';

const html = htm.bind(h);

export const DEFAULT_PLACEHOLDER =
  'Type a command…  (hide all windows · focus an agent · go to a tab · spawn…)';
export const WIZARD_PLACEHOLDER =
  'Speak an incantation…  (banish a familiar · scry a tab · summon…)';
export const WIZARD_EMPTY = 'No such incantation in this tome';
export const PAGE_FALLBACK = 10;

export function isCommandPaletteShortcutTarget(target) {
  const element = target?.nodeType === 1 ? target : target?.parentElement;
  if (!element) return false;
  const tag = String(element.tagName || '').toUpperCase();
  if (['INPUT', 'TEXTAREA', 'SELECT'].includes(tag)) return true;
  if (element.isContentEditable) return true;
  return !!element.closest?.('[contenteditable]:not([contenteditable="false"]), [role="textbox"], .cm-editor, .monaco-editor');
}

export function PaletteLauncher({ state, documentRef = document }) {
  useEffect(() => {
    const onKeyDown = (event) => {
      if (event.repeat || !(event.ctrlKey || event.metaKey)) return;
      if ((event.key || '').toLowerCase() !== 'k') return;
      // Ctrl/Cmd-K belongs to inputs and embedded editors while the palette is
      // closed. Once open, its own input intentionally keeps the toggle-close
      // behavior.
      if (!state.open.value && isCommandPaletteShortcutTarget(event.target)) return;
      event.preventDefault();
      if (state.open.value) {
        state.close();
        return;
      }
      // Do not stack the launcher over a form or management dialog. The
      // palette's own closed overlay is present but lacks .show, so it does not
      // block itself.
      if (documentRef.querySelector('.modal-overlay.show, .manage-overlay.show')) return;
      state.show();
    };
    const onOpenRequest = () => {
      if (state.open.value) return;
      if (documentRef.querySelector('.modal-overlay.show, .manage-overlay.show')) return;
      state.show();
    };
    documentRef.addEventListener('keydown', onKeyDown);
    documentRef.addEventListener(COMMAND_PALETTE_OPEN_EVENT, onOpenRequest);
    return () => {
      documentRef.removeEventListener('keydown', onKeyDown);
      documentRef.removeEventListener(COMMAND_PALETTE_OPEN_EVENT, onOpenRequest);
    };
  }, [documentRef, state]);

  return html`
    <button id="command-palette-btn" type="button"
      aria-label="Open the command palette (Ctrl/Cmd-K)" aria-haspopup="dialog"
      title="Command palette — a keyboard launcher (Ctrl/Cmd-K) to search and run dashboard actions: hide / focus all (or a group's, or one agent's) windows, jump to a tab, spawn an agent. Type to filter, ↑/↓ to pick, Enter to run."
      onClick=${state.show}>🔍</button>
  `;
}

export function PaletteOverlay({
  state,
  documentRef = document,
  wizardActive = isWizardActive,
}) {
  const inputRef = useRef(null);
  const listRef = useRef(null);
  const lastFocusRef = useRef(null);
  const wasOpenRef = useRef(false);
  const current = state.view.value;
  const wizard = wizardActive();

  const restoreFocus = () => {
    const previous = lastFocusRef.current;
    lastFocusRef.current = null;
    if (!previous || typeof previous.focus !== 'function') return;
    try { previous.focus(); } catch (_) { /* trigger was re-rendered away */ }
  };

  // Focus is Preact-owned along with visibility. Capture only on the closed →
  // open edge, and return it on every close edge (hotkey, Escape, backdrop, or
  // command execution). useLayoutEffect keeps the transition imperceptible.
  useLayoutEffect(() => {
    if (current.open && !wasOpenRef.current) {
      lastFocusRef.current = documentRef.activeElement;
      inputRef.current?.focus();
    } else if (!current.open && wasOpenRef.current) {
      restoreFocus();
    }
    wasOpenRef.current = current.open;
  }, [current.open, documentRef]);

  // Keep the active option visible after keyboard and mouse selection. The
  // guard accommodates DOM shims and a row removed by a concurrent render.
  useLayoutEffect(() => {
    if (!current.open || !current.commands.length) return;
    const active = listRef.current?.querySelector(`#palette-opt-${current.selected}`);
    if (typeof active?.scrollIntoView === 'function') active.scrollIntoView({ block: 'nearest' });
  }, [current.commands, current.open, current.selected]);

  useEffect(() => {
    const onWizard = () => {
      if (state.open.value) state.rebuild();
    };
    documentRef.addEventListener('tclaude:wizard', onWizard);
    return () => documentRef.removeEventListener('tclaude:wizard', onWizard);
  }, [documentRef, state]);

  // If the whole island is torn down while open (pagehide or mount rollback),
  // do not strand focus in a detached input.
  useEffect(() => () => {
    if (wasOpenRef.current) restoreFocus();
  }, []);

  const pageSize = () => {
    const list = listRef.current;
    const first = list?.querySelector('.palette-item');
    const itemHeight = first?.offsetHeight || 0;
    if (!itemHeight) return PAGE_FALLBACK;
    return Math.max(1, Math.floor(list.clientHeight / itemHeight));
  };

  const runSelected = () => state.runSelected({ beforeRun: restoreFocus });
  const onInputKeyDown = (event) => {
    switch (event.key) {
      case 'ArrowDown': event.preventDefault(); state.move(1); break;
      case 'ArrowUp': event.preventDefault(); state.move(-1); break;
      case 'PageDown': event.preventDefault(); state.movePage(1, pageSize()); break;
      case 'PageUp': event.preventDefault(); state.movePage(-1, pageSize()); break;
      case 'Enter': event.preventDefault(); runSelected(); break;
      case 'Escape': event.preventDefault(); state.close(); break;
      default: break;
    }
  };
  const dismissBackdrop = (event) => {
    if (event.currentTarget === event.target) state.close();
  };
  const activeDescendant = current.commands.length ? `palette-opt-${current.selected}` : null;

  return html`
    <div class=${`modal-overlay palette-overlay${current.open ? ' show' : ''}`}
      id="command-palette-modal" onClick=${dismissBackdrop}>
      <div class="palette-box" role="dialog" aria-modal="true" aria-label="Command palette">
        <div class="palette-title" aria-hidden="true">📖 The Spellbook</div>
        <input id="palette-input" ref=${inputRef} type="text" autocomplete="off" spellcheck="false"
          role="combobox" aria-expanded="true" aria-controls="palette-list" aria-autocomplete="list"
          aria-activedescendant=${activeDescendant}
          placeholder=${wizard ? WIZARD_PLACEHOLDER : DEFAULT_PLACEHOLDER}
          value=${current.query}
          onInput=${(event) => state.setQuery(event.currentTarget.value)}
          onKeyDown=${onInputKeyDown} />
        <div id="palette-list" ref=${listRef} class="palette-list" role="listbox" aria-label="Commands">
          ${current.commands.length ? current.commands.map((command, index) => html`
            <div key=${`${command.label}\u0000${index}`}
              class=${`palette-item${index === current.selected ? ' selected' : ''}${command.enabled === false ? ' disabled' : ''}`}
              data-idx=${index} id=${`palette-opt-${index}`} role="option"
              aria-selected=${index === current.selected ? 'true' : 'false'}
              aria-disabled=${command.enabled === false ? 'true' : 'false'}
              onMouseMove=${() => state.setSelected(index)}
              onClick=${() => { state.setSelected(index); runSelected(); }}>
              <span class="palette-ico">${command.icon || '•'}</span>
              <span class="palette-label">${command.label}</span>
              ${command.enabled === false || command.hint ? html`<span class="palette-hint">${command.enabled === false ? command.disabledReason || 'Unavailable' : command.hint}</span>` : null}
            </div>
          `) : html`<div class="palette-empty">${wizard ? WIZARD_EMPTY : 'No matching commands'}</div>`}
        </div>
        <div class="palette-foot">
          <span><kbd>↑</kbd><kbd>↓</kbd> navigate</span>
          <span><kbd>PgUp</kbd><kbd>PgDn</kbd> jump</span>
          <span><kbd>↵</kbd> run</span>
          <span><kbd>esc</kbd> close</span>
        </div>
      </div>
    </div>
  `;
}

export function mountPaletteIsland({
  buttonHost,
  modalHost,
  state,
  registerCleanup,
  documentRef = document,
  wizardActive = isWizardActive,
}) {
  if (!buttonHost || !modalHost) throw new TypeError('palette island requires button and modal hosts');
  if (!state?.view || typeof state.show !== 'function') throw new TypeError('palette island requires state');
  if (typeof registerCleanup !== 'function') throw new TypeError('palette island requires registerCleanup');

  registerCleanup(() => {
    state.close();
    render(null, modalHost);
    render(null, buttonHost);
  });
  render(html`<${PaletteLauncher} state=${state} documentRef=${documentRef} />`, buttonHost);
  render(html`<${PaletteOverlay} state=${state} documentRef=${documentRef}
    wizardActive=${wizardActive} />`, modalHost);
}
