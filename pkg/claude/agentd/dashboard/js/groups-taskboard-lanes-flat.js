// groups-taskboard-lanes-flat.js -- two of the lanes view's "lanes" modes
// (docs/taskboard.md) that do not give each agent a lane. Each card says who
// holds it with a corner marker: a yellow "!" when nobody does, a red "!"
// when the operator does, else the agent's avatar.
//
//   triage   three rows: You (the operator's tasks), Unassigned, and Agents
//            (every agent's tasks in one row). A drag onto You assigns the
//            task to the operator and onto Unassigned unassigns it, as the
//            lanes do. Agents does not say which agent, so a card from
//            another row is not taken there; an agent's card moves along it.
//   one row  every task in a single row. A drag moves a card between
//            columns and keeps its assignee.
//
// The drags, the cell "+" and the cards are the lanes' own
// (groups-taskboard-lanes.js); a cell draws its first FLAT_SHOWN cards and
// "+ n more".

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import {
  BlockDialog, Card, CellAdd, CellCards, LANE_COLUMNS, LaneHead, OPERATOR, UNASSIGNED, cellAdd, laneColumn, striped, useCellReveal, useLaneDrag,
} from './groups-taskboard-lanes.js';
import { actorLabel, agentLabel, archiveItems, boardTasks, canAssign, operatorName } from './groups-taskboard.js';
import { agentLink } from './cross-links.js';

const html = htm.bind(h);

// FLAT_SHOWN is how many cards a cell draws before "+ n more": more than a
// lane's, since a row here holds many lanes' tasks.
export const FLAT_SHOWN = 5;
// FLAT_MODES are the "lanes" modes this file draws.
export const FLAT_MODES = ['triage', 'row'];
// AGENTS is the Triage row of every agent's tasks.
export const AGENTS = 'agents';
const ALL = 'row';
const MIN_STAGE_PX = 90;
const HEAD_PX = 130;

// taskLane is the lanes' lane a task stands in: its agent, the operator, or
// Unassigned.
function taskLane(task) {
  return task.assignee?.agent_id || UNASSIGNED;
}

// flatRow is the row a task stands in for a mode.
export function flatRow(task, mode) {
  if (mode === ALL) return ALL;
  const lane = taskLane(task);
  return lane === OPERATOR || lane === UNASSIGNED ? lane : AGENTS;
}

// flatModel lays the board out per mode: the cells' tasks per row and
// column, in the board's order (done tasks newest first), as the lanes do.
export function flatModel(entry, mode, now = Date.now()) {
  const cells = new Map();
  const closed = [...(entry?.closed || [])].sort((a, b) => String(b.closed_at || '').localeCompare(String(a.closed_at || '')));
  for (const task of [...(entry?.tasks || []), ...closed]) {
    const column = laneColumn(task, now);
    if (!column) continue;
    const key = `${flatRow(task, mode)}|${column}`;
    if (!cells.has(key)) cells.set(key, []);
    cells.get(key).push(task);
  }
  return cells;
}

// cardMarker is a card's corner marker: who holds the task. A done task
// needs nobody, so it carries no "!".
export function cardMarker(task, group, snapshot) {
  const lane = taskLane(task);
  const done = task.status === 'done';
  if (lane === UNASSIGNED) {
    if (done) return null;
    return { class: 'tb-card-unassigned', badge: html`<span class="tb-card-marker tb-marker-unassigned" title="Unassigned" aria-label="Unassigned">!</span>` };
  }
  if (lane === OPERATOR) {
    const who = operatorName(group);
    const title = who === 'you' ? 'Assigned to you' : `Assigned to the ${who}`;
    if (done) return { class: '', badge: html`<span class="tb-card-marker tb-avatar tb-avatar-operator" title=${title} aria-label=${title}>★</span>` };
    return { class: 'tb-card-operator', badge: html`<span class="tb-card-marker tb-marker-operator" title=${title} aria-label=${title}>!</span>` };
  }
  // The member's current title, as its lane would say; a former member by
  // the name the task recorded.
  const member = (group?.members || []).find((m) => m.agent_id === lane);
  const name = member?.title || task.assignee?.name || agentLabel(lane, group, snapshot) || lane;
  // A member's avatar links to its row (cross-links.js).
  const link = member ? agentLink({ group: group.name, node: group.fleet_node?.id || '', id: lane, name }) : {};
  return { class: '', badge: html`<span class=${`tb-card-marker tb-avatar${member ? ' tb-xlink' : ''}`} ...${link} title=${name} aria-label=${`Assigned to ${name}`}>${String(name).slice(0, 1).toUpperCase()}</span>` };
}

// rowTarget is what a drop on a row's cell means: resolve(fromLane) is the
// lanes' lane it lands in, or null where it is not taken.
function rowTarget(row, column, group) {
  const key = `${row}|${column}`;
  if (row === ALL) return { key, column, resolve: (from) => from };
  if (row === OPERATOR) return { key, column, lane: OPERATOR, name: operatorName(group), member: true };
  if (row === UNASSIGNED) return { key, column, lane: UNASSIGNED, name: 'Unassigned', member: true };
  return { key, column, resolve: (from) => (from === UNASSIGNED || from === OPERATOR ? null : from) };
}

// rowStriped reports the Triage cells that cannot hold a card, as the lanes
// stripe them: the Agents row is an agent's lane.
function rowStriped(row, column) {
  if (row === ALL) return false;
  return striped(row === AGENTS ? AGENTS : row, column);
}

export function TaskboardFlat({ entry, group, snapshot, mode = 'triage', actions = null, readApi = null, wizard = false, now = Date.now(), onNewTask = null, onEdit = null }) {
  const tasks = entry.tasks || [];
  const byId = boardTasks(entry);
  const cells = flatModel(entry, mode, now);
  const drag = useLaneDrag(actions, group, tasks);
  const [open, setOpen] = useState({});
  useCellReveal(cells, FLAT_SHOWN, setOpen);
  const nameOf = (actor) => actorLabel(actor, group, snapshot);
  const [landed, setLanded] = useState(null);
  const landIn = (key, list, id = '') => {
    if (list.length >= FLAT_SHOWN && !open[key]) setOpen({ ...open, [key]: true });
    if (id) setLanded({ key, id });
  };
  const cell = (row, column) => {
    const key = `${row}|${column.key}`;
    const list = cells.get(key) || [];
    const stripe = rowStriped(row, column.key);
    const target = rowTarget(row, column.key, group);
    const over = drag.over?.key === key;
    // A cell's "+" starts a task in its column: unassigned, or in You the
    // operator's. Agents does not say which agent.
    const addLane = row === OPERATOR ? OPERATOR : row === AGENTS ? null : UNASSIGNED;
    const add = actions && onNewTask && addLane !== null && cellAdd(addLane, column.key, { assign: canAssign(actions), member: true });
    const lane = addLane === OPERATOR ? { id: OPERATOR, name: operatorName(group) } : { id: UNASSIGNED, name: 'Unassigned' };
    return html`<div key=${key} class=${`tb-cell${stripe ? ' tb-cell-striped' : ''}${over ? ' tb-cell-over' : ''}${add ? ' tb-cell-addable' : ''}`}
        data-board-row=${row || 'unassigned'} data-column=${column.key} data-tb-drop=${over ? drag.over.label : undefined}
        onDragOver=${actions ? (event) => drag.overCell(event, target) : undefined}
        onDrop=${actions ? (event) => { if (over) landIn(key, list, drag.dragging); void drag.dropCell(event, target); } : undefined}>
      <${CellCards} list=${list} limit=${FLAT_SHOWN} open=${!!open[key]} reveal=${landed?.key === key ? landed.id : ''} onToggle=${(on) => setOpen({ ...open, [key]: on })}
        card=${(task) => html`<${Card} key=${task.id} task=${task} byId=${byId} api=${readApi || actions?.api || null} nameOf=${nameOf}
          marker=${cardMarker(task, group, snapshot)}
          drag=${actions ? { ...drag, start: (event, t) => drag.start(event, t, { lane: taskLane(t), column: column.key }) } : null}
          menu=${archiveItems(task, actions)} onEdit=${actions ? onEdit : null} />`} />
      ${add && html`<${CellAdd} lane=${lane} column=${column} add=${add} wizard=${wizard} group=${group} entry=${entry}
        onNewTask=${(choice, initial) => { landIn(key, list); onNewTask(choice, initial); }} />`}
    </div>`;
  };
  const rowCount = (row) => LANE_COLUMNS.reduce((sum, column) => sum + (column.key === 'done' ? 0 : (cells.get(`${row}|${column.key}`) || []).length), 0);
  const head = (row) => {
    if (row === OPERATOR) return html`<${LaneHead} lane=${{ id: OPERATOR, name: operatorName(group), holds: (cells.get(`${OPERATOR}|progress`) || []).length }} />`;
    if (row === UNASSIGNED) return html`<${LaneHead} lane=${{ id: UNASSIGNED, name: 'Unassigned', count: rowCount(UNASSIGNED) }} />`;
    const n = rowCount(AGENTS);
    return html`<div class="tb-lane-head"><span class="tb-lane-name">Agents</span><span class="tb-lane-sub">${n} task${n === 1 ? '' : 's'} · every agent</span></div>`;
  };
  const triage = mode !== ALL;
  // You shows while the operator holds a task, or for a viewer who may give
  // the operator one, as the operator's lane does.
  const rows = triage
    ? [...(([...cells.keys()].some((key) => key.startsWith(`${OPERATOR}|`)) || canAssign(actions)) ? [OPERATOR] : []), UNASSIGNED, AGENTS]
    : [ALL];
  const count = (column) => [...cells.entries()].reduce((sum, [k, list]) => sum + (k.endsWith(`|${column}`) ? list.length : 0), 0);
  const cols = `${triage ? `${HEAD_PX}px ` : ''}repeat(5, minmax(${MIN_STAGE_PX}px, 1fr))`;
  const min = (triage ? HEAD_PX : 0) + 5 * MIN_STAGE_PX;
  return html`<div class="tb-lanes-wrap">
    ${drag.blocking && actions && html`<${BlockDialog} task=${drag.blocking.task} drop=${drag.blocking.drop} actions=${actions}
      group=${group} onClose=${() => drag.setBlocking(null)} />`}
    <div class=${`tb-lanes tb-lanes-flat${triage ? '' : ' tb-lanes-row'}`} data-taskboard-lanes=${group.name} data-lanes-by=${mode}
        style=${`--tb-lane-cols: ${cols}; --tb-lane-min: ${min}px`}
        onDragLeave=${actions ? drag.leaveGrid : undefined}>
      <div class="tb-lane tb-lane-columns">
        ${triage && html`<div class="ui-eyebrow tb-col-lane">Who</div>`}
        ${LANE_COLUMNS.map((column) => html`<div key=${column.key} class=${`ui-eyebrow tb-col-${column.key}`}>${wizard ? column.wizard : column.label} <span class="tb-col-count">${column.key === 'done' ? (entry.counts?.done_this_week ?? count('done')) : count(column.key)}</span></div>`)}
      </div>
      ${rows.map((row) => html`<div key=${row || 'unassigned'} class=${`tb-lane${row === OPERATOR ? ' tb-lane-operator' : ''}`} data-board-row-head=${row || 'unassigned'}>
        ${triage && head(row)}${LANE_COLUMNS.map((column) => cell(row, column))}
      </div>`)}
    </div>
  </div>`;
}
