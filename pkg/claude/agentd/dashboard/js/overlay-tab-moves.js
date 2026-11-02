// overlay-tab-moves.js -- where an overlay tab can go besides another overlay
// window (tcl-65bn0s): the Collab tab, or a browser window of its own. Each kind
// says how it gets there, so every tab offers the same moves, in the same words,
// from its menu as from its title bar.

// The words, the same for every kind of tab.
export const TO_COLLAB_LABEL = 'Open in the Collab tab';
// A separate window says Terminals where Collab is off, as the main nav does.
export const toTabsLabel = (collab = true) => (collab ? TO_COLLAB_LABEL : 'Open in the Terminals tab');
export const TO_WINDOW_LABEL = 'Open in separate window';
export const POPUP_BLOCKED = 'Could not open a separate window: allow pop-ups for this dashboard.';

const byKind = new Map();

// registerTabMoves says how a kind's tabs move. moves(tab) answers
// { toCollab, toWindow }, either null where that tab cannot go now. toWindow
// takes { rect, win, notify }: the rect of the window it leaves, to open as large
// where it was, and where to say a blocked pop-up kept it here. It returns the
// way to take the kind's moves back.
export function registerTabMoves(kind, moves) {
  byKind.set(kind, moves);
  return () => {
    if (byKind.get(kind) === moves) byKind.delete(kind);
  };
}

// tabMoves is what a tab offers now: { toCollab, toWindow }, each null if not.
export function tabMoves(tab) {
  const moves = tab && byKind.get(tab.kind)?.(tab);
  return { toCollab: moves?.toCollab || null, toWindow: moves?.toWindow || null };
}
