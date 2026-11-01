// overlay-tab-model.js -- what an overlay window holds (tcl-e4i44z): a list of
// tabs and the one showing. A chat, a shell or a board is a tab's kind, not a
// window's: a window is { id, tabs, active, minimized }, and a tab is
// { id, kind, label, ...what its kind needs }. A tab minimized by itself leaves
// its window, and waits as { tab, home }, home being the window it came from.
//
// A window's placement is saved under place, which is settled when the window
// is made, so what its tabs do later never moves it.
//
// Everything here is DOM-free so it can be tested directly.

export const CHAT_PLACEMENT_PREF = 'tclaude.dash.chat-window.placement';
export const TERMINAL_WINDOW_PREF = 'tclaude.dash.terminal-window.placement';
export const BOARD_OVERLAY_PLACEMENT_PREF = 'tclaude.dash.board-overlay.placement';

// Each kind's placements, by the field that names the tab's content.
const PLACEMENTS = Object.freeze({
  chat: [CHAT_PLACEMENT_PREF, 'agent'],
  terminal: [TERMINAL_WINDOW_PREF, 'paneKey'],
  board: [BOARD_OVERLAY_PLACEMENT_PREF, 'board'],
});

// placementKeyOf is where a window opened around tab saves its placement: where
// that content's own window has always saved it.
export function placementKeyOf(tab) {
  const [pref, field] = PLACEMENTS[tab.kind] || [];
  return pref ? `${pref}.${tab[field]}` : '';
}

// windowOfTab is a new window around one tab. It takes the tab's id, so a window
// saved before tabs keeps its place in the stack and in a split.
export function windowOfTab({ minimized = false, at, focusAsked, ...tab }) {
  return { id: tab.id, tabs: [tab], active: tab.id, minimized: !!minimized, place: placementKeyOf(tab), ...(at && { at }), ...(focusAsked && { focusAsked }) };
}

// activeTabOf is the tab a window shows, with what it takes from its window.
export function activeTabOf(window) {
  return tabsOfWindows([window]).find((tab) => tab.id === window.active) || null;
}

// windowHolding finds the window a tab is in.
export function windowHolding(windows, tabId) {
  return windows.find((window) => window.tabs.some((tab) => tab.id === tabId)) || null;
}

// tabsOfWindows lists every tab with what it takes from its window.
export function tabsOfWindows(windows) {
  return windows.flatMap((window) => window.tabs.map((tab) => ({
    ...tab, windowId: window.id, minimized: window.minimized, focusAsked: window.focusAsked, at: window.at,
  })));
}

const isObject = (value) => !!value && typeof value === 'object' && !Array.isArray(value);

// A session has one web view, so a tab seen once is not taken again.
function keptTabs(tabs, valid, seen) {
  return tabs.filter((tab) => {
    if (!isObject(tab) || typeof tab.id !== 'string' || seen.has(tab.id) || !valid(tab)) return false;
    seen.add(tab.id);
    return true;
  });
}

// A window saved before tabs is flat: its one tab's fields and its own together.
function savedWindow(raw, valid, seen) {
  if (!isObject(raw)) return null;
  const window = Array.isArray(raw.tabs) ? raw : windowOfTab(raw);
  const tabs = keptTabs(window.tabs, valid, seen);
  if (typeof window.id !== 'string' || !tabs.length) return null;
  const active = tabs.some((tab) => tab.id === window.active) ? window.active : tabs[0].id;
  const place = typeof window.place === 'string' && window.place.startsWith('tclaude.dash.') ? window.place : placementKeyOf(tabs[0]);
  return { ...window, tabs, active, minimized: !!window.minimized, place };
}

function savedMinimizedTab(raw, valid, seen) {
  const [tab] = keptTabs([raw?.tab], valid, seen);
  return tab ? { tab, home: typeof raw.home === 'string' ? raw.home : '' } : null;
}

// parseSavedOverlays reads the saved set in either shape it was written in,
// keeping only the tabs valid(tab) accepts. A window left with none goes.
export function parseSavedOverlays(raw, valid) {
  try {
    const saved = JSON.parse(raw || '[]');
    const seen = new Set();
    const windows = Array.isArray(saved) ? saved : saved?.windows;
    return {
      windows: (Array.isArray(windows) ? windows : []).map((window) => savedWindow(window, valid, seen)).filter(Boolean),
      minimizedTabs: (Array.isArray(saved?.minimizedTabs) ? saved.minimizedTabs : []).map((entry) => savedMinimizedTab(entry, valid, seen)).filter(Boolean),
    };
  } catch {
    return { windows: [], minimizedTabs: [] };
  }
}

// A drop point is not a place to reopen at.
export function savedOverlays(windows, minimizedTabs) {
  return JSON.stringify({ windows: windows.map(({ at: _at, ...window }) => window), minimizedTabs });
}
