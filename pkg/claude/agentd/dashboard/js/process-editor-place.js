// process-editor-place.js -- where a process template's editor is (tcl-kjl2fr):
// docked in the Processes tab, where it has always been, in a tab of an
// overlay window, or in a tab of the Collab strip (tcl-tnbtwp). A template is edited in one place at a time, and moving it
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
import { closeOverlayTab, focusOverlayWindow, openOverlayWindow, overlayHostUp, overlayTabs } from './overlay-instances.js';
import { editorToWindow, processEditorAway } from './process-editor-window.js';
import { isProcessTemplateId, processPaneKey, processPaneSeed } from './process-pane-model.js';

export const PROCESS_WINDOW_KIND = 'process';
export const processWindowId = (id) => `process:${id}`;

export { isProcessTemplateId, processPaneKey, processPaneSeed };

// The editors out of the Processes tab, by template id: { mount, editor }. The
// overlay tab or strip tab showing one borrows its mount, and gives it back
// when it hides.
const floating = new Map();

// The templates the Collab strip holds a tab for. The terminal shell keeps it
// current; an editor neither it nor an overlay tab holds is let go.
export const stripProcesses = signal([]);

// The Collab strip, as the terminal shell registers it: { open(seed),
// reveal(id), close(id) }. open answers the pane, reveal whether a tab of the
// template was there to show, and close takes a tab off without asking, the
// editor having moved on.
let strip = null;
export function registerProcessStrip(next) {
  strip = next;
  return () => {
    if (strip === next) strip = null;
  };
}

// The Processes tab: { open(mount, id, name, handoff), dock(id, live, name),
// release(id), canLeave(), confirmDiscard() }. release lets the editor open
// there go to another place, and answers it, { mount, editor, label }, or null.
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

// Views handed to an overlay window opened on a just-saved template (the Groups
// tab's full editor, tcl-kqiiv0), for its editor to open on, by id.
const seeds = new Map();

// openProcessOverlay opens a template's editor in an overlay window over the
// page that asked: the Groups tab's full editor, so the editor and a Get help
// chat sit side by side there. A template is edited in one place: one already
// open in a window comes forward, and one in a Collab strip tab or the
// Processes tab moves here, unsaved edits and all. view is the stored version
// it was handed, readable that the editor may open it before the list knows
// it. It answers false without an overlay host, for the caller to go to the
// Processes tab instead.
export function openProcessOverlay(id, label = '', { view = null, readable = false } = {}) {
  if (!isProcessTemplateId(id) || !overlayHostUp.peek()) return false;
  const away = processEditorAway(id);
  if (away) {
    try { away.focus?.(); } catch { /* browser focus is best-effort */ }
    return true;
  }
  const tab = floatingProcessTab(id);
  if (tab) {
    openOverlayWindow(tab);
    focusOverlayWindow(tab.id);
    return true;
  }
  if (stripProcesses.peek().includes(id)) {
    focusOverlayWindow(floatProcessTab(id, label));
    return true;
  }
  const docked = processEditorHost.peek()?.release?.(id) || null;
  if (docked && (editorDirty(docked.editor) || !view)) {
    // Unsaved edits there move here as they are; a newer save shows as the
    // head-review banner, as for any editor that falls behind.
    floatProcessEditor({ id, label: label || docked.label, editor: docked.editor });
  } else {
    // A clean one is behind the version just handed here: it goes, and the
    // overlay's editor opens on that version.
    if (docked) {
      docked.mount?.remove();
      docked.editor.destroy?.();
    }
    if (view) seeds.set(id, { view, readable });
    openOverlayWindow({ id: processWindowId(id), kind: PROCESS_WINDOW_KIND, template: id, label: label || id });
  }
  focusOverlayWindow(processWindowId(id));
  return true;
}

const opening = new Map();
// openProcessTab opens a template's editor as a Collab strip tab. A template is
// edited in one place: one in a browser window of its own or an overlay window
// comes forward there, and one docked in the Processes tab moves to the strip,
// unsaved edits and all.
export function openProcessTab(id, label = '') {
  if (!isProcessTemplateId(id) || !strip) return Promise.resolve(null);
  const away = processEditorAway(id);
  if (away) {
    try { away.focus?.(); } catch { /* browser focus is best-effort */ }
    return Promise.resolve({ key: processPaneKey(id), floating: true });
  }
  const tab = floatingProcessTab(id);
  if (tab) {
    openOverlayWindow(tab);
    focusOverlayWindow(tab.id);
    return Promise.resolve({ key: processPaneKey(id), floating: true });
  }
  if (strip.reveal(id)) return Promise.resolve({ key: processPaneKey(id) });
  const docked = processEditorHost.peek()?.release?.(id) || null;
  if (docked) floating.set(id, docked);
  return Promise.resolve(strip.open(processPaneSeed(id, label || docked?.label || id)));
}

// revealProcessTab shows the template's strip tab, if it has one.
export const revealProcessTab = (id) => !!strip?.reveal(id);

// processTabToCollab moves an overlay tab's editor to a strip tab: the strip
// takes it before the overlay tab lets it go.
export function processTabToCollab(tab) {
  if (!strip) return Promise.resolve(null);
  const pane = strip.open(processPaneSeed(tab.template, tab.label));
  closeOverlayTab(tab.id);
  return Promise.resolve(pane);
}

// floatProcessTab moves a strip tab's editor to an overlay window, and answers
// that window's id. The overlay tab takes it before the strip lets it go.
export function floatProcessTab(id, label, at = null) {
  openOverlayWindow({ id: processWindowId(id), kind: PROCESS_WINDOW_KIND, template: id, label: label || id, at });
  strip?.close(id);
  return processWindowId(id);
}

// processTabToWindow moves a strip tab's editor to a browser window of its own.
// One whose window never takes the draft comes back as returnLiveEditor says.
export function processTabToWindow(id, label, { rect = null, win = globalThis.window, notify = null } = {}) {
  const live = floating.get(id);
  if (!live) return false;
  const restore = (back) => void returnLiveEditor(id, label, back);
  if (!editorToWindow({ id, label, live, rect, win, notify, restore })) return false;
  floating.delete(id);
  live.mount.remove();
  strip?.close(id);
  return true;
}

// confirmProcessTabClose asks before a strip tab's close drops unsaved edits.
export async function confirmProcessTabClose(id) {
  const live = floating.get(id);
  if (!editorDirty(live?.editor)) return true;
  const confirmDiscard = processEditorHost.peek()?.confirmDiscard;
  return confirmDiscard ? !!(await confirmDiscard()) : false;
}

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
  // A seed is for the next editor of this template only, taken or not.
  const seed = seeds.get(id) || {};
  seeds.delete(id);
  if (floating.has(id)) return Promise.resolve(floating.get(id));
  if (opening.has(id)) return opening.get(id);
  const host = processEditorHost.peek();
  if (!host) return Promise.resolve(null);
  const mount = processEditorMount(slot.ownerDocument);
  slot.appendChild(mount);
  const back = handoffs.get(id) || null;
  handoffs.delete(id);
  const pending = Promise.resolve(host.open(mount, id, label, back?.handoff || null, seed)).then((editor) => {
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

// takeFloatingEditor hands a floating editor over to be docked. Its overlay or
// strip tab goes, and the editor with it is not torn down.
export function takeFloatingEditor(id) {
  const live = floating.get(id) || null;
  floating.delete(id);
  live?.mount.remove();
  const tab = floatingProcessTab(id);
  if (tab) closeOverlayTab(tab.id);
  if (stripProcesses.peek().includes(id)) strip?.close(id);
  return live;
}

// dockProcessEditor moves a floating editor back to the Processes tab, unless the
// editor open there has unsaved edits the operator keeps.
export async function dockProcessEditor(id, label = '') {
  const host = processEditorHost.peek();
  if (!host || !(await host.canLeave())) return false;
  label = floatingProcessTab(id)?.label || label;
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
export async function returnLiveEditor(id, label, live, { overlay = true } = {}) {
  if (overlay && overlayHostUp.peek()) {
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

// processDocked is whether the editor in mount is the Processes tab's own: not
// in an overlay window or a Collab tab.
export const processDocked = (mount) => !mount?.closest?.('.process-overlay, .mux-pane-process');

// processShortcutLive is whether Ctrl/Cmd+S and +O belong to the editor in mount:
// docked, when the Processes tab shows; elsewhere, when its place has the keyboard.
export function processShortcutLive(mount, docked) {
  if (!mount?.isConnected) return false;
  if (processDocked(mount)) return docked();
  return !!mount.contains?.(mount.ownerDocument?.activeElement);
}

// An overlay or strip tab closed for good, from the dock or a reload that dropped
// it, takes its editor with it. One moving to the Processes tab has already left
// this map, and one moving between an overlay and the strip is held by both a
// moment.
export function settleFloatingEditors(tabs = overlayTabs.peek(), stripped = stripProcesses.peek()) {
  const shown = new Set([...tabs.filter((tab) => tab.kind === PROCESS_WINDOW_KIND).map((tab) => tab.template), ...stripped]);
  for (const [id, live] of floating) {
    if (shown.has(id)) continue;
    floating.delete(id);
    live.mount.remove();
    live.editor?.destroy?.();
  }
}

effect(() => settleFloatingEditors(overlayTabs.value, stripProcesses.value));

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
