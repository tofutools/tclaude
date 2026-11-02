// overlay-tab-moves.js -- where an overlay tab can go besides another overlay
// window (tcl-65bn0s): the Collab tab, or a browser window of its own. Each kind
// says how it gets there, so every tab offers the same moves, in the same words,
// from its menu as from its title bar, and a drop on the Collab view uses them too.

import { overlayTabs } from './overlay-instances.js';
import { placeTerminalPane } from './terminals-tab.js';

// The words, the same for every kind of tab.
export const TO_COLLAB_LABEL = 'Open in the Collab tab';
// A separate window says Terminals where Collab is off, as the main nav does.
export const toTabsLabel = (collab = true) => (collab ? TO_COLLAB_LABEL : 'Open in the Terminals tab');
export const TO_WINDOW_LABEL = 'Open in separate window';
export const POPUP_BLOCKED = 'Could not open a separate window: allow pop-ups for this dashboard.';

const byKind = new Map();

// registerTabMoves says how a kind's tabs move. moves(tab) answers
// { toCollab, toWindow }, either null where that tab cannot go now, and a kind
// docked somewhere other than Collab names that place as toDock, { label, run },
// and may say how it closes for good as close(). toCollab may
// answer, or promise, the pane it opened, so a drop can place it. toWindow
// takes { rect, win, notify }: the rect of the window it leaves, to open as large
// where it was, and where to say a blocked pop-up kept it here. It returns the
// way to take the kind's moves back.
export function registerTabMoves(kind, moves) {
  byKind.set(kind, moves);
  return () => {
    if (byKind.get(kind) === moves) byKind.delete(kind);
  };
}

// tabMoves is what a tab offers now: { toCollab, toWindow, toDock, close }, each
// null if not.
export function tabMoves(tab) {
  const moves = tab && byKind.get(tab.kind)?.(tab);
  return { toCollab: moves?.toCollab || null, toWindow: moves?.toWindow || null, toDock: moves?.toDock || null, close: moves?.close || null };
}

// moveTabsToCollab moves overlay tabs to the Collab tab, in order, beside the pane
// keyed beside (after it with after), or at the end with none. A tab that cannot
// go there stays where it is.
export async function moveTabsToCollab(ids, { beside = '', after = false } = {}) {
  let anchor = beside;
  let behind = after;
  for (const id of ids) {
    const tab = overlayTabs.peek().find((candidate) => candidate.id === id);
    const { toCollab } = tabMoves(tab);
    if (!toCollab) continue;
    const key = (await toCollab())?.key;
    if (!key) continue;
    if (anchor && anchor !== key) placeTerminalPane(key, anchor, { after: behind });
    anchor = key;
    behind = true;
  }
}
