// overlay-instances.js -- which overlay windows are open (tcl-ppvf8g). A window
// holds tabs (overlay-tab-model.js), and has its own saved placement, place in
// the stack and place in a split, so a chat or a board is just one kind of tab,
// and there can be any number of either. The companion is the one window that
// is not here: it is always the same one. Everything that opens or finds a window
// names it by its tab, because a tab can move between windows.

import { batch, computed, effect, signal } from '@preact/signals';
import { useEffect, useState } from 'preact/hooks';
import { parsePlacement, viewportOf } from './overlay-window-model.js';
import { forgetOverlayWindow, raiseOverlayWindow, readPref, writePref } from './overlay-window.js';
import { minimizedTab, movedTab, parseSavedOverlays, removedTab, restoredTab, savedOverlays, tabsOfWindows, tornOutTab, windowAround, windowHolding } from './overlay-tab-model.js';

// In session storage, as the companion's open state is: a reload brings the
// windows back, another browser tab does not get a second set.
export const OVERLAY_WINDOWS_KEY = 'tclaude.dash.overlay-windows';

export const overlayWindows = signal([]);
// Tabs minimized on their own, which wait outside any window.
export const minimizedOverlayTabs = signal([]);
// Every tab with a place here, with what it takes from its window. A tab on the
// dock is one of them, in no window: the session still has its one view here.
export const overlayTabs = computed(() => [
  ...tabsOfWindows(overlayWindows.value),
  ...minimizedOverlayTabs.value.map(({ tab }) => ({ ...tab, windowId: '', minimized: true })),
]);
// Whether anything is there to show a window, which Collab off leaves there not.
export const overlayHostUp = signal(false);

function sessionStore(win) {
  try { return win?.sessionStorage ?? null; } catch { return null; }
}

const holding = (tabId) => windowHolding(overlayWindows.peek(), tabId);

function patchWindow(id, patch) {
  overlayWindows.value = overlayWindows.peek().map((window) => (window.id === id ? { ...window, ...patch } : window));
}

// Windows and the dock change together, and a window left with no tabs is gone
// from the stack and any split.
function commit({ windows, minimizedTabs = minimizedOverlayTabs.peek() }) {
  const gone = overlayWindows.peek().filter((window) => !windows.some((other) => other.id === window.id));
  batch(() => {
    overlayWindows.value = windows;
    minimizedOverlayTabs.value = minimizedTabs;
  });
  for (const window of gone) forgetOverlayWindow(window.id);
}

const dockState = () => ({ windows: overlayWindows.peek(), minimizedTabs: minimizedOverlayTabs.peek() });

// openOverlayWindow shows a tab: in the window holding it, back from the dock, or
// in a new window. A chat or a board opens in one place at a time.
export function openOverlayWindow(instance) {
  const window = holding(instance.id);
  if (window) patchWindow(window.id, { minimized: false, active: instance.id });
  else if (minimizedOverlayTabs.peek().some(({ tab }) => tab.id === instance.id)) commit(restoredTab(dockState(), instance.id));
  else overlayWindows.value = [...overlayWindows.peek(), windowAround(overlayWindows.peek(), instance, { at: instance.at })];
  raiseOverlayTab(instance.id);
}

// raiseOverlayTab brings the window holding a tab to the front.
export function raiseOverlayTab(id) {
  const window = holding(id);
  if (window) raiseOverlayWindow(window.id);
}

export function activateOverlayTab(id) {
  const window = holding(id);
  if (window && window.active !== id) patchWindow(window.id, { active: id });
}

// moveOverlayTab puts a tab at index in a window, its own or another.
export function moveOverlayTab(id, windowId, index) {
  commit({ windows: movedTab(overlayWindows.peek(), id, windowId, index) });
  raiseOverlayWindow(windowId);
}

// tearOverlayTabOut gives a tab a window of its own, at the drop point.
export function tearOverlayTabOut(id, at) {
  commit({ windows: tornOutTab(overlayWindows.peek(), id, at) });
  raiseOverlayTab(id);
}

// minimizeOverlayTab sends one tab to the dock. It stops nothing, and only the
// last tab takes the window with it.
export function minimizeOverlayTab(id) {
  commit(minimizedTab(dockState(), id));
}

// minimizeEveryTab is the window's close: each tab goes to the dock.
export function minimizeEveryTab(windowId) {
  const window = overlayWindows.peek().find((candidate) => candidate.id === windowId);
  for (const tab of window?.tabs || []) minimizeOverlayTab(tab.id);
}

// askOverlayClose shows a tab and asks it to put its own question about closing.
export function askOverlayClose(id) {
  const window = holding(id);
  if (window) patchWindow(window.id, { active: id, closeAsked: (window.closeAsked || 0) + 1 });
}

export function overlayCloseHeard(id) {
  const window = holding(id);
  if (window) patchWindow(window.id, { closeAsked: undefined });
}

// focusOverlayWindow asks a shown window to take the keyboard.
export function focusOverlayWindow(id) {
  const window = holding(id);
  if (window) patchWindow(window.id, { focusAsked: (window.focusAsked || 0) + 1 });
}

// closeOverlayTab takes a tab off for good, from a window or the dock.
export function closeOverlayTab(id) {
  commit(removedTab(dockState(), id));
}

export function minimizeOverlayWindow(id, minimized = true) {
  const window = holding(id);
  if (!window) return;
  patchWindow(window.id, { minimized });
  if (!minimized) raiseOverlayWindow(window.id);
}

export function labelOverlayWindow(id, label) {
  const window = holding(id);
  if (!window) {
    minimizedOverlayTabs.value = minimizedOverlayTabs.peek().map((entry) => (entry.tab.id === id ? { ...entry, tab: { ...entry.tab, label } } : entry));
    return;
  }
  if (window.tabs.find((tab) => tab.id === id).label === label) return;
  patchWindow(window.id, { tabs: window.tabs.map((tab) => (tab.id === id ? { ...tab, label } : tab)) });
}

// The drop point of a window dragged out, which only its first placement reads.
export function placedOverlayWindow(id) {
  const window = holding(id);
  if (window) patchWindow(window.id, { at: undefined });
}

// useDropPoint makes where a window was dropped its place from now on, floating,
// with every panel size it had still saved. It runs before the window reads its
// placement.
export function useDropPoint(instance, { win, prefs, prefKey, start }) {
  useState(() => {
    if (!instance.at) return;
    const saved = parsePlacement(readPref(prefs, prefKey)) || start(viewportOf(win));
    writePref(prefs, prefKey, JSON.stringify({ ...saved, ...instance.at, snap: '' }));
  });
  useEffect(() => { if (instance.at) placedOverlayWindow(instance.id); }, []);
}

// keepOverlayWindows reads the saved set, keeping only tabs valid(tab) accepts,
// and saves it from then on. It returns the way to stop saving.
export function keepOverlayWindows(win, valid) {
  if (!overlayWindows.peek().length) {
    try {
      const { windows, minimizedTabs } = parseSavedOverlays(sessionStore(win)?.getItem?.(OVERLAY_WINDOWS_KEY), valid);
      overlayWindows.value = windows;
      minimizedOverlayTabs.value = minimizedTabs;
    } catch { /* they just do not come back after a reload */ }
  }
  return effect(() => {
    const saved = savedOverlays(overlayWindows.value, minimizedOverlayTabs.value);
    try {
      sessionStore(win)?.setItem?.(OVERLAY_WINDOWS_KEY, saved);
    } catch { /* as above */ }
  });
}
