// board-overlay-state.js -- which boards float in overlay windows
// (board-overlay.js, tcl-h4y60w), held apart from the island so the terminal
// shell can move a board in and out of one. A board opens in one place at a
// time: a tab or a window. Opening a board starts here rather than in
// terminals-tab.js, whose facade stays free of Preact.

import { computed } from '@preact/signals';
import { boardPaneSeed } from './board-model.js';
import { closeOverlayWindow, labelOverlayWindow, openOverlayWindow, overlayTabs } from './overlay-instances.js';
import { openTerminalPane } from './terminals-tab.js';

export const BOARD_WINDOW_KIND = 'board';
export const boardWindowId = (id) => `board:${id}`;

// { id, label, minimized, ... } per board window, where id is the window's and
// board the board's.
export const boardOverlays = computed(() => overlayTabs.value.filter((tab) => tab.kind === BOARD_WINDOW_KIND));

const windowOf = (board) => boardOverlays.peek().find((window) => window.board === board);

// revealBoardWindow brings a floating board forward, and reports whether it was.
export function revealBoardWindow(id) {
  const floating = windowOf(id);
  if (floating) openOverlayWindow(floating);
  return !!floating;
}

// openBoardPane opens a collab board as a tab beside the terminals, or brings
// its tab forward. A board floating in a window stays there and comes forward
// instead.
export function openBoardPane(id, label, options) {
  if (revealBoardWindow(id)) return Promise.resolve(null);
  return openTerminalPane(boardPaneSeed(id, label), options);
}

// openLinkedBoard opens the board a URL names. Like a retired agent's deep link,
// a deleted board's opens nothing, and the router corrects the URL. Any other
// failure leaves the answer open: the tab opens and says what is wrong.
export async function openLinkedBoard(id, fetchImpl = (...args) => globalThis.fetch(...args)) {
  try {
    const response = await fetchImpl(`/api/boards/${encodeURIComponent(id)}`, { credentials: 'same-origin' });
    if (response?.status === 404) return null;
  } catch { /* see above */ }
  return openBoardPane(id);
}

// openBoardOverlay floats a board in a window of its own.
export function openBoardOverlay(id, label, at = null) {
  openOverlayWindow({ id: boardWindowId(id), kind: BOARD_WINDOW_KIND, board: id, label: label || id, at });
}

// returnBoardToTab moves a floating board back to a tab and shows it.
export function returnBoardToTab(id) {
  const floating = windowOf(id);
  if (!floating) return Promise.resolve(null);
  closeOverlayWindow(floating.id);
  return openBoardPane(id, floating.label);
}

// By id: a load still in flight when the window closed answers for nothing.
export function setBoardOverlayLabel(id, label) {
  if (label) labelOverlayWindow(boardWindowId(id), label);
}
