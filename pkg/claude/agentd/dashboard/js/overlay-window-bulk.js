// overlay-window-bulk.js -- actions on every overlay window at once (tcl-87awjd):
// show them all side by side (overlay-expose.js), untile them all, send them all
// to the dock, and bring them all back. The
// palette and the dock's menu offer the same actions in the same words, and each
// shows only when there is something for it to do.

import { batch, computed, effect } from '@preact/signals';
import { minimizeOverlayWindow, overlayWindows } from './overlay-instances.js';
import { OVERLAY_EXPOSE_LABEL, overlayExposeAvailable, showAllOverlayWindows } from './overlay-expose.js';
import { tiledOverlayWindowCount, untileOverlayWindows } from './overlay-window.js';
import { hasShownOverlay } from './overlay-stack.js';
import { matchesShortcut } from './keybindings.js';

export const OVERLAY_BULK_ACTIONS = {
  expose: { icon: '⧉', label: OVERLAY_EXPOSE_LABEL, hint: 'Spread out every overlay window, minimized ones too, to pick one', shortcut: 'expose' },
  untile: { icon: '⊟', label: 'Untile all windows', hint: 'Float every tiled window where it is, and end the split' },
  minimize: { icon: '▁', label: 'Minimize all windows', hint: 'Send every overlay window to the dock; tiled ones keep their cells', shortcut: 'windows-hide-toggle' },
  restore: { icon: '▣', label: 'Restore all windows', hint: 'Bring every minimized overlay window back', shortcut: 'windows-hide-toggle' },
};

const tabOf = (window) => window.active || window.tabs?.[0]?.id;

// minimizeAllOverlayWindows sends every shown window to the dock. A window in
// the split keeps its cell there, so restoring puts the split back as it was.
export function minimizeAllOverlayWindows() {
  const shown = overlayWindows.peek().filter((window) => !window.minimized);
  batch(() => { for (const window of shown) minimizeOverlayWindow(tabOf(window)); });
  return shown.length > 0;
}

// restoreAllOverlayWindows brings back every window on the dock. A tab put on
// the dock on its own stays there: it left its window, not with it.
export function restoreAllOverlayWindows() {
  const away = overlayWindows.peek().filter((window) => window.minimized);
  batch(() => { for (const window of away) minimizeOverlayWindow(tabOf(window), false); });
  return away.length > 0;
}

// The ids of the windows the hide toggle (Ctrl ;) sent to the dock and that
// are still there, or null when the next press should hide (tcl-ht7r39).
let hiddenByToggle = null;
let stopWatching = null;

function forgetHidden() {
  hiddenByToggle = null;
  stopWatching?.();
  stopWatching = null;
}

// toggleOverlayWindowsHidden minimizes every shown window and remembers which,
// or, when it did that last, brings back just those. A window opened, or
// brought back and minimized again by hand in between, is left where it is: a
// window leaves the toggle's memory the moment it is back on screen. With none
// of its windows still on the dock, the press minimizes again.
export function toggleOverlayWindowsHidden() {
  const windows = overlayWindows.peek();
  const hidden = hiddenByToggle;
  forgetHidden();
  const back = hidden ? windows.filter((window) => window.minimized && hidden.has(window.id)) : [];
  if (back.length > 0) {
    batch(() => { for (const window of back) minimizeOverlayWindow(tabOf(window), false); });
    return true;
  }
  const shown = windows.filter((window) => !window.minimized);
  if (shown.length === 0) return false;
  batch(() => { for (const window of shown) minimizeOverlayWindow(tabOf(window)); });
  const remembered = new Set(shown.map((window) => window.id));
  hiddenByToggle = remembered;
  stopWatching = effect(() => {
    for (const window of overlayWindows.value) if (!window.minimized) remembered.delete(window.id);
  });
  return true;
}

// resetOverlayWindowsHiddenForTest forgets what the toggle hid.
export function resetOverlayWindowsHiddenForTest() { forgetHidden(); }

// installOverlayHideKey lets its shortcut (Ctrl ;) hide and bring back the
// windows from anywhere, a focused terminal included: capture phase, before
// xterm sees the key.
export function installOverlayHideKey(win) {
  const onKey = (event) => {
    if (!matchesShortcut(event, 'windows-hide-toggle')) return;
    // Not over a dialog: the windows are not what is in the way there.
    if (hasShownOverlay()) return;
    event.preventDefault();
    event.stopPropagation();
    // A held key toggles once, not back and forth.
    if (!event.repeat) toggleOverlayWindowsHidden();
  };
  win?.addEventListener?.('keydown', onKey, true);
  return () => win?.removeEventListener?.('keydown', onKey, true);
}

// The actions there is something to do for now, in menu order.
export const overlayBulkActions = computed(() => {
  const windows = overlayWindows.value;
  const actions = [];
  if (overlayExposeAvailable.value) actions.push({ key: 'expose', run: () => showAllOverlayWindows() });
  if (tiledOverlayWindowCount.value > 0) actions.push({ key: 'untile', run: untileOverlayWindows });
  if (windows.some((window) => !window.minimized)) actions.push({ key: 'minimize', run: minimizeAllOverlayWindows });
  if (windows.some((window) => window.minimized)) actions.push({ key: 'restore', run: restoreAllOverlayWindows });
  return actions.map((action) => ({ ...action, ...OVERLAY_BULK_ACTIONS[action.key] }));
});

// buildOverlayBulkCommands is the palette's view of the same actions.
export function buildOverlayBulkCommands() {
  return overlayBulkActions.peek().map(({ key, icon, label, hint, shortcut, run }) => ({
    id: `overlay.${key}-all`,
    scope: 'overlay-windows',
    icon,
    label,
    hint,
    shortcut,
    keywords: `overlay windows window ${key} all tile tiling split dock${key === 'expose' ? ' expose exposé overview spread switch' : ''}`,
    run,
  }));
}
