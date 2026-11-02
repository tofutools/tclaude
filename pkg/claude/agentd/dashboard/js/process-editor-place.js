// process-editor-place.js -- where a process template's editor is (tcl-kjl2fr):
// docked in the Processes tab, where it has always been, or in a tab of an
// overlay window. A template is edited in one place at a time, and moving it
// moves the live editor itself: its mount, with the draft, the undo history,
// the selection, the scribe dock and the version a save is checked against, so
// nothing is saved or lost on the way.
//
// The mount is page-owned and re-homed between those places, as the dock's
// stable hosts are: Preact renders each place's empty slot and the editor's own
// tree inside the mount, and this file only creates and moves the mount.
// dashboard-imperative-boundary: platform-layout
//
// The Processes tab registers itself as the host: how an editor opens there, and
// with which options, so an overlay tab that comes back after a reload opens one
// as the tab would. This file stays free of the editor itself, which loads only
// when one opens.

import { effect, signal } from '@preact/signals';
import { closeOverlayTab, openOverlayWindow, overlayHostUp, overlayTabs } from './overlay-instances.js';

export const PROCESS_WINDOW_KIND = 'process';
export const processWindowId = (id) => `process:${id}`;

// The editors out of the Processes tab, by template id: { mount, editor }. The
// overlay tab showing one borrows its mount, and gives it back when it hides.
const floating = new Map();

// The Processes tab: { open(mount, id, name, handoff), dock(id, live, name),
// canLeave(), confirmDiscard() }.
export const processEditorHost = signal(null);

// registerProcessEditorHost installs the Processes tab, and returns the way to
// take it back.
export function registerProcessEditorHost(host) {
  processEditorHost.value = host;
  return () => {
    if (processEditorHost.peek() === host) processEditorHost.value = null;
  };
}

// processEditorMount is where an editor mounts. It is the page's, not a
// component's, as it moves with the editor in it.
export function processEditorMount(doc) {
  const mount = doc.createElement('div');
  mount.id = 'process-editor-canvas';
  mount.className = 'process-canvas-mount';
  mount.dataset.processMount = 'editor';
  return mount;
}

// showProcessEditorError puts why an editor could not open in its mount.
export function showProcessEditorError(mount, message) {
  const alert = mount.ownerDocument.createElement('div');
  alert.className = 'process-placeholder';
  alert.setAttribute('role', 'alert');
  alert.textContent = `Could not open editor: ${message}`;
  mount.replaceChildren(alert);
}

export const floatingProcessTab = (id) => overlayTabs.peek().find((tab) => tab.kind === PROCESS_WINDOW_KIND && tab.template === id) || null;

export function floatingProcessEditor(id) {
  return floating.get(id) || null;
}

// floatProcessEditor moves a live editor out of the Processes tab, into an
// overlay tab of its own.
export function floatProcessEditor({ id, label, editor, at = null }) {
  if (!id || !editor?.mount) return;
  floating.set(id, { mount: editor.mount, editor });
  editor.mount.remove();
  openOverlayWindow({ id: processWindowId(id), kind: PROCESS_WINDOW_KIND, template: id, label: label || id, at });
}

// editorDirty is whether an editor holds anything unsaved, an open dialog's
// draft included.
export const editorDirty = (editor) => !!(editor?.dirty ?? editor?.model?.dirty);

const opening = new Map();
// Drafts back from a browser window of their own, by id, for the overlay tab
// opening to take up (process-editor-window.js): { handoff, settle }.
const handoffs = new Map();

// floatHandoff opens an overlay tab on a draft handed back from a window. It
// answers, in time, whether an editor there took the draft up; a template
// already open here takes none.
export function floatHandoff(id, label, handoff) {
  if (floating.has(id) || opening.has(id) || handoffs.has(id) || floatingProcessTab(id)) return Promise.resolve(false);
  return new Promise((settle) => {
    handoffs.set(id, { handoff, settle });
    openOverlayWindow({ id: processWindowId(id), kind: PROCESS_WINDOW_KIND, template: id, label: label || id });
  });
}

// openFloatingEditor opens an editor for an overlay tab that has none, as after a
// reload, in a mount it puts in slot first. A tab shown twice while that loads
// gets the one editor.
export function openFloatingEditor(id, label, slot) {
  if (floating.has(id)) return Promise.resolve(floating.get(id));
  if (opening.has(id)) return opening.get(id);
  const host = processEditorHost.peek();
  if (!host) return Promise.resolve(null);
  const mount = processEditorMount(slot.ownerDocument);
  slot.appendChild(mount);
  const back = handoffs.get(id) || null;
  handoffs.delete(id);
  const pending = Promise.resolve(host.open(mount, id, label, back?.handoff || null)).then((editor) => {
    opening.delete(id);
    back?.settle(!!editor);
    if (!editor) return null;
    floating.set(id, { mount, editor });
    // A tab closed while it loaded lets it go.
    settleFloatingEditors();
    return floating.get(id) || null;
  }, (cause) => {
    opening.delete(id);
    back?.settle(false);
    mount.remove();
    throw cause;
  });
  opening.set(id, pending);
  return pending;
}

// takeFloatingEditor hands a floating editor over to be docked. Its overlay tab
// goes, and the editor with it is not torn down.
export function takeFloatingEditor(id) {
  const live = floating.get(id) || null;
  floating.delete(id);
  live?.mount.remove();
  const tab = floatingProcessTab(id);
  if (tab) closeOverlayTab(tab.id);
  return live;
}

// dockProcessEditor moves a floating editor back to the Processes tab, unless the
// editor open there has unsaved edits the operator keeps.
export async function dockProcessEditor(id) {
  const host = processEditorHost.peek();
  if (!host || !(await host.canLeave())) return false;
  const label = floatingProcessTab(id)?.label || '';
  const live = takeFloatingEditor(id);
  host.dock(id, live, label);
  return true;
}

// An editor a browser window never took, waiting for the Processes tab to be
// free: { id, label, live }. Nothing destroys it while it waits.
export const returningEditor = signal(null);

// returnLiveEditor gives back an editor a browser window never took: to an
// overlay window, which takes it without a question, or else to the Processes
// tab, if the editor open there may go. If that one stays, this one waits until
// it closes (processes-island.js), so neither loses its edits.
export async function returnLiveEditor(id, label, live) {
  if (overlayHostUp.peek()) {
    floatProcessEditor({ id, label, editor: live.editor });
    return;
  }
  const host = processEditorHost.peek();
  if (host && (await host.canLeave())) {
    host.dock(id, live, label);
    return;
  }
  live.mount.remove();
  returningEditor.value = { id, label, live };
}

// takeReturningEditor hands the waiting editor over, once.
export function takeReturningEditor() {
  const waiting = returningEditor.peek();
  returningEditor.value = null;
  return waiting;
}

// closeFloatingEditor takes a floating editor off for good, asking first when it
// holds unsaved edits.
export async function closeFloatingEditor(id) {
  const live = floating.get(id);
  const confirmDiscard = processEditorHost.peek()?.confirmDiscard;
  if (editorDirty(live?.editor) && !(await (confirmDiscard ? confirmDiscard() : false))) return false;
  const tab = floatingProcessTab(id);
  if (tab) closeOverlayTab(tab.id);
  return true;
}

// processShortcutLive is whether Ctrl/Cmd+S and +O belong to the editor in mount:
// docked, when the Processes tab shows; floating, when its window has the keyboard.
export function processShortcutLive(mount, docked) {
  if (!mount?.isConnected) return false;
  if (!mount.closest?.('.process-overlay')) return docked();
  return !!mount.contains?.(mount.ownerDocument?.activeElement);
}

// An overlay tab closed for good, from the dock or a reload that dropped it, takes
// its editor with it. One moving to the Processes tab has already left this map.
export function settleFloatingEditors(tabs = overlayTabs.peek()) {
  const shown = new Set(tabs.filter((tab) => tab.kind === PROCESS_WINDOW_KIND).map((tab) => tab.template));
  for (const [id, live] of floating) {
    if (shown.has(id)) continue;
    floating.delete(id);
    live.mount.remove();
    live.editor?.destroy?.();
  }
}

effect(() => settleFloatingEditors(overlayTabs.value));

// The mount of the editor last clicked or focused, so the command palette
// serves the editor the operator is at, docked or floating.
let touched = null;

// trackTouchedEditor follows that, and returns the way to stop.
export function trackTouchedEditor(doc) {
  const touch = (event) => {
    const mount = event.target?.closest?.('#process-editor-canvas');
    if (mount) touched = mount;
  };
  doc.addEventListener('pointerdown', touch, true);
  doc.addEventListener('focusin', touch, true);
  return () => {
    doc.removeEventListener('pointerdown', touch, true);
    doc.removeEventListener('focusin', touch, true);
  };
}

// paletteEditor is the editor the command palette serves: a floating one the
// operator was last at, while it shows, or else the docked one.
export function paletteEditor(docked) {
  if (touched?.isConnected && touched !== docked?.mount) {
    for (const live of floating.values()) if (live.mount === touched) return live.editor;
  }
  return docked;
}

let headsPending = false;

// observeFloatingHeads tells every floating editor its template's latest saved
// version, as the Processes tab does for the editor open there, so a save made
// elsewhere is offered for review rather than found at the next save.
export async function observeFloatingHeads(fetchImpl = globalThis.fetch) {
  if (headsPending || !floating.size) return false;
  headsPending = true;
  try {
    const response = await fetchImpl('/v1/process/template-heads', { credentials: 'same-origin', cache: 'no-store' });
    if (!response.ok) return false;
    const heads = (await response.json())?.heads || [];
    for (const [id, { editor }] of floating) {
      const head = heads.find((candidate) => candidate.id === id);
      if (head?.ref && head?.sourceHash && editor?.model?.template?.id === id) editor.observeExternalHead?.(head);
    }
    return true;
  } catch {
    return false;
  } finally {
    headsPending = false;
  }
}
