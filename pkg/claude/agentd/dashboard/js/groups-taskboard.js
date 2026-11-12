// groups-taskboard.js -- the Task board section of a group card (wizard:
// Quest board) (docs/taskboard.md, tcl-o1tzq6, tcl-n7i6dt). The queue in the operator's
// order, then the Ideas band, each row with its ref chip, who holds it, its
// state, a block reason and its dependencies; under them the burst capture
// box and a history line that unfolds the recently closed tasks.
//
// Actions (taskboard-actions.js), each the route of a CLI verb: drag a row by
// its handle to reorder it (across the Ideas band it is promoted or demoted),
// ✓ Next, Done, Block / Unblock with a reason, Edit, Drop and Reopen on the
// row; "+ New task" (groups-taskboard-new.js) writes a task in a form, asks
// an agent to write them, or imports tracker ids; in the lanes view, a
// cell's "+" adds a task right there.
//
// Data comes from GET /v1/taskboard/context (taskboard-context.js). While
// features.taskboard is off, or for a group this viewer cannot read, nothing
// is drawn. A linked node's group (the fused view, or a peer's page) is read
// from that node through its peer view, under the taskboard.read /
// taskboard.write grants it gave this node; its actions show only under
// taskboard.write (or unrestricted trust).

import { h } from 'preact';
import { useContext, useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { relTime, shortAgentId } from './helpers.js';
import { Button, Seg } from './ui-components.js';
import { RowMenu } from './row-menu.js';
import { dashPrefs } from './prefs.js';
import { shellConfirmDiscard, shellToast } from './shell-state.js';
import { createTaskboardApi, taskboardApi } from './taskboard-actions.js';
import { isPageNode, nodeFetch, nodeSnapshot, nodeUpload, onPeerPage } from './node-api.js';
import { dashboardState } from './snapshot-store.js';
import { markLanded } from './drop-landing.js';
import { dndMember } from './dnd-ops.js';
import { isWizardActive } from './slop.js';
import { FusedSection } from './groups-processes.js';
import { TaskboardLanes, laneColumn } from './groups-taskboard-lanes.js';
import { ColumnsMenu, HiddenColumnsTag, boardHidden, hiddenTally, mineHidden, setMineHidden, withColumnView } from './groups-taskboard-colvis.js';
import { FLAT_MODES, TaskboardFlat } from './groups-taskboard-lanes-flat.js';
import { hasCustomOrder, resetLaneOrder } from './groups-taskboard-lanes-layout.js';
import { ViewMenu, ViewMenuHeading, ViewMenuSep, ViewToggle } from './view-menu.js';
import { AskAgentDialog, NewTaskDialog, NewTaskMenu } from './groups-taskboard-new.js';
import { TrackerTaskDialog } from './groups-taskboard-tracker.js';
import { EditTaskDialog } from './groups-taskboard-edit.js';
import { TaskReasonDialog } from './groups-taskboard-reason.js';
import { TaskMoveDialog, offeredColumns } from './groups-taskboard-stage.js';
import { AnswerFor, QuestionTag, askerOf, canAnswer } from './groups-taskboard-answer.js';
import { CommentCount, myAuthor } from './groups-taskboard-comments.js';
import { RefChip, TaskTitle } from './groups-taskboard-card.js';
import { FilesCount } from './groups-taskboard-files.js';
import { listenForTransfer, runCrossTransfer, runTransfer, transferTargets } from './groups-taskboard-transfer.js';
import { agentLink, jumpToTask } from './cross-links.js';
import { closeTaskRoute, copyLinkItem, dropTaskRoute, openTaskRoute, sameTask, TaskLink, taskRoute } from './task-route.js';
import {
  COLUMN_STATE, actorAgentId, columnLabel, createTaskboardContextSource, taskRowState, taskboardContext, taskboardCountsText,
  useTaskboardEntry,
} from './taskboard-context.js';
import { ColGroup } from './table-kit.js';
import { EpicBoard, EpicChip, EpicFilterMenu, EpicGroupRow, EpicMark, EpicStrip, NO_EPIC, boardEntry, epicDimmed, epicMoved, epicOf, intoEpic, isEpic, useEpicBoard } from './groups-taskboard-epics.js';

export { RefChip };

const html = htm.bind(h);

// A linked node's boards are read and changed through that node's peer
// view: one context source (with its own snapshot-poll subscription) and one
// write client per node, built on first use.
const nodeBoards = new Map();

export function nodeTaskboard(node) {
  let board = nodeBoards.get(node);
  if (!board) {
    // A node id this page cannot address reads as a board that is off.
    let fetchImpl;
    try { fetchImpl = nodeFetch(node); } catch (_) { fetchImpl = async () => ({ ok: false, status: 404, json: async () => null }); }
    // Files go through this node's staged upload to that node.
    const upload = (path, init) => nodeUpload(node, path, init);
    board = { source: createTaskboardContextSource({ fetchImpl }), api: createTaskboardApi({ fetchImpl, upload }) };
    nodeBoards.set(node, board);
  }
  return board;
}

// peerHolds reports whether a linked node's view grants this node slug on
// the group: unrestricted trust, or that peer grant there (peer_view.groups
// lists the taskboard write, assign and manage grants per group).
export function peerHolds(peerView, groupName, slug) {
  if (!peerView) return false;
  if (peerView.level === 'unrestricted') return true;
  const held = peerView.groups?.[groupName];
  return Array.isArray(held) && held.includes(slug);
}

// peerWrites reports whether a linked node's view lets this node change the
// group's board.
export function peerWrites(peerView, groupName) {
  return peerHolds(peerView, groupName, 'taskboard.write');
}

// peerApis keeps one write client per (client, can) pair, so a peer group's
// api is the same object from render to render.
const peerApis = new WeakMap();

// peerApi is api for a linked node's group: null without taskboard.write,
// else api with can.assign set from taskboard.assign there.
function peerApi(api, peerView, groupName) {
  if (!api || !peerWrites(peerView, groupName)) return null;
  const assign = peerHolds(peerView, groupName, 'taskboard.assign');
  let byCan = peerApis.get(api);
  if (!byCan) { byCan = new Map(); peerApis.set(api, byCan); }
  const key = assign ? 'assign' : 'none';
  if (!byCan.has(key)) byCan.set(key, { ...api, can: { ...(api.can || {}), assign } });
  return byCan.get(key);
}

// ownBoardGroup is group as its own node names it. The fused view names a
// linked node's group name@node; that node's board, its grants and its
// routes all know it as name. Anything else is returned as it is.
export function ownBoardGroup(group) {
  const node = group?.fleet_node;
  const at = `@${node?.name || ''}`;
  if (!node || node.local || !group.name?.endsWith(at)) return group;
  return { ...group, name: group.name.slice(0, -at.length), shownName: group.name };
}

// boardOf picks the source and write client for group: this node's own, or
// the linked node's it lives on (api null where its grants allow no writes).
// readApi is the same client unfiltered, for reads (a task card's brief and
// comments), which taskboard.read allows without taskboard.write.
function boardOf(group, source, api, boards) {
  const node = group?.fleet_node && !group.fleet_node.local ? group.fleet_node.id : '';
  if (node) {
    const board = boards(node);
    return { source: board.source, api: peerApi(board.api, nodeSnapshot(node)?.peer_view, group.name), readApi: board.api };
  }
  if (onPeerPage()) {
    return { source, api: peerApi(api, dashboardState.snapshot.value?.peer_view, group.name), readApi: api };
  }
  return { source, api, readApi: api };
}

// agentLabel names an agent from the snapshot: the group's own members
// first (an assignee is one), then any group's.
export function agentLabel(agentId, group, snapshot) {
  if (!agentId) return '';
  const find = (members) => (members || []).find((member) => member.agent_id === agentId);
  const member = find(group?.members)
    || (snapshot?.groups || []).map((g) => find(g.members)).find(Boolean);
  return member?.title || shortAgentId(agentId, '');
}

// operatorName names the operator a board's "operator" assignee is: you on
// this node's boards, the linked node's operator on its boards (the fused
// view, or that node's page).
export function operatorName(group) {
  const node = group?.fleet_node;
  if (node && !node.local) return `operator of ${node.name || node.id}`;
  if (onPeerPage()) return 'their operator';
  return 'you';
}

export function actorLabel(actor, group, snapshot) {
  if (actor === 'human:operator') return 'operator';
  const id = actorAgentId(actor);
  return id ? agentLabel(id, group, snapshot) : actor || '';
}

const STATE_LABELS = {
  blocked: 'blocked', stalled: 'stalled', waiting: 'waiting', 'in-progress': 'in progress', queued: 'queued', idea: 'idea', open: 'open', done: 'done', dropped: 'dropped',
};


// boardTasks maps a board's tasks, live and closed, by id: what a
// dependency names.
export function boardTasks(entry) {
  return new Map([...(entry?.tasks || []), ...(entry?.closed || [])].map((task) => [task.id, task]));
}

// taskLabel names a task by its title, clipped to max, or by its id where
// the board does not have it (archived, or another board's).
export function taskLabel(id, byId, max = 28) {
  const title = byId?.get(id)?.title;
  if (!title) return id;
  return title.length > max ? `${title.slice(0, max - 1)}…` : title;
}

// taskFullLabel is a task's whole title and its id, for a tooltip.
const taskFullLabel = (id, byId) => (byId?.get(id)?.title ? `“${byId.get(id).title}” (${id})` : id);

export function DepChips({ task, byId }) {
  const unmet = new Set(task.unmet || []);
  return (task.depends_on || []).map((dep) => {
    const text = `after ${taskLabel(dep, byId)}`;
    return html`<span key=${dep} class=${`tb-dep${unmet.has(dep) ? ' unmet' : ''}`}
      title=${`${unmet.has(dep) ? 'Waits on' : 'Done:'} ${taskFullLabel(dep, byId)}`}>${text}</span>`;
  });
}

// Who is the List's Who cell. A live task nobody holds has the lanes' yellow
// "!", and one the operator holds the red "!", as a card in one row has
// them: the List has no lane to say it.
function Who({ task, group, snapshot }) {
  const agent = task.assignee?.agent_id;
  const live = task.status === 'open' || task.status === 'blocked';
  // An epic is no work of its own: nobody holds it.
  if (task.kind === 'epic') return html`<span class="tb-who tb-none" title="An epic has no assignee: its tasks do">—</span>`;
  if (!agent) {
    return html`<span class="tb-who">${live && html`<span class="tb-who-mark tb-marker-unassigned" aria-hidden="true">!</span>`}<span class="tb-unassigned">unassigned</span></span>`;
  }
  const operator = !!task.assignee?.operator;
  const name = operator ? operatorName(group) : task.assignee?.name || agentLabel(agent, group, snapshot);
  const by = actorLabel(task.assigned_by, group, snapshot);
  // The name ellipsizes in its cell; who assigned it, and when, is the
  // tooltip.
  const detail = [task.assigned_at && `assigned ${relTime(task.assigned_at)}`, by && `by ${by}`].filter(Boolean).join(' ');
  // An agent that is a member links to its row (cross-links.js); the
  // tooltip stays who assigned it.
  const member = !operator && (group?.members || []).some((m) => m.agent_id === agent);
  const link = member ? { ...agentLink({ group: group.name, node: group.fleet_node?.id || '', id: agent, name }), title: undefined } : {};
  return html`<span class=${`tb-who${operator ? ' tb-who-operator' : ''}`} title=${detail ? `${name} · ${detail}` : `${name} · ${agent}`}>
    ${operator && live ? html`<span class="tb-who-mark tb-marker-operator" aria-hidden="true">!</span>`
      : html`<span class="tb-avatar" aria-hidden="true">${operator ? '★' : name.slice(0, 1).toUpperCase()}</span>`}
    <span class=${`tb-who-name${member ? ' tb-xlink' : ''}`} ...${link}>${name}</span>
  </span>`;
}

// STATE_MEANING is what a state means, for a pill whose tooltip has nothing
// more to say.
const STATE_MEANING = {
  open: 'next in the queue', idea: 'captured, not yet in the queue', queued: 'waits on other tasks',
  'in-progress': 'an agent holds it', waiting: 'the operator holds it: an answer or a step an agent waits on', stalled: 'its agent went idle without reporting', blocked: 'waits on something outside the board',
  done: 'finished', dropped: 'dropped without being done',
};

// StatePill is a task's state as one word, as the Agents table's state pill
// is: the pill says what the state is, and its tooltip (title and
// aria-label on a focusable wrap) has what else there is to know: who holds
// it, why it is blocked, what it waits on, or else what the state means.
// No times: the Last and Age columns carry them.
function StatePill({ task, byId, assignee = '', columns = [] }) {
  const state = taskRowState(task);
  const custom = state.startsWith(COLUMN_STATE);
  const word = custom ? columnLabel(state, columns) : STATE_LABELS[state];
  const details = [];
  if (state === 'waiting' && task.assigned_by) details.push(`asked by ${assignee || task.assigned_by}`);
  if (state === 'in-progress' || state === 'stalled' || custom) {
    const who = task.assignee?.name || assignee || task.assignee?.agent_id || '';
    if (who) details.push(`with ${who}`);
    if (state === 'stalled') details.push('idle, no report');
  }
  if (state === 'queued') {
    const waits = (task.unmet || []).map((dep) => byId?.get(dep)?.title || dep);
    if (waits.length) details.push(`waits on ${waits.map((w) => `“${w}”`).join(', ')}`);
  }
  if (state === 'blocked' && task.block_reason) details.push(`“${task.block_reason.trim()}”`);
  if (custom) details.push('no idle alarm here');
  const title = `${word} · ${details.length ? details.join(' · ') : STATE_MEANING[state]}`;
  // The Agents pill's wrap (dashboard.css .state-pill-wrap): keyboard and
  // touch focus show data-full-status, as the pointer's title does.
  return html`<span class="state-pill-wrap tb-state-wrap" tabindex="0" title=${title} aria-label=${`state: ${title}`} data-full-status=${title}>
    <span class=${`ui-tag${state === 'open' ? ' ui-tag-accent' : ''} tb-state ${custom ? 'tb-state-column' : `tb-state-${state}`}`} aria-hidden="true">${word}</span>
  </span>`;
}

// useBoardActions runs a write and then re-reads the board, also after a
// refusal (a write may have half-landed); a refusal is a toast naming the
// action, and the result is false.
function useBoardActions(source, api) {
  const [busy, setBusy] = useState('');
  const run = async (key, label, fn) => {
    setBusy(key);
    try {
      const out = await fn(api);
      return out ?? true;
    } catch (cause) {
      shellToast(`${label}: ${cause?.message || cause}`, true);
      return false;
    } finally {
      setBusy('');
      await Promise.resolve(source.refresh?.()).catch(() => {});
    }
  };
  const refresh = () => Promise.resolve(source.refresh?.()).catch(() => {});
  return { busy, run, api, refresh };
}

// canAssign says whether the viewer may assign on this board. The api may
// carry `can.assign` (a peer node's board sets it from its peer_view grants);
// the local operator may always.
export function canAssign(actions) {
  return !!actions && actions.api?.can?.assign !== false;
}

// binDroppable says whether a dragged task may go in the groups view's bin:
// a live task the viewer may drop, as ⋯ Drop… offers it.
export function binDroppable(task, actions) {
  return task?.status !== 'done' && task?.status !== 'dropped' && canClose(task, actions);
}

// canClose reports whether the viewer may mark the task done or drop it: an
// assigned task is closed by its assignee or an assigner (taskboard.assign),
// as the status route checks; an unassigned one by any writer.
export function canClose(task, actions) {
  return !task?.assignee?.agent_id || canAssign(actions);
}

// RowActions is a row's leading icon cluster, where the Agents table has its
// focus/retire/⚙ cluster: the one action the row's state calls for (✓ Done,
// ✓ Next on an idea, ↺ Reopen in history), ✎ Edit, and ⋯ for the rest
// (rowMenu). Each names its CLI verb in its title.
function RowActions({ task, group, tasks, actions, onForm, columns = [] }) {
  const board = useContext(TaskLink);
  const state = taskRowState(task);
  const closed = state === 'done' || state === 'dropped';
  const go = (key, label, fn) => () => actions.run(`${task.id}:${key}`, label, fn);
  const icon = (key, glyph, title, onClick, extra = '') => html`<${Button} key=${key} size="sm" icon=${true}
    class=${`icon-btn tb-act tb-act-${key}${extra ? ` ${extra}` : ''}`} title=${title} aria-label=${title.split(':')[0]}
    busy=${actions.busy === `${task.id}:${key}`} disabled=${!!actions.busy} onClick=${onClick}>${glyph}<//>`;
  // A slot keeps the cluster's buttons lined up down the table.
  const slot = (key) => html`<span key=${key} class="tb-act-slot" aria-hidden="true"></span>`;
  let first;
  if (closed) first = icon('reopen', '↺', 'Reopen: put it back on the list where it was (or drag it where it should go)', go('reopen', 'Reopen', (api) => api.reopen(task.id)));
  else if (task.stage === 'idea') first = icon('promote', '✓', 'Next: taskboard promote, to the end of the queue', go('promote', 'Promote', (api) => api.stage(task.id, 'next')), 'tb-act-go');
  else if (canClose(task, actions)) first = icon('done', '✓', 'Done: taskboard done', go('done', 'Done', (api) => api.status(task.id, 'done')), 'tb-act-ok');
  else first = slot('done');
  const edit = icon('edit', '✎', 'Edit: title, brief, ref, where it is, assignee, dependencies and files', () => onForm('edit'));
  return html`<span class="row-actions tb-row-actions">${first}${edit}<${RowMenu} label=${task.title} items=${rowMenu({ task, group, tasks, actions, onForm, columns, board })}
    glyph="⋯" buttonClass="icon-btn tb-act tb-act-more" keepSlot=${true} title="More: assign, block, move or clone to a group, drop, archive (or right-click the row)" /></span>`;
}

// rowMenu is a row's ⋯ items (row-menu.js): assign (to a member), block or
// unblock, unassign, move or clone to another group (the groups a drag
// could reach, this node's or a linked one's, with a drop's confirmation)
// and drop.
function rowMenu({ task, group, tasks, actions, onForm, columns = [], board = null }) {
  const state = taskRowState(task);
  const closed = state === 'done' || state === 'dropped';
  // An epic is no work of its own: no assign, block, column, start or hold
  // (the store refuses each with "epic").
  const work = !isEpic(task);
  const run = (key, label, fn) => () => actions.run(`${task.id}:${key}`, label, fn);
  const item = (key, label, title, rest) => ({ label, attrs: { 'data-task-menu': key, title }, ...rest });
  const items = [];
  if (actions.busy) return items;
  if (canAnswer(task, actions)) {
    items.push(item('answer', 'Answer…', 'taskboard answer: pick an option and/or answer in words; the agent that asked gets it', { run: () => onForm('answer') }));
  }
  if (work && canAssign(actions) && !closed && task.status === 'open') {
    items.push(item('assign', 'Assign', `taskboard assign --to: give it to a member, the brief goes to its inbox${task.stage === 'idea' ? ' and the idea joins the queue' : ''} (or drag the row onto an agent)`, {
      children: () => {
        const members = assignableMembers(group);
        const mine = !!task.assignee?.operator;
        const operator = item('assign-operator', `${operatorName(group) === 'you' ? 'You (operator)' : operatorName(group)}${mine ? ' ✓' : ''}`,
          'taskboard assign --to operator: the operator answers or does it, and closes it as done', {
            key: 'operator', disabled: mine,
            run: () => actions.run(`${task.id}:assign`, 'Assign', (api) => api.assign(task.id, 'operator')),
          });
        if (!members.length) return [operator, { label: 'No member to assign to', disabled: true }];
        return [operator, ...members.map((member) => {
          const here = task.assignee?.agent_id === member.agent_id;
          return item(`assign-to`, `${member.title || member.agent_id}${member.online ? '' : ' (offline)'}${here ? ' ✓' : ''}`, `taskboard assign --to ${member.agent_id}`, {
            key: member.agent_id,
            disabled: here,
            run: () => actions.run(`${task.id}:assign`, 'Assign', (api) => api.assign(task.id, member.agent_id))
              .then((ok) => { if (ok) markLanded({ group: group.name, node: group.fleet_node?.id || '', section: 'agents', ids: [member.agent_id] }); }),
          });
        })];
      },
    }));
  }
  // Reopen…: the same picker, where it was by default (↺ on the row does
  // that in one click), or Idea, Next or a custom column, in one write.
  if (closed) items.push(item('reopen', 'Reopen…', 'taskboard reopen [--next|--idea|--stage <column>]: back on the board, where it was or elsewhere', { run: () => onForm('reopen') }));

  // Move to…, on a board with custom columns (In review, In CI): the stage
  // picker (groups-taskboard-stage.js) over Idea, Next and the columns,
  // `taskboard move --stage`.
  if (work && !closed && task.status === 'open' && offeredColumns(columns).length) {
    items.push(item('move-to', 'Move to…', 'taskboard move --stage <column>: Idea, Next or a column of the board\'s own (In review, …)', { run: () => onForm('move') }));
  }
  if (!closed && task.status === 'blocked') items.push(item('unblock', 'Unblock', 'taskboard unblock', { run: run('unblock', 'Unblock', (api) => api.status(task.id, 'open')) }));
  else if (work && !closed) items.push(item('block', 'Block…', 'taskboard block: with a reason', { run: () => onForm('block') }));
  // A task its assignee holds starts (in progress); a started one is put
  // back, held in its stage (tk_3826822a).
  if (work && task.status === 'open' && canAssign(actions) && task.assignee?.agent_id && typeof actions.api?.start === 'function') {
    if (task.held) items.push(item('start', 'Start', 'taskboard start: in progress, the stall clock runs', { run: run('start', 'Start', (api) => api.start(task.id)) }));
    else items.push(item('hold', 'Not started', `taskboard hold: back to ${task.stage === 'idea' ? 'the ideas' : 'next'}, still held by its assignee`, { run: run('hold', 'Hold', (api) => api.hold(task.id, task.stage)) }));
  }
  if (!closed && canAssign(actions) && task.assignee?.agent_id) {
    items.push(item('unassign', 'Unassign', 'taskboard unassign: take it back from its assignee', { run: run('unassign', 'Unassign', (api) => api.unassign(task.id)) }));
  }
  if (typeof actions.api?.moveToGroup === 'function') {
    // As a drop on a group's board: into its Ideas, where its operator
    // orders it.
    const stage = 'idea';
    const targets = (clone) => () => {
      const found = transferTargets(group.shownName || group.name);
      if (!found.length) return [{ label: 'No other group on this page', disabled: true }];
      return found.map((target) => item('to', target.shown, `taskboard move-to-group ${task.id} ${target.group}${clone ? ' --clone' : ''}`, {
        key: target.shown,
        run: () => (target.cross
          ? runCrossTransfer({ task, group, actions, to: target.group, node: target.node, nodeName: target.nodeName, fromNodeName: target.fromNodeName, stage, clone })
          : runTransfer({ task, group, tasks, actions, to: target.group, stage, clone })),
      }));
    };
    if (!closed) items.push(item('move', 'Move to group', 'taskboard move-to-group: or drag the row onto another group', { children: targets(false) }));
    items.push(item('clone', 'Clone to group', 'taskboard move-to-group --clone: or Ctrl/Cmd-drag the row onto another group', { children: targets(true) }));
  }
  if (!closed && canClose(task, actions)) {
    items.push(item('drop', 'Drop…', 'taskboard drop: take it off the board (park it in the tracker first)', { danger: true, run: () => onForm('drop') }));
  }
  items.push(...archiveItems(task, actions));
  if (board) items.push(copyLinkItem(task.id, board));
  return items;
}

// archiveItems is a closed task's ⋯ Archive (`taskboard archive`): it
// leaves the history, kept and recoverable (unarchive, reopen). A live task
// is dropped first, through ⋯ Drop…, which asks the reason. The route takes
// the board's write (or the assignee's own), as any change of a closed task.
export function archiveItems(task, actions) {
  if (!actions || actions.busy || typeof actions.api?.archive !== 'function') return [];
  if (task.status !== 'done' && task.status !== 'dropped') return [];
  return [{
    label: 'Archive',
    attrs: { 'data-task-menu': 'archive', title: 'taskboard archive: out of the history, kept (ls --archived; unarchive brings it back)' },
    run: () => actions.run(`${task.id}:archive`, 'Archive', (api) => api.archive(task.id)),
  }];
}

// openRowMenu opens the ⋯ menu inside el on a right-click, in place of the
// browser's own menu.
export function openRowMenu(event, selector) {
  const more = event.currentTarget?.querySelector?.(selector);
  if (!more || event.target?.closest?.('a[href], input, textarea')) return;
  event.preventDefault();
  more.click();
}

// assignableMembers are the group's members a task can go to: active
// agents with a stable id, online ones first.
export function assignableMembers(group) {
  return (group?.members || []).filter((member) => member.agent_id && !member.retired)
    .sort((a, b) => Number(!!b.online) - Number(!!a.online) || String(a.title || '').localeCompare(String(b.title || '')));
}

const DRAG_TITLE = 'Drag to reorder, onto an agent to assign, or onto another group to move it (Ctrl/Cmd: clone)';
// linkDrag is a drag of a link in the row (the 🔗 ref): the browser's own
// URL drag, not the task's.
const linkDrag = (event) => event.target !== event.currentTarget && !!event.target?.closest?.('a[href]');

function TaskRow({ task, byId, group, snapshot, tasks, actions, readApi, drag, onForm, columns = [], chip = true }) {
  const state = taskRowState(task);
  const closed = state === 'done' || state === 'dropped';
  const over = drag?.over?.id !== task.id ? '' : drag.over.where === 'before' ? ' tb-drop-before' : ' tb-drop-after';
  const deps = task.depends_on || [];
  const files = task.attachments?.length || 0;
  // The whole row drags, as an agent row does (⋮⋮ is its grip); a press on
  // a button, link or field in it is a click, not a drag (row-drag.js
  // bindTaskRowDrag).
  // A closed row drags too: dropped among the live tasks (or on an agent),
  // it is reopened there.
  const draggable = !!actions && !!drag;
  const dim = epicDimmed(useContext(EpicBoard), task);
  const row = html`<tr class=${`tb-row tb-row-${state.startsWith(COLUMN_STATE) ? 'column' : state}${closed ? ' tb-closed' : ''}${over}${drag?.dragging === task.id ? ' tb-dragging' : ''}${dim ? ' tb-epic-dim' : ''}`} data-task-id=${task.id}
      draggable=${draggable ? 'true' : undefined} data-dnd-task=${draggable ? task.id : undefined} title=${draggable ? (closed ? REOPEN_DRAG_TITLE : DRAG_TITLE) : undefined}
      onDragStart=${draggable ? (event) => { if (!linkDrag(event)) drag.start(event, task); } : undefined} onDragEnd=${draggable ? drag.end : undefined}
      onContextMenu=${actions ? (event) => openRowMenu(event, '.tb-row-actions .tb-act-more') : undefined}
      onDragOver=${closed ? undefined : (event) => drag?.overRow(event, task)} onDrop=${closed ? undefined : (event) => drag?.dropOnRow(event, task)}>
    <td class="tb-ic">${actions && html`<span class="tb-handle" aria-hidden="true">⋮⋮</span><${RowActions} task=${task} group=${group} tasks=${tasks}
        actions=${actions} columns=${columns} onForm=${(mode) => onForm(task.id, mode)} />`}</td>
    <td class="tb-title-cell"><div class="ui-fit tb-title-box">
      <${TaskTitle} task=${task} api=${readApi} nameOf=${(actor) => actorLabel(actor, group, snapshot)} onOpen=${actions ? () => onForm(task.id, 'edit') : null}
        chips=${{ ref: task.ref && html`<${RefChip} task=${task} />`, deps: html`<${DepChips} task=${task} byId=${byId} />` }} />
      <${EpicMark} task=${task} />${chip && html`<${EpicChip} task=${task} />`}
      <${QuestionTag} task=${task} askedBy=${actorLabel(askerOf(task), group, snapshot)} />
      <${CommentCount} task=${task} onOpen=${actions ? () => onForm(task.id, 'edit') : null} />
      ${task.status === 'blocked' && task.block_reason && html`<div class="tb-reason" title=${task.block_reason}>blocked: “${task.block_reason}”</div>`}
    </div></td>
    <td><${StatePill} task=${task} byId=${byId} columns=${columns} assignee=${task.assignee?.operator ? actorLabel(task.assigned_by, group, snapshot)
      : task.assignee?.agent_id ? agentLabel(task.assignee.agent_id, group, snapshot) : ''} /></td>
    <td class="tb-t" title=${task.updated_at || ''}>${task.updated_at ? relTime(task.updated_at) : ''}</td>
    <td class="tb-t" title=${task.created_at || ''}>${task.created_at ? relTime(task.created_at) : ''}</td>
    <td class="tb-who-cell"><${Who} task=${task} group=${group} snapshot=${snapshot} /></td>
    <td class="tb-ref-cell"><${RefChip} task=${task} max=${1} /></td>
    <td class="tb-deps-cell">${deps.length ? html`<${DepChips} task=${task} byId=${byId} />` : html`<span class="tb-none">—</span>`}</td>
    <td class="tb-files-cell">${files ? html`<${FilesCount} api=${readApi} task=${task} />` : html`<span class="tb-none">—</span>`}</td>
  </tr>`;
  return row;
}

// The list's columns: the leading cluster, Name, State, Last and Age in the
// Agents table's order and look, then the board's own Who, Ref, Deps and
// Files. Each is as wide as its content (dashboard.css .tb-table); Name takes
// the rest. The Ideas band and history are rows of the same table.
const TB_COLS = Object.freeze(['ctl', 'name', 'state', 'last', 'age', 'who', 'ref', 'deps', 'files'].map((key) => ({ key })));
const COLUMNS = html`<${ColGroup} cols=${TB_COLS} colClass=${(c) => `${c.key === 'files' ? 'ui-col-rest' : 'ui-col-fit'} tb-col-${c.key}`} />`;
const TABLE_COLS = TB_COLS.length;

const TASK_DRAG_TYPE = 'application/x-tclaude-task';

// useRowDrag reorders by dragging a row's handle onto another row (before
// or after it, by the pointer's half) or onto the Ideas band (demote to the
// top of the ideas). Its events stop here, so the group's own drags (agent
// rows, group reorder) never see them.
// agentDropRow is the member row of this group's own card under a task
// drag, with the member's stable agent id; a subgroup's or another card's
// agents are not targets (assignees must be members).
export function agentDropRow(target, group) {
  const row = target?.closest?.('tr[data-dnd-agent]');
  // The DOM names the group as the view shows it (name@node for a linked
  // node's group).
  const shown = group.shownName || group.name;
  if (!row || row.dataset.dndSourceGroup !== shown) return null;
  const card = row.closest('details[data-group-key]');
  if (card?.dataset.groupKey !== shown) return null;
  const member = assignableMembers(group).find((m) => m.agent_id === row.dataset.dndAgent);
  return member ? { row, member } : null;
}

const ASSIGN_OVER = 'tb-assign-over';

// assignByDrop is a drop that pairs a task with a member, either way round
// (a task onto an agent row, an agent row onto a task): assign an open
// task (taskboard assign --to), reopen a closed one with that agent. One
// already assigned there is left as it is.
function assignByDrop(actions, task, member, landing) {
  if (isClosed(task)) {
    return actions.run(`${task.id}:reopen`, 'Reopen', (api) => api.reopen(task.id, task.assignee?.agent_id === member.agent_id ? {} : { to: member.agent_id }));
  }
  if (task.assignee?.agent_id === member.agent_id) return Promise.resolve(false);
  return actions.run(`${task.id}:assign`, 'Assign', (api) => api.assign(task.id, member.agent_id))
    .then((ok) => { if (ok && landing) markLanded(landing); return ok; });
}

// TASK_AGENT_OVER marks the task under an agent drag (useAgentOnTask).
const TASK_AGENT_OVER = 'tb-agent-over';

// useAgentOnTask takes an agent row's drag (dnd.js, the Agents table) onto
// a task of this board, a List row or a lanes card: the drop assigns the
// task to that agent, as dragging the task onto the agent does. It takes
// only a member of this group on this board's node, onto an open task (or a
// closed one, which reopens with the agent), when the viewer may assign.
// Anything else stays dnd.js's: a drop on the group box moves or adds the
// agent to the group.
function useAgentOnTask(boardRef, group, node, tasks, actions) {
  const live = useRef(null);
  live.current = { group, node, tasks, actions };
  useEffect(() => {
    const doc = globalThis.document;
    if (!doc) return undefined;
    let lit = null;
    const unlight = () => {
      lit?.classList.remove(TASK_AGENT_OVER);
      lit?.removeAttribute('data-tb-assign');
      lit = null;
    };
    const target = (event) => {
      // Only a member drag: a task's own drag (or a stray file) is not one.
      const types = event.dataTransfer?.types;
      if (types && !Array.from(types).includes('application/x-tclaude-member')) return null;
      const payload = dndMember.payload();
      const { group: g, node: n, tasks: list, actions: a } = live.current;
      // Nodes compare as dnd.js names them (dndNode): '' for the page's own.
      if (!payload || !canAssign(a) || (payload.sourceNode || '') !== (isPageNode(n) ? '' : n)) return null;
      const el = event.target?.closest?.('[data-task-id]');
      if (!el || !boardRef.current?.contains(el)) return null;
      const task = list.find((t) => t.id === el.getAttribute('data-task-id'));
      if (!task || isEpic(task) || !(task.status === 'open' || isClosed(task))) return null;
      const member = assignableMembers(g).find((m) => m.agent_id === payload.agent);
      return member ? { el, task, member } : null;
    };
    const onOver = (event) => {
      const hit = target(event);
      if (!hit) { unlight(); return; }
      event.preventDefault();
      event.stopPropagation();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      const who = hit.member.title || hit.member.agent_id;
      const closed = isClosed(hit.task);
      const already = !closed && hit.task.assignee?.agent_id === hit.member.agent_id;
      const text = already ? `already assigned to ${who}` : `${closed ? '↺ reopen with' : '→ assign to'} ${who}`;
      dndMember.yieldTo(event, text);
      if (lit !== hit.el) { unlight(); lit = hit.el; lit.classList.add(TASK_AGENT_OVER); }
      lit.setAttribute('data-tb-assign', text);
    };
    const onDrop = (event) => {
      const hit = target(event);
      unlight();
      if (!hit) return;
      event.preventDefault();
      event.stopPropagation();
      dndMember.yieldTo(event, '');
      const { group: g, actions: a } = live.current;
      void assignByDrop(a, hit.task, hit.member, { group: g.name, node: g.fleet_node?.id || '', section: 'agents', ids: [hit.member.agent_id] });
    };
    doc.addEventListener('dragover', onOver, true);
    doc.addEventListener('drop', onDrop, true);
    doc.addEventListener('dragend', unlight, true);
    return () => {
      doc.removeEventListener('dragover', onOver, true);
      doc.removeEventListener('drop', onDrop, true);
      doc.removeEventListener('dragend', unlight, true);
      unlight();
    };
  }, []);
}

function clearAgentTarget(state) {
  state.row?.classList.remove(ASSIGN_OVER);
  state.row?.querySelector('td.name-cell')?.removeAttribute('data-tb-assign');
  state.row = null;
}

// epicRows, when the List is grouped by epic, is a ref to { of(task), name(id) }:
// the group a queue row is drawn in, and its epic's name.
function useRowDrag(tasks, actions, group = null, epicRows = null) {
  const dragged = useRef(null);
  const [over, setOver] = useState(null);
  const [dragging, setDragging] = useState('');
  // While a task is dragged, this group's agent rows take it: drop to
  // assign (taskboard assign --to). The listeners live only for the drag.
  const agentDrop = useRef({ row: null, off: null });
  const listenForAgents = () => {
    // A drag whose dragend never arrived (the source row re-rendered away)
    // must not leave its listeners behind to assign a second time.
    agentDrop.current.off?.();
    if (!group || !canAssign(actions)) return;
    const state = agentDrop.current;
    // Only an open task can be assigned (a blocked one is refused); a closed
    // one is reopened with that agent.
    const target = (event) => !isEpic(dragged.current) && (dragged.current?.status === 'open' || isClosed(dragged.current)) && agentDropRow(event.target, group);
    const onOver = (event) => {
      const hit = target(event);
      if (!hit) { clearAgentTarget(state); return; }
      event.preventDefault();
      event.stopPropagation();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      if (state.row === hit.row) return;
      clearAgentTarget(state);
      state.row = hit.row;
      hit.row.classList.add(ASSIGN_OVER);
      const already = dragged.current.assignee?.agent_id === hit.member.agent_id;
      hit.row.querySelector('td.name-cell')?.setAttribute('data-tb-assign',
        already ? 'already assigned here' : `assign “${taskLabel(dragged.current.id, new Map([[dragged.current.id, dragged.current]]))}” to ${hit.member.title || hit.member.agent_id}`);
    };
    const onDrop = (event) => {
      const hit = target(event);
      if (!hit) return;
      event.preventDefault();
      event.stopPropagation();
      const task = dragged.current;
      end();
      void assignByDrop(actions, task, hit.member, { group: group.name, node: group.fleet_node?.id || '', section: 'agents', ids: [hit.member.agent_id] });
    };
    document.addEventListener('dragover', onOver, true);
    document.addEventListener('drop', onDrop, true);
    state.off = () => {
      document.removeEventListener('dragover', onOver, true);
      document.removeEventListener('drop', onDrop, true);
      clearAgentTarget(state);
      state.off = null;
    };
  };
  useEffect(() => () => agentDrop.current.off?.(), []);
  // A drop on another group's board or header moves the task there
  // (groups-taskboard-transfer.js); its listeners too live for the drag.
  const transferOff = useRef(null);
  useEffect(() => () => transferOff.current?.(), []);
  const accept = (event) => {
    if (!dragged.current) return false;
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    return true;
  };
  const clear = () => {
    dragged.current = null; setOver(null); setDragging(''); agentDrop.current.off?.();
    transferOff.current?.(); transferOff.current = null;
  };
  const end = clear;
  return {
    over, dragging,
    start(event, task) {
      event.stopPropagation();
      dragged.current = task;
      setDragging(task.id);
      event.dataTransfer?.setData(TASK_DRAG_TYPE, task.id);
      // copyMove: Ctrl/Cmd onto another group clones (groups-taskboard-transfer.js).
      if (event.dataTransfer) event.dataTransfer.effectAllowed = 'copyMove';
      const row = event.currentTarget.closest?.('tr');
      if (row && event.dataTransfer?.setDragImage) event.dataTransfer.setDragImage(row, 12, 12);
      listenForAgents();
      transferOff.current?.();
      if (group && !isClosed(task)) transferOff.current = listenForTransfer({ task, group, tasks, actions, droppable: binDroppable(task, actions), onDrop: clear });
    },
    // dropUnassign takes the dragged task back from its assignee.
    overUnassign(event) {
      if (!dragged.current?.assignee || isClosed(dragged.current)) return;
      accept(event);
    },
    async dropUnassign(event) {
      if (!dragged.current?.assignee || !accept(event)) return;
      const task = dragged.current;
      clear();
      await actions.run(`${task.id}:assign`, 'Unassign', (api) => api.unassign(task.id));
    },
    // dragend: a clone the browser ended without a drop still runs.
    end() { transferOff.current?.({ ended: true }); clear(); },
    // leaveTable drops the marker once the pointer leaves the list.
    leaveTable(event) {
      if (!dragged.current || event.currentTarget.contains(event.relatedTarget)) return;
      setOver(null);
    },
    overRow(event, task) {
      if (!accept(event) || dragged.current.id === task.id) return;
      const rect = event.currentTarget.getBoundingClientRect();
      const where = event.clientY < rect.top + rect.height / 2 ? 'before' : 'after';
      if (over?.id !== task.id || over?.where !== where) setOver({ id: task.id, where });
    },
    overIdeas(event) {
      if (!accept(event)) return;
      if (over?.id !== IDEAS_BAND) setOver({ id: IDEAS_BAND, where: 'into' });
    },
    async dropOnRow(event, task) {
      if (!accept(event)) return;
      const moving = dragged.current;
      const where = over?.id === task.id ? over.where : 'before';
      clear();
      if (moving.id === task.id) return;
      if (isClosed(moving)) {
        // A closed task dropped among the live ones is reopened there, in
        // that task's stage, in one write; among the ideas, without its
        // assignee (which takes the assign right).
        const opts = { [where]: task.id };
        if (task.stage === 'idea' && moving.assignee?.agent_id) {
          if (!canAssign(actions)) { shellToast('An assigned task reopens in Next: taking its assignee off needs taskboard.assign', true); return; }
          opts.to = '';
        }
        await actions.run(`${moving.id}:move`, 'Reopen', (api) => api.reopen(moving.id, opts));
        return;
      }
      // Grouped by epic, a drop among another epic's queue rows puts the
      // task in that epic too, as a drag between epic lanes does, with Undo.
      const rows = epicRows?.current;
      const into = rows && !isEpic(moving) && task.stage !== 'idea' ? rows.of(task) : null;
      if (into !== null && into !== rows.of(moving)) {
        const epic = into === NO_EPIC ? '' : into;
        const label = epic ? `to ${rows.name(epic)}` : 'out of its epic';
        const ok = await actions.run(`${moving.id}:move`, 'Move', async (api) => {
          await intoEpic(moving.id, epic)(api);
          return api.move(moving.id, { [where]: task.id });
        });
        if (ok) epicMoved(actions, moving, label);
        return;
      }
      await actions.run(`${moving.id}:move`, 'Move', (api) => api.move(moving.id, { [where]: task.id }));
    },
    async dropOnIdeas(event) {
      if (!accept(event)) return;
      const moving = dragged.current;
      clear();
      const firstIdea = tasks.find((t) => t.stage === 'idea' && t.id !== moving.id);
      if (isClosed(moving)) {
        if (moving.assignee?.agent_id && !canAssign(actions)) {
          shellToast('An assigned task reopens in Next: taking its assignee off needs taskboard.assign', true);
          return;
        }
        const opts = firstIdea ? { before: firstIdea.id } : { stage: 'idea' };
        if (moving.assignee?.agent_id) opts.to = '';
        await actions.run(`${moving.id}:move`, 'Reopen', (api) => api.reopen(moving.id, opts));
      } else if (moving.stage !== 'idea') {
        await actions.run(`${moving.id}:move`, 'Demote', (api) => api.stage(moving.id, 'idea'));
      } else if (firstIdea) {
        await actions.run(`${moving.id}:move`, 'Move', (api) => api.move(moving.id, { before: firstIdea.id }));
      }
    },
  };
}

const IDEAS_BAND = '__ideas';
const REOPEN_DRAG_TITLE = 'Drag among the tasks above, onto the Ideas line or onto an agent to reopen it there';

// isClosed: a done or dropped task, which a drag reopens.
function isClosed(task) {
  return task?.status === 'done' || task?.status === 'dropped';
}

// HistoryLine is the "n done this week · ▸ history" line: a row of the list's
// table under its tasks (the closed ones follow it, in the same columns),
// or a line of its own under an empty list.
// ARCHIVE_AGES are the "Archive closed" ages: every such task, or those
// closed longer ago than an age (`taskboard archive <group> --done
// --dropped --older-than`).
export const ARCHIVE_AGES = [
  { age: '', label: 'All of them' },
  { age: '1d', label: 'Closed over a day ago' },
  { age: '7d', label: 'Closed over a week ago' },
  { age: '30d', label: 'Closed over 30 days ago' },
];

// ARCHIVE_KINDS are what "Archive closed ▾" archives: the done tasks, the
// dropped ones, or both. Done alone goes through archiveDone, which a
// linked node of any version has.
const ARCHIVE_KINDS = [
  { key: 'done', label: 'Done tasks', statuses: ['done'], flags: '--done' },
  { key: 'dropped', label: 'Dropped tasks', statuses: ['dropped'], flags: '--dropped' },
  { key: 'both', label: 'Done and dropped', statuses: ['done', 'dropped'], flags: '--done --dropped' },
];

// archiveMenu is the "Archive closed ▾" items: a kind the board has
// closed tasks of, then the ages. Null when there is nothing to archive or
// no way to.
function archiveMenu(group, actions, closed, counts) {
  if (!group || !actions || actions.busy || typeof actions.api?.archiveDone !== 'function') return null;
  const has = {
    // The closed list is capped: a count says so too when it holds none.
    done: closed.some((task) => task.status === 'done') || (counts.done_this_week || 0) > 0,
    dropped: typeof actions.api?.archiveClosed === 'function' && (closed.some((task) => task.status === 'dropped') || (counts.dropped || 0) > 0),
  };
  const kinds = ARCHIVE_KINDS.filter((kind) => kind.statuses.every((status) => has[status]));
  if (!kinds.length) return null;
  return kinds.map((kind) => ({
    key: kind.key, label: kind.label, attrs: { 'data-archive-kind': kind.key },
    children: ARCHIVE_AGES.map(({ age, label }) => ({
      key: `${kind.key}-${age || 'all'}`, label,
      attrs: { 'data-archive-age': age || 'all', title: `taskboard archive ${group.name} ${kind.flags}${age ? ` --older-than ${age}` : ''}` },
      run: () => actions.run(`${group.name}:archive-closed`, 'Archive closed', (api) => (kind.key === 'done'
        ? api.archiveDone(group.name, age)
        : api.archiveClosed(group.name, kind.statuses, age))),
    })),
  }));
}

function HistoryLine({ entry, group = null, actions = null, open, onToggle, wizard, row = false }) {
  const counts = entry.counts || {};
  const closed = entry.closed || [];
  const parts = [`${counts.done_this_week || 0} done this week`];
  if (counts.dropped) parts.push(`${counts.dropped} dropped`);
  const archive = archiveMenu(group, actions, closed, counts);
  const body = html`${parts.join(' · ')}
    ${closed.length > 0 && html` · <${Button} size="sm" variant="link" class="tb-history-toggle" aria-expanded=${open ? 'true' : 'false'}
      onClick=${onToggle}>${open ? '▾' : '▸'} ${wizard ? 'chronicle' : 'history'}<//>`}
    ${archive && html` · <span class="tb-archive-done"><${RowMenu} label="closed tasks" items=${archive} glyph="Archive closed ▾"
      title="Archive done or dropped tasks: they leave the history, kept (ls --archived)" buttonClass="tb-archive-done-btn" /></span>`}
    <span class="tb-history-note"> — ${wizard
      ? 'a quest leaves the board when it is fulfilled; it stays in the chronicle'
      : 'a task leaves the list when it is done or dropped; it stays in history'}</span>`;
  return row
    ? html`<tr class="tb-history-row"><td colspan=${TABLE_COLS}><div class="tb-history">${body}</div></td></tr>`
    : html`<div class="tb-history">${body}</div>`;
}

// TaskboardRows is the open section's list: the queue, the Ideas band, the
// capture box, the history line and, unfolded, the recently closed tasks.
// Without `actions` it is the read-only list.
// full is the board's whole entry, epics included, when entry is the one
// its views draw (groups-taskboard-epics.js boardEntry): the epic strip
// draws from it, and an epic's link opens its form.
export function TaskboardRows({ entry, full = entry, group, snapshot, wizard = isWizardActive(), actions = null, readApi = null, view = 'list', lanesBy = 'agent', hidden = LIST_HIDDEN_DEFAULT, onHidden = () => {}, onNewTask = null, onLayout = null, byEpic = false }) {
  // The List's Filter (ListFilter) hides task states; done and dropped are
  // the history, which the history line's toggle shows or hides together.
  const shows = (task) => !hidden.includes(taskRowState(task));
  const history = !hidden.includes('done') || !hidden.includes('dropped');
  const setHistory = (on) => onHidden(on ? hidden.filter((s) => s !== 'done' && s !== 'dropped') : [...new Set([...hidden, 'done', 'dropped'])]);
  const tasks = entry.tasks || [];
  const byId = boardTasks(entry);
  const epicRows = useRef(null);
  const drag = useRowDrag(tasks, actions, group, epicRows);
  // An agent row dragged onto a task assigns it (useAgentOnTask).
  const boardRef = useRef(null);
  const boardNode = group.fleet_node && !group.fleet_node.local ? group.fleet_node.id : '';
  useAgentOnTask(boardRef, group, boardNode, [...tasks, ...(entry.closed || [])], actions);
  const queue = tasks.filter((task) => task.stage !== 'idea' && shows(task));
  const ideas = tasks.filter((task) => task.stage === 'idea' && shows(task));
  const filtered = tasks.length - queue.length - ideas.length;
  // Against the whole entry: the drawn one leaves out the strip's epics and
  // what the Epic filter hides, which are on the board, not past its cap.
  const more = (full.counts?.live || 0) - (full.tasks || []).length;
  const name = group.name;
  // ✎ opens the edit form; the row's ⋯ Block… and Drop… ask their reason
  // in the reason dialog (groups-taskboard-reason.js).
  const [editing, setEditing] = useState('');
  const [reasoning, setReasoning] = useState(null);
  const [answering, setAnswering] = useState('');
  // The edit form is the task's link (task-route.js): opening it records a
  // history step, closing it steps back, and a link or Back/Forward opens
  // or closes it here.
  const node = group.fleet_node && !group.fleet_node.local ? group.fleet_node.id : '';
  const routeOf = (id) => ({ taskId: id, node, group: group.name });
  const jumped = useRef('');
  const board = useMemo(() => ({ group: group.name, node }), [group.name, node]);
  // A click (✎, the title) opens the form where the task already is: no
  // jump to it, which is for a link (jumped below).
  const openEdit = (id) => { jumped.current = id; setEditing(id); openTaskRoute(routeOf(id)); };
  // A task opened from the open form (an epic's Tasks) leaves it: unsaved
  // input asks first, as Cancel does.
  const openFromForm = async (id) => {
    if (dirtyRef.current && !(await shellConfirmDiscard())) return;
    dirtyRef.current = false;
    openEdit(id);
  };
  const closeEdit = () => { if (editing) closeTaskRoute(routeOf(editing)); setEditing(''); };
  const [moving, setMoving] = useState(null);
  const onForm = (id, mode) => {
    if (mode === 'edit') openEdit(id);
    else if (mode === 'answer') setAnswering(id);
    else if (mode === 'move' || mode === 'reopen') setMoving({ id, reopen: mode === 'reopen' });
    else setReasoning(mode ? { id, kind: mode } : null);
  };
  const listed = (id) => [...(full.tasks || []), ...(full.closed || [])].find((task) => task.id === id) || null;
  // A linked task the lists do not hold (past the board's cap, or
  // archived) is read on its own.
  const [fetched, setFetched] = useState(null);
  const want = taskRoute.value;
  const wanted = want && want.group === group.name && (want.node || '') === node ? want : null;
  const wantedListed = !!(wanted && listed(wanted.taskId));
  // dirtyRef is the open form's unsaved input (EditTaskDialog). Back,
  // Forward or a link that leaves a form with unsaved input asks first, as
  // Cancel does; keeping it puts the form's entry back.
  const dirtyRef = useRef(false);
  const asking = useRef(false);
  const agreed = useRef('');
  const [askedTick, setAskedTick] = useState(0);
  useEffect(() => {
    const leaving = editing && !sameTask(want, routeOf(editing));
    if (leaving && dirtyRef.current && agreed.current !== editing) {
      if (asking.current) return undefined;
      asking.current = true;
      const kept = editing;
      Promise.resolve(shellConfirmDiscard()).then((ok) => {
        asking.current = false;
        if (ok) { agreed.current = kept; setAskedTick((n) => n + 1); return; }
        openTaskRoute(routeOf(kept));
      });
      return undefined;
    }
    agreed.current = '';
    if (!wanted) {
      jumped.current = '';
      // Back (or a link to another task) closed the form the link opened.
      if (editing && !sameTask(want, routeOf(editing))) setEditing('');
      return undefined;
    }
    // A read-only board has no edit form to open.
    if (!actions) { dropTaskRoute(wanted); return undefined; }
    if (editing !== wanted.taskId) setEditing(wanted.taskId);
    let live = true;
    if (!wantedListed && fetched?.id !== wanted.taskId) {
      // A task off the list (history, archived) is read by id, which names
      // no group: one moved to another group since the link was made is
      // that board's, not this one's, and a gone one has no form.
      const read = readApi?.task ? readApi.task(wanted.taskId) : Promise.reject(new Error('no read'));
      read.then((task) => {
        if (!live) return;
        if (task?.id === wanted.taskId && (task.group || group.name) === group.name) setFetched(task);
        else dropTaskRoute(wanted);
      }).catch(() => { if (live) dropTaskRoute(wanted); });
    }
    if (jumped.current !== wanted.taskId) {
      jumped.current = wanted.taskId;
      jumpToTask({ shown: group.shownName || group.name, group: group.name, node, taskId: wanted.taskId });
    }
    return () => { live = false; };
  }, [want, wantedListed, askedTick]);
  const edited = editing && (listed(editing) || (fetched?.id === editing ? fetched : null));
  const rowProps = { byId, group, snapshot, tasks, actions, readApi: readApi || actions?.api || null, drag: actions ? drag : null, onForm, columns: entry.columns || [] };
  // The band heads every idea-stage row the filter leaves (a blocked or
  // assigned idea is not in the "ideas" state), so no idea passes for a queue
  // row a drop next to would demote into.
  const ideasBand = (ideas.length > 0 || (actions && queue.length > 0 && !hidden.includes('idea'))) && html`<tr class=${`tb-ideas-divider${drag.over?.id === IDEAS_BAND ? ' tb-drop-into' : ''}`}
      onDragOver=${actions ? drag.overIdeas : undefined} onDrop=${actions ? drag.dropOnIdeas : undefined}><td colspan=${TABLE_COLS}>
    <span class="ui-eyebrow">${wizard ? 'Visions' : 'Ideas'} · ${ideas.length} · captured, not yet in the queue${actions ? ' — ✓ Next promotes; drag across this line to promote or demote' : ''}</span></td></tr>`;
  // The Lanes view has no Filter: its history is all or nothing.
  const closed = view === 'list' ? (entry.closed || []).filter(shows) : (history ? entry.closed || [] : []);
  const historyLine = html`<${HistoryLine} entry=${entry} group=${group} actions=${actions} open=${history} wizard=${wizard} row=${tasks.length > 0} onToggle=${() => setHistory(!history)} />`;
  // Grouped by epic (the Filter's "group by epic"), the queue is one group
  // per open epic, in the board's order, then No epic: each under a head row
  // that folds it. One level: the ideas and the history stay as they are.
  const epicBoard = useContext(EpicBoard);
  const [folded, setFolded] = useState({});
  const grouped = view === 'list' && byEpic && epicBoard?.epics.length ? [...epicBoard.epics.map((epic) => epic.id), NO_EPIC]
    .map((id) => ({ id, rows: queue.filter((task) => (epicOf(task, epicBoard.byId) || NO_EPIC) === id) }))
    .filter(({ id, rows }) => rows.length || (id !== NO_EPIC && epicBoard.byId.get(id)?.stage !== 'idea' && (!epicBoard.filter.hide || epicBoard.filter.epic === id))) : null;
  epicRows.current = grouped ? { of: (task) => epicOf(task, epicBoard.byId) || NO_EPIC, name: (id) => epicBoard.byId.get(id)?.title || id } : null;
  // One table: the queue, the Ideas band, the ideas, the history line and the
  // closed tasks share its columns.
  const table = tasks.length ? html`<table class="tb-table ui-table-auto" onDragLeave=${actions ? drag.leaveTable : undefined}>${COLUMNS}
      <thead><tr><th class="tb-ic"></th><th title=${wizard ? 'Quests in the operator\'s order: top is next' : 'Tasks in the operator\'s order: top is next'}>Name</th>
        <th>State</th><th>Last</th><th>Age</th><th>Who</th><th>Ref</th><th>Deps</th><th>Files</th></tr></thead>
      <tbody>
        ${grouped ? grouped.map(({ id, rows }) => html`<${EpicGroupRow} key=${`epic-${id}`} epic=${id === NO_EPIC ? null : epicBoard.byId.get(id)} count=${rows.length}
            colspan=${TABLE_COLS} folded=${!!folded[id]} onFold=${(on) => setFolded({ ...folded, [id]: on })} onOpen=${actions ? openEdit : null} />
          ${!folded[id] && rows.map((task) => html`<${TaskRow} key=${task.id} task=${task} ...${rowProps} chip=${false} />`)}`)
          : queue.map((task) => html`<${TaskRow} key=${task.id} task=${task} ...${rowProps} />`)}
        ${ideasBand}
        ${ideas.map((task) => html`<${TaskRow} key=${task.id} task=${task} ...${rowProps} />`)}
        ${historyLine}
        ${closed.map((task) => html`<${TaskRow} key=${task.id} task=${task} ...${rowProps} />`)}
      </tbody>
    </table>` : null;
  const editDialog = edited && actions && html`<${EditTaskDialog} key=${edited.id} group=${group} entry=${full} task=${edited} onOpenTask=${openFromForm}
    me=${myAuthor(group.fleet_node && !group.fleet_node.local ? group.fleet_node.id : '')}
    api=${actions.api} canAssign=${canAssign(actions)} canClose=${canClose(edited, actions)} members=${assignableMembers(group)} wizard=${wizard}
    nameOf=${(actor) => actorLabel(actor, group, snapshot)} dirtyRef=${dirtyRef} onClose=${closeEdit} onSaved=${() => { void actions.refresh(); }} />`;
  const reasonTask = reasoning && actions && tasks.find((task) => task.id === reasoning.id);
  const reasonDialog = reasonTask && html`<${TaskReasonDialog} key=${`${reasonTask.id}-${reasoning.kind}`} kind=${reasoning.kind} task=${reasonTask}
    busy=${actions.busy === `${reasonTask.id}:form`} onClose=${() => setReasoning(null)}
    onSubmit=${(reason) => actions.run(`${reasonTask.id}:form`, reasoning.kind === 'block' ? 'Block' : 'Drop',
      (api) => api.status(reasonTask.id, reasoning.kind === 'block' ? 'blocked' : 'dropped', reason))} />`;
  const answerTask = answering && tasks.find((task) => task.id === answering);
  const moveTask = moving && actions && [...tasks, ...(entry.closed || [])].find((task) => task.id === moving.id);
  const moveDialog = moveTask && html`<${TaskMoveDialog} key=${`${moveTask.id}-${moving.reopen}`} task=${moveTask} reopen=${moving.reopen}
    columns=${entry.columns || []} canAssign=${canAssign(actions)} wizard=${wizard} busy=${actions.busy === `${moveTask.id}:move`}
    onClose=${() => setMoving(null)} onSubmit=${(write) => actions.run(`${moveTask.id}:move`, write.name, write.run)} />`;
  return html`<${TaskLink.Provider} value=${board}><div class="tb-board" data-taskboard=${name} ref=${boardRef}>
    ${!(view === 'lanes' && lanesBy === 'epic') && html`<${EpicStrip} entry=${full} actions=${actions} onEdit=${actions ? openEdit : null} />`}
    ${editDialog}
    ${reasonDialog}
    ${moveDialog}
    ${answerTask && html`<${AnswerFor} task=${answerTask} actions=${actions} askedBy=${actorLabel(askerOf(answerTask), group, snapshot)} onClose=${() => setAnswering('')} />`}
    ${view === 'list' && canAssign(actions) && tasks.find((task) => task.id === drag.dragging)?.assignee && html`<div class="tb-unassign-drop"
      onDragOver=${drag.overUnassign} onDrop=${drag.dropUnassign}>⤺ drop here to unassign · or onto an agent above to reassign</div>`}
    ${view === 'lanes' && FLAT_MODES.includes(lanesBy) ? html`<${TaskboardFlat} entry=${entry} group=${group} snapshot=${snapshot} mode=${lanesBy} actions=${actions} readApi=${readApi || actions?.api || null} wizard=${wizard} onNewTask=${onNewTask}
      onEdit=${openEdit} />` : view === 'lanes' ? html`<${TaskboardLanes} entry=${entry} group=${group} snapshot=${snapshot} by=${lanesBy} actions=${actions} readApi=${readApi || actions?.api || null} wizard=${wizard} onNewTask=${onNewTask} onLayout=${onLayout}
      onEdit=${openEdit} />` : table || html`<div class="gf-empty ui-empty tb-empty">${wizard ? 'No quests on the board.' : 'No tasks on the board.'}${actions
      ? ' Add one with ＋ above.' : html` Add one with <code>tclaude agent taskboard add ${name} "…"</code>.`}</div>`}
    ${view === 'list' && filtered > 0 && html`<div class="tb-more tb-filtered muted" data-tb-filtered=${filtered}>${filtered} ${filtered === 1 ? (wizard ? 'quest' : 'task') : (wizard ? 'quests' : 'tasks')} hidden by the filter ·
      <${Button} size="sm" variant="link" class="tb-filter-reset" onClick=${() => onHidden(hidden.filter((state) => state === 'done' || state === 'dropped'))}>show all<//></div>`}
    ${more > 0 && html`<div class="tb-more muted">+ ${more} more · <code>tclaude agent taskboard ls ${name}</code></div>`}
    ${(view === 'lanes' || !tasks.length) && historyLine}
    ${(view === 'lanes' || !tasks.length) && closed.length > 0 && html`<table class="tb-table ui-table-auto tb-closed-table">${COLUMNS}<tbody>
      ${closed.map((task) => html`<${TaskRow} key=${task.id} task=${task} ...${rowProps} />`)}
    </tbody></table>`}
  </div><//>`;
}

const VIEW_PREF = (group) => `tclaude.dash.groups.taskboard.view.${group}`;

// LIST_STATES are the task states the List's Filter offers, in the order a
// task goes through them; a state added later (in review, in CI) joins here.
export const LIST_STATES = [
  ['open', 'open', 'next in the queue'], ['queued', 'queued', 'waits on other tasks'], ['idea', 'ideas', 'captured, not yet in the queue'],
  ['in-progress', 'in progress', 'an agent holds it'], ['waiting', 'waiting', 'the operator holds it: an agent waits on them'],
  ['stalled', 'stalled', 'its agent went idle without reporting'], ['blocked', 'blocked', 'waits on something outside the board'],
  ['done', 'done', 'finished this week'], ['dropped', 'dropped', 'dropped without being done'],
];
// listStates are the Filter's states for a board: LIST_STATES with the
// board's custom columns (`col:<slug>`) after in progress.
export function listStates(columns = []) {
  const at = LIST_STATES.findIndex(([key]) => key === 'in-progress') + 1;
  const custom = (columns || []).map((column) => [COLUMN_STATE + column.slug, column.label, `in the board's ${column.label} column`]);
  return [...LIST_STATES.slice(0, at), ...custom, ...LIST_STATES.slice(at)];
}
// The list hides what is closed unless asked, as before the Filter.
export const LIST_HIDDEN_DEFAULT = ['done', 'dropped'];
const LIST_HIDDEN_PREF = (group) => `tclaude.dash.groups.taskboard.list-hidden.${group}`;

// listHiddenDefault is what the List hides before a choice: the closed
// tasks, and the columns the board hides (tk_bf5d2b02), Ideas as the idea
// state.
export function listHiddenDefault(entry) {
  const board = boardHidden(entry).map((key) => (key === 'ideas' ? 'idea' : key));
  return [...new Set([...LIST_HIDDEN_DEFAULT, ...board])];
}

// listHidden reads the states the List hides, per group and viewer: the
// default until a choice is made. It stores what is hidden, so a state added
// later shows until hidden.
export function listHidden(group, defaults = LIST_HIDDEN_DEFAULT) {
  try {
    const raw = dashPrefs.getItem(LIST_HIDDEN_PREF(group));
    const list = raw ? JSON.parse(raw) : null;
    if (Array.isArray(list)) return list.filter((state) => LIST_STATES.some(([key]) => key === state) || String(state).startsWith(COLUMN_STATE));
  } catch (_) { /* a malformed value reads as the default */ }
  return [...defaults];
}

const LIST_BY_EPIC_PREF = (group) => `tclaude.dash.groups.taskboard.list-by-epic.${group}`;
const LANES_BY_PREF = (group) => `tclaude.dash.groups.taskboard.lanes-by.${group}`;
// The "lanes" modes: a lane per agent, grouped by agent, role or subgroup;
// or triage (You · Unassigned · Agents) and one row
// (groups-taskboard-lanes-flat.js).
const LANES_BY = ['agent', 'role', 'subgroup', 'triage', 'row', 'epic'];
const LANES_BY_LABEL = { row: 'one row' };
const LANES_BY_TITLE = {
  triage: 'Three lanes: you, unassigned, and every agent\'s tasks together',
  row: 'Every task in one row',
  epic: 'A lane per open epic, and No epic: drag a card to another epic\'s lane to move it there',
};

// Lanes in triage mode is the default view; a viewer's remembered choice
// still wins.
export function taskboardView(group) {
  return dashPrefs.getItem(VIEW_PREF(group)) === 'list' ? 'list' : 'lanes';
}

export function taskboardLanesBy(group) {
  const by = dashPrefs.getItem(LANES_BY_PREF(group));
  return LANES_BY.includes(by) ? by : 'triage';
}

// useViewPrefs reads the section's List | Lanes choice and the lanes' "by",
// remembered per group and per viewer in dashPrefs like the section folds.
function useViewPrefs(group, listDefaults = LIST_HIDDEN_DEFAULT) {
  const [, bump] = useState(0);
  const set = (key, value) => { dashPrefs.setItem(key, value); bump((n) => n + 1); };
  return {
    view: taskboardView(group),
    by: taskboardLanesBy(group),
    listHidden: listHidden(group, listDefaults),
    listDefaults,
    // The columns this viewer hides on the board (groups-taskboard-colvis.js).
    mineColumns: mineHidden(group),
    setMineColumns: (keys) => { setMineHidden(group, keys); bump((n) => n + 1); },
    // null puts the default back.
    setListHidden: (value) => {
      if (value) dashPrefs.setItem(LIST_HIDDEN_PREF(group), JSON.stringify(value));
      else dashPrefs.removeItem(LIST_HIDDEN_PREF(group));
      bump((n) => n + 1);
    },
    setView: (value) => set(VIEW_PREF(group), value),
    // The List grouped by epic (ListFilter's "group by epic").
    listByEpic: dashPrefs.getItem(LIST_BY_EPIC_PREF(group)) === '1',
    setListByEpic: (on) => { if (on) dashPrefs.setItem(LIST_BY_EPIC_PREF(group), '1'); else dashPrefs.removeItem(LIST_BY_EPIC_PREF(group)); bump((n) => n + 1); },
    setBy: (value) => set(LANES_BY_PREF(group), value),
    // The lanes' own order (groups-taskboard-lanes-layout.js): whether one
    // is stored, and putting it back to the default.
    customOrder: hasCustomOrder(group),
    resetOrder: () => { resetLaneOrder(group); bump((n) => n + 1); },
    refresh: () => bump((n) => n + 1),
  };
}

// ViewSelector is the section's List | Lanes switch and, in Lanes, the
// "lanes" agent | role | subgroup | triage | one row switch, with "reset order" once the
// lanes have been reordered.
function ViewSelector({ prefs }) {
  return html`<${Seg} class="tb-view" aria-label="Task board view">
    <${Button} size="sm" pressed=${prefs.view === 'list'} onClick=${() => prefs.setView('list')}>☰ List<//>
    <${Button} size="sm" pressed=${prefs.view === 'lanes'} onClick=${() => prefs.setView('lanes')}>⇶ Lanes<//>
  <//>${prefs.view === 'lanes' && html`<span class="ui-hint tb-lanes-by-label">lanes</span><${Seg} class="tb-view" aria-label="Lanes">
    ${LANES_BY.map((by) => html`<${Button} key=${by} size="sm" pressed=${prefs.by === by} title=${LANES_BY_TITLE[by]} onClick=${() => prefs.setBy(by)}>${LANES_BY_LABEL[by] || by}<//>`)}
  <//>${prefs.customOrder && !FLAT_MODES.includes(prefs.by) && html`<${Button} size="sm" variant="link" class="tb-lanes-reset" title="Put the lanes and their headings back in the default order"
    onClick=${prefs.resetOrder}>reset order<//>`}`}`;
}

// ListFilter is the List's Filter dropdown (view-menu.js, as the Groups
// tab's view and the Collab panel's Filter): which task states the list
// shows, with Reset. Its count is the states that differ from the default.
function ListFilter({ group, prefs, columns = [], epics = false }) {
  const [open, setOpen] = useState(false);
  const hidden = prefs.listHidden;
  const states = listStates(columns);
  const defaults = prefs.listDefaults || LIST_HIDDEN_DEFAULT;
  const changes = states.filter(([key]) => hidden.includes(key) !== defaults.includes(key)).length;
  // Element ids from the group's name, every other character spelled out,
  // so name@node and a local name-node never share one.
  const id = `tb-filter-${String(group).replace(/[^a-zA-Z0-9-]/g, (c) => `_${c.codePointAt(0).toString(16)}_`)}`;
  return html`<${ViewMenu} open=${open} onOpenChange=${setOpen} label="Filter ▾" badge=${changes} buttonClass="tb-list-filter"
      ids=${{ button: `${id}-btn`, menu: `${id}-menu`, badge: `${id}-badge` }} title="Choose which task states the list shows">
    <${ViewMenuHeading}>State<//>
    ${states.map(([key, text, title]) => html`<${ViewToggle} key=${key} id=${`${id}-${key.replace(':', '-')}`} title=${title} checked=${!hidden.includes(key)}
      onChange=${(on) => prefs.setListHidden(on ? hidden.filter((state) => state !== key) : [...hidden, key])}>${text}<//>`)}
    ${epics && html`<${ViewMenuSep} /><${ViewMenuHeading}>Group<//>
      <${ViewToggle} id=${`${id}-by-epic`} title="The queue under its epics, each with a head that folds it" checked=${prefs.listByEpic}
        onChange=${prefs.setListByEpic}>group by epic<//>`}
    <${ViewMenuSep} />
    <button data-ui-exempt type="button" class="view-menu-reset" data-tb-filter-reset="" disabled=${changes === 0}
      onClick=${() => prefs.setListHidden(null)}>Reset</button>
  <//>`;
}

// TaskboardSection is the group card's Task board: folded, one line of
// counts; it opens on its own when something needs the operator, until they
// fold or open it themselves (remembered per group). `api` is the writes'
// client; null draws the read-only list.
export function TaskboardSection({ group: shown, snapshot, source: ownSource = taskboardContext, api: ownApi = taskboardApi, boards = nodeTaskboard }) {
  // The board, its grants and every write use the group's own name; the
  // section's fold and hooks keep the name the view shows (name@node for a
  // linked node's group), which is unique across nodes.
  const group = ownBoardGroup(shown);
  const { source, api, readApi } = boardOf(group, ownSource, ownApi, boards);
  const entry = useTaskboardEntry(group.name, source);
  const actions = useBoardActions(source, api);
  // adding is the open "+ New task" dialog: { kind: 'form' | 'ask' | 'import',
  // initial } (initial pre-fills the form, from a lanes cell's "+").
  const [adding, setAdding] = useState(null);
  const prefs = useViewPrefs(shown.name, listHiddenDefault(entry));
  const epics = useEpicBoard(shown.name, entry);
  // Once the section has opened on its own (something needed the
  // operator), it stays open for this page, so clearing the last blocked
  // task never folds it under a form or the capture box in use. The
  // operator's own fold or open is remembered and wins as before.
  const autoOpened = useRef(false);
  if (!entry) return null;
  const wizard = isWizardActive();
  const counts = entry.counts || {};
  if ((counts.needs_you || 0) > 0) autoOpened.current = true;
  // The views draw the board without its open epics (the strip has them),
  // under the Epic filter.
  const drawn = boardEntry(entry, epics.filter);
  // The lanes draw the columns neither the board nor this viewer hides.
  const view = withColumnView(drawn, { group: group.name, mine: prefs.mineColumns, onMine: prefs.setMineColumns, actions: api ? actions : null });
  const lanes = prefs.view === 'lanes';
  // choose opens what the add menu picked; a lanes cell's "+" passes its
  // start state (and the lane's agent) along.
  const choose = (what, initial = {}) => {
    setAdding({ kind: what, initial });
  };
  // As Agents and Resources: open, the controls are a toolbar row heading
  // the section (＋▾, then the view switch); folded, the ＋▾ rides the
  // section line.
  const addMenu = api && html`<${NewTaskMenu} group=${group} entry=${entry} wizard=${wizard} onChoose=${choose} />`;
  const toolbar = html`<div class="group-header-actions gf-title-row tb-toolbar">${addMenu}<${ViewSelector} prefs=${prefs} />${
    prefs.view === 'list' && html`<${ListFilter} group=${shown.name} prefs=${prefs} columns=${entry.columns || []} epics=${epics.epics.length > 0} />`}<${EpicFilterMenu} group=${shown.name} />${
    lanes && html`<${ColumnsMenu} entry=${view} group=${shown.name} />`}${
    lanes && html`<${HiddenColumnsTag} entry=${view} tally=${hiddenTally(view, laneColumn)} />`}</div>`;
  const closeAdding = () => setAdding(null);
  const dialog = !api || !adding ? null : adding.kind === 'form' || adding.kind === 'epic'
    ? html`<${NewTaskDialog} group=${group} entry=${entry} api=${api} wizard=${wizard} canAssign=${canAssign(actions)}
        initial=${adding.kind === 'epic' ? { ...adding.initial, kind: 'epic' } : adding.initial}
        members=${assignableMembers(group)} onClose=${closeAdding} onDone=${() => { void Promise.resolve(source.refresh?.()).catch(() => {}); }} />`
    : adding.kind === 'ask' ? html`<${AskAgentDialog} group=${group} entry=${entry} api=${api} wizard=${wizard} initial=${adding.initial} onClose=${closeAdding} />`
    : adding.kind === 'import' ? html`<${TrackerTaskDialog} group=${group} entry=${entry} api=${api} wizard=${wizard} initial=${adding.initial}
        onClose=${closeAdding} onDone=${() => { void Promise.resolve(source.refresh?.()).catch(() => {}); }} />` : null;
  return html`<${EpicBoard.Provider} value=${epics}>${dialog}<${FusedSection} group=${shown.name} section="taskboard" label=${wizard ? 'Quest board' : 'Task board'}
      summary=${html`<${TaskboardCounts} counts=${counts} who=${operatorName(group)} columns=${entry.columns || []} />`} auto=${autoOpened.current}
      closedActions=${addMenu || null} attrs=${{ 'data-fused-taskboard': shown.name }}>
    ${toolbar}
    <${TaskboardRows} entry=${lanes ? view : drawn} full=${entry} group=${group} snapshot=${snapshot} wizard=${wizard} actions=${api ? actions : null} readApi=${readApi}
      view=${prefs.view} lanesBy=${prefs.by} byEpic=${prefs.listByEpic}
      hidden=${prefs.listHidden} onHidden=${prefs.setListHidden} onNewTask=${choose} onLayout=${prefs.refresh} />
  <//><//>`;
}

// TaskboardCounts is the section head's total; the columns show the
// breakdown, which the tooltip carries too.
// The tasks waiting on the operator stand out beside it.
function TaskboardCounts({ counts, who = 'you', columns = [] }) {
  const waiting = counts.waiting_on_you || 0;
  return html`<span class="tb-counts" title=${taskboardCountsText(counts, who, columns)}>
    <span class=${`gf-count${counts.live ? '' : ' zero'}`}>${counts.live || 0}</span>
    ${waiting > 0 && html`<span class="ui-tag tb-waiting-tag" data-tb-waiting=${waiting}>${waiting} waiting on ${who}</span>`}
  </span>`;
}
