// collab-rail-model.js -- what the Collab rail lists (tcl-lx7nb7): every
// group's resources, by group, then the agents' terminals, each in one of
// four buckets the filter chips name. Pure: the rail (collab-rail.js) feeds
// it the snapshot, this browser's places and its open panes, overlay tabs and
// background sessions.

import { resourceKey, resourceState, stripResourceKey } from './resource-model.js';

export const RAIL_FILTERS = Object.freeze([
  ['all', 'All'], ['open', 'Open'], ['minimized', 'Minimized'], ['background', 'Background'], ['closed', 'Closed'],
]);

// bucketOf is the chip a state counts under: open here (a tab or a window),
// on the dock, running out of sight, or not live.
export function bucketOf(view) {
  if (view === 'open-tab' || view === 'floating') return 'open';
  if (view === 'minimized') return 'minimized';
  if (view === 'running' || view === 'background') return 'background';
  return 'closed';
}

// The rail's mark at a row's end, per bucket.
export const RAIL_MARK = Object.freeze({ open: '●', minimized: '–', background: 'bg', closed: 'closed' });

const KIND_GLYPH = Object.freeze({ page: '◧', board: '▦', process: '⚙', shell: '▭', terminal: '▭' });
export const railGlyph = (kind) => KIND_GLYPH[kind] || '▭';

// The strip pane key a resource opens under, for closing or popping it out.
export function resourcePaneKey(resource) {
  return resource.kind === 'shell' ? `groupterm:${resource.id}` : resourceKey(resource);
}

// railItems builds the rail's sections: one per group that has a resource
// (in the snapshot's group order), then the agents' terminals.
export function railItems({ resources = [], groups = [], places = new Map(), panes = [], overlays = [], background = [], titleOf = (row) => row.label || row.tmux_session } = {}) {
  const names = new Map(groups.map((g) => [g.id, g.name]));
  const order = new Map(groups.map((g, index) => [g.id, index]));
  const byGroup = new Map();
  for (const r of resources) {
    if (!r || !names.has(r.group_id)) continue;
    const view = resourceState(r, places);
    const item = {
      key: resourceKey(r), kind: r.kind, title: r.title || r.id, view, bucket: bucketOf(view), resource: r,
      group: names.get(r.group_id), paneKey: resourcePaneKey(r),
    };
    if (!byGroup.has(r.group_id)) byGroup.set(r.group_id, []);
    byGroup.get(r.group_id).push(item);
  }
  const sections = [...byGroup.entries()]
    .sort(([a], [b]) => order.get(a) - order.get(b))
    .map(([id, items]) => ({ id, name: names.get(id), items }));

  // Terminals: the strip's agent and session panes, the overlay windows'
  // terminal tabs, then the sessions running with nothing attached here.
  const terminals = [];
  for (const pane of panes) {
    if (!pane?.key || stripResourceKey(pane.key) || pane.seed?.page || pane.seed?.board || pane.seed?.process) continue;
    terminals.push({ key: `pane:${pane.key}`, kind: 'terminal', title: pane.label || pane.key, view: 'open-tab', bucket: 'open', paneKey: pane.key });
  }
  for (const tab of overlays) {
    if (tab?.kind !== 'terminal' || String(tab.paneKey || '').startsWith('groupterm:')) continue;
    const view = tab.minimized ? 'minimized' : 'floating';
    terminals.push({ key: `overlay:${tab.id}`, kind: 'terminal', title: tab.label || tab.id, view, bucket: bucketOf(view), overlay: tab });
  }
  for (const row of background) {
    if (!row || row.kind === 'board' || row.kind === 'group_shell') continue;
    terminals.push({ key: `bg:${row.node?.id || ''}:${row.tmux_session || row.key}`, kind: 'terminal', title: titleOf(row), view: 'background', bucket: 'background', row });
  }
  return { sections, terminals };
}

// filterRail keeps the items in the chosen bucket whose title or group holds
// the query, dropping a section left empty. counts are before the bucket
// filter, after the search.
export function filterRail({ sections, terminals }, { bucket = 'all', query = '' } = {}) {
  const q = query.trim().toLowerCase();
  const matches = (item) => !q || item.title.toLowerCase().includes(q) || (item.group || '').toLowerCase().includes(q);
  const counts = { all: 0, open: 0, minimized: 0, background: 0, closed: 0 };
  const keep = (items) => items.filter((item) => {
    if (!matches(item)) return false;
    counts.all += 1;
    counts[item.bucket] += 1;
    return bucket === 'all' || item.bucket === bucket;
  });
  const shown = sections.map((s) => ({ ...s, items: keep(s.items) })).filter((s) => s.items.length);
  return { sections: shown, terminals: keep(terminals), counts };
}
