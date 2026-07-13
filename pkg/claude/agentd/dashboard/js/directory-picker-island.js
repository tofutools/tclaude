import { h, render } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { configureDirectoryPickerBridge } from './helpers.js';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);
const PAGE_FALLBACK = 10;

function pageSize(list) {
  const first = list?.querySelector('.directory-picker-entry');
  const itemHeight = first?.offsetHeight || 0;
  if (!itemHeight) return PAGE_FALLBACK;
  return Math.max(1, Math.floor(list.clientHeight / itemHeight));
}

function withTrailingSlash(path) {
  if (!path || path === '/') return path;
  return `${path.replace(/\/+$/, '')}/`;
}

export function directoryFilterTerm(inputPath, viewPath) {
  if (!viewPath) return null;
  const normalizedView = viewPath === '/' ? '/' : `${viewPath.replace(/\/+$/, '')}/`;
  if (inputPath === viewPath || inputPath === normalizedView) return '';
  if (!inputPath.startsWith(normalizedView)) return null;
  const term = inputPath.slice(normalizedView.length);
  return term.includes('/') ? null : term;
}

export function filterDirectories(directories, term) {
  if (!term) return directories;
  const needle = term.toLowerCase();
  const prefixes = [];
  const substrings = [];
  for (const directory of directories) {
    const name = directory.name.toLowerCase();
    if (name.startsWith(needle)) prefixes.push(directory);
    else if (name.includes(needle)) substrings.push(directory);
  }
  return [...prefixes, ...substrings];
}

export function DirectoryPickerApp({ state, actions }) {
  const request = state.request.value;
  const pathRef = useRef(null);
  const listRef = useRef(null);
  const activeEntryRef = useRef(null);
  const generation = useRef(0);
  const [view, setView] = useState(null);
  const [path, setPath] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const browse = useCallback(async (target, appendSlash = false) => {
    const requested = String(target || '').trim();
    const currentGeneration = ++generation.current;
    setPath(requested);
    setActiveIndex(0);
    setBusy(true);
    setError('');
    try {
      const result = await actions.browse(requested);
      if (currentGeneration !== generation.current) return;
      setView(result);
      const openedPath = result.path || requested;
      setPath(appendSlash ? withTrailingSlash(openedPath) : openedPath);
      setActiveIndex(0);
    } catch (browseError) {
      if (currentGeneration !== generation.current) return;
      setError(browseError?.message || String(browseError));
    } finally {
      if (currentGeneration === generation.current) setBusy(false);
    }
  }, [actions]);

  useEffect(() => {
    if (!request) return undefined;
    setView(null);
    setPath(request.startDir);
    setActiveIndex(0);
    setError('');
    void browse(request.startDir);
    return () => { generation.current += 1; };
  }, [request]);

  const directories = view?.directories || [];
  const filterTerm = directoryFilterTerm(path, view?.path);
  const filtering = filterTerm !== null && filterTerm !== '';
  const selecting = filterTerm !== null;
  const visibleDirectories = filtering ? filterDirectories(directories, filterTerm) : directories;
  const selectedIndex = visibleDirectories.length ? Math.min(activeIndex, visibleDirectories.length - 1) : -1;
  const activeDirectory = selecting && selectedIndex >= 0 ? visibleDirectories[selectedIndex] : null;
  const optionID = (directory) => `directory-picker-option-${directories.indexOf(directory)}`;

  useEffect(() => {
    activeEntryRef.current?.scrollIntoView?.({ block: 'nearest' });
  }, [selectedIndex, filterTerm, view?.path]);

  if (!request) return null;
  const validated = !!view?.path && filterTerm === '';
  const typedOtherPath = !!view?.path && filterTerm === null && path.trim() !== view.path;
  const count = filtering
    ? `${visibleDirectories.length} of ${directories.length} folders`
    : `${directories.length} folders`;
  const close = () => state.finish({ canceled: true });
  const choose = () => {
    if (validated && !busy) state.finish({ path: view.path });
  };
  return html`<${Overlay}
    id="directory-picker-modal"
    dialogClass="directory-picker-modal"
    labelledby="directory-picker-title"
    onClose=${close}
    onSubmitHotkey=${choose}
  >
    <h3 id="directory-picker-title">${request.title}</h3>
    <form class="directory-picker-path" onSubmit=${(event) => {
      event.preventDefault();
      void browse(activeDirectory?.path || path, true);
    }}>
      <label for="directory-picker-path">Host path</label>
      <input ref=${pathRef} id="directory-picker-path" value=${path}
        onInput=${(event) => {
          setPath(event.currentTarget.value);
          setActiveIndex(0);
        }}
        onKeyDown=${(event) => {
          if (!activeDirectory || busy) return;
          if (event.key === 'ArrowDown') {
            event.preventDefault();
            setActiveIndex(Math.min(selectedIndex + 1, visibleDirectories.length - 1));
          } else if (event.key === 'ArrowUp') {
            event.preventDefault();
            setActiveIndex(Math.max(selectedIndex - 1, 0));
          } else if (event.key === 'PageDown') {
            event.preventDefault();
            setActiveIndex(Math.min(
              selectedIndex + pageSize(listRef.current),
              visibleDirectories.length - 1,
            ));
          } else if (event.key === 'PageUp') {
            event.preventDefault();
            setActiveIndex(Math.max(selectedIndex - pageSize(listRef.current), 0));
          } else if (event.key === 'Tab' && filtering && !event.shiftKey && path !== activeDirectory.path) {
            event.preventDefault();
            setPath(activeDirectory.path);
            setActiveIndex(0);
          }
        }}
        role=${selecting ? 'combobox' : undefined}
        aria-expanded=${selecting ? 'true' : undefined}
        aria-autocomplete=${selecting ? 'list' : undefined}
        aria-activedescendant=${activeDirectory ? optionID(activeDirectory) : undefined}
        aria-controls="directory-picker-folders"
        autocomplete="off" spellcheck="false" data-select-on-focus />
      <button type="submit" disabled=${busy}>${busy ? 'Loading…' : 'Go'}</button>
    </form>
    <div class="directory-picker-nav">
      <button type="button" disabled=${busy || !view?.parent}
        onClick=${() => void browse(view?.parent)}>↑ Parent</button>
      <button type="button" disabled=${busy || !view?.home || view?.home === view?.path}
        onClick=${() => void browse(view?.home)}>⌂ Home</button>
      <span class="directory-picker-count" role="status" aria-live="polite">${view ? count : ''}</span>
    </div>
    ${typedOtherPath && html`<div class="directory-picker-hint">Press Enter to open the typed path.</div>`}
    <div ref=${listRef} id="directory-picker-folders" class="directory-picker-list"
      role=${selecting ? 'listbox' : 'list'} aria-label="Folders">
      ${visibleDirectories.map((directory, index) => {
        const active = selecting && index === selectedIndex;
        return html`<div
        key=${directory.path} role=${selecting ? 'presentation' : 'listitem'} class="directory-picker-entry"
      ><button id=${selecting ? optionID(directory) : undefined}
          type="button" title=${directory.path} disabled=${busy}
          ref=${active ? activeEntryRef : undefined}
          class=${active ? 'active' : undefined}
          role=${selecting ? 'option' : undefined}
          aria-selected=${selecting ? active ? 'true' : 'false' : undefined}
          tabIndex=${selecting ? -1 : undefined}
          onClick=${() => void browse(directory.path)}
        ><span aria-hidden="true">📁</span><span>${directory.name}</span></button></div>`;
      })}
      ${view && !visibleDirectories.length && html`<p class="directory-picker-empty">
        ${filtering ? 'No matching folders' : 'No subdirectories'}
      </p>`}
    </div>
    <div role="alert" class="directory-picker-error">${error}</div>
    <div class="modal-buttons">
      <button type="button" onClick=${close}>Cancel</button>
      <span class="spacer"></span>
      <button type="button" class="primary" disabled=${busy || !validated}
        onClick=${choose}>Use this folder</button>
    </div>
  </${Overlay}>`;
}

export function mountDirectoryPickerIsland({ host, state, actions, prefersWeb, registerCleanup }) {
  render(html`<${DirectoryPickerApp} state=${state} actions=${actions} />`, host);
  configureDirectoryPickerBridge({ open: state.open, prefersWeb });
  registerCleanup(() => {
    configureDirectoryPickerBridge(null);
    state.finish({ canceled: true });
    render(null, host);
  });
}
