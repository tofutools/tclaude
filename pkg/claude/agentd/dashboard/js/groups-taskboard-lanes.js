// groups-taskboard-lanes.js -- the Task board's swim lanes (docs/taskboard.md):
// the stage columns Ideas → Next → In progress → Blocked → Done
// this week, one lane per agent (grouped by role, by subgroup, or not at
// all) under an Unassigned lane. Every drag is one route, the one its CLI
// verb uses:
//
//   card → another column, same lane   promote · demote · block --reason ·
//                                      unblock · done · reopen; in an agent's
//                                      lane, onto In progress start, onto
//                                      Ideas or Next hold --idea / --next
//   card → an agent's lane             assign <task> --to <agent>: onto In
//                                      progress it starts, onto Ideas or
//                                      Next it is held there (--idea/--next)
//   card → the Unassigned lane         unassign <task>
//   card → another lane AND column     both, in that order: assign then block
//                                      --reason; unassign then demote or block
//
//   card → a custom column          move <task> --stage <column> (In review,
//                                      In CI: a column of the board's own,
//                                      groups-taskboard-columns.js); out of
//                                      one onto Next or In progress,
//                                      --stage next
//
// The one cell that cannot hold a card is striped: In progress in the
// Unassigned lane. An agent's (or the operator's) Ideas and Next hold what
// it was given and has not started (tk_3826822a). A cell a new task can
// land in has a small "+" in its lower-right corner that opens the New task
// form filled from the cell (add [--next|--idea] [--assignee <agent>]). Column widths and the order of the lanes and their headings
// are this viewer's own (groups-taskboard-lanes-layout.js).

import { h } from 'preact';
import { useContext, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { TaskReasonDialog } from './groups-taskboard-reason.js';
import {
  applyLaneOrder, gridColumns, gridMinWidth, headedGroups, laneOrder, laneWidths, moveHead, moveLane, resizeColumns, setLaneWidths, useOpenCells,
} from './groups-taskboard-lanes-layout.js';
import { markLanded } from './drop-landing.js';
import { REVEAL_FRESH_MS, agentLink, taskRevealRequest } from './cross-links.js';
import { listenForTransfer } from './groups-taskboard-transfer.js';
import { DepChips, RefChip, actorLabel, archiveItems, boardTasks, assignableMembers, binDroppable, canAssign, openRowMenu, operatorName } from './groups-taskboard.js';
import { RowMenu } from './row-menu.js';
import { NewTaskMenu } from './groups-taskboard-new.js';
import { CommentCount } from './groups-taskboard-comments.js';
import { TaskTitle } from './groups-taskboard-card.js';
import { copyLinkItem, TaskLink } from './task-route.js';
import { COLUMN_STATE, columnLabel } from './taskboard-context.js';
import { AddColumnButton, ColumnHeadMenu, useColumnNaming } from './groups-taskboard-columns.js';
import { LaneLimit, QueuedChip, useLimitDialog } from './groups-taskboard-limits.js';
import { HIDEABLE_BUILTINS } from './groups-taskboard-colvis.js';
import { AnswerFor, QuestionCard, QuestionTag, answerTarget, askerOf, openQuestions, questionOpen } from './groups-taskboard-answer.js';
import { dropSide, onOrderKey, orderItems, orderPeers } from './groups-taskboard-order.js';
import { EpicBoard, EpicChip, EpicMark, epicColour, epicDimmed, epicMoved, intoEpic, isEpic } from './groups-taskboard-epics.js';
import { BACKLOG, backlogItems, backlogSupported, backlogTasks } from './groups-taskboard-backlog.js';
import { ReadyChip, ReadyHead, groupable, groupedCell, readinessPeers, readyMenuItem, sameGroup, useReadyGrouping } from './groups-taskboard-readiness.js';

const html = htm.bind(h);

export const LANE_COLUMNS = [
  { key: 'ideas', label: 'Ideas', wizard: 'Visions' },
  { key: 'next', label: 'Next', wizard: 'Next' },
  { key: 'progress', label: 'In progress', wizard: 'Underway' },
  { key: 'blocked', label: 'Blocked', wizard: 'Blocked' },
  { key: 'done', label: 'Done this week', wizard: 'Fulfilled this week' },
];

// isCustomColumn reports a custom column's key (`col:<slug>`).
export const isCustomColumn = (key) => typeof key === 'string' && key.startsWith(COLUMN_STATE);

// boardColumns are a board's lanes columns: LANE_COLUMNS with the board's
// custom columns (entry.columns, In review, In CI, …) after In progress,
// keyed `col:<slug>`. The columns in entry.hiddenColumns (the board's and the
// viewer's hidden ones, groups-taskboard-colvis.js) are left out; a cordoned
// custom column is marked, and takes no drop or "+".
export function boardColumns(entry) {
  const custom = (entry?.columns || []).map((column) => ({
    key: COLUMN_STATE + column.slug, label: column.label, wizard: column.label, slug: column.slug, custom: true,
    cordoned: column.state === 'cordoned',
  }));
  const hidden = entry?.hiddenColumns || [];
  if (!custom.length && !hidden.length) return LANE_COLUMNS;
  const at = LANE_COLUMNS.findIndex((column) => column.key === 'progress') + 1;
  return [...LANE_COLUMNS.slice(0, at), ...custom, ...LANE_COLUMNS.slice(at)].filter((column) => !hidden.includes(column.key));
}

// columnClass is a column's head class, and cellClass the extra class of its
// cells.
export const columnClass = (column) => `ui-eyebrow ${column.custom ? 'tb-col-custom' : `tb-col-${column.key}`}${column.cordoned ? ' tb-col-cordoned' : ''}`;
export const cellClass = (column) => `${column.custom ? ' tb-cell-custom' : ''}${column.cordoned ? ' tb-cell-cordoned' : ''}`;

const HIDEABLE_KEYS = HIDEABLE_BUILTINS.map((b) => b.key);

// headMenu reports a column head that has a ⋯ (ColumnHeadMenu): a custom
// column, a hideable one, In progress on a board with options, and any
// column a viewer may group by readiness (groups-taskboard-readiness.js).
export function headMenu(column, entry) {
  return column.custom || HIDEABLE_KEYS.includes(column.key) || (column.key === 'progress' && !!entry?.options) || groupable(column);
}
const WEEK_MS = 7 * 24 * 3600 * 1000;
// CELL_SHOWN is how many cards a cell draws before "+ n more": a cell
// holding many tasks would otherwise stretch its whole row (and push the
// grid's sideways scrollbar off-screen). "+ n more" opens the cell in place.
export const CELL_SHOWN = 3;
export const UNASSIGNED = '';
// OPERATOR is the operator's lane: the tasks assigned to the operator of the
// board's node ("You" here, a linked node's operator on its boards), pinned
// above Unassigned.
export const OPERATOR = 'operator';
const TASK_DRAG_TYPE = 'application/x-tclaude-task';

// laneColumn is the column a task stands in, derived as the design's table
// says; null for a task the lanes leave out (dropped, done before this week).
export function laneColumn(task, now = Date.now()) {
  if (task.status === 'dropped') return null;
  if (task.status === 'done') {
    const closed = Date.parse(task.closed_at || '');
    return Number.isFinite(closed) && now - closed <= WEEK_MS ? 'done' : null;
  }
  // A parked task is in no lane: the Backlog column holds it.
  if (task.stage === BACKLOG) return null;
  const assigned = !!task.assignee?.agent_id;
  if (task.status === 'blocked' || (assigned && task.stalled)) return 'blocked';
  // A task in one of the board's custom columns stands there, whoever holds
  // it (tcl-plby5h).
  if (task.column) return COLUMN_STATE + task.column;
  // A held task (assigned, not started) sits in its stage; a node that
  // predates holding sends no held, so its assigned tasks are in progress.
  if (assigned && !task.held) return 'progress';
  return task.stage === 'idea' ? 'ideas' : 'next';
}

// striped reports the cells that cannot hold a card: In progress in the
// Unassigned lane.
export function striped(lane, column) {
  return lane === UNASSIGNED && column === 'progress';
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
    if (id && id !== OPERATOR && !known.has(id)) {
      known.add(id);
      const task = [...cells.entries()].find(([k]) => k.startsWith(`${id}|`))[1][0];
      lanes.push({ id, member: null, name: task.assignee?.name || id.slice(0, 12) });
    }
  }
  for (const lane of lanes) {
    lane.state = memberState(lane.member);
    lane.holds = holds.get(lane.id) || 0;
  }
  const questions = operatorQuestions(entry);
  // The operator's lane holds what waits on it in any column (tk_3cb51ce0):
  // all but its ideas and what is done.
  const operatorHolds = [...cells.entries()].filter(([key]) => key.startsWith(`${OPERATOR}|`) && !['ideas', 'done'].includes(key.slice(OPERATOR.length + 1)))
    .reduce((sum, [, tasks]) => sum + tasks.length, 0) + questions.length;
  return { cells, questions, groups: laneGroups(lanes, group, by, snapshot), operatorHolds };
}

// operatorQuestions are the open questions on tasks the operator does not
// hold (tk_8b2c86f7): each is a card in the operator's lane (or Triage's
// You), In progress, while its task stays in its own lane. A task the
// operator holds shows Answer… on its own card.
export function operatorQuestions(entry) {
  const out = [];
  for (const task of entry?.tasks || []) {
    if (task.assignee?.operator) continue;
    for (const question of openQuestions(task)) out.push({ task, question });
  }
  return out;
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
// twoStep runs first, then second, and remembers a first write that landed:
// a retry after the second was refused (the block dialog stays open) runs only
// the second, rather than repeating an unassign the server would refuse.
function twoStep(first, second) {
  let firstDone = false;
  return async (api, reason) => {
    if (!firstDone) { await first(api); firstDone = true; }
    return second(api, reason);
  };
}

// epicDrop is a drop on an epic's row (the lanes' epic mode,
// groups-taskboard-lanes-flat.js): onto another epic's row the task moves to
// that epic (`edit --parent`, or out of every epic onto No epic), first, then
// whatever the column drop does; undo puts it back in its epic. An epic is
// in no epic, so it does not change rows. Without an epic row it is the
// lane drop as is.
export function epicDrop(from, to, drop) {
  if (to.epic === undefined || (from.epic || '') === to.epic) return drop;
  const task = from.task;
  if (isEpic(task) || (drop === null && (from.lane !== to.lane || from.column !== to.column))) return null;
  const into = (epic) => intoEpic(task.id, epic);
  const label = to.epic ? `to ${to.epicName}` : 'out of its epic';
  return {
    ...(drop || {}),
    kind: drop ? `epic+${drop.kind}` : 'epic',
    label: drop ? `${label} · ${drop.label}` : label,
    run: drop ? twoStep(into(to.epic), drop.run) : into(to.epic),
    epicMove: true,
  };
}

export function laneDrop(task, from, to, opts = {}) {
  // An epic is no work of its own: it is never assigned, blocked, started
  // or put in a column (the store refuses each), only moved between Ideas
  // and Next in its own lane.
  if (isEpic(task) && (to.lane !== from.lane || !['ideas', 'next'].includes(to.column))) return null;
  const drop = baseLaneDrop(task, from, to, opts);
  // A card in a custom column dropped onto Next or In progress leaves the
  // column too, whatever else the drop does (an assign, an unassign, an
  // unblock): the column write goes last, so the card lands where it was
  // dropped rather than back in its column.
  if (!drop || !task?.column || drop.kind.startsWith('uncolumn') || (to.column !== 'next' && to.column !== 'progress')) return drop;
  const out = (api) => api.stage(task.id, 'next');
  return { ...drop, kind: `${drop.kind}+uncolumn`, label: `${drop.label} · out of ${columnLabel(COLUMN_STATE + task.column, to.columns)}`,
    run: twoStep(drop.run, out) };
}

function baseLaneDrop(task, from, to, { assign = true } = {}) {
  if (!task || (from.lane === to.lane && from.column === to.column) || striped(to.lane, to.column)) return null;
  if (from.column === BACKLOG || to.column === BACKLOG) return backlogDrop(task, from, to, { assign });
  const open = task.status === 'open';
  const block = (api, reason) => api.status(task.id, 'blocked', reason);
  if (isCustomColumn(to.column)) return columnDrop(task, from, to, { assign });
  if (task.status === 'done') return reopenDrop(task, from, to, { assign, block });
  // Out of a custom column onto Next or In progress, in its own lane:
  // `move --stage next`; onto In progress a held task is started too, and
  // onto Next a started one is held back, so it lands where it was dropped.
  if (task.column && open && from.lane === to.lane && (to.column === 'next' || to.column === 'progress')) {
    const label = `out of ${columnLabel(COLUMN_STATE + task.column, to.columns)}`;
    const out = (api) => api.stage(task.id, 'next');
    const started = !!task.assignee?.agent_id && !task.held;
    if (to.column === 'progress' && task.held) return { kind: 'uncolumn+start', label: `${label} · start`, run: twoStep(out, (api) => api.start(task.id)) };
    if (to.column === 'next' && started) return { kind: 'uncolumn+hold', label: `${label}, not started`, run: twoStep(out, (api) => api.hold(task.id, 'next')) };
    return { kind: 'uncolumn', label, run: out };
  }
  if (from.lane !== to.lane) {
    if (!assign) return null;
    if (to.lane === UNASSIGNED) {
      if (!task.assignee?.agent_id || task.status === 'done') return null;
      const unassign = (api) => api.unassign(task.id);
      // An open task in an agent's Blocked column is a stalled one: onto
      // the Unassigned lane's Blocked it is blocked too, not only unassigned.
      if (to.column === 'blocked' && open) {
        return { kind: 'unassign+block', label: 'unassign · block (asks why)', reason: true, run: twoStep(unassign, block) };
      }
      if (to.column === 'next' || to.column === from.column) return { kind: 'unassign', label: 'unassign', run: unassign };
      if (to.column === 'ideas' && open) {
        return { kind: 'unassign+demote', label: 'unassign · demote to ideas', run: twoStep(unassign, (api) => api.stage(task.id, 'idea')) };
      }
      return null;
    }
    // Only a member can be assigned (a former member's lane holds what it
    // had), and only an open task.
    if (!open || to.member === false) return null;
    const name = to.name || to.lane;
    const give = (api) => api.assign(task.id, to.lane);
    if (to.column === 'progress') return { kind: 'assign', label: `assign to ${name}`, agent: to.lane, run: give };
    if (to.column === 'next' || to.column === 'ideas') {
      const stage = to.column === 'ideas' ? 'idea' : 'next';
      return { kind: 'assign-hold', label: `give to ${name}, ${stage === 'idea' ? 'an idea' : 'queued'}, not started`, agent: to.lane,
        run: (api) => api.assign(task.id, to.lane, { hold: stage }) };
    }
    if (to.column === 'blocked') {
      return { kind: 'assign+block', label: `assign to ${name} · block (asks why)`, agent: to.lane, reason: true, run: twoStep(give, block) };
    }
    return null;
  }
  // In an agent's (or the operator's) own lane: onto In progress starts a
  // task it holds; onto Ideas or Next puts it back there, not started.
  if (to.lane !== UNASSIGNED && task.assignee?.agent_id && open && ['progress', 'ideas', 'next'].includes(to.column)) {
    if (!assign) return null;
    if (to.column === 'progress') {
      if (!task.held) return null;
      return { kind: 'start', label: 'start', run: (api) => api.start(task.id) };
    }
    if (to.column === 'ideas' || to.column === 'next') {
      const stage = to.column === 'ideas' ? 'idea' : 'next';
      return { kind: 'hold', label: stage === 'idea' ? 'back to ideas, not started' : 'back to next, not started', run: (api) => api.hold(task.id, stage) };
    }
  }
  switch (to.column) {
    case 'ideas':
      if (!open || task.stage === 'idea') return null;
      return { kind: 'demote', label: 'demote to ideas', run: (api) => api.stage(task.id, 'idea') };
    case 'next':
    case 'progress':
      if (task.status === 'blocked') return { kind: 'unblock', label: 'unblock', run: (api) => api.status(task.id, 'open') };
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

// backlogDrop is a card dragged into or out of the Backlog column
// (tk_5d2547b0). Into it, a live task is parked (`taskboard backlog`), in
// whoever's hands it is. Out of it, `from.lane` is the lane of its holder:
// onto Ideas or Next of that lane it comes back to that stage (its
// assignee still holds it, not started); onto another lane it is given to
// that lane's agent, held there, or taken off its assignee onto the
// Unassigned lane; onto In progress its agent starts it. Blocked and Done
// keep their own meaning; a custom column moves it there.
export function backlogDrop(task, from, to, { assign = true } = {}) {
  const live = task.status === 'open' || task.status === 'blocked';
  if (!live || isEpic(task)) return null;
  if (to.column === BACKLOG) {
    if (task.stage === BACKLOG) return null;
    return { kind: 'park', label: 'park in backlog', run: (api) => api.stage(task.id, BACKLOG) };
  }
  if (isCustomColumn(to.column)) return columnDrop(task, from, to, { assign });
  const stage = { ideas: 'idea', next: 'next' }[to.column];
  const words = stage === 'idea' ? 'back to ideas' : 'to next';
  if (from.lane === to.lane) {
    if (stage && task.assignee?.agent_id) {
      // hold is an assign-level write unless the viewer is the assignee.
      if (!assign) return null;
      return { kind: 'unpark-hold', label: `${words}, not started`, run: (api) => api.hold(task.id, stage) };
    }
    if (stage) return { kind: 'unpark', label: words, run: (api) => api.stage(task.id, stage) };
    if (to.column === 'progress' && task.assignee?.agent_id && assign) {
      return { kind: 'unpark+start', label: 'start', run: (api) => api.start(task.id) };
    }
    if (to.column === 'done' && (!task.assignee?.agent_id || assign)) {
      return { kind: 'done', label: 'done', run: (api) => api.status(task.id, 'done') };
    }
    return null;
  }
  if (!assign || task.status !== 'open') return null;
  if (to.lane === UNASSIGNED) {
    if (!stage) return null;
    return { kind: 'unassign+unpark', label: `unassign · ${words}`,
      run: twoStep((api) => api.unassign(task.id), (api) => api.stage(task.id, stage)) };
  }
  if (to.member === false) return null;
  const name = to.name || to.lane;
  if (to.column === 'progress') return { kind: 'assign', label: `assign to ${name}`, agent: to.lane, run: (api) => api.assign(task.id, to.lane) };
  if (!stage) return null;
  return { kind: 'assign-hold', label: `give to ${name}, ${stage === 'idea' ? 'an idea' : 'queued'}, not started`, agent: to.lane,
    run: (api) => api.assign(task.id, to.lane, { hold: stage }) };
}

// columnDrop is a card dragged onto a custom column (`move --stage
// <column>`): in its own lane it moves there (a blocked one is unblocked
// first); onto another lane it is assigned (or unassigned) first, as any
// lane change, and needs the assign right. A done card is not taken: reopen
// it first.
function columnDrop(task, from, to, { assign }) {
  if (task.status !== 'open' && task.status !== 'blocked') return null;
  const slug = to.column.slice(COLUMN_STATE.length);
  if (task.column === slug && task.status === 'open' && from.lane === to.lane) return null;
  const label = `to ${to.columnLabel || columnLabel(to.column, to.columns)}`;
  const move = (api) => api.stage(task.id, slug);
  if (from.lane === to.lane) {
    if (task.status === 'blocked') {
      return { kind: 'unblock+column', label: `unblock · ${label}`, run: twoStep((api) => api.status(task.id, 'open'), move) };
    }
    return { kind: 'column', label, run: move };
  }
  if (!assign || task.status !== 'open') return null;
  if (to.lane === UNASSIGNED) {
    if (!task.assignee?.agent_id) return null;
    return { kind: 'unassign+column', label: `unassign · ${label}`, run: twoStep((api) => api.unassign(task.id), move) };
  }
  if (to.member === false) return null;
  const name = to.name || to.lane;
  return { kind: 'assign+column', label: `assign to ${name} · ${label}`, agent: to.lane, run: twoStep((api) => api.assign(task.id, to.lane), move) };
}

// reopenDrop is a done card dragged out of Done (`taskboard reopen`): it is
// reopened in the column's stage and the lane's hands, in one write, and
// onto Blocked it is blocked after (asking why). A move to another lane
// changes the assignee, so it needs the assign right, as any lane change.
function reopenDrop(task, from, to, { assign, block }) {
  if (to.column === 'done') return null;
  const held = task.assignee?.agent_id || '';
  const opts = {};
  let label = 'reopen';
  if (from.lane !== to.lane) {
    if (!assign || to.member === false) return null;
    opts.to = to.lane === UNASSIGNED ? '' : to.lane;
    label = to.lane === UNASSIGNED ? 'reopen · unassign' : `reopen · assign to ${to.name || to.lane}`;
  }
  const lane = from.lane !== to.lane ? opts.to : held;
  if (to.column === 'ideas') {
    // An idea has no assignee: only the Unassigned lane's Ideas take one.
    if (lane) return null;
    opts.stage = 'idea';
    label += ' · as an idea';
  } else if (to.column === 'next' || to.column === 'progress') {
    if (task.stage === 'idea' || from.lane !== to.lane) opts.stage = 'next';
  }
  const reopen = (api) => api.reopen(task.id, opts);
  if (to.column === 'blocked') {
    return { kind: 'reopen+block', label: `${label} · block (asks why)`, reason: true, run: twoStep(reopen, block) };
  }
  return { kind: 'reopen', label, run: reopen };
}

// cellAdd is the start state the "+" in a cell carries into the add menu's
// choice, or null where a new task cannot start: every cell the form can
// create a task in. The Unassigned lane's cells set the column; an agent's
// lane also sets the assignee (Ideas and Next hold it there, not started),
// only for a viewer who may assign. Striped cells take none.
export function cellAdd(lane, column, { assign = true, member = true } = {}) {
  if (striped(lane, column)) return null;
  const start = { ideas: 'idea', next: 'next', progress: 'progress', blocked: 'blocked', done: 'done' }[column];
  if (!start) return null;
  if (lane === UNASSIGNED) return { start, to: '' };
  if (!assign || !member) return null;
  return { start, to: lane };
}

// cardHint is what a card's column does not say: why it is blocked, or that
// its agent went quiet. No times (the List's Last and Age carry them).
function cardHint(task) {
  if (task.status === 'blocked') return html`<span class="ui-tag ui-tag-warn tb-card-why" title=${task.block_reason || undefined}>${task.block_reason || 'blocked'}</span>`;
  if (task.stalled && task.assignee?.agent_id) return html`<span class="ui-tag ui-tag-warn" title="its agent went idle without reporting">idle, no report</span>`;
  // In a custom column (In review, …) its agent may idle: no stall alarm.
  if (task.column && task.assignee?.agent_id && !task.assignee?.operator && !task.held) {
    return html`<span class="tb-paused" title="In a custom column the idle check is paused: its agent waits on a review or CI, not on the work">⏸ no idle alarm</span>`;
  }
  return null;
}

// A card's title opens the edit form (onEdit), as the List's does, and so
// does a double-click on the card; hovering the title shows the task's
// card (groups-taskboard-card.js), which carries its id.
// `marker` (lanes triage and one row, groups-taskboard-lanes-flat.js) is a corner badge
// saying who holds the task, with a class for the card's edge.
// menu, when given, is the card's ⋯ items (Archive): its ⋯ shows on hover,
// and a right-click opens it. onAnswer, when given, puts Answer… on a
// question still to answer.
// dropAt is 'before' or 'after' while a drag points at the slot on that side
// of the card; onKey takes the card's key presses (Alt+↑/↓/Home/End moves it,
// groups-taskboard-order.js).
export function Card({ task, byId, drag, api = null, nameOf = null, onEdit = null, marker = null, menu: given = null, onAnswer = null, chip = true, dropAt = '', onKey = null }) {
  const done = task.status === 'done';
  const dim = epicDimmed(useContext(EpicBoard), task);
  // An editable card on a board has ⋯ Copy link (task-route.js), beside its own items.
  const board = useContext(TaskLink);
  const menu = board && onEdit ? [...(given || []), copyLinkItem(task.id, board)] : given;
  return html`<div class=${`ui-card tb-card${marker ? ` tb-card-marked ${marker.class}` : ''}${done ? ' tb-card-done' : ''}${task.stage === 'idea' && task.status === 'open' ? ' tb-card-idea' : ''}${task.stage === BACKLOG ? ' tb-card-parked' : ''}${drag?.dragging === task.id ? ' tb-dragging' : ''}${isEpic(task) ? ` tb-card-epic tb-epic-c${epicColour(task.id)}` : ''}${dim ? ' tb-epic-dim' : ''}${dropAt ? ` tb-drop-${dropAt}` : ''}`}
      data-task-id=${task.id} draggable=${drag ? 'true' : undefined} data-dnd-card=${drag ? task.id : undefined}
      onKeyDown=${onKey || undefined}
      onDblClick=${onEdit ? (event) => { if (event.target?.closest?.('.ui-popover')) return; event.preventDefault(); onEdit(task.id); } : undefined}
      onContextMenu=${menu?.length ? (event) => openRowMenu(event, '.tb-card-more') : undefined}
      onDragStart=${drag ? (event) => drag.start(event, task) : undefined} onDragEnd=${drag ? drag.end : undefined}>
    ${marker?.badge}
    ${menu?.length > 0 && html`<span class="tb-card-menu"><${RowMenu} label=${task.title} items=${menu} buttonClass="tb-card-more" /></span>`}
    <div class="tb-card-title"><${TaskTitle} task=${task} api=${api} nameOf=${nameOf} onOpen=${onEdit ? () => onEdit(task.id) : null}
      chips=${{ ref: task.ref && html`<${RefChip} task=${task} />`, deps: html`<${DepChips} task=${task} byId=${byId} />` }} /></div>
    ${((chip && task.parent_id) || isEpic(task)) && html`<div class="tb-card-epic-line"><${EpicMark} task=${task} />${chip && html`<${EpicChip} task=${task} />`}</div>`}
    ${!done && html`<div class="tb-card-meta">${task.ref && html`<${RefChip} task=${task} />`}<${DepChips} task=${task} byId=${byId} /><${QueuedChip} task=${task} />${cardHint(task)}<${CommentCount} task=${task} onOpen=${onEdit ? () => onEdit(task.id) : null} />
      <${QuestionTag} task=${task} askedBy=${nameOf && askerOf(task) ? nameOf(askerOf(task)) : ''} /></div>`}
    ${onAnswer && questionOpen(task) && html`<div class="tb-card-answer"><${Button} size="sm" variant="primary" class="tb-answer-open"
      onClick=${(event) => { event.stopPropagation(); onAnswer(task.id); }}>Answer…<//></div>`}
  </div>`;
}

// CellCards is a cell's cards with its toggle. Closed, a cell holding more
// than `limit` cards draws the first ones and "+ n more". Open, it draws them
// all, growing up to a height cap and then scrolling inside, with "▴ Show
// less" under the scroll area so it stays in reach. Show less scrolls the
// cards back to the top and keeps the toggle in view, so the page does not
// jump away from where the pointer is. card(task) draws one card. `reveal`
// is a task just dropped into the cell: once its card is drawn, the cell
// scrolls to it, so it does not land below the fold.
export function CellCards({ list, limit, open, onToggle, card, reveal = '' }) {
  const scrollRef = useRef(null);
  const lessRef = useRef(null);
  const revealed = useRef('');
  useLayoutEffect(() => {
    if (!reveal || revealed.current === reveal) return;
    const el = scrollRef.current?.querySelector?.(`[data-task-id="${reveal}"]`);
    if (!el) return;
    revealed.current = reveal;
    el.scrollIntoView?.({ block: 'nearest' });
  });
  const capped = list.length > limit;
  const visible = capped && !open ? list.slice(0, limit) : list;
  const collapse = () => {
    if (scrollRef.current) scrollRef.current.scrollTop = 0;
    onToggle(false);
    const later = globalThis.requestAnimationFrame || ((fn) => globalThis.setTimeout(fn, 0));
    const cell = lessRef.current?.closest?.('.tb-cell');
    later(() => cell?.scrollIntoView?.({ block: 'nearest' }));
  };
  return html`<div ref=${scrollRef} class=${`tb-cell-cards${capped && open ? ' tb-cell-cards-open' : ''}`}>${visible.map(card)}</div>
    ${capped && (open
    ? html`<${Button} size="sm" variant="ghost" class="tb-cell-more tb-cell-less" aria-expanded="true" domRef=${lessRef}
        title="Show only the first cards again" onClick=${collapse}>▴ Show less<//>`
    : html`<${Button} size="sm" variant="link" class="tb-cell-more" aria-expanded="false" data-hidden-tasks=${list.slice(limit).map((task) => task.id).join(' ')}
        onClick=${() => onToggle(true)}>+ ${list.length - visible.length} more<//>`)}`;
}

// useCellReveal opens the cell holding a task a cross-link asked to see
// (cross-links.js) past its cap, also when the cell is drawn only because
// that click unfolded the section. setOpen takes an updater. grouping (the
// board's ReadyGrouping) draws a grouped column's cells in their grouped
// order, which decides what is past the cap.
export function useCellReveal(cells, limit, setOpen, grouping = null) {
  const want = taskRevealRequest.value;
  useEffect(() => {
    if (!want || Date.now() - want.at > REVEAL_FRESH_MS) return;
    for (const [key, cellList] of cells) {
      const column = key.slice(key.indexOf('|') + 1);
      const list = groupedCell(cellList, !!grouping?.has(column) && groupable({ key: column })).list;
      if (list.slice(limit).some((task) => task.id === want.taskId)) setOpen((now) => (now[key] ? now : { ...now, [key]: true }));
    }
  }, [want?.seq]);
}

// cellName names a cell for the move items' tooltips: "Next", or
// "builder · In progress" in an agent's lane.
export function cellName(lane, column) {
  const label = column.label || column.key;
  return lane?.id && lane.name ? `${lane.name} · ${label}` : label;
}

const liveTask = (task) => task?.status === 'open' || task?.status === 'blocked';

// sameCell reports whether a drag from `from` points back into its own cell
// (the same lane, column and, in the epic rows, epic).
function sameCell(from, to) {
  if (from.lane !== to.lane || from.column !== to.column) return false;
  return to.epic === undefined || (from.epic || '') === to.epic;
}

// dropSlot is the slot a drag of task points at in a cell drawing list: the
// card under the pointer, and before or after it ({ anchor, where }); null
// off the cards; { self: true } on the dragged card itself, where a drop
// leaves it be.
export function dropSlot(event, task, list) {
  const el = event?.target?.closest?.('[data-task-id]');
  if (!el || !list) return null;
  if (el.getAttribute('data-task-id') === task.id) return { self: true };
  const anchor = list.find((t) => t.id === el.getAttribute('data-task-id'));
  if (!anchor || !liveTask(anchor)) return null;
  return { anchor, where: dropSide(event, el) };
}

const short = (title) => (title.length > 32 ? `${title.slice(0, 31)}…` : title);

// reorderDrop is a drag of task back into its own cell: before or after the
// card it points at, among the cards of its stage, or after the last of them
// off the cards. null where it would not move; inert in Done, whose order is
// when each task closed.
export function reorderDrop(task, slot, to, actions) {
  if (to.column === 'done') return { kind: 'derived', inert: true, label: 'Done is ordered by when tasks closed' };
  if (typeof actions?.reorder !== 'function' || slot?.self) return null;
  // Grouped by readiness (groups-taskboard-readiness.js), a card moves
  // within its group.
  if (to.grouped && slot?.anchor && !sameGroup(task, slot.anchor)) {
    return { kind: 'derived', inert: true, label: 'Grouped by readiness: a card moves within its group' };
  }
  // By id: a refresh during the drag draws new task objects.
  const at = (id) => (peers || []).findIndex((t) => t.id === id);
  const peers = orderPeers(to.grouped ? readinessPeers(to.list, task) : to.list, task);
  const i = at(task.id);
  if (i < 0) return null;
  let anchor = slot?.anchor;
  let where = slot?.where;
  if (!slot) { anchor = peers[peers.length - 1]; where = 'after'; }
  const j = at(anchor.id);
  if (j < 0 || j === i || (where === 'before' && j === i + 1) || (where === 'after' && j === i - 1)) return null;
  return { kind: 'reorder', label: `Move ${where} “${short(anchor.title)}”`,
    position: { [where]: anchor.id, stage: anchor.stage }, slot: { id: anchor.id, where } };
}

// slotted is a drop into another cell that also puts the task at the slot
// pointed at, when the drop lands it in the anchor's stage (Ideas: idea;
// Next, In progress and the custom columns: next). A drop that asks a reason
// (Blocked), or onto Blocked or Done, which mix stages or order by time,
// keeps its own place.
export function slotted(drop, task, to, slot, actions) {
  if (!drop || drop.reason || !slot || slot.self || typeof actions?.reorder !== 'function') return drop;
  // Into a column grouped by readiness, at a slot in another group, it
  // keeps its own place: the slot would not be where it is drawn.
  if (to.grouped && !sameGroup(task, slot.anchor)) return drop;
  const stage = to.column === 'ideas' ? 'idea' : to.column === BACKLOG ? BACKLOG : (to.column === 'blocked' || to.column === 'done') ? '' : 'next';
  if (!stage || slot.anchor.stage !== stage) return drop;
  const { anchor, where } = slot;
  return { ...drop, kind: `${drop.kind}+order`, label: `${drop.label} · ${where} “${short(anchor.title)}”`,
    run: twoStep(drop.run, (api) => api.move(task.id, { [where]: anchor.id, stage })), slot: { id: anchor.id, where }, baseKind: drop.baseKind || drop.kind };
}

// useLaneDrag drives the card drags. A drop target `to` names its lane, or
// carries resolve(fromLane) instead: the lane a drop from fromLane means
// there, or null where the cell takes none (triage's Agents row and one row,
// groups-taskboard-lanes-flat.js), and a
// key of its own for the cell it marks.
export function useLaneDrag(actions, group, tasks = []) {
  const dragged = useRef(null);
  const [dragging, setDragging] = useState('');
  const [over, setOver] = useState(null);
  const [blocking, setBlocking] = useState(null);
  // A drop on another group's board or header moves the task there
  // (groups-taskboard-transfer.js); its listeners live for the drag.
  const transferOff = useRef(null);
  useEffect(() => () => transferOff.current?.(), []);
  const assign = canAssign(actions);
  const clear = () => { dragged.current = null; setDragging(''); setOver(null); transferOff.current?.(); transferOff.current = null; };
  const hit = (event, to) => {
    // Only this board's task drag drives the cells: a text, link or file
    // drag after a missed dragend (the card re-rendered away mid-drag) must
    // not run the stale task's action.
    const types = event.dataTransfer?.types;
    if (types && !Array.from(types).includes(TASK_DRAG_TYPE)) return null;
    const from = dragged.current;
    const lane = from && to.resolve ? to.resolve(from.lane) : to.lane;
    if (lane === null || lane === undefined) return null;
    const at = { ...to, lane };
    const slot = from ? dropSlot(event, from.task, to.list) : null;
    // Within its own cell a card is reordered; elsewhere the drop does what
    // the cell means, then puts the task at the slot when it lands in the
    // anchor's stage.
    const drop = from && (sameCell(from, at) ? reorderDrop(from.task, slot, at, actions)
      : slotted(epicDrop(from, at, laneDrop(from.task, from, at, { assign })), from.task, at, slot, actions));
    if (!drop) return null;
    // A cell whose order is derived says so, and takes nothing.
    if (drop.inert) return drop;
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
      // copyMove: Ctrl/Cmd onto another group clones (groups-taskboard-transfer.js).
      if (event.dataTransfer) event.dataTransfer.effectAllowed = 'copyMove';
      transferOff.current?.();
      transferOff.current = listenForTransfer({ task, group, tasks, actions, droppable: binDroppable(task, actions), onDrop: clear });
    },
    // dragend: a clone the browser ended without a drop still runs.
    end() { transferOff.current?.({ ended: true }); clear(); },
    overCell(event, to) {
      const drop = hit(event, to);
      const key = drop ? to.key || `${to.lane}|${to.column}` : '';
      const slot = drop?.slot ? `${drop.slot.id}|${drop.slot.where}` : '';
      if ((over?.key || '') !== key || (over?.slotKey || '') !== slot || (over?.label || '') !== (drop?.label || '')) {
        setOver(drop ? { key, label: drop.label, slot: drop.slot || null, slotKey: slot, inert: !!drop.inert } : null);
      }
    },
    leaveGrid(event) {
      if (dragged.current && !event.currentTarget.contains(event.relatedTarget)) setOver(null);
    },
    async dropCell(event, to) {
      const drop = hit(event, to);
      if (!drop || drop.inert) { if (drop) clear(); return; }
      const { task } = dragged.current;
      clear();
      if (drop.reason) { setBlocking({ task, drop }); return; }
      if (drop.kind === 'reorder') { await actions.reorder(task, drop.position); return; }
      const ok = await actions.run(`${task.id}:lane`, drop.label, (api) => drop.run(api));
      if (ok && drop.epicMove) epicMoved(actions, task, drop.label);
      if (ok && (drop.baseKind || drop.kind) === 'assign') markLanded({ group: group.name, node: group.fleet_node?.id || '', section: 'agents', ids: [drop.agent] });
    },
  };
}

// BacklogColumn is the Backlog, shown (tk_5d2547b0): one column before the
// lanes, in no lane, with the board's parked tasks in their order. It takes
// a card from any cell (parked, in whoever's hands it is), and its cards
// drag out onto any lane's Ideas, Next or In progress (backlogDrop) or
// within it to reorder. Each card's ⋯ has Back to ideas. onHide hides it.
export function BacklogColumn({ entry, group, snapshot, drag, actions = null, readApi = null, onEdit = null, onHide = null }) {
  const grouping = useReadyGrouping();
  const column = { key: BACKLOG, label: 'Backlog' };
  const grouped = grouping.has(BACKLOG);
  const shown = groupedCell(backlogTasks(entry), grouped);
  const { list } = shown;
  const ready = readyMenuItem(column, grouping);
  const byId = boardTasks(entry);
  const target = { resolve: (fromLane) => fromLane, column: BACKLOG, key: BACKLOG, list, grouped };
  const over = drag.over?.key === BACKLOG;
  const slot = over ? drag.over.slot : null;
  const where = 'the backlog';
  return html`<section class=${`tb-backlog${over ? (drag.over.inert ? ' tb-cell-inert' : ' tb-cell-over') : ''}`} data-tb-backlog-column=${group.name}
      aria-label="Backlog" data-tb-drop=${over ? drag.over.label : undefined}
      onDragOver=${actions ? (event) => drag.overCell(event, target) : undefined}
      onDragLeave=${actions ? drag.leaveGrid : undefined}
      onDrop=${actions ? (event) => { void drag.dropCell(event, target); } : undefined}>
    <div class="ui-eyebrow tb-backlog-head">Backlog <span class="tb-col-count">${list.length}</span><${ReadyChip} column=${column} />${
      ready && html`<span class="tb-col-menu"><${RowMenu} label="the Backlog column" items=${[ready]} buttonClass="tb-col-more" title="More for the Backlog column" /></span>`}${onHide && html`<${Button} size="sm" variant="ghost" icon=${true}
      class="tb-backlog-hide" title="Hide the backlog" aria-label="Hide the backlog" onClick=${onHide}>✕<//>`}</div>
    <div class="tb-backlog-note">Parked: never started, queued or nudged.</div>
    ${list.map((task) => [shown.heads?.get(task.id) && html`<${ReadyHead} key=${`ready-${task.id}`} head=${shown.heads.get(task.id)} />`,
      html`<${Card} key=${task.id} task=${task} byId=${byId} api=${readApi || actions?.api || null} nameOf=${(actor) => actorLabel(actor, group, snapshot)}
      drag=${actions ? { ...drag, start: (event, t) => drag.start(event, t, { lane: t.assignee?.agent_id || UNASSIGNED, column: BACKLOG }) } : null}
      menu=${[...orderItems(task, shown.peers(task), actions, where), ...backlogItems(task, actions), ...archiveItems(task, actions)]} onEdit=${actions ? onEdit : null}
      dropAt=${slot?.id === task.id ? slot.where : ''} onKey=${actions ? (event) => onOrderKey(event, task, shown.peers(task), actions) : null} />`])}
    ${!list.length && html`<div class="ui-empty tb-backlog-empty">${actions ? 'Empty. Drag a card here, or use its ⋯ Move to backlog, to park it.' : 'Empty.'}</div>`}
  </section>`;
}

// BlockDialog asks the reason a drop onto Blocked needs, in the board's
// reason dialog, then runs the drop (with an assign or unassign first, for a
// move across lanes too).
export function BlockDialog({ task, drop, actions, group, onClose }) {
  const submit = async (reason) => {
    const ok = await actions.run(`${task.id}:lane`, 'Block', (api) => drop.run(api, reason));
    if (ok && drop.epicMove) epicMoved(actions, task, drop.label.replace(/ \(asks why\)$/, ''));
    if (ok && drop.agent) markLanded({ group: group.name, node: group.fleet_node?.id || '', section: 'agents', ids: [drop.agent] });
    return ok;
  };
  return html`<${TaskReasonDialog} kind="block" task=${task} action=${drop.kind === 'block' ? '' : drop.label.replace(/ \(asks why\)$/, '')}
    busy=${actions.busy === `${task.id}:lane`} onSubmit=${submit} onClose=${onClose} />`;
}

// `link` (cross-links.js) makes an agent's name a link to its row.
export function LaneHead({ lane, order, link = null }) {
  if (lane.id === OPERATOR) {
    const you = lane.name === 'you';
    return html`<div class="tb-lane-head tb-lane-head-operator" data-lane-operator="true" title=${you ? 'Tasks assigned to you, the operator: agents wait on them' : `Tasks assigned to the ${lane.name}`}>
      <span class="tb-lane-name"><span class="tb-avatar tb-avatar-operator" aria-hidden="true">★</span><span class="tb-lane-label">${you ? 'You' : lane.name}</span></span>
      <span class="tb-lane-sub tb-lane-waiting">${lane.holds} waiting on ${lane.name}</span>
    </div>`;
  }
  if (lane.id === UNASSIGNED) {
    return html`<div class="tb-lane-head"><span class="tb-lane-name">Unassigned</span><span class="tb-lane-sub">${lane.count} task${lane.count === 1 ? '' : 's'} · operator order</span></div>`;
  }
  const marker = order?.over?.kind === 'lane' && order.over.key === lane.id ? (order.over.after ? ' tb-order-after' : ' tb-order-before') : '';
  return html`<div class=${`tb-lane-head${marker}`} data-lane-agent=${lane.id} ...${order ? order.laneProps(lane) : {}}>
    <span class=${`tb-lane-name${link ? ' tb-xlink' : ''}`} ...${link || {}} title=${lane.name}><span class="tb-avatar" aria-hidden="true">${lane.name.slice(0, 1).toUpperCase()}</span><span class="tb-lane-label">${lane.name}</span></span>
    <span class=${`tb-lane-sub tb-lane-state-${lane.state.replace(/\s+/g, '-')}`}>${lane.state}${lane.member?.owner ? ' · owner' : ''}</span>
    <span class="tb-lane-sub">holds ${lane.holds}</span>
  </div>`;
}

const LANE_DRAG_TYPE = 'application/x-tclaude-lane';

// layoutKey is the group the lanes' layout is kept under: the name the view
// shows (name@node for a linked node's group), as the section's own view
// prefs are, so two nodes' same-named groups keep their own layout.
function layoutKey(group) {
  return group.shownName || group.name;
}

// useLaneOrder drags a lane head onto another lane head in its heading, or a
// heading onto another heading, to reorder them (a view preference, stored
// per group for this viewer). Alt+↑/↓ on a focused head does the same.
// Unassigned stays first.
function useLaneOrder(group, groups, by, onChange) {
  const dragged = useRef(null);
  const [over, setOver] = useState(null);
  // refocus is the head an Alt+arrow just moved: the re-render moves its
  // row in the DOM, which drops focus, so it is focused again.
  const refocus = useRef(null);
  useLayoutEffect(() => {
    const target = refocus.current;
    if (!target) return;
    refocus.current = null;
    const grid = [...document.querySelectorAll('[data-taskboard-lanes]')].find((el) => el.getAttribute('data-taskboard-lanes') === group.name);
    const attr = target.kind === 'lane' ? 'data-lane-agent' : 'data-lane-group';
    const el = grid && [...grid.querySelectorAll(`[${attr}]`)].find((node) => node.getAttribute(attr) === target.key);
    if (el && document.activeElement !== el) el.focus();
  });
  const groupOf = (laneId) => groups.find((g) => g.lanes.some((lane) => lane.id === laneId));
  const apply = (kind, moving, target, after) => {
    if (kind === 'lane') moveLane(layoutKey(group), groups, by, moving, target, after);
    else moveHead(layoutKey(group), groups, by, moving, target, after);
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
      refocus.current = { kind, key };
      apply(kind, key, list[j], event.key === 'ArrowDown');
    },
  });
  return {
    over,
    laneProps: (lane) => props('lane', lane.id, () => (groupOf(lane.id)?.lanes || []).map((l) => l.id)),
    headProps: (lg) => props('head', lg.key, () => headedGroups(groups).map((g) => g.key)),
  };
}

// useColumnResize drags a column head's right edge to resize the column;
// a double-click resets every width. Arrow keys on a focused edge move it
// 16px. Widths are stored per group for this viewer.
function useColumnResize(group, onChange, columns = LANE_COLUMNS) {
  const keys = ['lane', ...columns.map((column) => column.key)];
  const [live, setLive] = useState(null);
  const gridRef = useRef(null);
  const measure = () => {
    const head = gridRef.current?.querySelector('.tb-lane-columns');
    const cells = head ? [...head.children].filter((el) => !el.classList.contains('tb-col-resize')) : [];
    return cells.map((el) => el.getBoundingClientRect().width);
  };
  const commit = (widths) => { setLaneWidths(layoutKey(group), widths); setLive(null); onChange(); };
  const handle = (index, label) => ({
    role: 'separator', 'aria-orientation': 'vertical', tabindex: '0', 'aria-label': `Resize the ${label} column`,
    title: 'Drag to resize · double-click to reset all widths · ←/→ when focused',
    'data-col-resize': keys[index],
    onPointerDown: (event) => {
      if (event.button !== undefined && event.button !== 0) return;
      event.preventDefault();
      event.stopPropagation();
      const measured = measure();
      if (measured.length !== keys.length) return;
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
      if (measured.length !== keys.length) return;
      event.preventDefault();
      commit(resizeColumns(measured, index, event.key === 'ArrowLeft' ? -16 : 16));
    },
    onClick: (event) => event.stopPropagation(),
  });
  return { gridRef, handle, stages: columns.length, widths: live || laneWidths(layoutKey(group), columns.length) };
}

// laneGridStyle sizes the grid for its columns: the stored widths, or the
// default template when a board's custom columns make more than five.
export function laneGridStyle(resize) {
  const cols = gridColumns(resize.widths, resize.stages);
  return `${cols ? `--tb-lane-cols: ${cols}; ` : ''}--tb-lane-min: ${gridMinWidth(resize.widths, resize.stages)}px`;
}

// TaskboardLanes is the section's swim-lane view of the same board the list
// shows. Without `actions` it is read-only: no drags.
// CellAdd is a cell's "+": a small button in its lower-right corner that
// opens the add menu (the section's ＋▾ menu, anchored here); whichever
// choice is made starts from the cell: its column, and the lane's agent as
// the assignee.
export function CellAdd({ lane, column, add, onNewTask, wizard, group, entry, parent = '', where: rowName = '' }) {
  const where = `${rowName || (lane.id === UNASSIGNED ? 'Unassigned' : lane.id === OPERATOR && lane.name === 'you' ? 'You' : lane.name)} · ${wizard ? column.wizard : column.label}`;
  const what = wizard ? 'quest' : 'task';
  const initial = { start: add.start, to: add.to, toName: add.to ? lane.name : '', ...(parent ? { parent } : {}) };
  return html`<${NewTaskMenu} group=${group} entry=${entry} wizard=${wizard}
    cell=${{ title: `New ${what} here: ${where}`, label: `New ${what} in ${where}` }}
    onChoose=${(choice) => onNewTask(choice, initial)} />`;
}

export function TaskboardLanes({ entry, group, snapshot, by = 'agent', actions = null, readApi = null, wizard = false, now = Date.now(), onNewTask = null, onLayout = null, onEdit = null, backlog = false, onHideBacklog = null }) {
  const tasks = entry.tasks || [];
  const byId = boardTasks(entry);
  const [, bumpLayout] = useState(0);
  const relayout = () => { bumpLayout((n) => n + 1); onLayout?.(); };
  const model = laneModel(entry, group, by, snapshot, now);
  const { cells } = model;
  const columns = boardColumns(entry);
  const custom = entry.columns || [];
  const groups = applyLaneOrder(model.groups, laneOrder(layoutKey(group)), by);
  const drag = useLaneDrag(actions, group, tasks);
  const order = useLaneOrder(group, groups, by, relayout);
  const resize = useColumnResize(group, relayout, columns);
  const naming = useColumnNaming(group, actions);
  const limits = useLimitDialog(group, entry, actions);
  const [unfolded, setUnfolded] = useState({});
  const [open, setOpen] = useOpenCells(layoutKey(group), `lanes-${by}`);
  const grouping = useReadyGrouping();
  useCellReveal(cells, CELL_SHOWN, setOpen, grouping);
  // laneTasks are the ids of the tasks in a lane's cells.
  const laneTasks = (lane) => columns.flatMap((column) => (cells.get(`${lane.id}|${column.key}`) || []).map((task) => task.id));
  // A cross-link to a task in a folded offline lane unfolds that lane group,
  // as its cell opens past the cap (useCellReveal).
  const want = taskRevealRequest.value;
  useEffect(() => {
    if (!want || Date.now() - want.at > REVEAL_FRESH_MS) return;
    const lg = groups.find((g) => g.offline && g.lanes.some((lane) => laneTasks(lane).includes(want.taskId)));
    if (lg) setUnfolded((now) => (now[lg.key] ? now : { ...now, [lg.key]: true }));
  }, [want?.seq]);
  const [answering, setAnswering] = useState('');
  const answerable = canAssign(actions) && typeof actions?.api?.answer === 'function';
  // landIn opens a full cell a task is dropped or added into: the task lands
  // at its place in the order, which may be past the cap, and would vanish
  // behind "+ n more".
  const [landed, setLanded] = useState(null);
  const landIn = (key, list, id = '') => {
    if (list.length >= CELL_SHOWN && !open[key]) setOpen({ ...open, [key]: true });
    if (id) setLanded({ key, id });
  };
  const cell = (lane, column) => {
    const key = `${lane.id}|${column.key}`;
    const grouped = groupable(column) && grouping.has(column.key);
    const shown = groupedCell(cells.get(key) || [], grouped);
    const { list } = shown;
    const stripe = striped(lane.id, column.key);
    const assignable = lane.id === UNASSIGNED || lane.id === OPERATOR || !!lane.member;
    const target = { lane: lane.id, column: column.key, columnLabel: column.label, columns: custom, name: lane.name, member: assignable, list, grouped };
    const over = drag.over?.key === key;
    const slot = over ? drag.over.slot : null;
    const where = cellName(lane, column);
    // A cordoned column takes no new task; a card already in it may still
    // move within it (reorder, or another lane's hands).
    const takes = actions && (!column.cordoned || byId.get(drag.dragging)?.column === column.slug);
    const add = actions && !column.cordoned && onNewTask && cellAdd(lane.id, column.key, { assign: canAssign(actions), member: assignable });
    return html`<div key=${key} class=${`tb-cell${stripe ? ' tb-cell-striped' : ''}${cellClass(column)}${over ? (drag.over.inert ? ' tb-cell-inert' : ' tb-cell-over') : ''}${add ? ' tb-cell-addable' : ''}`}
        data-lane=${lane.id || 'unassigned'} data-column=${column.key} data-tb-drop=${over ? drag.over.label : undefined}
        onDragOver=${takes ? (event) => drag.overCell(event, target) : undefined}
        onDrop=${takes ? (event) => { if (over) landIn(key, list, drag.dragging); void drag.dropCell(event, target); } : undefined}>
      <${CellCards} list=${list} limit=${CELL_SHOWN} open=${!!open[key]} reveal=${landed?.key === key ? landed.id : ''} onToggle=${(on) => setOpen({ ...open, [key]: on })}
        card=${(task) => [shown.heads?.get(task.id) && html`<${ReadyHead} key=${`ready-${task.id}`} head=${shown.heads.get(task.id)} />`,
          html`<${Card} key=${task.id} task=${task} byId=${byId} api=${readApi || actions?.api || null} nameOf=${(actor) => actorLabel(actor, group, snapshot)}
          drag=${actions ? { ...drag, start: (event, t) => drag.start(event, t, { lane: lane.id, column: column.key }) } : null}
          menu=${[...orderItems(task, shown.peers(task), actions, where), ...backlogItems(task, backlogSupported(entry) ? actions : null), ...archiveItems(task, actions)]} onEdit=${actions ? onEdit : null} onAnswer=${answerable ? setAnswering : null}
          dropAt=${slot?.id === task.id ? slot.where : ''} onKey=${actions ? (event) => onOrderKey(event, task, shown.peers(task), actions) : null} />`]} />
      ${lane.id === OPERATOR && column.key === 'progress' && model.questions.map(({ task, question }) => html`<${QuestionCard} key=${`${task.id}#${question.n}`}
        task=${task} question=${question} askedBy=${actorLabel(question.asked_by, group, snapshot)} onOpen=${actions ? onEdit : null}
        onAnswer=${answerable ? (id, n) => setAnswering(`${id}#${n}`) : null} />`)}
      ${add && html`<${CellAdd} lane=${lane} column=${column} add=${add} wizard=${wizard} group=${group} entry=${entry}
        onNewTask=${(choice, initial) => { landIn(key, list); onNewTask(choice, initial); }} />`}
    </div>`;
  };
  const row = (lane) => html`<div key=${lane.id || 'unassigned'} class=${`tb-lane${lane.id === OPERATOR ? ' tb-lane-operator' : ''}`} data-lane-row=${lane.id || 'unassigned'}>
    <${LaneHead} lane=${lane} order=${lane.id === UNASSIGNED || lane.id === OPERATOR ? null : order}
      link=${lane.member ? agentLink({ group: group.name, node: group.fleet_node?.id || '', id: lane.id, name: lane.name }) : null} />${columns.map((column) => cell(lane, column))}
  </div>`;
  // The operator's lane shows while it holds a task, or for a viewer who
  // may give the operator one (by a drag or its "+").
  const operatorLane = ([...cells.keys()].some((key) => key.startsWith(`${OPERATOR}|`)) || model.questions.length > 0 || canAssign(actions))
    && { id: OPERATOR, name: operatorName(group), holds: model.operatorHolds };
  const unassigned = { id: UNASSIGNED, name: 'Unassigned',
    count: columns.reduce((sum, column) => sum + (column.key === 'done' ? 0 : (cells.get(`|${column.key}`) || []).length), 0) };
  const count = (key) => [...cells.entries()].reduce((sum, [k, list]) => sum + (k.endsWith(`|${key}`) ? list.length : 0), 0);
  const target = answerTarget(answering);
  const answerTask = answering && tasks.find((task) => task.id === target.id);
  return html`<div class="tb-lanes-wrap">
    ${naming.dialog}${limits.dialog}
    ${answerTask && html`<${AnswerFor} task=${answerTask} n=${target.n} actions=${actions} askedBy=${actorLabel(askerOf(answerTask, target.n), group, snapshot)} onClose=${() => setAnswering('')} />`}
    ${drag.blocking && actions && html`<${BlockDialog} task=${drag.blocking.task} drop=${drag.blocking.drop} actions=${actions}
      group=${group} onClose=${() => drag.setBlocking(null)} />`}
    <div class=${backlog ? 'tb-lanes-split' : undefined}>
    ${backlog && html`<${BacklogColumn} entry=${entry} group=${group} snapshot=${snapshot} drag=${drag} actions=${actions} readApi=${readApi} onEdit=${onEdit} onHide=${onHideBacklog} />`}
    <div class="tb-lanes" data-taskboard-lanes=${group.name} data-lanes-by=${by} ref=${resize.gridRef}
        style=${laneGridStyle(resize)}
        onDragLeave=${actions ? drag.leaveGrid : undefined}>
      <div class="tb-lane tb-lane-columns">
        <div class="ui-eyebrow tb-col-lane">Lane<span class="tb-col-resize" ...${resize.handle(0, 'lane')}></span></div>
        ${columns.map((column, i) => html`<div key=${column.key} class=${columnClass(column)} data-col=${column.key}
            title=${column.cordoned ? 'Cordoned: it takes no new task; its tasks may leave' : undefined}>${wizard ? column.wizard : column.label} <span class="tb-col-count">${column.key === 'done' ? (entry.counts?.done_this_week ?? count('done')) : count(column.key)}</span>${
          column.key === 'done' && html`<span class="tb-col-note" title="Ordered by when each task closed, newest first: not by hand">by close time</span>`}<${LaneLimit} entry=${entry} column=${column} /><${ReadyChip} column=${column} />${
          headMenu(column, entry) && html`<${ColumnHeadMenu} column=${column} columns=${custom} group=${group} actions=${actions} entry=${entry} onRename=${(c) => naming.open('rename', c)} limits=${limits} />`}${
          i === columns.length - 1 && html`<${AddColumnButton} naming=${naming} columns=${custom} />`}${
          i < columns.length - 1 && html`<span class="tb-col-resize" ...${resize.handle(i + 1, column.label)}></span>`}</div>`)}
      </div>
      ${operatorLane && row(operatorLane)}
      ${row(unassigned)}
      ${groups.map((lg) => {
        const folded = lg.offline && !unfolded[lg.key];
        const head = lg.label || lg.offline;
        const movable = !!lg.label && lg.key !== 'former' && headedGroups(groups).length > 1;
        const marker = order.over?.kind === 'head' && order.over.key === lg.key ? (order.over.after ? ' tb-order-after' : ' tb-order-before') : '';
        return [
          head && html`<div key=${`${lg.key}-head`} class=${`tb-lane-group${marker}`} data-lane-group=${lg.key} ...${movable ? order.headProps(lg) : {}}>
            <span class="tb-lane-group-name"><span class="ui-eyebrow">${lg.label}</span>
            <span class="tb-lane-sub">${lg.lanes.length} agent${lg.lanes.length === 1 ? '' : 's'}${lg.offline ? ' · offline' : ''}</span></span>
            ${lg.offline && html`<${Button} size="sm" variant="link" class="tb-lane-fold" aria-expanded=${folded ? 'false' : 'true'}
              data-hidden-tasks=${folded ? lg.lanes.flatMap(laneTasks).join(' ') : undefined}
              onClick=${() => setUnfolded({ ...unfolded, [lg.key]: folded })}>${folded ? '▸ show offline lanes' : '▾ fold offline lanes'}<//>`}
          </div>`,
          ...(folded ? [] : lg.lanes.map(row)),
        ];
      })}
    </div>
    </div>
  </div>`;
}
