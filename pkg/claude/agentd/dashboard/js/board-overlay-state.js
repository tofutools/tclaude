// board-overlay-state.js -- which board floats in the overlay window
// (board-overlay.js, tcl-h4y60w), held apart from the island so the terminal
// shell can move a board in and out of it. One board at a time. Opening a board
// starts here rather than in terminals-tab.js, whose facade stays free of Preact.

import { signal } from '@preact/signals';
import { boardPaneSeed } from './board-model.js';
import { openTerminalPane } from './terminals-tab.js';

// { id, label }, or null while the overlay is closed.
export const boardOverlay = signal(null);
// Bumped to bring the window forward: someone asked for the board it shows.
export const boardOverlayRaise = signal(0);

// placeBoardInOverlay shows a board in the overlay and returns the board it
// displaced, if any: that one needs a tab to go back to.
export function placeBoardInOverlay(id, label) {
  const previous = boardOverlay.peek();
  boardOverlay.value = { id, label: label || id };
  boardOverlayRaise.value += 1;
  return previous && previous.id !== id ? previous : null;
}

// openBoardPane opens a collab board as a tab beside the terminals, or brings
// its tab forward. A board floating in the overlay stays there and comes
// forward instead: it opens in one place at a time.
export function openBoardPane(id, label, options) {
  if (boardOverlay.peek()?.id === id) {
    boardOverlayRaise.value += 1;
    return Promise.resolve(null);
  }
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

// openBoardOverlay floats a board in the overlay window. The board it displaces
// goes back to a tab, quietly: nothing is closed by opening something else.
export function openBoardOverlay(id, label) {
  const displaced = placeBoardInOverlay(id, label);
  if (displaced) void openBoardPane(displaced.id, displaced.label, { reveal: false, activate: false });
}

// returnBoardToTab moves the overlay's board back to a tab and shows it.
export function returnBoardToTab() {
  const board = boardOverlay.peek();
  if (!board) return Promise.resolve(null);
  boardOverlay.value = null;
  return openBoardPane(board.id, board.label);
}

// By id: a load still in flight when the board was replaced answers for the old one.
export function setBoardOverlayLabel(id, label) {
  const current = boardOverlay.peek();
  if (current?.id === id && label && label !== current.label) boardOverlay.value = { ...current, label };
}
