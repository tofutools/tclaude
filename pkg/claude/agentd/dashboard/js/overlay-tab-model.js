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
// A window's tabs can fall into groups, as a Terminals strip's do: window.groups
// lists { id, name, color, collapsed }, and a member tab names its groupId. A group
// is a run of tabs side by side, and goes with its last member. settled keeps that
// true, so every change to a window's tabs goes through it.
//
// Everything here is DOM-free so it can be tested directly.

import { MAX_TERMINAL_TAB_GROUPS, normalizeColor, normalizeGrouping, sanitizeGroupName } from './tab-group-model.js';

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
export function windowOfTab({ minimized = false, at, focusAsked, windowId: _window, closeAsked: _asked, groupId: _group, ...tab }) {
  return { id: tab.id, tabs: [tab], active: tab.id, minimized: !!minimized, place: placementKeyOf(tab), ...(at && { at }), ...(focusAsked && { focusAsked }) };
}

// settled is a window with its groups in order: a member whose group is gone
// leaves it, a group with no members goes, and each group's members sit together.
function settled(window) {
  const groups = window.groups || [];
  const tabs = window.tabs.map((tab) => (!tab.groupId || groups.some((group) => group.id === tab.groupId) ? tab : withoutGroup(tab)));
  const groupOf = new Map(tabs.map((tab) => [tab.id, tab.groupId]));
  const inOrder = normalizeGrouping(tabs.map((tab) => ({ ...tab, key: tab.id })), (key) => groupOf.get(key)).map(({ key: _key, ...tab }) => tab);
  const kept = groups.filter((group) => tabs.some((tab) => tab.groupId === group.id));
  const { groups: _groups, ...rest } = window;
  return { ...rest, tabs: inOrder, ...(kept.length && { groups: kept }) };
}

function withoutGroup({ groupId: _group, ...tab }) {
  return tab;
}

const memberOf = (tab, groupId) => (groupId ? { ...tab, groupId } : withoutGroup(tab));

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

// groupsOf lists a window's groups, in the order they show.
export function groupsOf(window) {
  return [...new Set(window.tabs.map((tab) => tab.groupId).filter(Boolean))].map((id) => (window.groups || []).find((group) => group.id === id)).filter(Boolean);
}

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
  return next?.tabs.length ? [settled(next)] : [];
});

// movedTab puts a tab at index among the tabs of a window, which may be its own,
// and shows it there, in groupId if it was dropped inside one. The window it left
// goes with its last tab.
export function movedTab(windows, tabId, targetId, index, groupId = '') {
  const source = windowHolding(windows, tabId);
  const onlyTab = source?.id === targetId && source.tabs.length < 2;
  if (!source || onlyTab || !windows.some((window) => window.id === targetId)) return windows;
  const tab = source.tabs.find((candidate) => candidate.id === tabId);
  const left = replaced(windows, source.id, withoutTab(source, tabId));
  const target = left.find((window) => window.id === targetId);
  const tabs = [...target.tabs];
  tabs.splice(Math.max(0, Math.min(index, tabs.length)), 0, memberOf(tab, groupId));
  return replaced(left, targetId, { ...target, tabs, active: tabId, minimized: false });
}

// A group's own id can be taken where it lands.
const freshGroupId = (window, wanted = '') => {
  const taken = new Set((window.groups || []).map((group) => group.id));
  if (wanted && !taken.has(wanted)) return wanted;
  let n = taken.size + 1;
  while (taken.has(`group-${n}`)) n += 1;
  return `group-${n}`;
};

// movedGroup puts a whole group at index among a window's tabs, its own or
// another's, and shows the tab it was showing or its first.
export function movedGroup(windows, sourceId, groupId, targetId, index) {
  const source = windows.find((window) => window.id === sourceId);
  const group = source?.groups?.find((candidate) => candidate.id === groupId);
  const members = source?.tabs.filter((tab) => tab.groupId === groupId) || [];
  if (!group || !windows.some((window) => window.id === targetId)) return windows;
  const leaving = new Set(members.map((tab) => tab.id));
  const rest = source.tabs.filter((tab) => !leaving.has(tab.id));
  const shown = leaving.has(source.active) ? source.active : members[0].id;
  const left = sourceId === targetId ? windows : replaced(windows, sourceId, { ...source, tabs: rest, active: leaving.has(source.active) ? rest[0]?.id : source.active });
  const target = left.find((window) => window.id === targetId);
  const tabs = sourceId === targetId ? rest : [...target.tabs];
  const id = sourceId === targetId ? groupId : freshGroupId(target, groupId);
  tabs.splice(Math.max(0, Math.min(index, tabs.length)), 0, ...members.map((tab) => ({ ...tab, groupId: id })));
  const groups = sourceId === targetId ? target.groups : [...(target.groups || []), { ...group, id }];
  return replaced(left, targetId, { ...target, tabs, groups, active: shown, minimized: false });
}

// tornOutGroup gives a group a window of its own, at at, unless it is all its
// window has.
export function tornOutGroup(windows, sourceId, groupId, at) {
  const source = windows.find((window) => window.id === sourceId);
  const members = source?.tabs.filter((tab) => tab.groupId === groupId) || [];
  if (!members.length || members.length === source.tabs.length) return windows;
  const leaving = new Set(members.map((tab) => tab.id));
  const rest = source.tabs.filter((tab) => !leaving.has(tab.id));
  const left = replaced(windows, sourceId, { ...source, tabs: rest, active: leaving.has(source.active) ? rest[0].id : source.active });
  const shown = members.find((tab) => tab.id === source.active) || members[0];
  const opened = windowAround(left, shown, { at });
  return [...left, settled({ ...opened, tabs: members, active: shown.id, groups: source.groups })];
}

// groupedTab starts a group of one tab, or adds a tab to a group of its window,
// or with no group takes it out of the one it is in.
export function groupedTab(windows, tabId, group = null) {
  const window = windowHolding(windows, tabId);
  if (!window) return windows;
  const tabs = window.tabs.map((tab) => (tab.id === tabId ? memberOf(tab, group?.id) : tab));
  const fresh = group && !(window.groups || []).some((candidate) => candidate.id === group.id);
  return replaced(windows, window.id, { ...window, tabs, ...(fresh && { groups: [...(window.groups || []), group] }) });
}

// newTabGroup is a group for a window, numbered so its default name is unused.
export function newTabGroup(window) {
  const count = (window.groups || []).length;
  if (count >= MAX_TERMINAL_TAB_GROUPS) return null;
  const id = freshGroupId(window);
  return { id, name: `group ${id.slice('group-'.length)}`, color: normalizeColor('', count), collapsed: false };
}

// changedGroup is a group patched: renamed, say. A group collapsing hands the
// showing tab to a tab outside it, when one can be seen.
export function changedGroup(windows, windowId, groupId, patch) {
  const window = windows.find((candidate) => candidate.id === windowId);
  if (!window) return windows;
  const groups = (window.groups || []).map((group) => (group.id === groupId ? { ...group, ...patch } : group));
  const hidden = new Set(groups.filter((group) => group.collapsed).map((group) => group.id));
  const member = window.tabs.find((tab) => tab.id === window.active)?.groupId === groupId;
  const outside = patch.collapsed && member ? window.tabs.find((tab) => !hidden.has(tab.groupId)) : null;
  return replaced(windows, windowId, { ...window, groups, active: outside?.id || window.active });
}

// dissolvedGroup ungroups its tabs, which stay where they are.
export function dissolvedGroup(windows, windowId, groupId) {
  const window = windows.find((candidate) => candidate.id === windowId);
  if (!window) return windows;
  return replaced(windows, windowId, { ...window, groups: (window.groups || []).filter((group) => group.id !== groupId) });
}

// tornOutTab gives a tab a window of its own, at at. The last tab of a window has
// nothing to leave.
export function tornOutTab(windows, tabId, at) {
  const source = windowHolding(windows, tabId);
  if (!source || source.tabs.length < 2) return windows;
  const tab = source.tabs.find((candidate) => candidate.id === tabId);
  const left = replaced(windows, source.id, withoutTab(source, tabId));
  return [...left, windowAround(left, withoutGroup(tab), { at })];
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
  return settled({ ...window, tabs, active, minimized: !!window.minimized, place, groups: savedGroups(window.groups) });
}

// A saved name is shown again, so it passes what a typed one does.
function savedGroups(groups) {
  return (Array.isArray(groups) ? groups : []).filter((group) => isObject(group) && typeof group.id === 'string').slice(0, MAX_TERMINAL_TAB_GROUPS)
    .map(({ id, name, color, collapsed }, index) => ({ id, name: sanitizeGroupName(name), color: normalizeColor(color, index), collapsed: !!collapsed }));
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
