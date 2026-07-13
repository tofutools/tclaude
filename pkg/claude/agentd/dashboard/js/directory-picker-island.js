import { h, render } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { configureDirectoryPickerBridge } from './helpers.js';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

export function DirectoryPickerApp({ state, actions }) {
  const request = state.request.value;
  const pathRef = useRef(null);
  const generation = useRef(0);
  const [view, setView] = useState(null);
  const [path, setPath] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const browse = useCallback(async (target) => {
    const requested = String(target || '').trim();
    const currentGeneration = ++generation.current;
    setPath(requested);
    setBusy(true);
    setError('');
    try {
      const result = await actions.browse(requested);
      if (currentGeneration !== generation.current) return;
      setView(result);
      setPath(result.path || requested);
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
    setError('');
    void browse(request.startDir);
    return () => { generation.current += 1; };
  }, [request]);

  if (!request) return null;
  const validated = !!view?.path && path === view.path;
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
      void browse(path);
    }}>
      <label for="directory-picker-path">Host path</label>
      <input ref=${pathRef} id="directory-picker-path" value=${path}
        onInput=${(event) => setPath(event.currentTarget.value)}
        autocomplete="off" spellcheck="false" data-select-on-focus />
      <button type="submit" disabled=${busy}>${busy ? 'Loading…' : 'Go'}</button>
    </form>
    <div class="directory-picker-nav">
      <button type="button" disabled=${busy || !view?.parent}
        onClick=${() => void browse(view?.parent)}>↑ Parent</button>
      <button type="button" disabled=${busy || !view?.home || view?.home === view?.path}
        onClick=${() => void browse(view?.home)}>⌂ Home</button>
      <span class="directory-picker-count">${view ? `${view.directories?.length || 0} folders` : ''}</span>
    </div>
    <div class="directory-picker-list" role="list" aria-label="Folders">
      ${(view?.directories || []).map((directory) => html`<div
        key=${directory.path} role="listitem" class="directory-picker-entry"
      ><button type="button" title=${directory.path} disabled=${busy}
          onClick=${() => void browse(directory.path)}
        ><span aria-hidden="true">📁</span><span>${directory.name}</span></button></div>`)}
      ${view && !view.directories?.length && html`<p class="directory-picker-empty">No subdirectories</p>`}
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
