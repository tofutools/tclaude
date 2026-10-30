// terminal-board-tabs.js -- which collab boards are open as tabs of the terminal
// shell. A terminal pane does not outlive the page, but a board has nothing to
// reattach, so its tab comes back after a reload: this keeps the open boards in
// a preference and reopens them behind features.collab. While the flag is off
// nothing is restored, and any board tab there is goes (the preference stays,
// for when it turns on again).
//
// Signals and a preference only, so it is testable without a rendered shell.

import { dashPrefs } from './prefs.js';
import { boardPaneSeed, isBoardId } from './board-model.js';

export const BOARD_TABS_PREF = 'tclaude.dash.terminals.boards';
const MAX_BOARD_TABS = 64;
const MAX_LABEL = 120;

// parseBoardTabs reads the preference back, dropping anything malformed.
export function parseBoardTabs(raw) {
  let parsed = null;
  try { parsed = JSON.parse(raw || 'null'); } catch { return []; }
  if (!Array.isArray(parsed)) return [];
  const seen = new Set();
  const tabs = [];
  for (const tab of parsed) {
    if (!isBoardId(tab?.id) || seen.has(tab.id) || tabs.length >= MAX_BOARD_TABS) continue;
    seen.add(tab.id);
    tabs.push({ id: tab.id, label: typeof tab.label === 'string' ? tab.label.slice(0, MAX_LABEL) : '' });
  }
  return tabs;
}

function openBoards(panes) {
  return panes.filter((pane) => pane.seed?.board);
}

export function bindBoardTabs({
  state,
  snapshot,
  prefs = dashPrefs,
  fetchImpl = (...args) => globalThis.fetch(...args),
} = {}) {
  // Restored once per time the flag is seen on. Saving waits for the restore,
  // or a tab opened meanwhile would overwrite the tabs still to come.
  let restoring = 0;
  let started = false;
  let saving = false;
  let saved = null;

  function save(panes) {
    if (!saving) return;
    const next = JSON.stringify(openBoards(panes).map((pane) => ({ id: pane.seed.board, label: pane.label })));
    if (next === saved) return;
    saved = next;
    try { prefs.setItem(BOARD_TABS_PREF, next); } catch { /* the tabs stay open */ }
  }

  async function restore() {
    const run = ++restoring;
    saved = prefs.getItem(BOARD_TABS_PREF);
    const tabs = parseBoardTabs(saved);
    // A board deleted since stays closed. If the list cannot be read, keep the
    // tabs: each one says what is wrong when it is looked at.
    let known = null;
    if (tabs.length) {
      try {
        const response = await fetchImpl('/api/boards', { credentials: 'same-origin' });
        const body = response.ok ? await response.json() : null;
        if (Array.isArray(body?.boards)) known = new Map(body.boards.map((board) => [board.id, board.title]));
      } catch { /* see above */ }
    }
    if (run !== restoring) return;
    for (const { id, label } of tabs) {
      if (known && !known.has(id)) continue;
      state.openPane({ ...boardPaneSeed(id, known?.get(id) || label), restored: true }, { reveal: false, activate: false });
    }
    saving = true;
    save(state.panes.peek());
  }

  const stopGate = snapshot.subscribe((value) => {
    if (!value) return;
    if (value.collab_enabled === true) {
      if (!started) {
        started = true;
        void restore();
      }
      return;
    }
    started = false;
    saving = false;
    restoring += 1;
    state.removePanes(openBoards(state.panes.peek()).map((pane) => pane.key));
  });
  const stopSave = state.panes.subscribe(save);

  return () => {
    restoring += 1;
    stopGate();
    stopSave();
  };
}
