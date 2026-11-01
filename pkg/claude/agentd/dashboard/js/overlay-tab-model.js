// overlay-tab-model.js -- what an overlay window holds (tcl-e4i44z): a list of
// tabs and the one showing. A chat, a shell or a board is a tab's kind, not a
// window's: a window is { id, tabs, active, minimized }, and a tab is
// { id, kind, label, ...what its kind needs }. A tab minimized by itself leaves
// its window, and waits as { tab, home }, home being the window it came from.
//
// Everything here is DOM-free so it can be tested directly.

// windowOfTab is a new window around one tab. It takes the tab's id, so a window
// saved before tabs keeps its place in the stack and in a split.
export function windowOfTab({ minimized = false, at, focusAsked, ...tab }) {
  return { id: tab.id, tabs: [tab], active: tab.id, minimized: !!minimized, ...(at && { at }), ...(focusAsked && { focusAsked }) };
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
  return { ...window, tabs, active, minimized: !!window.minimized };
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
