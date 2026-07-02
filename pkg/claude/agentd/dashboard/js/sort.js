// sort.js — clickable column sorting for the dashboard tables.
//
// The active sort ({col, dir} per table) lives here, persisted to
// localStorage; cycleSort advances it through asc/desc/off, sortHead
// renders the <thead>, applySort orders rows. Also holds the per-table
// column specs and value accessors. Extracted from dashboard.js as
// part of the Stage 2 module split.

import { esc } from './helpers.js';
import { dashPrefs } from './prefs.js';

// --- column sorting --------------------------------------------------
// Every primary table (group members, cron, sudo, links) has
// clickable headers. The active sort — a {col, dir} pair keyed by a
// stable table name — lives in sortState and is mirrored to the
// server-backed dashPrefs store so it survives reloads, the 2s
// auto-refresh, and daemon restarts (the random per-start port made
// the old localStorage copy reset every restart). Clicking a header
// cycles asc → desc → unsorted; the third click drops back to the
// server's own ordering.
const SORT_LS_KEY = 'tclaude.dash.sort';
let sortState = {};

// loadSortState (re)seeds sortState from dashPrefs. dashPrefs reads are
// only valid after initDashPrefs() has populated its cache, so this is
// called from the boot IIFE rather than at import time — the cache is
// empty during module evaluation.
function loadSortState() {
  try { sortState = JSON.parse(dashPrefs.getItem(SORT_LS_KEY)) || {}; }
  catch (_) { sortState = {}; }
}

// cycleSort advances one table's sort through the three-state cycle
// and persists the result.
function cycleSort(tableKey, col) {
  const cur = sortState[tableKey];
  if (!cur || cur.col !== col) {
    sortState[tableKey] = { col, dir: 'asc' };
  } else if (cur.dir === 'asc') {
    sortState[tableKey] = { col, dir: 'desc' };
  } else {
    delete sortState[tableKey];
  }
  try { dashPrefs.setItem(SORT_LS_KEY, JSON.stringify(sortState)); }
  catch (_) { /* write-through is best-effort — sort still works in-memory */ }
}

// sortHead builds a table's <thead> from a column spec. Each spec
// entry is {label, col}; entries without a `col` (the online-dot and
// row-action columns) render as plain, non-clickable headers. The
// active column shows a solid ▲/▼; the rest carry a faint arrow that
// surfaces on hover, hinting they're clickable.
function sortHead(tableKey, cols) {
  const active = sortState[tableKey];
  const cells = cols.map(c => {
    if (!c.col) return `<th>${esc(c.label || '')}</th>`;
    const on = active && active.col === c.col;
    const arrow = on ? (active.dir === 'asc' ? '▲' : '▼') : '▾';
    const cls = on ? 'sortable sort-active' : 'sortable';
    return `<th class="${cls}" data-sort-table="${esc(tableKey)}" `
         + `data-sort-col="${esc(c.col)}" title="Sort by ${esc(c.label)}">`
         + `${esc(c.label)}<span class="sort-arrow">${arrow}</span></th>`;
  });
  return `<thead><tr>${cells.join('')}</tr></thead>`;
}

// cmpSortValues orders two non-empty accessor outputs: booleans and
// numbers compare naturally, everything else as case-insensitive
// strings (ISO timestamps included — lexical order is chronological).
function cmpSortValues(a, b) {
  if (typeof a === 'boolean' || typeof b === 'boolean') {
    return (a === b) ? 0 : (a ? -1 : 1);
  }
  if (typeof a === 'number' && typeof b === 'number') {
    return a - b;
  }
  return String(a).toLowerCase().localeCompare(String(b).toLowerCase());
}

// applySort returns a sorted copy of `rows` for the given table.
// With no active sort (or an accessor the table doesn't define) the
// original array is handed back untouched, preserving server order.
// Blank/nullish cells always sort last, whichever the direction, so
// empty values never crowd the top.
function applySort(tableKey, rows, accessors) {
  const st = sortState[tableKey];
  if (!st || !accessors || !accessors[st.col]) return rows;
  const get = accessors[st.col];
  const sign = st.dir === 'desc' ? -1 : 1;
  return rows.slice().sort((ra, rb) => {
    const a = get(ra), b = get(rb);
    const ae = (a == null || a === ''), be = (b == null || b === '');
    if (ae || be) return ae === be ? 0 : (ae ? 1 : -1);
    return sign * cmpSortValues(a, b);
  });
}

// Column specs + value accessors for each sortable table. The `col`
// strings are opaque keys shared between the header (sortHead) and
// the sorter (applySort); they need not match the data field name.
// MEMBER_COLS [0] is the combined controls column — the status dot
// plus the per-row action cluster (focus/hide + ⚙ cog) share one
// label-less, non-sortable leading cell, so all of an agent's
// controls sit together at the left of the row.
const MEMBER_COLS = [
  { label: '' },
  { label: 'ID', col: 'id' },
  { label: 'Name', col: 'title' },
  { label: 'State', col: 'state' },
  { label: 'Last', col: 'last' },
  { label: 'Age', col: 'age' },
  { label: 'CWD', col: 'cwd' },
  { label: 'Branch', col: 'branch' },
  { label: 'Role', col: 'role' },
  { label: 'Description', col: 'descr' },
];
const MEMBER_ACCESSORS = {
  // id sorts on the stable agent_id the column now displays (conv-id fallback).
  id:     m => m.agent_id || m.conv_id,
  title:  m => m.title,
  state:  m => (m.state || {}).status,
  last:   m => (m.state || {}).last_hook,
  // age sorts on the raw creation timestamp (ISO → lexical = chrono):
  // ascending = oldest first, descending = newest first. The default
  // listing already arrives newest-first from the server, which this
  // column surfaces.
  age:    m => m.created_at,
  cwd:    m => m.current_dir || (m.state || {}).cwd,
  branch: m => m.branch,
  role:   m => m.role,
  descr:  m => m.descr,
};

// The Jobs tab's unified job table (tabs.js renderJobsTab) — rows are
// {kind, export?, cron?} from /api/jobs, so every accessor branches on the
// kind. Pagination + the text filter are server-side; this sort orders the
// SERVED WINDOW only, like the retired/conversations/replaced sub-tables.
// The default server order (newest activity first) is what the third header
// click falls back to. The leading state-dot and trailing action columns
// stay non-sortable.
const JOBS_COLS = [
  { label: '' },
  { label: 'Kind', col: 'kind' },
  { label: 'ID', col: 'id' },
  { label: 'Name', col: 'name' },
  { label: 'Agent', col: 'agent' },
  { label: 'Status', col: 'status' },
  { label: 'When', col: 'when' },
  { label: 'Info', col: 'info' },
  { label: '' },
];
const JOBS_ACCESSORS = {
  kind: r => r.kind,
  id:   r => (r.cron ? r.cron.id : r.export?.id),
  name: r => (r.cron ? r.cron.name : (r.export?.title || '')),
  agent: r => r.cron
    ? (r.cron.group_name || r.cron.target_label || r.cron.target_agent || r.cron.target_conv)
    : (r.export?.conv_label || r.export?.conv_id),
  // status groups by lifecycle word: cron enabled/disabled, export
  // cloning/requested/running/ready/failed.
  status: r => (r.cron ? (r.cron.enabled ? 'enabled' : 'disabled') : r.export?.status),
  // when sorts on the raw ISO stamp (lexical ≈ chronological): cron = last
  // run, export = started. Export stamps are RFC3339Nano, whose trimmed
  // trailing zeros can misorder within the same second — fine for a window
  // display sort (never rely on this ordering server-side).
  when: r => (r.cron ? r.cron.last_run_at : r.export?.created_at),
  // info is numeric per kind — cron interval seconds vs export artifact
  // bytes. Comparing across kinds is meaningless but stable; within a kind
  // (or with a kind filter) it's the natural magnitude sort.
  info: r => (r.cron ? r.cron.interval_seconds : r.export?.artifact_size),
};

const SUDO_COLS = [
  { label: 'Agent', col: 'conv' },
  { label: 'Slug', col: 'slug' },
  { label: 'Granted at', col: 'granted' },
  { label: 'Expires in', col: 'expires' },
  { label: 'Reason', col: 'reason' },
  { label: 'Granted by', col: 'by' },
  { label: '' },
];
const SUDO_ACCESSORS = {
  conv:    r => r.conv_title || r.agent_id || r.conv_id,
  slug:    r => r.slug,
  granted: r => r.granted_at,
  expires: r => r.remaining_seconds,
  reason:  r => r.reason,
  by:      r => r.granted_by,
};

const LINK_COLS = [
  { label: 'ID', col: 'id' },
  { label: 'From', col: 'from' },
  { label: '' },
  { label: 'To', col: 'to' },
  { label: 'Mode', col: 'mode' },
  { label: 'Created', col: 'created' },
  { label: '' },
];
const LINK_ACCESSORS = {
  id:      l => l.id,
  from:    l => l.from,
  to:      l => l.to,
  mode:    l => l.mode,
  created: l => l.created_at,
};

// The virtual "Replaced generations" sub-table (render.js). Lowercase
// labels match its existing archival style; the leading online-dot and
// trailing action columns stay non-sortable. The default server order is
// already newest-replaced-first (collectReplacedGenerationsSnapshot), so
// dropping the sort (third header click) falls back to that.
const REPLACED_COLS = [
  { label: '' },
  { label: 'conv', col: 'conv' },
  { label: 'title', col: 'title' },
  { label: 'of agent', col: 'actor' },
  { label: 'replaced', col: 'replaced' },
  { label: '' },
];
const REPLACED_ACCESSORS = {
  conv:     a => a.conv_id,
  title:    a => a.title,
  actor:    a => a.actor_title || a.actor_conv_id,
  // replaced sorts on the raw ISO timestamp (lexical = chronological):
  // ascending = oldest first, descending = newest first.
  replaced: a => a.replaced_at,
};

// The virtual "Retired" sub-table (render.js). Like its siblings the
// leading online-dot and trailing action columns stay non-sortable. The
// id column now leads with the retired actor's stable agent_id (conv-id
// fallback), so it sorts on that — the same key the column displays. The
// default server order is newest-retired-first (collectRetiredSnapshot),
// which dropping the sort (third header click) falls back to.
const RETIRED_COLS = [
  { label: '' },
  { label: 'id', col: 'id' },
  { label: 'title', col: 'title' },
  { label: 'retired', col: 'retired' },
  { label: 'by', col: 'by' },
  { label: 'reason', col: 'reason' },
  { label: '' },
];
const RETIRED_ACCESSORS = {
  // id sorts on the stable agent_id the column now displays (conv-id fallback).
  id:      a => a.agent_id || a.conv_id,
  title:   a => a.title,
  // retired sorts on the raw ISO timestamp (lexical = chronological):
  // ascending = oldest first, descending = newest first.
  retired: a => a.retired_at,
  by:      a => a.retired_by_display || a.retired_by,
  reason:  a => a.retire_reason,
};

// The virtual "Conversations" sub-table (render.js) — recent non-agent
// conversations. These rows are plain conversations, NOT agents, so they
// carry no stable agent_id: the id column stays a conv-id and sorts on it.
// Leading online-dot and trailing promote-action columns are non-sortable.
const CONVERSATIONS_COLS = [
  { label: '' },
  { label: 'conv', col: 'conv' },
  { label: 'title', col: 'title' },
  { label: 'last activity', col: 'last' },
  { label: '' },
];
const CONVERSATIONS_ACCESSORS = {
  conv:  c => c.conv_id,
  title: c => c.title,
  // last sorts on the raw ISO modified stamp (lexical = chronological).
  last:  c => c.modified,
};

// The virtual "Pending" sub-table (render.js) — dashboard spawns still
// behind a startup gate. A pending spawn has no conv-id yet (it is keyed
// on its label), so the id column stays the label. Leading online-dot and
// trailing focus-action columns are non-sortable. The default server order
// is newest-pending-first, which dropping the sort falls back to.
const PENDING_COLS = [
  { label: '' },
  { label: 'label', col: 'label' },
  { label: 'name', col: 'name' },
  { label: 'group', col: 'group' },
  { label: 'dir', col: 'dir' },
  { label: 'age', col: 'age' },
  { label: '' },
];
const PENDING_ACCESSORS = {
  label: p => p.label,
  name:  p => p.name || p.role,
  group: p => p.group,
  dir:   p => p.cwd,
  // age sorts on the raw ISO spawn time (lexical = chronological):
  // ascending = oldest first, descending = newest first.
  age:   p => p.created_at,
};

export {
  cycleSort, sortHead, applySort, loadSortState,
  MEMBER_COLS, MEMBER_ACCESSORS, JOBS_COLS, JOBS_ACCESSORS,
  SUDO_COLS, SUDO_ACCESSORS, LINK_COLS, LINK_ACCESSORS,
  REPLACED_COLS, REPLACED_ACCESSORS,
  RETIRED_COLS, RETIRED_ACCESSORS,
  CONVERSATIONS_COLS, CONVERSATIONS_ACCESSORS,
  PENDING_COLS, PENDING_ACCESSORS,
};
