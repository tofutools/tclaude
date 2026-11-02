// overlay-tab-model.js -- what an overlay window holds (tcl-e4i44z): a list of
// tabs and the one showing. A chat, a shell or a board is a tab's kind, not a
// window's: a window is { id, tabs, active, minimized }, and a tab is
// { id, kind, label, ...what its kind needs }. A tab minimized by itself leaves
// its window, and waits as { tab, home, place }: the window it came from and that
// window's placement. A window with no tabs left is gone.
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
export function windowOfTab({ minimized = false, at, focusAsked, windowId: _window, closeAsked: _asked, ...tab }) {
  return { id: tab.id, tabs: [tab], active: tab.id, minimized: !!minimized, place: placementKeyOf(tab), ...(at && { at }), ...(focusAsked && { focusAsked }) };
}

// activeTabOf is the tab a window shows, with what it takes from its window.
export function activeTabOf(window) {
  return tabsOfWindows([window]).find((tab) => tab.id === window.active) || null;
}

const withNumber = (wanted, taken) => {
  if (!taken.has(wanted)) return wanted;
  let n = 2;
  while (taken.has(`${wanted}#${n}`)) n += 1;
  return `${wanted}#${n}`;
};

// windowAround is a new window around one tab among windows. A tab that left the
// window named after it cannot take that id or that placement, which stay there.
export function windowAround(windows, tab, { id = tab.id, place = '', at = null } = {}) {
  const window = windowOfTab({ ...tab, at });
  return {
    ...window,
    id: withNumber(id, new Set(windows.map((other) => other.id))),
    place: withNumber(place || window.place, new Set(windows.map((other) => other.place))),
  };
}

// withoutTab is a window after a tab left it. Its neighbour takes over showing.
function withoutTab(window, tabId) {
  const at = window.tabs.findIndex((tab) => tab.id === tabId);
  const tabs = window.tabs.filter((tab) => tab.id !== tabId);
  const active = window.active === tabId ? tabs[Math.min(at, tabs.length - 1)]?.id : window.active;
  return { ...window, tabs, active };
}

const replaced = (windows, id, next) => windows.flatMap((window) => {
  if (window.id !== id) return [window];
  return next?.tabs.length ? [next] : [];
});

// movedTab puts a tab at index among the tabs of a window, which may be its own,
// and shows it there. The window it left goes with its last tab.
export function movedTab(windows, tabId, targetId, index) {
  const source = windowHolding(windows, tabId);
  const onlyTab = source?.id === targetId && source.tabs.length < 2;
  if (!source || onlyTab || !windows.some((window) => window.id === targetId)) return windows;
  const tab = source.tabs.find((candidate) => candidate.id === tabId);
  const left = replaced(windows, source.id, withoutTab(source, tabId));
  const target = left.find((window) => window.id === targetId);
  const tabs = [...target.tabs];
  tabs.splice(Math.max(0, Math.min(index, tabs.length)), 0, tab);
  return replaced(left, targetId, { ...target, tabs, active: tabId, minimized: false });
}

// tornOutTab gives a tab a window of its own, at at. The last tab of a window has
// nothing to leave.
export function tornOutTab(windows, tabId, at) {
  const source = windowHolding(windows, tabId);
  if (!source || source.tabs.length < 2) return windows;
  const tab = source.tabs.find((candidate) => candidate.id === tabId);
  const left = replaced(windows, source.id, withoutTab(source, tabId));
  return [...left, windowAround(left, tab, { at })];
}

// minimizedTab sends a window's tab to the dock on its own.
export function minimizedTab({ windows, minimizedTabs }, tabId) {
  const source = windowHolding(windows, tabId);
  if (!source) return { windows, minimizedTabs };
  const tab = source.tabs.find((candidate) => candidate.id === tabId);
  return {
    windows: replaced(windows, source.id, withoutTab(source, tabId)),
    minimizedTabs: [...minimizedTabs, { tab, home: source.id, place: source.place }],
  };
}

// restoredTab brings a tab back from the dock into the window it came from, or
// into a new one that takes that window's place, for any tab that left with it.
export function restoredTab({ windows, minimizedTabs }, tabId) {
  const entry = minimizedTabs.find(({ tab }) => tab.id === tabId);
  if (!entry) return { windows, minimizedTabs };
  const rest = minimizedTabs.filter((other) => other !== entry);
  const home = windows.find((window) => window.id === entry.home);
  if (home) {
    const next = { ...home, tabs: [...home.tabs, entry.tab], active: tabId, minimized: false };
    return { windows: replaced(windows, home.id, next), minimizedTabs: rest };
  }
  return { windows: [...windows, windowAround(windows, entry.tab, { id: entry.home || tabId, place: entry.place })], minimizedTabs: rest };
}

// removedTab takes a tab off for good, wherever it is.
export function removedTab({ windows, minimizedTabs }, tabId) {
  const source = windowHolding(windows, tabId);
  return {
    windows: source ? replaced(windows, source.id, withoutTab(source, tabId)) : windows,
    minimizedTabs: minimizedTabs.filter(({ tab }) => tab.id !== tabId),
  };
}

// dockEntriesOf is what the dock shows: one pill for each minimized window, with
// its active tab, and one for each tab minimized on its own.
export function dockEntriesOf(windows, minimizedTabs) {
  return [
    ...windows.filter((window) => window.minimized).map((window) => ({ key: window.id, tab: activeTabOf(window), tabs: window.tabs })),
    ...minimizedTabs.map(({ tab }) => ({ key: `tab:${tab.id}`, tab, tabs: [tab] })),
  ];
}

// dropIndex is where a tab dropped at x lands among tabs drawn at spans, which
// leave out the tab itself: { left, width } in order.
export function dropIndex(spans, x) {
  const after = spans.findIndex(({ left, width }) => x < left + width / 2);
  return after < 0 ? spans.length : after;
}

// pulledOff reports whether a point is well clear of a strip's rectangle.
export function pulledOff(rect, { x, y }, margin) {
  return x < rect.left - margin || x > rect.right + margin || y < rect.top - margin || y > rect.bottom + margin;
}

// windowHolding finds the window a tab is in.
export function windowHolding(windows, tabId) {
  return windows.find((window) => window.tabs.some((tab) => tab.id === tabId)) || null;
}

// tabsOfWindows lists every tab with what it takes from its window.
export function tabsOfWindows(windows) {
  return windows.flatMap((window) => window.tabs.map((tab) => ({
    ...tab, windowId: window.id, minimized: window.minimized, focusAsked: window.focusAsked, closeAsked: window.closeAsked, at: window.at,
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
  if (!tab) return null;
  return { tab, home: typeof raw.home === 'string' ? raw.home : '', place: typeof raw.place === 'string' && raw.place.startsWith('tclaude.dash.') ? raw.place : '' };
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

// A drop point is not a place to reopen at, and a question to ask is not kept.
export function savedOverlays(windows, minimizedTabs) {
  return JSON.stringify({ windows: windows.map(({ at: _at, closeAsked: _asked, ...window }) => window), minimizedTabs });
}
