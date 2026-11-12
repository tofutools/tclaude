// groups-taskboard-lanes-flat.js -- two of the lanes view's "lanes" modes
// (docs/taskboard.md) that do not give each agent a lane. Each card says who
// holds it with a corner marker: a yellow "!" when nobody does, a red "!"
// when the operator does, else the agent's avatar. In Triage the You and
// Unassigned rows say it already, so their cards carry none (lanesSay).
//
//   triage   three rows: You (the operator's tasks), Unassigned, and Agents
//            (every agent's tasks in one row). A drag onto You assigns the
//            task to the operator and onto Unassigned unassigns it, as the
//            lanes do. Agents does not say which agent, so a card from
//            another row is not taken there; an agent's card moves along it.
//   one row  every task in a single row. A drag moves a card between
//            columns and keeps its assignee.
//   epic     one row per open epic (the strip's), its head the epic itself
//            (name, progress, open), and No epic last. A drag between rows
//            moves the task to the other epic, with Undo; between columns
//            it moves as one row does. An epic still an idea has no row: it
//            is a card in No epic's Ideas until promoted.
//
// The drags, the cell "+" and the cards are the lanes' own
// (groups-taskboard-lanes.js); a cell draws its first FLAT_SHOWN cards and
// "+ n more".

import { h } from 'preact';
import { useContext, useState } from 'preact/hooks';
import htm from 'htm';
import {
  BlockDialog, Card, CellAdd, CellCards, LaneHead, OPERATOR, UNASSIGNED, boardColumns, cellAdd, cellClass, columnClass, laneColumn, operatorQuestions, striped, useCellReveal, useLaneDrag,
} from './groups-taskboard-lanes.js';
import { actorLabel, agentLabel, archiveItems, boardTasks, canAssign, operatorName } from './groups-taskboard.js';
import { agentLink } from './cross-links.js';
import { useOpenCells } from './groups-taskboard-lanes-layout.js';
import { AnswerFor, QuestionCard, answerTarget, askerOf } from './groups-taskboard-answer.js';
import { AddColumnButton, ColumnHeadMenu, useColumnNaming } from './groups-taskboard-columns.js';
import { LaneLimit, useLimitDialog } from './groups-taskboard-limits.js';
import { HIDEABLE_BUILTINS } from './groups-taskboard-colvis.js';
import { EpicBoard, EpicNudge, EpicProgress, NO_EPIC, epicColour, epicOf, progressParts } from './groups-taskboard-epics.js';

const html = htm.bind(h);

// FLAT_SHOWN is how many cards a cell draws before "+ n more": more than a
// lane's, since a row here holds many lanes' tasks.
export const FLAT_SHOWN = 5;
// FLAT_MODES are the "lanes" modes this file draws.
export const FLAT_MODES = ['triage', 'row', 'epic'];
// EPIC_ROWS is the mode of a row per epic.
export const EPIC_ROWS = 'epic';
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

// flatRow is the row a task stands in for a mode; in epic mode, epics is
// the live epics by id (a task of a closed epic is in No epic).
export function flatRow(task, mode, epics = null) {
  if (mode === ALL) return ALL;
  if (mode === EPIC_ROWS) return epicOf(task, epics) || NO_EPIC;
  const lane = taskLane(task);
  return lane === OPERATOR || lane === UNASSIGNED ? lane : AGENTS;
}

// flatModel lays the board out per mode: the cells' tasks per row and
// column, in the board's order (done tasks newest first), as the lanes do.
export function flatModel(entry, mode, now = Date.now(), epics = null) {
  const cells = new Map();
  const closed = [...(entry?.closed || [])].sort((a, b) => String(b.closed_at || '').localeCompare(String(a.closed_at || '')));
  for (const task of [...(entry?.tasks || []), ...closed]) {
    const column = laneColumn(task, now);
    if (!column) continue;
    const key = `${flatRow(task, mode, epics)}|${column}`;
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

// lanesSay reports a card whose Triage row already says who holds it (You,
// Unassigned): it carries no marker, nor the marker's coloured edge. The
// Agents row's cards keep their avatars, and one row keeps every marker.
export function lanesSay(mode, task) {
  if (mode === ALL || mode === EPIC_ROWS) return false;
  const lane = taskLane(task);
  return lane === UNASSIGNED || lane === OPERATOR;
}

// rowTarget is what a drop on a row's cell means: resolve(fromLane) is the
// lanes' lane it lands in, or null where it is not taken.
function rowTarget(row, col, group, columns = [], epicName = '') {
  const column = col.key;
  const key = `${row}|${column}`;
  const at = { key, column, columnLabel: col.label, columns };
  if (row === ALL) return { ...at, resolve: (from) => from };
  // An epic's row keeps the card's hands and puts it in the epic.
  if (epicName) return { ...at, resolve: (from) => from, epic: row === NO_EPIC ? '' : row, epicName };
  if (row === OPERATOR) return { ...at, lane: OPERATOR, name: operatorName(group), member: true };
  if (row === UNASSIGNED) return { ...at, lane: UNASSIGNED, name: 'Unassigned', member: true };
  return { ...at, resolve: (from) => (from === UNASSIGNED || from === OPERATOR ? null : from) };
}

// rowStriped reports the Triage cells that cannot hold a card, as the lanes
// stripe them: the Agents row is an agent's lane.
function rowStriped(row, column) {
  if (row === ALL || !(row === OPERATOR || row === UNASSIGNED || row === AGENTS)) return false;
  return striped(row === AGENTS ? AGENTS : row, column);
}

const HIDEABLE_KEYS = HIDEABLE_BUILTINS.map((b) => b.key);

export function TaskboardFlat({ entry, group, snapshot, mode = 'triage', actions = null, readApi = null, wizard = false, now = Date.now(), onNewTask = null, onEdit = null }) {
  const tasks = entry.tasks || [];
  const byId = boardTasks(entry);
  const epicBoard = useContext(EpicBoard);
  const byEpic = mode === EPIC_ROWS;
  const epics = byEpic ? epicBoard?.byId || new Map() : null;
  const cells = flatModel(entry, mode, now, epics);
  const columns = boardColumns(entry);
  const drag = useLaneDrag(actions, group, tasks);
  const [open, setOpen] = useOpenCells(group.shownName || group.name, mode);
  useCellReveal(cells, FLAT_SHOWN, setOpen);
  const [answering, setAnswering] = useState('');
  // A column head's ⋯ and the last head's ＋ column, as in the lanes by
  // agent (groups-taskboard-columns.js).
  const naming = useColumnNaming(group, actions);
  const limits = useLimitDialog(group, entry, actions);
  const custom = entry.columns || [];
  const answerable = canAssign(actions) && typeof actions?.api?.answer === 'function';
  // Triage's You also holds the open questions on the agents' tasks.
  const questions = mode === ALL ? [] : operatorQuestions(entry);
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
    const target = rowTarget(row, column, group, entry.columns || [], byEpic ? epicName(row) : '');
    const over = drag.over?.key === key;
    // A cell's "+" starts a task in its column: unassigned, or in You the
    // operator's. Agents does not say which agent.
    const addLane = row === OPERATOR ? OPERATOR : row === AGENTS ? null : UNASSIGNED;
    // An epic's "+" starts the task in that epic.
    const parent = byEpic && row !== NO_EPIC ? row : '';
    // A cordoned column takes no new task; a card already in it may still
    // move within it (reorder, or another lane's hands).
    const takes = actions && (!column.cordoned || byId.get(drag.dragging)?.column === column.slug);
    const add = actions && !column.cordoned && onNewTask && addLane !== null && cellAdd(addLane, column.key, { assign: canAssign(actions), member: true });
    const lane = addLane === OPERATOR ? { id: OPERATOR, name: operatorName(group) } : { id: UNASSIGNED, name: 'Unassigned' };
    return html`<div key=${key} class=${`tb-cell${stripe ? ' tb-cell-striped' : ''}${cellClass(column)}${over ? ' tb-cell-over' : ''}${add ? ' tb-cell-addable' : ''}`}
        data-board-row=${row || 'unassigned'} data-column=${column.key} data-tb-drop=${over ? drag.over.label : undefined}
        onDragOver=${takes ? (event) => drag.overCell(event, target) : undefined}
        onDrop=${takes ? (event) => { if (over) landIn(key, list, drag.dragging); void drag.dropCell(event, target); } : undefined}>
      <${CellCards} list=${list} limit=${FLAT_SHOWN} open=${!!open[key]} reveal=${landed?.key === key ? landed.id : ''} onToggle=${(on) => setOpen({ ...open, [key]: on })}
        card=${(task) => html`<${Card} key=${task.id} task=${task} byId=${byId} api=${readApi || actions?.api || null} nameOf=${nameOf}
          marker=${lanesSay(mode, task) ? null : cardMarker(task, group, snapshot)}
          chip=${!byEpic}
          drag=${actions ? { ...drag, start: (event, t) => drag.start(event, t, { lane: taskLane(t), column: column.key, ...(byEpic ? { epic: row === NO_EPIC ? '' : row } : {}) }) } : null}
          menu=${archiveItems(task, actions)} onEdit=${actions ? onEdit : null} onAnswer=${answerable ? setAnswering : null} />`} />
      ${row === OPERATOR && column.key === 'progress' && questions.map(({ task, question }) => html`<${QuestionCard} key=${`${task.id}#${question.n}`}
        task=${task} question=${question} askedBy=${nameOf(question.asked_by)} onOpen=${actions ? onEdit : null}
        onAnswer=${answerable ? (id, n) => setAnswering(`${id}#${n}`) : null} />`)}
      ${add && html`<${CellAdd} lane=${lane} column=${column} add=${add} wizard=${wizard} group=${group} entry=${entry} parent=${parent} where=${byEpic ? epicName(row) : ''}
        onNewTask=${(choice, initial) => { landIn(key, list); onNewTask(choice, initial); }} />`}
    </div>`;
  };
  // epicName is an epic row's name, as its head says it.
  function epicName(row) { return row === NO_EPIC ? 'No epic' : epics?.get(row)?.title || row; }
  const rowCount = (row) => columns.reduce((sum, column) => sum + (column.key === 'done' ? 0 : (cells.get(`${row}|${column.key}`) || []).length), 0);
  const head = (row) => {
    if (byEpic) return epicHead(row);
    if (row === OPERATOR) return html`<${LaneHead} lane=${{ id: OPERATOR, name: operatorName(group), holds: (cells.get(`${OPERATOR}|progress`) || []).length + questions.length }} />`;
    if (row === UNASSIGNED) return html`<${LaneHead} lane=${{ id: UNASSIGNED, name: 'Unassigned', count: rowCount(UNASSIGNED) }} />`;
    const n = rowCount(AGENTS);
    return html`<div class="tb-lane-head"><span class="tb-lane-name">Agents</span><span class="tb-lane-sub">${n} task${n === 1 ? '' : 's'} · every agent</span></div>`;
  };
  // An epic's head is the epic: its name in its colour, its progress, and
  // open (its edit form); No epic says how many tasks are in none.
  const epicHead = (row) => {
    if (row === NO_EPIC) {
      const n = rowCount(NO_EPIC);
      return html`<div class="tb-lane-head tb-epic-lane-head tb-epic-lane-none"><span class="tb-lane-name"><span class="tb-epic-dot tb-epic-dot-none"></span>No epic</span><span class="tb-lane-sub">${n} task${n === 1 ? '' : 's'}</span></div>`;
    }
    const epic = epics.get(row);
    return html`<div class=${`tb-lane-head tb-epic-lane-head tb-epic-c${epicColour(row)}`} data-epic-lane=${row}>
      <span class="tb-lane-name"><span class="tb-epic-dot"></span>${epic?.title || row}</span>
      <${EpicProgress} epic=${epic} />
      <span class="tb-lane-sub">${progressParts(epic?.progress).join(' · ')}</span>
      ${epic && html`<${EpicNudge} epic=${epic} actions=${actions} />`}
      ${actions && onEdit && html`<button type="button" data-ui-exempt class="tb-epic-lane-open" data-epic-open=${row} title="Open the epic: its brief, progress and tasks" onClick=${() => onEdit(row)}>open ›</button>`}
    </div>`;
  };
  const triage = mode !== ALL;
  // The epic rows: the strip's epics in the board's order (an epic still an
  // idea only once it holds a task, else it is a card in Ideas), the one the
  // Epic filter hides others for alone, and No epic last.
  const epicRows = () => {
    const filter = epicBoard?.filter || {};
    const holds = (id) => [...cells.keys()].some((key) => key.startsWith(`${id}|`));
    const all = [...[...(epics?.values() || [])].filter((epic) => epic.stage !== 'idea' || holds(epic.id)).map((epic) => epic.id), NO_EPIC];
    // Hidden others leave the rows that hold what the filter shows: the
    // epic's own lane, and No epic while it holds the epic's card (an idea).
    return filter.hide && filter.epic ? all.filter((row) => row === filter.epic || holds(row)) : all;
  };
  // You shows while the operator holds a task, or for a viewer who may give
  // the operator one, as the operator's lane does.
  const rows = byEpic ? epicRows() : triage
    ? [...(([...cells.keys()].some((key) => key.startsWith(`${OPERATOR}|`)) || questions.length > 0 || canAssign(actions)) ? [OPERATOR] : []), UNASSIGNED, AGENTS]
    : [ALL];
  const count = (column) => [...cells.entries()].reduce((sum, [k, list]) => sum + (k.endsWith(`|${column}`) ? list.length : 0), 0);
  const cols = `${triage ? `${HEAD_PX}px ` : ''}repeat(${columns.length}, minmax(${MIN_STAGE_PX}px, 1fr))`;
  const min = (triage ? HEAD_PX : 0) + columns.length * MIN_STAGE_PX;
  const target = answerTarget(answering);
  const answerTask = answering && tasks.find((task) => task.id === target.id);
  return html`<div class="tb-lanes-wrap">
    ${naming.dialog}${limits.dialog}
    ${answerTask && html`<${AnswerFor} task=${answerTask} n=${target.n} actions=${actions} askedBy=${actorLabel(askerOf(answerTask, target.n), group, snapshot)} onClose=${() => setAnswering('')} />`}
    ${drag.blocking && actions && html`<${BlockDialog} task=${drag.blocking.task} drop=${drag.blocking.drop} actions=${actions}
      group=${group} onClose=${() => drag.setBlocking(null)} />`}
    <div class=${`tb-lanes tb-lanes-flat${triage ? '' : ' tb-lanes-row'}`} data-taskboard-lanes=${group.name} data-lanes-by=${mode}
        style=${`--tb-lane-cols: ${cols}; --tb-lane-min: ${min}px`}
        onDragLeave=${actions ? drag.leaveGrid : undefined}>
      <div class="tb-lane tb-lane-columns">
        ${triage && html`<div class="ui-eyebrow tb-col-lane">${byEpic ? 'Epic' : 'Who'}</div>`}
        ${columns.map((column, i) => html`<div key=${column.key} class=${columnClass(column)}>${wizard ? column.wizard : column.label} <span class="tb-col-count">${column.key === 'done' ? (entry.counts?.done_this_week ?? count('done')) : count(column.key)}</span><${LaneLimit} entry=${entry} column=${column} />${
          (column.custom || HIDEABLE_KEYS.includes(column.key) || (column.key === 'progress' && entry.options)) && html`<${ColumnHeadMenu} column=${column} columns=${custom} group=${group} actions=${actions} entry=${entry} onRename=${(c) => naming.open('rename', c)} limits=${limits} />`}${
          i === columns.length - 1 && html`<${AddColumnButton} naming=${naming} columns=${custom} />`}</div>`)}
      </div>
      ${rows.map((row) => html`<div key=${row || 'unassigned'} class=${`tb-lane${row === OPERATOR ? ' tb-lane-operator' : ''}${byEpic && row === NO_EPIC && !rowCount(NO_EPIC) && !cells.has(`${NO_EPIC}|done`) ? ' tb-lane-folded' : ''}`} data-board-row-head=${row || 'unassigned'}>
        ${triage && head(row)}${columns.map((column) => cell(row, column))}
      </div>`)}
    </div>
  </div>`;
}
