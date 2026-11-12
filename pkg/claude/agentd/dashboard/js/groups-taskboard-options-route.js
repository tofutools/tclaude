// groups-taskboard-options-route.js -- which board's Board options drawer
// is open (groups-taskboard-options.js), as an overlay route:
// /groups?board-options=<group>[#idle|#events|#columns], a linked node's
// board as inst_…:<group>. A link or Back/Forward restores it; opening and
// closing the drawer records a history step, so Back closes it.
//
// Its own module so the column ⋯ (groups-taskboard-columns.js) can open the
// drawer without importing the drawer, which uses the column dialogs.

import { signal } from '@preact/signals';
import { announceOverlay, registerOverlayRoute } from './overlay-routes.js';
import { revealDropTarget } from './drop-landing.js';
import { formatBoardOptionsRoute, parseBoardOptionsRoute, sectionOfHash } from './groups-taskboard-options-model.js';

// boardOptionsRoute is the board whose drawer is open: { group, node,
// section, column }, group as its own node names it and node '' for this
// node's. The section's board that matches draws the drawer.
export const boardOptionsRoute = signal(null);

export function openBoardOptions({ group, node = '', section = '', column = '' }) {
  const open = boardOptionsRoute.value;
  const same = open && open.group === group && (open.node || '') === (node || '');
  boardOptionsRoute.value = { group, node: node || '', section, column };
  if (!same) announceOverlay();
}

export function closeBoardOptions() {
  if (!boardOptionsRoute.value) return;
  boardOptionsRoute.value = null;
  announceOverlay({ closed: true });
}

// boardNode is the linked node a board lives on ('' for this node's, and on
// a peer's own page for the page's), as a board options route names it.
export function boardNode(group) {
  const node = group?.fleet_node;
  return node && !node.local ? node.id : '';
}

// isBoardOptionsFor says whether the open drawer is the board's.
export function isBoardOptionsFor(target, group, node = '') {
  return !!target && target.group === group && (target.node || '') === (node || '');
}

// A link (or Back/Forward) asks for a board's options: unfold its group and
// Task board so the board draws the drawer, at the section the link's
// #fragment names.
registerOverlayRoute('board-options', {
  value: () => (boardOptionsRoute.value ? formatBoardOptionsRoute(boardOptionsRoute.value) : ''),
  restore: (value) => {
    const target = parseBoardOptionsRoute(value);
    boardOptionsRoute.value = target ? { ...target, section: sectionOfHash(globalThis.location?.hash), column: '' } : null;
    if (target) {
      try { revealDropTarget({ group: target.group, node: target.node, section: 'taskboard' }); } catch (_) { /* a link must never break the page */ }
    }
  },
});

