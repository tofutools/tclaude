// overlay-instances.js -- which overlay windows are open (tcl-ppvf8g). A window
// is an instance: { id, kind, label, minimized, ...what its kind needs }. Each
// has its own saved placement, place in the stack and place in a split, so a
// chat or a board is just one kind, and there can be any number of either. The
// companion is the one window that is not here: it is always the same one.

import { signal } from '@preact/signals';
import { forgetOverlayWindow, raiseOverlayWindow } from './overlay-window.js';

// In session storage, as the companion's open state is: a reload brings the
// windows back, another browser tab does not get a second set.
export const OVERLAY_WINDOWS_KEY = 'tclaude.dash.overlay-windows';

export const overlayWindows = signal([]);

function sessionStore(win) {
  try { return win?.sessionStorage ?? null; } catch { return null; }
}

function patchWindow(id, patch) {
  overlayWindows.value = overlayWindows.peek().map((window) => (window.id === id ? { ...window, ...patch } : window));
}

// openOverlayWindow shows a window, or brings back the one already open with
// that id: a chat or a board opens in one place at a time.
export function openOverlayWindow(instance) {
  if (overlayWindows.peek().some((window) => window.id === instance.id)) patchWindow(instance.id, { minimized: false });
  else overlayWindows.value = [...overlayWindows.peek(), { minimized: false, ...instance }];
  raiseOverlayWindow(instance.id);
}

// focusOverlayWindow asks a shown window to take the keyboard.
export function focusOverlayWindow(id) {
  const window = overlayWindows.peek().find((candidate) => candidate.id === id);
  if (window) patchWindow(id, { focusAsked: (window.focusAsked || 0) + 1 });
}

export function closeOverlayWindow(id) {
  overlayWindows.value = overlayWindows.peek().filter((window) => window.id !== id);
  forgetOverlayWindow(id);
}

export function minimizeOverlayWindow(id, minimized = true) {
  patchWindow(id, { minimized });
  if (!minimized) raiseOverlayWindow(id);
}

export function labelOverlayWindow(id, label) {
  if (overlayWindows.peek().find((window) => window.id === id)?.label !== label) patchWindow(id, { label });
}

// The drop point of a window dragged out, which only its first placement reads.
export function placedOverlayWindow(id) {
  patchWindow(id, { at: undefined });
}

// keepOverlayWindows reads the saved set, keeping only windows valid(window)
// accepts, and saves it from then on. It returns the way to stop saving.
export function keepOverlayWindows(win, valid) {
  if (!overlayWindows.peek().length) {
    try {
      const saved = JSON.parse(sessionStore(win)?.getItem?.(OVERLAY_WINDOWS_KEY) || '[]');
      overlayWindows.value = (Array.isArray(saved) ? saved : []).filter((window) => typeof window?.id === 'string' && valid(window));
    } catch { /* they just do not come back after a reload */ }
  }
  return overlayWindows.subscribe((windows) => {
    try {
      sessionStore(win)?.setItem?.(OVERLAY_WINDOWS_KEY, JSON.stringify(windows.map(({ at: _at, ...window }) => window)));
    } catch { /* as above */ }
  });
}
