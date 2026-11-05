// collab-rail-model.js -- what the Collab side panel lists
// (tcl-lx7nb7, tcl-8nfzbb): every group's resources and its agents'
// terminals, by group, each in one of four buckets. Active (open here, on the
// dock, or running out of sight) is what shows by default; closed items show
// under the Closed chip and in a search. A group shell that is not running is
// not listed at all (the server lists it only while it runs); the Groups
// tab's ＋ → Shell starts one.
// Pure: the panel (collab-rail.js) and the home (collab-home.js, for the agents
// that need the operator) feed it the snapshot, this browser's places, its open panes, overlay tabs and
// background sessions, and when each item was last used here.

import { overlayResourceKey, resourceKey, resourceState, stripResourceKey } from './resource-model.js';

// The panel's filter (tcl-t1onur): which states and kinds it lists. Where an
// item is (open here, in the background, closed) applies to everything; what
// an agent is doing (needs you, working, idle) applies to agents. An item
// shows when its place and its kind are checked, and, for an agent, what it is
// doing. The default is everything but Closed.
export const FILTER_STATES = Object.freeze([
  ['needs_you', 'Needs you'], ['working', 'Working'], ['idle', 'Idle'],
  ['background', 'Background'], ['open', 'Open'], ['closed', 'Closed'],
]);
export const FILTER_KINDS = Object.freeze([
  ['agents', 'Agents'], ['shells', 'Shells'], ['boards', 'Boards'], ['pages', 'Pages'], ['processes', 'Processes'],
]);
export const DEFAULT_FILTER = Object.freeze({
  states: Object.freeze(FILTER_STATES.map(([key]) => key).filter((key) => key !== 'closed')),
  kinds: Object.freeze(FILTER_KINDS.map(([key]) => key)),
});

// readFilter is a stored filter ({ states, kinds }), or the default for
// anything unreadable. Unknown keys are dropped.
export function readFilter(raw) {
  try {
    const value = typeof raw === 'string' ? JSON.parse(raw) : raw;
    const keep = (list, known) => (Array.isArray(list) ? list.filter((key) => known.some(([k]) => k === key)) : null);
    const states = keep(value?.states, FILTER_STATES);
    const kinds = keep(value?.kinds, FILTER_KINDS);
    if (!states || !kinds) return DEFAULT_FILTER;
    return { states, kinds };
  } catch {
    return DEFAULT_FILTER;
  }
}

// filterChanges is how many boxes differ from the default: the count on the
// filter button.
export function filterChanges(filter) {
  const differs = (list, base, all) => all.filter(([key]) => list.includes(key) !== base.includes(key)).length;
  return differs(filter.states, DEFAULT_FILTER.states, FILTER_STATES) + differs(filter.kinds, DEFAULT_FILTER.kinds, FILTER_KINDS);
}

// itemPlace, itemKind and itemActivity are the filter's view of an item.
const itemPlace = (item) => (item.bucket === 'minimized' ? 'open' : item.bucket);
const KIND_OF = Object.freeze({ shell: 'shells', board: 'boards', page: 'pages', process: 'processes' });
const itemKind = (item) => (item.kind === 'terminal' ? (isAgentItem(item) ? 'agents' : 'shells') : KIND_OF[item.kind] || 'shells');
function itemActivity(item) {
  if (needsYou(item)) return 'needs_you';
  return item.status?.key === 'working' ? 'working' : 'idle';
}

// matchesFilter is whether the filter lists item.
export function matchesFilter(item, filter = DEFAULT_FILTER) {
  if (!filter.states.includes(itemPlace(item)) || !filter.kinds.includes(itemKind(item))) return false;
  return !isAgentItem(item) || filter.states.includes(itemActivity(item));
}

// bucketOf is the chip a state counts under: open here (a tab or a window),
// on the dock, running out of sight, or not live.
export function bucketOf(view) {
  if (view === 'open-tab' || view === 'floating') return 'open';
  if (view === 'minimized') return 'minimized';
  if (view === 'running' || view === 'background') return 'background';
  return 'closed';
}

// BUCKET_RANK orders items within a group: open here first, closed last.
const BUCKET_RANK = Object.freeze({ open: 0, minimized: 1, background: 2, closed: 3 });

// isAgentItem is a terminal with an agent behind it: the Collab view's main
// thing, listed before a group's resources.
export const isAgentItem = (item) => item.kind === 'terminal' && !!item.status?.agent;
const itemTier = (item) => (item.bucket === 'closed' ? 2 : isAgentItem(item) ? 0 : 1);

// AGENT_RANK orders a group's agents by what they are doing, so the order is
// fixed: the ones that need the operator, then working, then idle, then the
// rest; by name within each.
const AGENT_RANK = Object.freeze({ awaiting_permission: 0, awaiting_input: 0, working: 1, idle: 2 });
const agentRank = (item) => AGENT_RANK[item.status?.key] ?? 3;

// needsYou is an agent waiting on the operator.
export const needsYou = (item) => isAgentItem(item) && agentRank(item) === 0;

// compareItems orders a group's items: agents (by agentRank, then name), then
// live resources (open here first, then the snapshot's order), then closed
// ones, the most recently used first. Opening a live item never moves it, so
// the row under the pointer stays the one clicked.
function compareItems(a, b) {
  const tier = itemTier(a) - itemTier(b);
  if (tier) return tier;
  if (isAgentItem(a)) return agentRank(a) - agentRank(b) || a.title.localeCompare(b.title);
  if (a.bucket === 'closed') return b.usedAt - a.usedAt;
  return BUCKET_RANK[a.bucket] - BUCKET_RANK[b.bucket];
}

// itemState is the word or two an item's row ends with: what an agent is
// doing when its light cannot say it, or where a resource is when that is not
// plain (a page live out of
// sight, a window on the dock). A closed item says nothing: the Closed chip
// and the Recent heading already do.
export function itemState(item) {
  // An agent's light already says working or idle; only the states it cannot
  // say get words.
  if (isAgentItem(item)) return AGENT_STATE[item.status.key] || '';
  if (item.bucket === 'minimized') return 'docked';
  if (item.bucket === 'background') return item.kind === 'page' ? 'live' : '';
  return '';
}

// AGENT_STATE is what an agent's row says it is doing, in a word or two.
const AGENT_STATE = Object.freeze({ awaiting_permission: 'needs you', awaiting_input: 'needs you', waking: 'starting' });

const KIND_GLYPH = Object.freeze({ page: '◧', board: '▦', process: '⚙', shell: '▭', terminal: '▭' });
export const railGlyph = (kind) => KIND_GLYPH[kind] || '▭';

// The strip pane key a resource opens under, for closing or popping it out.
export function resourcePaneKey(resource) {
  return resource.kind === 'shell' ? `groupterm:${resource.id}` : resourceKey(resource);
}

export const UNGROUPED = Object.freeze({ id: 'other', name: 'Not in a group' });
// OTHER_SESSIONS holds the sessions running here that cannot be attached (a
// superseded generation, a tmux session tclaude did not start, one locked
// against attaching): they belong to no group, and are listed, last, so
// nothing runs unnoticed and each can still be killed.
export const OTHER_SESSIONS = Object.freeze({ id: 'other-sessions', name: 'Other sessions' });

// agentGroupName is the one group an agent is listed under: the group it was
// spawned into while it is still a member, else its first group. '' for
// none, or for a group not listed here.
export function agentGroupName(agent, names) {
  if (!agent) return '';
  const groups = Array.isArray(agent.groups) ? agent.groups : [];
  if (agent.spawn_group && groups.includes(agent.spawn_group) && names.has(agent.spawn_group)) return agent.spawn_group;
  return groups.find((name) => names.has(name)) || '';
}

// railItems builds the sections: one per group with anything to list, then
// Not in a group, ordered with what is in use first. Each item carries its
// group's name ('' for none) and when it was last used here (usedAt, ms).
//   statusOf(item) is a terminal item's terminalTabStatus (its agent and
//   what it is doing), if any; the item keeps it as status.
//   used is a Map of item key → the last time it was opened here.
export function railItems({
  resources = [], groups = [], places = new Map(), panes = [], overlays = [], background = [],
  titleOf = (row) => row.label || row.tmux_session, statusOf = () => null, used = new Map(),
} = {}) {
  const names = new Map(groups.map((g) => [g.id, g.name]));
  const byName = new Map(groups.map((g) => [g.name, g.id]));
  const order = new Map(groups.map((g, index) => [g.id, index]));
  const byGroup = new Map();
  const add = (groupID, item) => {
    if (!byGroup.has(groupID)) byGroup.set(groupID, []);
    byGroup.get(groupID).push(item);
  };
  const stamp = (item) => ({ ...item, usedAt: used.get(item.key) || 0 });

  for (const r of resources) {
    if (!r || !names.has(r.group_id)) continue;
    const view = resourceState(r, places);
    const bucket = bucketOf(view);
    // A group shell that is not running is not the rail's to list.
    if (r.kind === 'shell' && bucket === 'closed') continue;
    // A group's shell is named for its group: "Group shell" twice says nothing.
    const title = r.kind === 'shell' ? `${names.get(r.group_id)} shell` : r.title || r.id;
    add(r.group_id, stamp({
      key: resourceKey(r), kind: r.kind, title, view, bucket, resource: r,
      group: names.get(r.group_id), paneKey: resourcePaneKey(r),
    }));
  }

  // A resource open here that no group lists (a board linked to none, a page
  // shown outside a group, a shell whose group was archived) still shows.
  const listed = new Set([...byGroup.values()].flat().map((item) => item.key));
  for (const pane of panes) {
    const key = stripResourceKey(pane?.key);
    if (!key || listed.has(key)) continue;
    listed.add(key);
    add(UNGROUPED.id, stamp({ key, kind: key.slice(0, key.indexOf(':')), title: pane.label || key, view: 'open-tab', bucket: 'open', group: '', paneKey: pane.key }));
  }
  for (const tab of overlays) {
    const key = overlayResourceKey(tab);
    if (!key || listed.has(key)) continue;
    listed.add(key);
    const view = tab.minimized ? 'minimized' : 'floating';
    add(UNGROUPED.id, stamp({ key, kind: key.slice(0, key.indexOf(':')), title: tab.label || key, view, bucket: bucketOf(view), group: '', overlay: tab }));
  }

  // Terminals go under their agent's group: the strip's agent and session
  // panes, the overlay windows' terminal tabs, then the sessions running with
  // nothing attached here.
  const terminal = (item) => {
    const status = statusOf(item) || null;
    const name = agentGroupName(status?.agent, byName);
    add(name ? byName.get(name) : UNGROUPED.id, stamp({ ...item, kind: 'terminal', group: name, status }));
  };
  for (const pane of panes) {
    if (!pane?.key || stripResourceKey(pane.key) || pane.seed?.page || pane.seed?.board || pane.seed?.process) continue;
    terminal({ key: `pane:${pane.key}`, title: pane.label || pane.key, view: 'open-tab', bucket: 'open', paneKey: pane.key, pane });
  }
  for (const tab of overlays) {
    if (tab?.kind !== 'terminal' || String(tab.paneKey || '').startsWith('groupterm:')) continue;
    const view = tab.minimized ? 'minimized' : 'floating';
    terminal({ key: `overlay:${tab.id}`, title: tab.label || tab.id, view, bucket: bucketOf(view), overlay: tab });
  }
  // A background session counts once: not when an overlay window holds it
  // (a minimized one is detached, so the listing has it too). One that can be
  // attached goes under its agent's group; one that cannot (a superseded
  // generation, a stray tmux session, a locked one: it has no key) goes under
  // Other sessions, where it can be killed. A board's agent sessions ride on
  // the board's own item (its row menu ends them), and a group shell's session
  // on its group's shell item (its row menu kills it); one whose item no
  // group lists is listed under Not in a group.  A peer's rows are never this
  // node's resources: they list as terminals.
  const held = new Set(overlays.map((tab) => tab?.paneKey).filter(Boolean));
  const boardSessions = new Map();
  const shellSessions = new Map();
  for (const row of background) {
    if (!row || held.has(row.key)) continue;
    if (row.kind === 'board' && row.board && !row.node) {
      boardSessions.set(`board:${row.board}`, row);
      continue;
    }
    if (row.kind === 'group_shell' && row.key && !row.node) {
      shellSessions.set(row.key, row);
      continue;
    }
    const key = `bg:${row.node?.id || ''}:${row.tmux_session || row.key}`;
    if (!row.key) {
      add(OTHER_SESSIONS.id, stamp({ key, kind: 'terminal', title: titleOf(row), view: 'background', bucket: 'background', group: '', status: null, row }));
      continue;
    }
    terminal({ key, title: titleOf(row), view: 'background', bucket: 'background', row });
  }
  for (const items of byGroup.values()) {
    for (const [index, item] of items.entries()) {
      const sessions = item.kind === 'board' && boardSessions.get(item.key);
      // The snapshot calls a board closed whatever runs on it: one with
      // sessions is in the background, where the Background box lists it.
      if (sessions) {
        boardSessions.delete(item.key);
        const live = item.bucket === 'closed' ? { view: 'background', bucket: 'background' } : {};
        items[index] = { ...item, ...live, ...(sessions.killable ? { sessions } : {}) };
      }
      const shell = item.kind === 'shell' && shellSessions.get(item.paneKey);
      if (shell) {
        shellSessions.delete(item.paneKey);
        if (shell.killable) items[index] = { ...item, shellSession: shell };
      }
    }
  }
  for (const [key, row] of shellSessions) {
    add(UNGROUPED.id, stamp({ key: `bg::${row.tmux_session || key}`, kind: 'terminal', title: titleOf(row), view: 'background', bucket: 'background', group: '', status: null, row }));
  }
  // A board with sessions that no group lists still shows, so they can be
  // reached and ended.
  for (const [key, row] of boardSessions) {
    add(UNGROUPED.id, stamp({ key, kind: 'board', title: titleOf(row), view: 'background', bucket: 'background', group: '', row,
      sessions: row.killable ? row : undefined }));
  }

  const sections = [...byGroup.entries()].map(([id, items]) => {
    const sorted = items.map((item, index) => ({ item, index }))
      .sort((a, b) => compareItems(a.item, b.item) || a.index - b.index)
      .map(({ item }) => item);
    const name = id === UNGROUPED.id ? UNGROUPED.name : id === OTHER_SESSIONS.id ? OTHER_SESSIONS.name : names.get(id);
    return { id, name, items: sorted };
  });
  return { sections: sortSections(sections, order) };
}

// sortSections puts a group with an agent that needs the operator first, then
// one with something open here, then one with a live agent, then one with
// anything active, then the rest; within each, the snapshot's group order
// (Not in a group last). Recent use does not move a group, so a click never
// shifts the list under the pointer.
function sortSections(sections, order) {
  const live = (i) => i.bucket !== 'closed';
  const tier = (s) => (s.items.some(needsYou) ? 0 : s.items.some((i) => i.bucket === 'open') ? 1
    : s.items.some((i) => live(i) && isAgentItem(i)) ? 2 : s.items.some(live) ? 3 : 4);
  const rank = (s) => (s.id === UNGROUPED.id ? Infinity : order.get(s.id) ?? Infinity);
  // Other sessions belong to no group and come last, whatever they hold.
  const last = (s) => (s.id === OTHER_SESSIONS.id ? 1 : 0);
  return [...sections].sort((a, b) => last(a) - last(b) || tier(a) - tier(b) || rank(a) - rank(b));
}

// matchesQuery is whether an item's title or group holds the query.
export function matchesQuery(item, query) {
  const q = String(query || '').trim().toLowerCase();
  return !q || item.title.toLowerCase().includes(q) || (item.group || '').toLowerCase().includes(q);
}

// filterRail keeps the items the filter lists, dropping a section left empty.
// A search finds everything, closed items included, whatever the filter.
// hidden is how many matching items the filter leaves out.
export function filterRail({ sections }, { filter = DEFAULT_FILTER, query = '' } = {}) {
  const searching = !!String(query || '').trim();
  let hidden = 0;
  const keep = (items) => items.filter((item) => {
    if (!matchesQuery(item, query)) return false;
    if (searching || matchesFilter(item, filter)) return true;
    hidden += 1;
    return false;
  });
  const shown = sections.map((s) => ({ ...s, items: keep(s.items) })).filter((s) => s.items.length);
  return { sections: shown, hidden };
}
