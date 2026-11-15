// groups-taskboard-process.js -- a board task's process (tk_8aa88514) on the
// desktop board: the line of its run on its card and row (◈ name · step N/T
// label · waiting on X, a segment per step; a click opens the run), the
// "will follow X when started · change · no process" line of a task not
// started yet, and the ⋯ Run with a process… item, which opens the group's
// start form for the task.
//
// Processes are opt-in: without processes on this node, on a linked node's
// board, or for a group with no saved process and no default way of working
// (and no task of its own choice), nothing here shows.
import { createContext, h } from 'preact';
import { useContext } from 'preact/hooks';
import htm from 'htm';
import { processContext } from './process-context.js';
import { TaskRunLine, WillFollowLine, taskProcessFollowed, taskRunOf } from './process-task-run.js';
import { processesAvailable } from './feature-availability.js';
import { RowMenu } from './row-menu.js';

const html = htm.bind(h);

// TaskProcess carries the board's process side to its cards and rows: null
// where processes are not offered.
export const TaskProcess = createContext(null);

// taskProcessBoard is that value for a board: the process context, the
// board's default way of working, the group's saved processes, and the
// writes (a task's choice through the board's own api).
export function taskProcessBoard({ group, entry, snapshot, actions, context = processContext.context.value }) {
  if (!group || !entry || !processesAvailable(snapshot)) return null;
  if (group.fleet_node && !group.fleet_node.local) return null;
  const groupEntry = (context?.groups || []).find((row) => row.group === group.name) || null;
  const saved = (groupEntry?.processes || []).map((row) => ({ id: row.templateId, name: row.name || row.templateId }));
  const boardDefault = String(entry.process || '');
  return {
    group, context, saved, boardDefault,
    offered: saved.length > 0 || !!boardDefault,
    nameOf: (id) => saved.find((row) => row.id === id)?.name || id,
    agentName: (id) => (group.members || []).find((member) => member.agent_id === id)?.title || '',
    actions,
  };
}

// openRun opens a run in the Groups tab's run focus. Loaded on demand, so
// the board does not pull the Groups tab in.
async function openRun(group, run) {
  const { groupsState } = await import('./groups-state.js');
  const { focusRun } = await import('./groups-focus.js');
  focusRun(groupsState, { group: run.scope?.group || group.name, runId: run.runId });
}

// runWithProcess opens the group's start form for task, on the process it
// follows if any.
export async function runWithProcess(board, task) {
  const { openGroupProcess } = await import('./process-start-handoff.js');
  await openGroupProcess({
    group: board.group.name, templateId: taskProcessFollowed(task, board.boardDefault),
    task: { id: task.id, title: task.title },
  });
}

// runWithProcessItem is the ⋯ Run with a process… item of a task that can
// run one: open work, not an epic, no run in progress, on a board that
// offers processes.
export function runWithProcessItem(board, task) {
  if (!board || !runnable(board, task)) return null;
  return {
    label: 'Run with a process…', attrs: { 'data-task-menu': 'run-process', title: `process run <template> --task ${task.id}: the run carries the task, and its end closes it` },
    run: () => runWithProcess(board, task),
  };
}

function closed(task) {
  return task.status === 'done' || task.status === 'dropped';
}

function runnable(board, task) {
  if (closed(task) || task.kind === 'epic') return false;
  if (!board.offered && !task.process) return false;
  const run = taskRunOf(board.context, task.id);
  return !run || isEnded(run);
}

function isEnded(run) {
  const status = String(run.status || '').toLowerCase();
  return status === 'completed' || status === 'failed' || status === 'canceled';
}

// TaskProcessLine is a card's or a row's process line: the task's run, else
// the process it will follow when started.
export function TaskProcessLine({ task, compact = false }) {
  const board = useContext(TaskProcess);
  if (!board || !task || task.kind === 'epic') return null;
  const run = taskRunOf(board.context, task.id);
  if (run) {
    return html`<${TaskRunLine} run=${run} compact=${compact} nameOf=${board.agentName} onOpen=${(picked) => { void openRun(board.group, picked); }} />`;
  }
  if (closed(task) || task.started_at || task.status === 'blocked') return null;
  const followed = taskProcessFollowed(task, board.boardDefault);
  if (!followed) return null;
  const api = board.actions?.api;
  const choose = typeof api?.edit === 'function'
    ? (value) => board.actions.run(`${task.id}:process`, 'Process', (writer) => writer.edit(task.id, { process: value }))
    : null;
  const others = board.saved.filter((row) => row.id !== followed);
  return html`<span class="tk-follow-row">
    <${WillFollowLine} task=${task} boardDefault=${board.boardDefault} nameOf=${board.nameOf}
      onNone=${choose ? () => choose('none') : null} />
    ${choose && others.length > 0 && html`<${RowMenu} label=${task.title} glyph="change" title="Follow another process when it starts"
      buttonClass="tk-follow-change" items=${others.map((row) => ({
        key: row.id, label: row.name, attrs: { 'data-task-follow-pick': row.id },
        run: () => choose(row.id === board.boardDefault ? '' : row.id),
      }))} />`}
  </span>`;
}
