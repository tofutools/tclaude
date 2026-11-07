// groups-taskboard-lanes.js -- the Task board's swim lanes (docs/taskboard.md):
// the stage columns Ideas → Next → In progress → Blocked · needs you → Done
// this week, one lane per agent (grouped by role, by subgroup, or not at
// all) under an Unassigned lane. Every drag is one route, the one its CLI
// verb uses:
//
//   card → another column, same lane   promote · demote · block --reason ·
//                                      unblock · done · reopen
//   card → an agent's lane             assign <task> --to <agent>
//   card → the Unassigned lane         unassign <task>
//   card → another lane AND column     both, in that order: assign then block
//                                      --reason; unassign then demote or block
//
// Cells that cannot hold a card are striped: Ideas in an agent's lane (an
// idea is unassigned), In progress in the Unassigned lane. A cell a new task
// can land in has a small "+" in its lower-right corner that opens the New
// task form filled from the cell (add [--next], then assign --to for an
// agent's lane). Column widths and the order of the lanes and their headings
// are this viewer's own (groups-taskboard-lanes-layout.js).

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';
import { Button, Input } from './ui-components.js';
import {
  LAYOUT_COLUMNS, applyLaneOrder, gridColumns, laneOrder, laneWidths, moveHead, moveLane, resizeColumns, setLaneWidths,
} from './groups-taskboard-lanes-layout.js';
import { markLanded } from './drop-landing.js';
import { DepChips, RefChip, assignableMembers, canAssign } from './groups-taskboard.js';

const html = htm.bind(h);

export const LANE_COLUMNS = [
  { key: 'ideas', label: 'Ideas', wizard: 'Visions' },
  { key: 'next', label: 'Next', wizard: 'Next' },
  { key: 'progress', label: 'In progress', wizard: 'Underway' },
  { key: 'blocked', label: 'Blocked · needs you', wizard: 'Blocked · needs you' },
  { key: 'done', label: 'Done this week', wizard: 'Fulfilled this week' },
];

const WEEK_MS = 7 * 24 * 3600 * 1000;
// DONE_SHOWN is how many done cards a cell draws before "+ n more".
const DONE_SHOWN = 3;
export const UNASSIGNED = '';
const TASK_DRAG_TYPE = 'application/x-tclaude-task';

// laneColumn is the column a task stands in, derived as the design's table
// says; null for a task the lanes leave out (dropped, done before this week).
export function laneColumn(task, now = Date.now()) {
  if (task.status === 'dropped') return null;
  if (task.status === 'done') {
    const closed = Date.parse(task.closed_at || '');
    return Number.isFinite(closed) && now - closed <= WEEK_MS ? 'done' : null;
  }
  const assigned = !!task.assignee?.agent_id;
  if (task.status === 'blocked' || (assigned && task.stalled)) return 'blocked';
  if (assigned) return 'progress';
  return task.stage === 'idea' ? 'ideas' : 'next';
}

// striped reports the cells that cannot hold a card.
export function striped(lane, column) {
  return lane === UNASSIGNED ? column === 'progress' : column === 'ideas';
}

function memberState(member) {
  if (!member) return 'not a member';
  if (!member.online) return 'offline';
  return member.state?.status || 'idle';
}

// laneModel lays a board out: the cells' tasks per lane and column, and the
// agent lanes in their lane groups. `by` is agent | role | subgroup.
export function laneModel(entry, group, by = 'agent', snapshot = null, now = Date.now()) {
  const cells = new Map();
  const put = (lane, column, task) => {
    const key = `${lane}|${column}`;
    if (!cells.has(key)) cells.set(key, []);
    cells.get(key).push(task);
  };
  const holds = new Map();
  const closed = [...(entry?.closed || [])].sort((a, b) => String(b.closed_at || '').localeCompare(String(a.closed_at || '')));
  for (const task of [...(entry?.tasks || []), ...closed]) {
    const column = laneColumn(task, now);
    if (!column) continue;
    const lane = task.assignee?.agent_id || UNASSIGNED;
    put(lane, column, task);
    if (lane && column !== 'done') holds.set(lane, (holds.get(lane) || 0) + 1);
  }
  const members = assignableMembers(group);
  const lanes = members.map((member) => ({ id: member.agent_id, member, name: member.title || member.agent_id }));
  // An assignee who is no longer a member keeps a lane while it holds tasks.
  const known = new Set(lanes.map((lane) => lane.id));
  for (const key of cells.keys()) {
    const id = key.slice(0, key.indexOf('|'));
    if (id && !known.has(id)) {
      known.add(id);
      const task = [...cells.entries()].find(([k]) => k.startsWith(`${id}|`))[1][0];
      lanes.push({ id, member: null, name: task.assignee?.name || id.slice(0, 12) });
    }
  }
  for (const lane of lanes) {
    lane.state = memberState(lane.member);
    lane.holds = holds.get(lane.id) || 0;
  }
  return { cells, groups: laneGroups(lanes, group, by, snapshot) };
}

function laneGroups(lanes, group, by, snapshot) {
  const groups = [];
  const into = (key, label) => {
    let found = groups.find((g) => g.key === key);
    if (!found) { found = { key, label, lanes: [] }; groups.push(found); }
    return found;
  };
  // An assignee who left the group gets a lane group of its own, which never
  // folds: what it holds (blocked or stalled too) stays in sight.
  const former = lanes.filter((lane) => !lane.member);
  lanes = lanes.filter((lane) => lane.member);
  if (by === 'role') {
    for (const lane of lanes) {
      const role = lane.member.role || '';
      into(`role:${role}`, role || 'no role').lanes.push(lane);
    }
  } else if (by === 'subgroup') {
    // A subgroup's members fold under the subgroup; the rest stay under the
    // group itself.
    const children = (snapshot?.groups || []).filter((g) => g.parent === group?.name);
    for (const lane of lanes) {
      const child = children.find((g) => (g.members || []).some((m) => m.agent_id === lane.id));
      if (child) into(`sub:${child.name}`, `↳ ${child.name}`).lanes.push(lane);
      else into('sub:', group?.name || '').lanes.push(lane);
    }
    groups.sort((a, b) => Number(a.key !== 'sub:') - Number(b.key !== 'sub:'));
  } else {
    for (const lane of lanes) into(lane.state === 'offline' ? 'offline' : 'agents', lane.state === 'offline' ? 'offline' : '').lanes.push(lane);
    groups.sort((a, b) => Number(a.key === 'offline') - Number(b.key === 'offline'));
  }
  for (const g of groups) g.offline = g.lanes.length > 0 && g.lanes.every((lane) => lane.state === 'offline');
  let out = groups;
  if (by === 'role') {
    // The owner's role leads, offline roles trail; otherwise as first seen.
    const rank = (g) => (g.offline ? 2 : g.lanes.some((lane) => lane.member?.owner) ? 0 : 1);
    out = groups.map((g, i) => [g, i]).sort(([a, i], [b, j]) => rank(a) - rank(b) || i - j).map(([g]) => g);
  }
  if (former.length) out.push({ key: 'former', label: 'no longer members', lanes: former, offline: false });
  return out;
}

// laneDrop says what dropping `task` from its cell onto another does: the
// write and its words, or null when the cell does not take it. `reason`
// asks a block reason before it runs. A move to another lane AND another
// column is the two writes in a row: the lane first (assign, unassign),
// then the column (block, demote).
export function laneDrop(task, from, to, { assign = true } = {}) {
  if (!task || (from.lane === to.lane && from.column === to.column) || striped(to.lane, to.column)) return null;
  const open = task.status === 'open';
  const block = (api, reason) => api.status(task.id, 'blocked', reason);
  if (from.lane !== to.lane) {
    if (!assign) return null;
    if (to.lane === UNASSIGNED) {
      if (!task.assignee?.agent_id || task.status === 'done') return null;
      const unassign = (api) => api.unassign(task.id);
      if (to.column === 'next' || to.column === from.column) return { kind: 'unassign', label: 'unassign', run: unassign };
      if (to.column === 'ideas' && open) {
        return { kind: 'unassign+demote', label: 'unassign · demote to ideas',
          run: async (api) => { await unassign(api); return api.stage(task.id, 'idea'); } };
      }
      if (to.column === 'blocked' && open) {
        return { kind: 'unassign+block', label: 'unassign · block (asks why)', reason: true,
          run: async (api, reason) => { await unassign(api); return block(api, reason); } };
      }
      return null;
    }
    // Only a member can be assigned (a former member's lane holds what it
    // had), and only an open task.
    if (!open || to.member === false) return null;
    const name = to.name || to.lane;
    const give = (api) => api.assign(task.id, to.lane);
    if (to.column === 'progress' || to.column === 'next') return { kind: 'assign', label: `assign to ${name}`, agent: to.lane, run: give };
    if (to.column === 'blocked') {
      return { kind: 'assign+block', label: `assign to ${name} · block (asks why)`, agent: to.lane, reason: true,
        run: async (api, reason) => { await give(api); return block(api, reason); } };
    }
    return null;
  }
  switch (to.column) {
    case 'ideas':
      if (!open || task.stage === 'idea') return null;
      return { kind: 'demote', label: 'demote to ideas', run: (api) => api.stage(task.id, 'idea') };
    case 'next':
    case 'progress':
      if (task.status === 'blocked') return { kind: 'unblock', label: 'unblock', run: (api) => api.status(task.id, 'open') };
      if (task.status === 'done') return { kind: 'reopen', label: 'reopen', run: (api) => api.status(task.id, 'open') };
      if (to.column === 'next' && open && task.stage === 'idea') return { kind: 'promote', label: 'promote to next', run: (api) => api.stage(task.id, 'next') };
      return null;
    case 'blocked':
      if (!open) return null;
      return { kind: 'block', label: 'block (asks why)', reason: true, run: block };
    case 'done':
      // An assigned task is closed by its assignee or an assigner.
      if ((!open && task.status !== 'blocked') || (task.assignee?.agent_id && !assign)) return null;
      return { kind: 'done', label: 'done', run: (api) => api.status(task.id, 'done') };
    default:
      return null;
  }
}

// cellAdd is what the "+" in a cell pre-fills, or null where a new task
// cannot land: Ideas and Next set the stage; an agent's lane (Next or In
// progress) sets the assignee, only for a viewer who may assign. Blocked and
// Done take no new task, and striped cells none at all.
export function cellAdd(lane, column, { assign = true, member = true } = {}) {
  if (striped(lane, column)) return null;
  if (lane === UNASSIGNED) {
    return column === 'ideas' || column === 'next' ? { stage: column === 'ideas' ? 'idea' : 'next', to: '' } : null;
  }
  if (!assign || !member || (column !== 'next' && column !== 'progress')) return null;
  return { stage: 'next', to: lane };
}

function cardHint(task) {
  if (task.status === 'blocked') return html`<span class="ui-tag ui-tag-warn tb-card-why">${task.block_reason || 'blocked'}</span>`;
  if (task.stalled && task.assignee?.agent_id) {
    const idle = relTime(task.idle_since).replace(/ ago$/, '');
    return html`<span class="ui-tag ui-tag-warn">${idle ? `idle ${idle}, no report` : 'stalled'}</span>`;
  }
  if (task.assignee?.agent_id && task.status === 'open' && task.assigned_at) {
    return html`<span class="ui-tag">${relTime(task.assigned_at).replace(/ ago$/, '')}</span>`;
  }
  return null;
}

function Card({ task, n, numbers, drag }) {
  const done = task.status === 'done';
  return html`<div class=${`ui-card tb-card${done ? ' tb-card-done' : ''}${task.stage === 'idea' && task.status === 'open' ? ' tb-card-idea' : ''}${drag?.dragging === task.id ? ' tb-dragging' : ''}`}
      data-task-id=${task.id} draggable=${drag ? 'true' : undefined} title=${task.id}
      onDragStart=${drag ? (event) => drag.start(event, task) : undefined} onDragEnd=${drag ? drag.end : undefined}>
    <div class="tb-card-title">${n ? html`<span class="tb-card-n">${n}</span>` : null}${task.title}</div>
    ${!done && html`<div class="tb-card-meta">${task.ref && html`<${RefChip} task=${task} />`}<${DepChips} task=${task} numbers=${numbers} />${cardHint(task)}</div>`}
  </div>`;
}

function useLaneDrag(actions, group) {
  const dragged = useRef(null);
  const [dragging, setDragging] = useState('');
  const [over, setOver] = useState(null);
  const [blocking, setBlocking] = useState(null);
  const assign = canAssign(actions);
  const clear = () => { dragged.current = null; setDragging(''); setOver(null); };
  const hit = (event, to) => {
    // Only this board's task drag drives the cells: a text, link or file
    // drag after a missed dragend (the card re-rendered away mid-drag) must
    // not run the stale task's action.
    const types = event.dataTransfer?.types;
    if (types && !Array.from(types).includes(TASK_DRAG_TYPE)) return null;
    const from = dragged.current;
    const drop = from && laneDrop(from.task, from, to, { assign });
    if (!drop) return null;
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    return drop;
  };
  return {
    dragging, over, blocking, setBlocking,
    start(event, task, from) {
      event.stopPropagation();
      dragged.current = { task, ...from };
      setDragging(task.id);
      event.dataTransfer?.setData(TASK_DRAG_TYPE, task.id);
      if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
    },
    end: clear,
    overCell(event, to) {
      const drop = hit(event, to);
      const key = drop ? `${to.lane}|${to.column}` : '';
      if ((over?.key || '') !== key) setOver(drop ? { key, label: drop.label } : null);
    },
    leaveGrid(event) {
      if (dragged.current && !event.currentTarget.contains(event.relatedTarget)) setOver(null);
    },
    async dropCell(event, to) {
      const drop = hit(event, to);
      if (!drop) return;
      const { task } = dragged.current;
      clear();
      if (drop.reason) { setBlocking({ task, drop }); return; }
      const ok = await actions.run(`${task.id}:lane`, drop.label, (api) => drop.run(api));
      if (ok && drop.kind === 'assign') markLanded({ group: group.name, node: group.fleet_node?.id || '', section: 'agents', ids: [drop.agent] });
    },
  };
}

// BlockForm asks the reason a drop onto Blocked needs, then runs the drop
// (with an assign or unassign first, for a move across lanes too).
function BlockForm({ task, drop, numbers, actions, group, onClose }) {
  const [reason, setReason] = useState('');
  const submit = async () => {
    if (!reason.trim() || actions.busy) return;
    const ok = await actions.run(`${task.id}:lane`, 'Block', (api) => drop.run(api, reason.trim()));
    if (ok && drop.agent) markLanded({ group: group.name, node: group.fleet_node?.id || '', section: 'agents', ids: [drop.agent] });
    if (ok) onClose();
  };
  const n = numbers.get(task.id);
  return html`<div class="tb-form tb-lanes-block" data-task-form="block">
    <span class="ui-hint">${drop.kind === 'block' ? 'Block' : drop.label.replace(/ \(asks why\)$/, '')} ${n ? `#${n}` : task.id} “${task.title}”:</span>
    <${Input} size="sm" class="tb-form-reason" aria-label="Block reason" autoFocus placeholder="why is it blocked? (required)"
      value=${reason} maxLength=${400} onInput=${(event) => setReason(event.currentTarget.value)}
      onKeyDown=${(event) => {
        if (event.key === 'Enter') { event.preventDefault(); void submit(); }
        if (event.key === 'Escape') { event.preventDefault(); onClose(); }
      }} />
    <${Button} size="sm" variant="primary" busy=${actions.busy === `${task.id}:lane`} disabled=${!reason.trim()} onClick=${() => void submit()}>Block<//>
    <${Button} size="sm" variant="ghost" onClick=${onClose}>Cancel<//>
  </div>`;
}

function LaneHead({ lane, order }) {
  if (lane.id === UNASSIGNED) {
    return html`<div class="tb-lane-head"><span class="tb-lane-name">Unassigned</span><span class="tb-lane-sub">${lane.count} task${lane.count === 1 ? '' : 's'} · operator order</span></div>`;
  }
  const marker = order?.over?.kind === 'lane' && order.over.key === lane.id ? (order.over.after ? ' tb-order-after' : ' tb-order-before') : '';
  return html`<div class=${`tb-lane-head${marker}`} data-lane-agent=${lane.id} ...${order ? order.laneProps(lane) : {}}>
    <span class="tb-lane-name" title=${lane.name}><span class="tb-avatar" aria-hidden="true">${lane.name.slice(0, 1).toUpperCase()}</span><span class="tb-lane-label">${lane.name}</span></span>
    <span class=${`tb-lane-sub tb-lane-state-${lane.state.replace(/\s+/g, '-')}`}>${lane.state}${lane.member?.owner ? ' · owner' : ''}</span>
    <span class="tb-lane-sub">holds ${lane.holds}</span>
  </div>`;
}

const LANE_DRAG_TYPE = 'application/x-tclaude-lane';

// useLaneOrder drags a lane head onto another lane head in its heading, or a
// heading onto another heading, to reorder them (a view preference, stored
// per group for this viewer). Alt+↑/↓ on a focused head does the same.
// Unassigned stays first.
function useLaneOrder(group, groups, by, onChange) {
  const dragged = useRef(null);
  const [over, setOver] = useState(null);
  const groupOf = (laneId) => groups.find((g) => g.lanes.some((lane) => lane.id === laneId));
  const apply = (kind, moving, target, after) => {
    if (kind === 'lane') moveLane(group.name, groups, moving, target, after);
    else moveHead(group.name, groups, by, moving, target, after);
    onChange();
  };
  // accepts says whether the drag in flight may land on target: a lane
  // within its own heading, a heading among the headings.
  const accepts = (event, kind, key) => {
    const types = event.dataTransfer?.types;
    if (types && !Array.from(types).includes(LANE_DRAG_TYPE)) return false;
    const from = dragged.current;
    if (!from || from.kind !== kind || from.key === key) return false;
    return kind === 'head' || groupOf(from.key) === groupOf(key);
  };
  const half = (event) => {
    const rect = event.currentTarget.getBoundingClientRect?.();
    return !!rect && rect.height > 0 && event.clientY > rect.top + rect.height / 2;
  };
  const props = (kind, key, keys) => ({
    draggable: 'true',
    tabindex: '0',
    title: kind === 'lane' ? 'Drag to reorder the lanes (Alt+↑/↓)' : 'Drag to reorder the headings (Alt+↑/↓)',
    onDragStart: (event) => {
      event.stopPropagation();
      dragged.current = { kind, key };
      event.dataTransfer?.setData(LANE_DRAG_TYPE, key);
      if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
    },
    onDragEnd: () => { dragged.current = null; setOver(null); },
    onDragOver: (event) => {
      if (!accepts(event, kind, key)) return;
      event.preventDefault();
      event.stopPropagation();
      const after = half(event);
      if (over?.key !== key || over?.after !== after) setOver({ kind, key, after });
    },
    onDrop: (event) => {
      if (!accepts(event, kind, key)) return;
      event.preventDefault();
      event.stopPropagation();
      const moving = dragged.current.key;
      const after = half(event);
      dragged.current = null;
      setOver(null);
      apply(kind, moving, key, after);
    },
    onKeyDown: (event) => {
      if (!event.altKey || (event.key !== 'ArrowUp' && event.key !== 'ArrowDown')) return;
      const list = keys();
      const i = list.indexOf(key);
      const j = event.key === 'ArrowUp' ? i - 1 : i + 1;
      if (i < 0 || j < 0 || j >= list.length) return;
      event.preventDefault();
      apply(kind, key, list[j], event.key === 'ArrowDown');
    },
  });
  return {
    over,
    laneProps: (lane) => props('lane', lane.id, () => (groupOf(lane.id)?.lanes || []).map((l) => l.id)),
    headProps: (lg) => props('head', lg.key, () => groups.filter((g) => g.key !== 'former').map((g) => g.key)),
  };
}

// useColumnResize drags a column head's right edge to resize the column;
// a double-click resets every width. Arrow keys on a focused edge move it
// 16px. Widths are stored per group for this viewer.
function useColumnResize(group, onChange) {
  const [live, setLive] = useState(null);
  const gridRef = useRef(null);
  const measure = () => {
    const head = gridRef.current?.querySelector('.tb-lane-columns');
    const cells = head ? [...head.children].filter((el) => !el.classList.contains('tb-col-resize')) : [];
    return cells.map((el) => el.getBoundingClientRect().width);
  };
  const commit = (widths) => { setLaneWidths(group.name, widths); setLive(null); onChange(); };
  const handle = (index, label) => ({
    role: 'separator', 'aria-orientation': 'vertical', tabindex: '0', 'aria-label': `Resize the ${label} column`,
    title: 'Drag to resize · double-click to reset all widths · ←/→ when focused',
    'data-col-resize': LAYOUT_COLUMNS[index],
    onPointerDown: (event) => {
      if (event.button !== undefined && event.button !== 0) return;
      event.preventDefault();
      event.stopPropagation();
      const measured = measure();
      if (measured.length !== LAYOUT_COLUMNS.length) return;
      const startX = event.clientX;
      let latest = null;
      const move = (e) => { latest = resizeColumns(measured, index, e.clientX - startX); setLive(latest); };
      const up = () => {
        window.removeEventListener('pointermove', move);
        window.removeEventListener('pointerup', up);
        window.removeEventListener('pointercancel', up);
        if (latest) commit(latest);
        else setLive(null);
      };
      window.addEventListener('pointermove', move);
      window.addEventListener('pointerup', up);
      window.addEventListener('pointercancel', up);
    },
    onDblClick: (event) => { event.preventDefault(); commit(null); },
    onKeyDown: (event) => {
      if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
      const measured = measure();
      if (measured.length !== LAYOUT_COLUMNS.length) return;
      event.preventDefault();
      commit(resizeColumns(measured, index, event.key === 'ArrowLeft' ? -16 : 16));
    },
    onClick: (event) => event.stopPropagation(),
  });
  return { gridRef, handle, widths: live || laneWidths(group.name) };
}

// TaskboardLanes is the section's swim-lane view of the same board the list
// shows. Without `actions` it is read-only: no drags.
// CellAdd is a cell's "+": a small button in its lower-right corner that
// opens the New task form filled from the cell (its stage, and the lane's
// agent as the assignee).
function CellAdd({ lane, column, add, onNewTask, wizard }) {
  const where = `${lane.id === UNASSIGNED ? 'Unassigned' : lane.name} · ${wizard ? column.wizard : column.label}`;
  const what = wizard ? 'quest' : 'task';
  return html`<${Button} size="sm" variant="ghost" class="tb-cell-add" title=${`New ${what} here: ${where}`}
    aria-label=${`New ${what} in ${where}`} onClick=${() => onNewTask({ stage: add.stage, to: add.to })}>+<//>`;
}

export function TaskboardLanes({ entry, group, snapshot, by = 'agent', actions = null, wizard = false, now = Date.now(), onNewTask = null, onLayout = null }) {
  const tasks = entry.tasks || [];
  const numbers = new Map(tasks.map((task, i) => [task.id, i + 1]));
  const [, bumpLayout] = useState(0);
  const relayout = () => { bumpLayout((n) => n + 1); onLayout?.(); };
  const model = laneModel(entry, group, by, snapshot, now);
  const { cells } = model;
  const groups = applyLaneOrder(model.groups, laneOrder(group.name), by);
  const drag = useLaneDrag(actions, group);
  const order = useLaneOrder(group, groups, by, relayout);
  const resize = useColumnResize(group, relayout);
  const [unfolded, setUnfolded] = useState({});
  const [doneOpen, setDoneOpen] = useState({});
  const cell = (lane, column) => {
    const key = `${lane.id}|${column.key}`;
    const list = cells.get(key) || [];
    const stripe = striped(lane.id, column.key);
    const target = { lane: lane.id, column: column.key, name: lane.name, member: lane.id === UNASSIGNED || !!lane.member };
    const over = drag.over?.key === key;
    const shown = column.key === 'done' && !doneOpen[lane.id] ? list.slice(0, DONE_SHOWN) : list;
    const add = actions && onNewTask && cellAdd(lane.id, column.key, { assign: canAssign(actions), member: lane.id === UNASSIGNED || !!lane.member });
    return html`<div key=${key} class=${`tb-cell${stripe ? ' tb-cell-striped' : ''}${over ? ' tb-cell-over' : ''}${add ? ' tb-cell-addable' : ''}`}
        data-lane=${lane.id || 'unassigned'} data-column=${column.key} data-tb-drop=${over ? drag.over.label : undefined}
        onDragOver=${actions ? (event) => drag.overCell(event, target) : undefined}
        onDrop=${actions ? (event) => void drag.dropCell(event, target) : undefined}>
      ${shown.map((task) => html`<${Card} key=${task.id} task=${task} n=${numbers.get(task.id)} numbers=${numbers}
        drag=${actions ? { ...drag, start: (event, t) => drag.start(event, t, { lane: lane.id, column: column.key }) } : null} />`)}
      ${shown.length < list.length && html`<${Button} size="sm" variant="link" class="tb-cell-more"
        onClick=${() => setDoneOpen({ ...doneOpen, [lane.id]: true })}>+ ${list.length - shown.length} more<//>`}
      ${add && html`<${CellAdd} lane=${lane} column=${column} add=${add} onNewTask=${onNewTask} wizard=${wizard} />`}
    </div>`;
  };
  const row = (lane) => html`<div key=${lane.id || 'unassigned'} class="tb-lane" data-lane-row=${lane.id || 'unassigned'}>
    <${LaneHead} lane=${lane} order=${lane.id === UNASSIGNED ? null : order} />${LANE_COLUMNS.map((column) => cell(lane, column))}
  </div>`;
  const unassigned = { id: UNASSIGNED, name: 'Unassigned',
    count: LANE_COLUMNS.reduce((sum, column) => sum + (column.key === 'done' ? 0 : (cells.get(`|${column.key}`) || []).length), 0) };
  const count = (key) => [...cells.entries()].reduce((sum, [k, list]) => sum + (k.endsWith(`|${key}`) ? list.length : 0), 0);
  return html`<div class="tb-lanes-wrap">
    ${drag.blocking && actions && html`<${BlockForm} task=${drag.blocking.task} drop=${drag.blocking.drop} numbers=${numbers} actions=${actions}
      group=${group} onClose=${() => drag.setBlocking(null)} />`}
    <div class="tb-lanes" data-taskboard-lanes=${group.name} data-lanes-by=${by} ref=${resize.gridRef}
        style=${resize.widths ? `--tb-lane-cols: ${gridColumns(resize.widths)}` : undefined}
        onDragLeave=${actions ? drag.leaveGrid : undefined}>
      <div class="tb-lane tb-lane-columns">
        <div class="ui-eyebrow tb-col-lane">Lane<span class="tb-col-resize" ...${resize.handle(0, 'lane')}></span></div>
        ${LANE_COLUMNS.map((column, i) => html`<div key=${column.key} class=${`ui-eyebrow tb-col-${column.key}`}>${wizard ? column.wizard : column.label} <span class="tb-col-count">${column.key === 'done' ? (entry.counts?.done_this_week ?? count('done')) : count(column.key)}</span>${
          i < LANE_COLUMNS.length - 1 && html`<span class="tb-col-resize" ...${resize.handle(i + 1, column.label)}></span>`}</div>`)}
      </div>
      ${row(unassigned)}
      ${groups.map((lg) => {
        const folded = lg.offline && !unfolded[lg.key];
        const head = lg.label || lg.offline;
        const movable = lg.key !== 'former' && groups.filter((g) => g.key !== 'former').length > 1;
        const marker = order.over?.kind === 'head' && order.over.key === lg.key ? (order.over.after ? ' tb-order-after' : ' tb-order-before') : '';
        return [
          head && html`<div key=${`${lg.key}-head`} class=${`tb-lane-group${marker}`} data-lane-group=${lg.key} ...${movable ? order.headProps(lg) : {}}>
            <span class="ui-eyebrow">${lg.label}</span>
            <span class="tb-lane-sub">${lg.lanes.length} agent${lg.lanes.length === 1 ? '' : 's'}${lg.offline ? ' · offline' : ''}</span>
            ${lg.offline && html`<${Button} size="sm" variant="link" class="tb-lane-fold" aria-expanded=${folded ? 'false' : 'true'}
              onClick=${() => setUnfolded({ ...unfolded, [lg.key]: folded })}>${folded ? '▸ show offline lanes' : '▾ fold offline lanes'}<//>`}
          </div>`,
          ...(folded ? [] : lg.lanes.map(row)),
        ];
      })}
    </div>
  </div>`;
}
