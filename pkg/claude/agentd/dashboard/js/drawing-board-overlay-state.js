// drawing-board-overlay-state.js -- which drawing boards float in overlay windows
// (drawing-board-overlay.js, tcl-h4y60w), held apart from the island so the terminal
// shell can move a drawing board in and out of one. A drawing board opens in one place at a
// time: a tab or a window. Opening a drawing board starts here rather than in
// terminals-tab.js, whose facade stays free of Preact.

import { computed } from '@preact/signals';
import { drawingBoardPaneSeed } from './drawing-board-model.js';
import { closeOverlayTab, labelOverlayWindow, openOverlayWindow, overlayTabs } from './overlay-instances.js';
import { openTerminalPane } from './terminals-tab.js';

export const DRAWING_BOARD_WINDOW_KIND = 'drawing_board';
export const drawingBoardWindowId = (id) => `drawing_board:${id}`;

// { id, label, minimized, ... } per drawing board window, where id is the window's and
// drawing board the drawing board's.
export const drawingBoardOverlays = computed(() => overlayTabs.value.filter((tab) => tab.kind === DRAWING_BOARD_WINDOW_KIND));

const windowOf = (drawingBoard) => drawingBoardOverlays.peek().find((window) => window.drawingBoard === drawingBoard);

// revealDrawingBoardWindow brings a floating drawing board forward, and reports whether it was.
export function revealDrawingBoardWindow(id) {
  const floating = windowOf(id);
  if (floating) openOverlayWindow(floating);
  return !!floating;
}

// openDrawingBoardPane opens a collab drawing board as a tab beside the terminals, or brings
// its tab forward. A drawing board floating in a window stays there and comes forward
// instead.
export function openDrawingBoardPane(id, label, options) {
  if (revealDrawingBoardWindow(id)) return Promise.resolve(null);
  return openTerminalPane(drawingBoardPaneSeed(id, label), options);
}

// openLinkedDrawingBoard opens the drawing board a URL names. Like a retired agent's deep link,
// a deleted drawing board's opens nothing, and the router corrects the URL. Any other
// failure leaves the answer open: the tab opens and says what is wrong.
export async function openLinkedDrawingBoard(id, fetchImpl = (...args) => globalThis.fetch(...args)) {
  try {
    const response = await fetchImpl(`/api/drawing-boards/${encodeURIComponent(id)}`, { credentials: 'same-origin' });
    if (response?.status === 404) return null;
  } catch { /* see above */ }
  return openDrawingBoardPane(id);
}

// openDrawingBoardOverlay floats a drawing board in a window of its own.
export function openDrawingBoardOverlay(id, label, at = null) {
  openOverlayWindow({ id: drawingBoardWindowId(id), kind: DRAWING_BOARD_WINDOW_KIND, drawingBoard: id, label: label || id, at });
}

// returnDrawingBoardToTab moves a floating drawing board back to a tab and shows it.
export function returnDrawingBoardToTab(id) {
  const floating = windowOf(id);
  if (!floating) return Promise.resolve(null);
  closeOverlayTab(floating.id);
  return openDrawingBoardPane(id, floating.label);
}

// By id: a load still in flight when the window closed answers for nothing.
export function setDrawingBoardOverlayLabel(id, label) {
  if (label) labelOverlayWindow(drawingBoardWindowId(id), label);
}
