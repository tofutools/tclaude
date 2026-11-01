// overlay-instances.js -- which overlay windows are open (tcl-ppvf8g). A window
// holds tabs (overlay-tab-model.js), and has its own saved placement, place in
// the stack and place in a split, so a chat or a board is just one kind of tab,
// and there can be any number of either. For now every window here holds one.
// The companion is the one window that is not here: it is always the same one.
// Everything that opens or finds a window names it by its tab.

import { computed, effect, signal } from '@preact/signals';
import { useEffect, useState } from 'preact/hooks';
import { parsePlacement, viewportOf } from './overlay-window-model.js';
import { forgetOverlayWindow, raiseOverlayWindow, readPref, writePref } from './overlay-window.js';
import { parseSavedOverlays, savedOverlays, tabsOfWindows, windowHolding, windowOfTab } from './overlay-tab-model.js';

// In session storage, as the companion's open state is: a reload brings the
// windows back, another browser tab does not get a second set.
export const OVERLAY_WINDOWS_KEY = 'tclaude.dash.overlay-windows';

export const overlayWindows = signal([]);
// Every tab of every window, with what it takes from its window.
export const overlayTabs = computed(() => tabsOfWindows(overlayWindows.value));
// Tabs minimized on their own, which wait outside any window: { tab, home }.
export const minimizedOverlayTabs = signal([]);

function sessionStore(win) {
  try { return win?.sessionStorage ?? null; } catch { return null; }
}

const holding = (tabId) => windowHolding(overlayWindows.peek(), tabId);

function patchWindow(id, patch) {
  overlayWindows.value = overlayWindows.peek().map((window) => (window.id === id ? { ...window, ...patch } : window));
}

// openOverlayWindow shows a tab's window, or brings back the one already holding
// it: a chat or a board opens in one place at a time.
export function openOverlayWindow(instance) {
  const window = holding(instance.id);
  if (window) patchWindow(window.id, { minimized: false, active: instance.id });
  else overlayWindows.value = [...overlayWindows.peek(), windowOfTab(instance)];
  raiseOverlayWindow(window?.id || instance.id);
}

// focusOverlayWindow asks a shown window to take the keyboard.
export function focusOverlayWindow(id) {
  const window = holding(id);
  if (window) patchWindow(window.id, { focusAsked: (window.focusAsked || 0) + 1 });
}

export function closeOverlayWindow(id) {
  const windowId = holding(id)?.id || id;
  overlayWindows.value = overlayWindows.peek().filter((window) => window.id !== windowId);
  forgetOverlayWindow(windowId);
}

export function minimizeOverlayWindow(id, minimized = true) {
  const window = holding(id);
  if (!window) return;
  patchWindow(window.id, { minimized });
  if (!minimized) raiseOverlayWindow(window.id);
}

export function labelOverlayWindow(id, label) {
  const window = holding(id);
  if (!window || window.tabs.find((tab) => tab.id === id).label === label) return;
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
