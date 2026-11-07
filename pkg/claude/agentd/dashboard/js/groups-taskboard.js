// groups-taskboard.js -- the Task board section of a group card (wizard:
// Quest board), and its counts on the group's summary line
// (docs/taskboard.md, tcl-o1tzq6, tcl-n7i6dt). The queue in the operator's
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
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';
import { Button, Check, Input, Seg, Select, Textarea } from './ui-components.js';
import { RowMenu } from './row-menu.js';
import { useAgentColumns } from './agent-align.js';
import { dashPrefs } from './prefs.js';
import { shellToast } from './shell-state.js';
import { createTaskboardApi, importRefs, taskboardApi } from './taskboard-actions.js';
import { nodeFetch, nodeSnapshot, nodeUpload, onPeerPage } from './node-api.js';
import { dashboardState } from './snapshot-store.js';
import { markLanded } from './drop-landing.js';
import { isWizardActive } from './slop.js';
import { FusedSection } from './groups-processes.js';
import { TaskboardLanes } from './groups-taskboard-lanes.js';
import { hasCustomOrder, resetLaneOrder } from './groups-taskboard-lanes-layout.js';
import { AskAgentDialog, NewTaskDialog, NewTaskMenu } from './groups-taskboard-new.js';
import { EditTaskDialog } from './groups-taskboard-edit.js';
import { listenForTransfer, runCrossTransfer, runTransfer, transferTargets } from './groups-taskboard-transfer.js';
import {
  actorAgentId, createTaskboardContextSource, taskRowState, taskboardContext, taskboardCountParts, taskboardCountsText,
  useTaskboardEntry,
} from './taskboard-context.js';

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
function boardOf(group, source, api, boards) {
  const node = group?.fleet_node && !group.fleet_node.local ? group.fleet_node.id : '';
  if (node) {
    const board = boards(node);
    return { source: board.source, api: peerApi(board.api, nodeSnapshot(node)?.peer_view, group.name) };
  }
  if (onPeerPage()) {
    return { source, api: peerApi(api, dashboardState.snapshot.value?.peer_view, group.name) };
  }
  return { source, api };
}

// agentLabel names an agent from the snapshot: the group's own members
// first (an assignee is one), then any group's.
function agentLabel(agentId, group, snapshot) {
  if (!agentId) return '';
  const find = (members) => (members || []).find((member) => member.agent_id === agentId);
  const member = find(group?.members)
    || (snapshot?.groups || []).map((g) => find(g.members)).find(Boolean);
  return member?.title || agentId.slice(0, 12);
}

function actorLabel(actor, group, snapshot) {
  if (actor === 'human:operator') return 'operator';
  const id = actorAgentId(actor);
  return id ? agentLabel(id, group, snapshot) : actor || '';
}

const STATE_LABELS = {
  blocked: 'blocked', stalled: 'stalled', 'in-progress': 'in progress', queued: 'queued', idea: 'idea', open: 'open', done: 'done', dropped: 'dropped',
};

export function RefChip({ task }) {
  const ref = task.ref;
  if (!ref) return html`<span class="tb-ref-none">—</span>`;
  const label = ref.label || ref.url || ref.kind;
  const title = `${ref.kind}${ref.url ? ` · ${ref.url}` : ''}`;
  // The store only keeps http(s) URLs; check again before making a link.
  if (ref.url && /^https?:\/\//i.test(ref.url)) {
    return html`<a class="ui-tag ui-tag-accent tb-ref" href=${ref.url} target="_blank" rel="noopener noreferrer" title=${title}>🔗 ${label}</a>`;
  }
  return html`<span class="ui-tag ui-tag-accent tb-ref" title=${title}>🔗 ${label}</span>`;
}

export function DepChips({ task, numbers }) {
  const unmet = new Set(task.unmet || []);
  return (task.depends_on || []).map((dep) => {
    const n = numbers.get(dep);
    const text = n ? `after #${n}` : `after ${dep}`;
    return html`<span key=${dep} class=${`tb-dep${unmet.has(dep) ? ' unmet' : ''}`}
      title=${`${unmet.has(dep) ? 'Waits on' : 'Done:'} ${dep}`}>${text}</span>`;
  });
}

function Who({ task, group, snapshot }) {
  const agent = task.assignee?.agent_id;
  if (!agent) return html`<span class="tb-unassigned">unassigned</span>`;
  const name = task.assignee?.name || agentLabel(agent, group, snapshot);
  const by = actorLabel(task.assigned_by, group, snapshot);
  // The name ellipsizes in its cell; who assigned it, and when, is the
  // tooltip.
  const detail = [task.assigned_at && `assigned ${relTime(task.assigned_at)}`, by && `by ${by}`].filter(Boolean).join(' ');
  return html`<span class="tb-who" title=${detail ? `${name} · ${detail}` : `${name} · ${agent}`}>
    <span class="tb-avatar" aria-hidden="true">${name.slice(0, 1).toUpperCase()}</span>
    <span class="tb-who-name">${name}</span>
  </span>`;
}

function StatePill({ task, numbers }) {
  const state = taskRowState(task);
  let text = STATE_LABELS[state];
  if (state === 'in-progress' && task.assigned_at) text = `in progress · ${relTime(task.assigned_at).replace(/ ago$/, '')}`;
  if (state === 'stalled' && task.idle_since) text = `stalled · idle ${relTime(task.idle_since).replace(/ ago$/, '')}`;
  if (state === 'queued') {
    const waits = (task.unmet || []).map((dep) => (numbers.get(dep) ? `#${numbers.get(dep)}` : dep));
    if (waits.length) text = `queued · waits on ${waits.join(', ')}`;
  }
  if (state === 'idea' && task.created_at) text = `idea · ${relTime(task.created_at)}`;
  if (state === 'done' || state === 'dropped') {
    const when = relTime(task.closed_at);
    if (when) text = `${text} · ${when}`;
  }
  return html`<span class=${`ui-tag${state === 'open' ? ' ui-tag-accent' : ''} tb-state tb-state-${state}`}>${text}</span>`;
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
function RowActions({ task, group, tasks, actions, onForm }) {
  const state = taskRowState(task);
  const closed = state === 'done' || state === 'dropped';
  const go = (key, label, fn) => () => actions.run(`${task.id}:${key}`, label, fn);
  const icon = (key, glyph, title, onClick, extra = '') => html`<${Button} key=${key} size="sm" icon=${true}
    class=${`icon-btn tb-act tb-act-${key}${extra ? ` ${extra}` : ''}`} title=${title} aria-label=${title.split(':')[0]}
    busy=${actions.busy === `${task.id}:${key}`} disabled=${!!actions.busy} onClick=${onClick}>${glyph}<//>`;
  // A slot keeps the cluster's buttons lined up down the table.
  const slot = (key) => html`<span key=${key} class="tb-act-slot" aria-hidden="true"></span>`;
  let first;
  if (closed) first = icon('reopen', '↺', 'Reopen: put it back on the list where it was', go('reopen', 'Reopen', (api) => api.status(task.id, 'open')));
  else if (task.stage === 'idea') first = icon('promote', '✓', 'Next: taskboard promote, to the end of the queue', go('promote', 'Promote', (api) => api.stage(task.id, 'next')), 'tb-act-go');
  else if (canClose(task, actions)) first = icon('done', '✓', 'Done: taskboard done', go('done', 'Done', (api) => api.status(task.id, 'done')), 'tb-act-ok');
  else first = slot('done');
  const edit = icon('edit', '✎', 'Edit: title, brief, ref, where it is, assignee, dependencies and files', () => onForm('edit'));
  return html`<span class="row-actions tb-row-actions">${first}${edit}<${RowMenu} label=${task.title} items=${rowMenu({ task, group, tasks, actions, onForm })}
    glyph="⋯" buttonClass="icon-btn tb-act tb-act-more" keepSlot=${true} title="More: assign, block, move or clone to a group, drop" /></span>`;
}

// rowMenu is a row's ⋯ items (row-menu.js): assign (to a member), block or
// unblock, unassign, move or clone to another group (the groups a drag
// could reach, this node's or a linked one's, with a drop's confirmation)
// and drop.
function rowMenu({ task, group, tasks, actions, onForm }) {
  const state = taskRowState(task);
  const closed = state === 'done' || state === 'dropped';
  const run = (key, label, fn) => () => actions.run(`${task.id}:${key}`, label, fn);
  const item = (key, label, title, rest) => ({ label, attrs: { 'data-task-menu': key, title }, ...rest });
  const items = [];
  if (actions.busy) return items;
  if (canAssign(actions) && !closed && task.status === 'open') {
    items.push(item('assign', 'Assign', `taskboard assign --to: give it to a member, the brief goes to its inbox${task.stage === 'idea' ? ' and the idea joins the queue' : ''} (or drag the row onto an agent)`, {
      children: () => {
        const members = assignableMembers(group);
        if (!members.length) return [{ label: 'No member to assign to', disabled: true }];
        return members.map((member) => {
          const here = task.assignee?.agent_id === member.agent_id;
          return item(`assign-to`, `${member.title || member.agent_id}${member.online ? '' : ' (offline)'}${here ? ' ✓' : ''}`, `taskboard assign --to ${member.agent_id}`, {
            key: member.agent_id,
            disabled: here,
            run: () => actions.run(`${task.id}:assign`, 'Assign', (api) => api.assign(task.id, member.agent_id))
              .then((ok) => { if (ok) markLanded({ group: group.name, node: group.fleet_node?.id || '', section: 'agents', ids: [member.agent_id] }); }),
          });
        });
      },
    }));
  }
  if (!closed && task.status === 'blocked') items.push(item('unblock', 'Unblock', 'taskboard unblock', { run: run('unblock', 'Unblock', (api) => api.status(task.id, 'open')) }));
  else if (!closed) items.push(item('block', 'Block…', 'taskboard block: with a reason', { run: () => onForm('block') }));
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
  return items;
}

// RowForm is the inline form under a row: the reason for a block or a drop
// (the row's ⋯). Enter submits, Escape closes. Everything else is the edit
// form (groups-taskboard-edit.js).
// assignableMembers are the group's members a task can go to: active
// agents with a stable id, online ones first.
export function assignableMembers(group) {
  return (group?.members || []).filter((member) => member.agent_id && !member.retired)
    .sort((a, b) => Number(!!b.online) - Number(!!a.online) || String(a.title || '').localeCompare(String(b.title || '')));
}

function RowForm({ task, mode, actions, onClose }) {
  const [reason, setReason] = useState('');
  const submit = async () => {
    let ok;
    if (mode === 'block') {
      if (!reason.trim()) return;
      ok = await actions.run(`${task.id}:form`, 'Block', (api) => api.status(task.id, 'blocked', reason.trim()));
    } else {
      ok = await actions.run(`${task.id}:form`, 'Drop', (api) => api.status(task.id, 'dropped', reason.trim()));
    }
    if (ok) onClose();
  };
  const keys = (event) => {
    if (event.key === 'Enter') { event.preventDefault(); if (!actions.busy) void submit(); }
    if (event.key === 'Escape') { event.preventDefault(); onClose(); }
  };
  const busy = actions.busy === `${task.id}:form`;
  return html`<tr class="tb-form-row" data-task-form=${mode}><td></td><td colspan=${TABLE_COLS - 1}>
    <div class="tb-form"><${Input} size="sm" class="tb-form-reason" aria-label=${mode === 'block' ? 'Block reason' : 'Drop reason'} autoFocus
        placeholder=${mode === 'block' ? 'why is it blocked? (required)' : 'why drop it? (optional, e.g. "parked in awb")'}
        value=${reason} maxLength=${400} onInput=${(event) => setReason(event.currentTarget.value)} onKeyDown=${keys} />
      <${Button} size="sm" variant=${mode === 'drop' ? 'danger' : 'primary'} busy=${busy}
        disabled=${busy || (mode === 'block' && !reason.trim())} onClick=${() => void submit()}>${mode === 'block' ? 'Block' : 'Drop'}<//>
      <${Button} size="sm" variant="ghost" onClick=${onClose}>Cancel<//>
    </div>
  </td></tr>`;
}

const DRAG_TITLE = 'Drag to reorder, onto an agent to assign, or onto another group to move it (Ctrl/Cmd: clone)';
// linkDrag is a drag of a link in the row (the 🔗 ref): the browser's own
// URL drag, not the task's.
const linkDrag = (event) => event.target !== event.currentTarget && !!event.target?.closest?.('a[href]');

function TaskRow({ task, n, numbers, group, snapshot, tasks, actions, drag, form, onForm }) {
  const state = taskRowState(task);
  const closed = state === 'done' || state === 'dropped';
  const over = drag?.over?.id !== task.id ? '' : drag.over.where === 'before' ? ' tb-drop-before' : ' tb-drop-after';
  const deps = task.depends_on || [];
  const files = task.attachments?.length || 0;
  // The whole row drags, as an agent row does (⋮⋮ is its grip); a press on
  // a button, link or field in it is a click, not a drag (row-drag.js
  // bindTaskRowDrag).
  const draggable = !!actions && !closed && !!drag;
  const row = html`<tr class=${`tb-row tb-row-${state}${closed ? ' tb-closed' : ''}${over}${drag?.dragging === task.id ? ' tb-dragging' : ''}`} data-task-id=${task.id}
      draggable=${draggable ? 'true' : undefined} data-dnd-task=${draggable ? task.id : undefined} title=${draggable ? DRAG_TITLE : undefined}
      onDragStart=${draggable ? (event) => { if (!linkDrag(event)) drag.start(event, task); } : undefined} onDragEnd=${draggable ? drag.end : undefined}
      onDragOver=${closed ? undefined : (event) => drag?.overRow(event, task)} onDrop=${closed ? undefined : (event) => drag?.dropOnRow(event, task)}>
    <td class="tb-ic">${actions && html`${closed ? html`<span class="tb-handle tb-handle-off" aria-hidden="true"></span>` : html`<span class="tb-handle"
        aria-hidden="true">⋮⋮</span>`}<${RowActions} task=${task} group=${group} tasks=${tasks}
        actions=${actions} onForm=${(mode) => onForm(task.id, mode)} />`}</td>
    <td class="tb-title-cell">
      ${n ? html`<span class="tb-n" title="Its place in the operator's order: top is next">${n}</span>` : ''}
      <span class="tb-title">${task.title}</span>
      <span class="tb-id" title=${task.id}>${task.id}</span>
      ${task.status === 'blocked' && task.block_reason && html`<div class="tb-reason">blocked: “${task.block_reason}”</div>`}
    </td>
    <td><${StatePill} task=${task} numbers=${numbers} /></td>
    <td class="tb-t" title=${task.updated_at || ''}>${task.updated_at ? relTime(task.updated_at) : ''}</td>
    <td class="tb-t" title=${task.created_at || ''}>${task.created_at ? relTime(task.created_at) : ''}</td>
    <td class="tb-who-cell"><${Who} task=${task} group=${group} snapshot=${snapshot} /></td>
    <td class="tb-ref-cell"><${RefChip} task=${task} /></td>
    <td class="tb-deps-cell">${deps.length ? html`<${DepChips} task=${task} numbers=${numbers} />` : html`<span class="tb-none">—</span>`}</td>
    <td class="tb-files-cell">${files ? html`<span class="tb-files" title=${`${files} file${files === 1 ? '' : 's'} on this task`}>📎 ${files}</span>` : html`<span class="tb-none">—</span>`}</td>
  </tr>`;
  if (!form || !actions) return row;
  return [row, html`<${RowForm} key=${`${task.id}-form`} task=${task} mode=${form} actions=${actions} onClose=${() => onForm(task.id, null)} />`];
}

// The list's columns. The leading cluster, Name, State, Last and Age take
// the Agents table's tracks above (useAgentColumns), as a Resources table
// does, so they line up down the group's card; Who, Ref, Deps and Files
// are the board's own. The Ideas band and history are rows of the same table.
const COLUMNS = html`<colgroup><col class="tb-col-ctl" /><col class="tb-col-name" /><col class="tb-col-state" /><col class="tb-col-last" /><col class="tb-col-age" /><col class="tb-col-who" /><col class="tb-col-ref" /><col class="tb-col-deps" /><col class="tb-col-files" /></colgroup>`;
const TABLE_COLS = 9;

// alignWithAgents sets the list's cluster, Name, State, Last and Age widths
// to the Agents table's (agent-align.js), or back to its own without one.
function alignWithAgents(table, cols) {
  // Not narrower than the row's own cluster (⋮⋮ ✓ ✎ ⋯).
  table.style.setProperty('--tb-ctl-w', cols?.ctl >= 100 ? `${cols.ctl}px` : '');
  for (const key of ['name', 'state', 'last', 'age']) table.style.setProperty(`--tb-${key}-w`, cols?.[key] ? `${cols[key]}px` : '');
}

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

function clearAgentTarget(state) {
  state.row?.classList.remove(ASSIGN_OVER);
  state.row?.querySelector('td.name-cell')?.removeAttribute('data-tb-assign');
  state.row = null;
}

function useRowDrag(tasks, actions, group = null, numbers = new Map()) {
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
    // Only an open task can be assigned (a blocked one is refused).
    const target = (event) => dragged.current?.status === 'open' && agentDropRow(event.target, group);
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
      const n = numbers.get(dragged.current.id);
      const already = dragged.current.assignee?.agent_id === hit.member.agent_id;
      hit.row.querySelector('td.name-cell')?.setAttribute('data-tb-assign',
        already ? 'already assigned here' : `assign ${n ? `#${n}` : dragged.current.id} to ${hit.member.title || hit.member.agent_id}`);
    };
    const onDrop = (event) => {
      const hit = target(event);
      if (!hit) return;
      event.preventDefault();
      event.stopPropagation();
      const task = dragged.current;
      end();
      if (task.assignee?.agent_id === hit.member.agent_id) return;
      void actions.run(`${task.id}:assign`, 'Assign', (api) => api.assign(task.id, hit.member.agent_id))
        .then((ok) => { if (ok) markLanded({ group: group.name, node: group.fleet_node?.id || '', section: 'agents', ids: [hit.member.agent_id] }); });
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
      if (group) transferOff.current = listenForTransfer({ task, group, tasks, actions, onDrop: clear });
    },
    // dropUnassign takes the dragged task back from its assignee.
    overUnassign(event) {
      if (!dragged.current?.assignee) return;
      accept(event);
    },
    async dropUnassign(event) {
      if (!dragged.current?.assignee || !accept(event)) return;
      const task = dragged.current;
      clear();
      await actions.run(`${task.id}:assign`, 'Unassign', (api) => api.unassign(task.id));
    },
    end: clear,
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
      await actions.run(`${moving.id}:move`, 'Move', (api) => api.move(moving.id, { [where]: task.id }));
    },
    async dropOnIdeas(event) {
      if (!accept(event)) return;
      const moving = dragged.current;
      clear();
      const firstIdea = tasks.find((t) => t.stage === 'idea' && t.id !== moving.id);
      if (moving.stage !== 'idea') {
        await actions.run(`${moving.id}:move`, 'Demote', (api) => api.stage(moving.id, 'idea'));
      } else if (firstIdea) {
        await actions.run(`${moving.id}:move`, 'Move', (api) => api.move(moving.id, { before: firstIdea.id }));
      }
    },
  };
}

const IDEAS_BAND = '__ideas';

// importSummary says what an import did beyond the count: the items that
// kept their id as the title (with the server's note, e.g. a GitHub ref read
// from the dashboard) and the refused ones.
function importSummary(out) {
  const results = out?.results || [];
  const notes = results.filter((result) => result.task && result.note);
  const refused = results.filter((result) => result.error);
  const parts = [];
  if (notes.length) parts.push(`${notes.length} kept the id as the title (${notes.map((result) => `${result.input}: ${result.note}`).join('; ')})`);
  if (refused.length) parts.push(`${refused.length} refused (${refused.map((result) => `${result.input}: ${result.code || result.error}`).join('; ')})`);
  return parts.join(' · ');
}

// ImportPanel is "+ New task › From awb/Linear/GitHub…": paste tracker ids, one task per id,
// titled from the tracker (taskboard add --from-awb / --from-linear /
// --from-github).
function ImportPanel({ group, actions, initial = {}, onClose }) {
  const [kind, setKind] = useState('awb');
  const [ids, setIds] = useState('');
  // An import lands in Ideas or Next: a cell's Next (or an agent's lane)
  // starts it queued; its assignee or block is set on the board afterwards.
  const [next, setNext] = useState(!!initial.start && initial.start !== 'idea' && initial.start !== 'done');
  const later = initial.to || initial.start === 'blocked' || initial.start === 'done';
  const [note, setNote] = useState('');
  const refs = importRefs(kind, ids);
  const submit = async () => {
    if (!refs.length || actions.busy) return;
    setNote(`looking up ${refs.length} title${refs.length === 1 ? '' : 's'}…`);
    const out = await actions.run('import', 'Import', (api) => api.importRefs(group.name, refs, { next }));
    if (!out) { setNote(''); return; }
    const more = importSummary(out);
    if (!more && !out.failed) { onClose(); return; }
    // Only the refs that were not sent stay in the box, so Import again
    // never adds the landed ones twice.
    setIds(out.failed ? out.failed.remaining.join(', ') : '');
    setNote(`${out.created || 0} added${more ? ` · ${more}` : ''}${out.failed
      ? ` · stopped: ${out.failed.message}; ${out.failed.remaining.length} not sent, left in the box` : ''}`);
  };
  return html`<div class="tb-import" data-taskboard-import=${group.name}>
    <span class="ui-eyebrow">Import from a tracker · one task per id, titled from the tracker</span>
    <div class="tb-import-row">
      <${Select} size="sm" aria-label="Tracker" value=${kind} onChange=${(event) => setKind(event.currentTarget.value)}>
        <option value="awb">awb</option><option value="linear">Linear</option><option value="github">GitHub</option>
      <//>
      <${Textarea} size="sm" class="tb-import-ids" rows="2" aria-label="Tracker ids" value=${ids} autoFocus
        placeholder=${kind === 'github' ? 'owner/repo#12, or issue / PR URLs' : kind === 'linear' ? 'LIN-12, LIN-34' : 'tcl-a1b2c3, tcl-d4e5f6'}
        onInput=${(event) => setIds(event.currentTarget.value)} />
    </div>
    <div class="tb-import-row">
      <${Check} checked=${next} onChange=${(event) => setNext(event.currentTarget.checked)}>add to the queue (next)<//>
      ${note && html`<span class="tb-capture-note" role="status">${note}</span>`}
      ${!note && later && html`<span class="ui-hint tb-import-later">an import lands in Ideas or Next; assign, block or close the tasks on the board afterwards</span>`}
      <span class="tb-spacer"></span>
      <${Button} size="sm" variant="primary" busy=${actions.busy === 'import'} disabled=${!refs.length || !!actions.busy}
        onClick=${() => void submit()}>Import ${refs.length || ''}<//>
      <${Button} size="sm" variant="ghost" onClick=${onClose}>Close<//>
    </div>
  </div>`;
}

// HistoryLine is the "n done this week · ▸ history" line: a row of the list's
// table under its tasks (the closed ones follow it, in the same columns),
// or a line of its own under an empty list.
function HistoryLine({ entry, open, onToggle, wizard, row = false }) {
  const counts = entry.counts || {};
  const closed = entry.closed || [];
  const parts = [`${counts.done_this_week || 0} done this week`];
  if (counts.dropped) parts.push(`${counts.dropped} dropped`);
  const body = html`${parts.join(' · ')}
    ${closed.length > 0 && html` · <${Button} size="sm" variant="link" class="tb-history-toggle" aria-expanded=${open ? 'true' : 'false'}
      onClick=${onToggle}>${open ? '▾' : '▸'} ${wizard ? 'chronicle' : 'history'}<//>`}
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
export function TaskboardRows({ entry, group, snapshot, wizard = isWizardActive(), actions = null, importing = false, importInitial = {}, onImportClose = () => {}, view = 'list', lanesBy = 'agent', onNewTask = null, onLayout = null }) {
  const [history, setHistory] = useState(false);
  const [forms, setForms] = useState({});
  const tasks = entry.tasks || [];
  const numbers = new Map(tasks.map((task, i) => [task.id, i + 1]));
  const drag = useRowDrag(tasks, actions, group, numbers);
  const queue = tasks.filter((task) => task.stage !== 'idea');
  const ideas = tasks.filter((task) => task.stage === 'idea');
  // Numbers run down the whole list, as the operator reads it.
  const more = (entry.counts?.live || 0) - tasks.length;
  const name = group.name;
  // ✎ opens the edit form; the row's ⋯ Block… and Drop… are inline.
  const [editing, setEditing] = useState('');
  const onForm = (id, mode) => (mode === 'edit' ? setEditing(id) : setForms((current) => ({ ...current, [id]: mode })));
  const edited = editing && [...tasks, ...(entry.closed || [])].find((task) => task.id === editing);
  const rowProps = { numbers, group, snapshot, tasks, actions, drag: actions ? drag : null, onForm };
  const tableRef = useRef(null);
  useAgentColumns(tableRef, alignWithAgents);
  const ideasBand = (ideas.length > 0 || (actions && queue.length > 0)) && html`<tr class=${`tb-ideas-divider${drag.over?.id === IDEAS_BAND ? ' tb-drop-into' : ''}`}
      onDragOver=${actions ? drag.overIdeas : undefined} onDrop=${actions ? drag.dropOnIdeas : undefined}><td colspan=${TABLE_COLS}>
    <span class="ui-eyebrow">${wizard ? 'Visions' : 'Ideas'} · ${ideas.length} · captured, not yet in the queue${actions ? ' — ✓ Next promotes; drag across this line to promote or demote' : ''}</span></td></tr>`;
  const closed = history ? (entry.closed || []) : [];
  const historyLine = html`<${HistoryLine} entry=${entry} open=${history} wizard=${wizard} row=${tasks.length > 0} onToggle=${() => setHistory(!history)} />`;
  // One table: the queue, the Ideas band, the ideas, the history line and the
  // closed tasks share its columns.
  const table = tasks.length ? html`<table class="tb-table" ref=${tableRef} onDragLeave=${actions ? drag.leaveTable : undefined}>${COLUMNS}
      <thead><tr><th class="tb-ic"></th><th title=${wizard ? 'Quests in the operator\'s order: top is next' : 'Tasks in the operator\'s order: top is next'}>Name</th>
        <th>State</th><th>Last</th><th>Age</th><th>Who</th><th>Ref</th><th>Deps</th><th>Files</th></tr></thead>
      <tbody>
        ${queue.map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${numbers.get(task.id)} form=${forms[task.id]} ...${rowProps} />`)}
        ${ideasBand}
        ${ideas.map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${numbers.get(task.id)} form=${forms[task.id]} ...${rowProps} />`)}
        ${historyLine}
        ${closed.map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${0} form=${null} ...${rowProps} />`)}
      </tbody>
    </table>` : null;
  const editDialog = edited && actions && html`<${EditTaskDialog} key=${edited.id} group=${group} entry=${entry} task=${edited}
    api=${actions.api} canAssign=${canAssign(actions)} canClose=${canClose(edited, actions)} members=${assignableMembers(group)} wizard=${wizard}
    nameOf=${(actor) => actorLabel(actor, group, snapshot)} onClose=${() => setEditing('')} onSaved=${() => { void actions.refresh(); }} />`;
  return html`<div class="tb-board" data-taskboard=${name}>
    ${editDialog}
    ${importing && actions && html`<${ImportPanel} key=${JSON.stringify(importInitial || {})} group=${group} actions=${actions} initial=${importInitial} onClose=${onImportClose} />`}
    ${view === 'list' && canAssign(actions) && tasks.find((task) => task.id === drag.dragging)?.assignee && html`<div class="tb-unassign-drop"
      onDragOver=${drag.overUnassign} onDrop=${drag.dropUnassign}>⤺ drop here to unassign · or onto an agent above to reassign</div>`}
    ${view === 'lanes' ? html`<${TaskboardLanes} entry=${entry} group=${group} snapshot=${snapshot} by=${lanesBy} actions=${actions} wizard=${wizard} onNewTask=${onNewTask} onLayout=${onLayout}
      onEdit=${(id) => setEditing(id)} />` : table || html`<div class="gf-empty ui-empty tb-empty">${wizard ? 'No quests on the board.' : 'No tasks on the board.'}${actions
      ? ' Add one with ＋ above.' : html` Add one with <code>tclaude agent taskboard add ${name} "…"</code>.`}</div>`}
    ${more > 0 && html`<div class="tb-more muted">+ ${more} more · <code>tclaude agent taskboard ls ${name}</code></div>`}
    ${(view === 'lanes' || !tasks.length) && historyLine}
    ${(view === 'lanes' || !tasks.length) && closed.length > 0 && html`<table class="tb-table tb-closed-table">${COLUMNS}<tbody>
      ${closed.map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${0} form=${null} ...${rowProps} />`)}
    </tbody></table>`}
  </div>`;
}

const VIEW_PREF = (group) => `tclaude.dash.groups.taskboard.view.${group}`;

const LANES_BY_PREF = (group) => `tclaude.dash.groups.taskboard.lanes-by.${group}`;
const LANES_BY = ['agent', 'role', 'subgroup'];

export function taskboardView(group) {
  return dashPrefs.getItem(VIEW_PREF(group)) === 'lanes' ? 'lanes' : 'list';
}

export function taskboardLanesBy(group) {
  const by = dashPrefs.getItem(LANES_BY_PREF(group));
  return LANES_BY.includes(by) ? by : 'agent';
}

// useViewPrefs reads the section's List | Lanes choice and the lanes' "by",
// remembered per group and per viewer in dashPrefs like the section folds.
function useViewPrefs(group) {
  const [, bump] = useState(0);
  const set = (key, value) => { dashPrefs.setItem(key, value); bump((n) => n + 1); };
  return {
    view: taskboardView(group),
    by: taskboardLanesBy(group),
    setView: (value) => set(VIEW_PREF(group), value),
    setBy: (value) => set(LANES_BY_PREF(group), value),
    // The lanes' own order (groups-taskboard-lanes-layout.js): whether one
    // is stored, and putting it back to the default.
    customOrder: hasCustomOrder(group),
    resetOrder: () => { resetLaneOrder(group); bump((n) => n + 1); },
    refresh: () => bump((n) => n + 1),
  };
}

// ViewSelector is the section's List | Lanes switch and, in Lanes, the
// "lanes by" agent | role | subgroup switch, with "reset order" once the
// lanes have been reordered.
function ViewSelector({ prefs }) {
  return html`<${Seg} class="tb-view" aria-label="Task board view">
    <${Button} size="sm" pressed=${prefs.view === 'list'} onClick=${() => prefs.setView('list')}>☰ List<//>
    <${Button} size="sm" pressed=${prefs.view === 'lanes'} onClick=${() => prefs.setView('lanes')}>⇶ Lanes<//>
  <//>${prefs.view === 'lanes' && html`<span class="ui-hint tb-lanes-by-label">lanes by</span><${Seg} class="tb-view" aria-label="Lanes by">
    ${LANES_BY.map((by) => html`<${Button} key=${by} size="sm" pressed=${prefs.by === by} onClick=${() => prefs.setBy(by)}>${by}<//>`)}
  <//>${prefs.customOrder && html`<${Button} size="sm" variant="link" class="tb-lanes-reset" title="Put the lanes and their headings back in the default order"
    onClick=${prefs.resetOrder}>reset order<//>`}`}`;
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
  const { source, api } = boardOf(group, ownSource, ownApi, boards);
  const entry = useTaskboardEntry(group.name, source);
  const actions = useBoardActions(source, api);
  const [importing, setImporting] = useState(false);
  const [importInitial, setImportInitial] = useState({});
  // adding is the open "+ New task" dialog: { kind: 'form' | 'ask',
  // initial } (initial pre-fills the form, from a lanes cell's "+").
  const [adding, setAdding] = useState(null);
  const prefs = useViewPrefs(shown.name);
  // Once the section has opened on its own (something needed the
  // operator), it stays open for this page, so clearing the last blocked
  // task never folds it under a form or the capture box in use. The
  // operator's own fold or open is remembered and wins as before.
  const autoOpened = useRef(false);
  if (!entry) return null;
  const wizard = isWizardActive();
  const counts = entry.counts || {};
  if ((counts.needs_you || 0) > 0) autoOpened.current = true;
  // choose opens what the add menu picked; a lanes cell's "+" passes its
  // start state (and the lane's agent) along.
  const choose = (what, initial = {}) => {
    if (what === 'import') { setImportInitial(initial); setImporting(true); } else setAdding({ kind: what, initial });
  };
  // As Agents and Resources: open, the controls are a toolbar row heading
  // the section (＋▾, then the view switch); folded, the ＋▾ rides the
  // section line.
  const addMenu = api && html`<${NewTaskMenu} group=${group} entry=${entry} wizard=${wizard} onChoose=${choose} />`;
  const toolbar = html`<div class="group-header-actions gf-title-row tb-toolbar">${addMenu}<${ViewSelector} prefs=${prefs} /></div>`;
  const closeAdding = () => setAdding(null);
  const dialog = !api || !adding ? null : adding.kind === 'form'
    ? html`<${NewTaskDialog} group=${group} entry=${entry} api=${api} wizard=${wizard} canAssign=${canAssign(actions)} initial=${adding.initial}
        members=${assignableMembers(group)} onClose=${closeAdding} onDone=${() => { void Promise.resolve(source.refresh?.()).catch(() => {}); }} />`
    : adding.kind === 'ask' ? html`<${AskAgentDialog} group=${group} entry=${entry} api=${api} wizard=${wizard} initial=${adding.initial} onClose=${closeAdding} />` : null;
  return html`${dialog}<${FusedSection} group=${shown.name} section="taskboard" label=${wizard ? 'Quest board' : 'Task board'}
      summary=${html`<${TaskboardCounts} counts=${counts} />`} auto=${autoOpened.current}
      closedActions=${addMenu || null} force=${importing} attrs=${{ 'data-fused-taskboard': shown.name }}>
    ${toolbar}
    <${TaskboardRows} entry=${entry} group=${group} snapshot=${snapshot} wizard=${wizard} actions=${api ? actions : null}
      importing=${importing} importInitial=${importInitial} onImportClose=${() => setImporting(false)} view=${prefs.view} lanesBy=${prefs.by}
      onNewTask=${choose} onLayout=${prefs.refresh} />
  <//>`;
}

// TaskboardCounts is the section head's total; the columns (and the group's
// summary line) show the breakdown, which the tooltip carries too.
function TaskboardCounts({ counts }) {
  return html`<span class="tb-counts" title=${taskboardCountsText(counts)}>
    <span class=${`gf-count${counts.live ? '' : ' zero'}`}>${counts.live || 0}</span>
  </span>`;
}

// TaskboardSummaryChip carries the counts on the group's summary line, so a
// folded group still says what is on its board. Nothing when it is empty.
export function TaskboardSummaryChip({ group: shown, source: ownSource = taskboardContext, boards = nodeTaskboard }) {
  const group = ownBoardGroup(shown);
  const { source } = boardOf(group, ownSource, null, boards);
  const entry = useTaskboardEntry(group.name, source);
  if (!entry || !entry.counts?.live) return null;
  const parts = taskboardCountParts(entry.counts);
  const wizard = isWizardActive();
  return html`<span class="group-taskboard-chip" data-taskboard-chip=${shown.name} title=${`${wizard ? 'Quest board' : 'Task board'}: ${taskboardCountsText(entry.counts)}`}>
    ${wizard ? 'quest board' : 'task board'}: ${parts.length
      ? parts.map((part, i) => html`<span key=${part.key} class=${`tb-count tb-count-${part.key}`}>${i ? ' · ' : ''}${part.text}</span>`)
      : `${entry.counts.live}`}
  </span>`;
}
