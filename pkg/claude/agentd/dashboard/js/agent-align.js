// agent-align.js -- lining a group's other tables up under its agents
// table: a Resources table (groups-resources.js) and the Task board's list
// (groups-taskboard.js) take the agents table's Name, State, Last and Age
// column widths, so those columns run straight down the group's card.

import { useEffect, useLayoutEffect, useRef } from 'preact/hooks';

// ALIGNED_COLS are the agents table's headers (data-sort-col) a Resources
// table lines its own up under, in order, each with this table's width
// variable: the controls take whatever lies left of Name.
const ALIGNED_COLS = Object.freeze([
  { col: 'title', key: 'name', min: 60 },
  { col: 'state', key: 'state', min: 40 },
  { col: 'last', key: 'last', min: 30 },
  { col: 'age', key: 'age', min: 30 },
]);

// agentColumns finds, for a Resources table, where the agents table above it
// in the same group starts its Name, State, Last and Age columns, as widths
// for this table's controls, Name, State, Last and Age columns ({ ctl, name,
// state, last, age }, 0 for one not to set); null when there is no agents
// table to line up with (folded, or none). A column runs to where the next
// of these starts, so one the agents table shows between them (Process) is
// taken in and the next still lines up; without that next one (hidden), it
// is its header's own width.
export function agentColumns(table) {
  const section = table?.closest?.('[data-fused-section]');
  // The members table, by its Name header: a pending-spawns table may come
  // first in the section.
  const name = section?.parentElement?.querySelector?.(':scope > [data-fused-section="agents"] thead th[data-sort-col="title"]');
  const head = name?.closest('thead');
  if (!head) return null;
  const left = table.getBoundingClientRect().left;
  const nameLeft = name?.getBoundingClientRect().left;
  if (!name || !(nameLeft > left)) return null;
  const out = { ctl: Math.round(nameLeft - left) };
  const rects = ALIGNED_COLS.map(({ col }) => (col === 'title' ? name : head.querySelector(`th[data-sort-col="${col}"]`))?.getBoundingClientRect() || null);
  ALIGNED_COLS.forEach(({ key, min }, i) => {
    const own = rects[i];
    const next = rects[i + 1];
    // Name has no width of its own to fall back to: the table's rest takes it.
    const width = !own ? 0 : next ? Math.round(next.left - own.left) : key === 'name' ? 0 : Math.round(own.width);
    out[key] = width > min ? width : 0;
  });
  return out;
}

// agentHeads are the elements whose size moves the agents table's columns:
// the section, and its members table's header cells (a column can widen as
// its rows fill in while the section keeps its size).
function agentHeads(table) {
  const agents = table?.closest?.('[data-fused-section]')?.parentElement?.querySelector?.(':scope > [data-fused-section="agents"]');
  if (!agents) return [];
  const head = agents.querySelector('thead th[data-sort-col="title"]')?.closest('thead');
  return [agents, ...(head ? head.querySelectorAll('th') : [])];
}

// useAgentColumns calls apply(table, agentColumns(table)) on every render and
// whenever the agents table above changes its columns, for the table to set
// its column widths from. The table may come and go (a board's list is drawn
// only once it has tasks), so the watch starts once it is there, and follows
// the agents table's header as it is redrawn.
export function useAgentColumns(ref, apply) {
  const watch = useRef({ observer: null, watched: [] });
  const latest = useRef(apply);
  latest.current = apply;
  const run = () => {
    const table = ref.current;
    if (table) latest.current(table, agentColumns(table));
  };
  useLayoutEffect(() => {
    run();
    const heads = agentHeads(ref.current);
    const state = watch.current;
    if (heads.length === state.watched.length && heads.every((el, i) => el === state.watched[i])) return;
    state.observer?.disconnect();
    state.observer = null;
    state.watched = heads;
    if (!heads.length || typeof ResizeObserver !== 'function') return;
    state.observer = new ResizeObserver(() => run());
    for (const el of heads) state.observer.observe(el);
  });
  useEffect(() => () => watch.current.observer?.disconnect(), []);
}
