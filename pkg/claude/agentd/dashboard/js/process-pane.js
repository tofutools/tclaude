// process-pane.js -- a process template's editor in a tab of the Collab strip
// (tcl-tnbtwp), beside terminals and boards. Like an overlay tab
// (process-overlay.js), the tab shows the live editor from wherever it was
// (process-editor-place.js): its mount, with the draft, undo, selection and
// the version a save is checked against. A tab opened in the background opens
// its editor the first time it is looked at.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import {
  dockProcessEditor, floatingProcessEditor, observeFloatingHeads, openFloatingEditor, processEditorHost,
} from './process-editor-place.js';

const html = htm.bind(h);

// useFloatingEditor shows the template's editor in slot: the live one, or, once
// enabled, one it opens when there is none. It answers { error, ready }: what
// went wrong, if anything did, and whether the editor is in slot.
export function useFloatingEditor(slot, id, label, enabled = true) {
  const host = processEditorHost.value;
  const [error, setError] = useState('');
  const [ready, setReady] = useState(false);
  useLayoutEffect(() => {
    let gone = false;
    let shown = null;
    const show = (live) => {
      if (gone || !live) return;
      slot.current?.appendChild(live.mount);
      shown = live.mount;
      setReady(true);
    };
    const live = floatingProcessEditor(id);
    if (live) show(live);
    else if (host && enabled) openFloatingEditor(id, label, slot.current).then(show, (cause) => { if (!gone) setError(cause?.message || String(cause)); });
    return () => {
      gone = true;
      // One already moved to another place stays there.
      if (shown?.parentNode === slot.current) shown.remove();
    };
  }, [id, host, enabled]);
  return { error, ready };
}

// ProcessPane is the strip's pane for a process tab: where the editor can go
// from here, and the editor.
export function ProcessPane({ pane, active, actions, fetchImpl = globalThis.fetch, collab = true, scope = null }) {
  const id = pane.seed.process;
  const label = pane.label || id;
  const slot = useRef(null);
  const seen = useRef(false);
  if (active) seen.current = true;
  const { error, ready } = useFloatingEditor(slot, id, label, seen.current);
  // A tab opened from its URL knows only the id until its editor reads the name.
  const name = ready ? floatingProcessEditor(id)?.editor?.model?.template?.name || '' : '';
  useEffect(() => {
    if (name && name !== pane.label) actions.setPaneLabel?.(pane.key, name);
  }, [name, pane.label]);
  // A save made somewhere else reaches this editor as it does the docked one.
  useEffect(() => {
    if (!active) return undefined;
    const poll = () => void observeFloatingHeads(fetchImpl);
    document.addEventListener('tclaude:snapshot', poll);
    return () => document.removeEventListener('tclaude:snapshot', poll);
  }, [active, fetchImpl]);
  return html`<div class=${`mux-pane mux-pane-process${active ? ' active' : ''}${scope?.hidden ? ' scope-hidden' : ''}`} id=${pane.id} role="tabpanel" data-process-pane=${id}>
    <div class="mux-process-head">
      <span class="ui-eyebrow">Process</span>
      <span class="mux-process-name" title=${id}>${label}</span>
      <span class="spacer"></span>
      ${collab ? html`<${Button} size="sm" class="mux-btn" title="Float this editor in a window over the dashboard, unsaved edits and all" onClick=${() => void actions.openPaneInOverlay(pane.key)}>Open in overlay<//>` : null}
      <${Button} size="sm" class="mux-btn" title="Move this editor to the Processes tab, unsaved edits and all" onClick=${() => void dockProcessEditor(id, label)}>Open in the Processes tab<//>
      <${Button} size="sm" class="mux-btn" title="Move this editor to a browser window of its own, unsaved edits and all" onClick=${() => void actions.popOutPane(pane.key)}>Open in separate window<//>
    </div>
    <div class="process-overlay-body">
      ${error && html`<div class="process-placeholder" role="alert">Could not open editor: ${error}</div>`}
      ${!error && !ready && seen.current && html`<div class="process-placeholder">Loading ${label}…</div>`}
      <div ref=${slot} class="process-overlay-slot"></div>
    </div>
  </div>`;
}
