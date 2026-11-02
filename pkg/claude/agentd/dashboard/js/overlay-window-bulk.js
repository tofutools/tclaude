// overlay-window-bulk.js -- actions on every overlay window at once (tcl-87awjd):
// untile them all, send them all to the dock, and bring them all back. The
// palette and the dock's menu offer the same actions in the same words, and each
// shows only when there is something for it to do.

import { batch, computed } from '@preact/signals';
import { minimizeOverlayWindow, overlayWindows } from './overlay-instances.js';
import { tiledOverlayWindowCount, untileOverlayWindows } from './overlay-window.js';

export const OVERLAY_BULK_ACTIONS = {
  untile: { icon: '⊟', label: 'Untile all windows', hint: 'Float every tiled window where it is, and end the split' },
  minimize: { icon: '▁', label: 'Minimize all windows', hint: 'Send every overlay window to the dock; tiled ones keep their cells' },
  restore: { icon: '▣', label: 'Restore all windows', hint: 'Bring every minimized overlay window back' },
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

// The actions there is something to do for now, in menu order.
export const overlayBulkActions = computed(() => {
  const windows = overlayWindows.value;
  const actions = [];
  if (tiledOverlayWindowCount.value > 0) actions.push({ key: 'untile', run: untileOverlayWindows });
  if (windows.some((window) => !window.minimized)) actions.push({ key: 'minimize', run: minimizeAllOverlayWindows });
  if (windows.some((window) => window.minimized)) actions.push({ key: 'restore', run: restoreAllOverlayWindows });
  return actions.map((action) => ({ ...action, ...OVERLAY_BULK_ACTIONS[action.key] }));
});

// buildOverlayBulkCommands is the palette's view of the same actions.
export function buildOverlayBulkCommands() {
  return overlayBulkActions.peek().map(({ key, icon, label, hint, run }) => ({
    id: `overlay.${key}-all`,
    scope: 'overlay-windows',
    icon,
    label,
    hint,
    keywords: `overlay windows window ${key} all tile tiling split dock`,
    run,
  }));
}
