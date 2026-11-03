// process-overlay.js -- a process template's editor in a tab of an overlay
// window (tcl-kjl2fr), beside chats, terminals and boards. The editor is the
// live one from the Processes tab (process-editor-place.js): this tab shows its
// mount while the tab is the one showing, and hands it back when it hides, so
// switching tabs, minimizing or moving to another window keeps every unsaved
// edit. A tab that comes back after a reload has no editor yet, and opens one
// from the saved template, as the Processes tab would.

import { h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { OVERLAY_FOOTER_ROOM, clampPlacement } from './overlay-window-model.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { minimizeOverlayWindow } from './overlay-instances.js';
import { OverlayWindow } from './overlay-window.js';
import { TO_COLLAB_LABEL, TO_WINDOW_LABEL, registerTabMoves } from './overlay-tab-moves.js';
import { editorToWindow } from './process-editor-window.js';
import {
  PROCESS_WINDOW_KIND, closeFloatingEditor, dockProcessEditor, floatingProcessEditor, isProcessTemplateId, observeFloatingHeads,
  processEditorHost, processTabToCollab, returnLiveEditor, takeFloatingEditor,
} from './process-editor-place.js';
import { useFloatingEditor } from './process-pane.js';

const html = htm.bind(h);

export const TO_PROCESSES_LABEL = 'Open in the Processes tab';

// A process editor goes to a Collab tab, back to the Processes tab, or out to a
// browser window of its own, and asks before a close drops unsaved edits. One
// still opening has nothing to move yet.
registerTabMoves(PROCESS_WINDOW_KIND, (tab) => ({
  toCollab: () => processTabToCollab(tab),
  toDock: { label: TO_PROCESSES_LABEL, run: () => dockProcessEditor(tab.template) },
  toWindow: floatingProcessEditor(tab.template) ? ({ rect, win, notify }) => moveToWindow(tab, { rect, win, notify }) : null,
  close: () => closeFloatingEditor(tab.template),
}));

// A window that never takes the draft up gives the editor back.
function moveToWindow(tab, { rect, win, notify }) {
  const live = takeFloatingEditor(tab.template);
  if (!live) return;
  const restore = (back) => void returnLiveEditor(tab.template, tab.label, back);
  if (!editorToWindow({ id: tab.template, label: tab.label, live, rect, win, notify, restore })) restore(live);
}

// An editor wants room: most of the screen, short of the footer.
export function processStartPlacement(viewport) {
  const w = Math.min(1180, viewport.width - 48);
  const h = Math.min(760, viewport.height - OVERLAY_FOOTER_ROOM - 80);
  return clampPlacement({ x: Math.max(24, (viewport.width - w) / 2), y: 48, w, h, snap: '' }, viewport);
}

export function ProcessOverlayWindow({ instance, overlay, strip, more = null, close, snapshot, fetchImpl, notify, win }) {
  const slot = useRef(null);
  const open = !instance.minimized;
  const look = windowControlsLook(snapshot.value?.window_controls);
  const { error } = useFloatingEditor(slot, instance.template, instance.label);
  // A save made somewhere else reaches this editor as it does the docked one.
  useEffect(() => {
    const poll = () => { if (open) void observeFloatingHeads(fetchImpl); };
    win?.document?.addEventListener('tclaude:snapshot', poll);
    return () => win?.document?.removeEventListener('tclaude:snapshot', poll);
  }, [open, fetchImpl, win]);
  const label = instance.label || instance.template;
  const toCollab = () => void processTabToCollab(instance);
  const title = html`${strip}
    ${look !== 'classic' ? more : html`
    <button data-ui-exempt type="button" class="board-overlay-action" title="Move this editor to a tab of the Collab view, unsaved edits and all"
      onClick=${toCollab}>${TO_COLLAB_LABEL}</button>
    <button data-ui-exempt type="button" class="board-overlay-action" title="Move this editor back to the Processes tab, unsaved edits and all"
      onClick=${() => void dockProcessEditor(instance.template)}>${TO_PROCESSES_LABEL}</button>
    <button data-ui-exempt type="button" class="board-overlay-action" title="Move this editor to a browser window of its own, unsaved edits and all"
      onClick=${() => floatingProcessEditor(instance.template) && moveToWindow(instance, { rect: overlay.windowRef.current?.getBoundingClientRect?.(), win, notify })}>${TO_WINDOW_LABEL}</button>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="board-overlay-close" title="Minimize" aria-label=${`Minimize ${label}`}
      onClick=${() => minimizeOverlayWindow(instance.id)}>–</button>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="board-overlay-close" title=${close.title} aria-label=${`Close ${label}`}
      onClick=${close.onClose}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.windowId} window=${overlay} open=${open} className="board-overlay process-overlay" frameClass="board-overlay-resize-frame"
    titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Process ${label}`} title=${title}
    controls=${{ look, closeLabel: `Close ${label}`, closeTitle: close.title, onClose: close.onClose, onMinimize: () => minimizeOverlayWindow(instance.id), onCollab: snapshot.value?.collab_enabled === true ? toCollab : null }}>
    <div class="process-overlay-body">
      ${error && html`<div class="process-placeholder" role="alert">Could not open editor: ${error}</div>`}
      ${!error && !processEditorHost.value && !floatingProcessEditor(instance.template) && html`<div class="process-placeholder">Loading ${label}…</div>`}
      <div ref=${slot} class="process-overlay-slot"></div>
    </div>
  <//>`;
}

// A tab names its template, by the store's rule for an id: only a stored
// template floats.
export const validProcessTab = (tab) => isProcessTemplateId(tab.template);
