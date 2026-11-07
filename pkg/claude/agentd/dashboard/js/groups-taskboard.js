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
// row; the capture box adds a line or a pasted list; "+ from awb/Linear…"
// imports tracker ids. Assigning and the lanes view come in later slices.
//
// Data comes from GET /v1/taskboard/context (taskboard-context.js). While
// features.taskboard is off, or for a group this viewer cannot read, nothing
// is drawn. A linked node's group (the fused view, or a peer's page) is read
// from that node through its peer view, under the taskboard.read /
// taskboard.write grants it gave this node; its actions show only under
// taskboard.write (or unrestricted trust).

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';
import { Button, Check, Input, Seg, Select, Textarea } from './ui-components.js';
import { dashPrefs } from './prefs.js';
import { shellToast } from './shell-state.js';
import { captureLines, createTaskboardApi, importRefs, isTrackerLine, taskboardApi } from './taskboard-actions.js';
import { nodeFetch, nodeSnapshot, onPeerPage } from './node-api.js';
import { dashboardState } from './snapshot-store.js';
import { isWizardActive } from './slop.js';
import { FusedSection } from './groups-processes.js';
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
    board = { source: createTaskboardContextSource({ fetchImpl }), api: createTaskboardApi({ fetchImpl }) };
    nodeBoards.set(node, board);
  }
  return board;
}

// peerWrites reports whether a linked node's view lets this node change the
// group's board: unrestricted trust, or its taskboard.write grant there.
export function peerWrites(peerView, groupName) {
  if (!peerView) return false;
  if (peerView.level === 'unrestricted') return true;
  const held = peerView.groups?.[groupName];
  return Array.isArray(held) && held.includes('taskboard.write');
}

// boardOf picks the source and write client for group: this node's own, or
// the linked node's it lives on (api null where its grants allow no writes).
function boardOf(group, source, api, boards) {
  const node = group?.fleet_node && !group.fleet_node.local ? group.fleet_node.id : '';
  if (node) {
    const board = boards(node);
    return { source: board.source, api: peerWrites(nodeSnapshot(node)?.peer_view, group.name) ? board.api : null };
  }
  if (onPeerPage()) {
    return { source, api: api && peerWrites(dashboardState.snapshot.value?.peer_view, group.name) ? api : null };
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
  blocked: 'blocked', 'in-progress': 'in progress', queued: 'queued', idea: 'idea', open: 'open', done: 'done', dropped: 'dropped',
};

function RefChip({ task }) {
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

function DepChips({ task, numbers }) {
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
  const name = agentLabel(agent, group, snapshot);
  const by = actorLabel(task.assigned_by, group, snapshot);
  return html`<span class="tb-who" title=${task.assigned_at ? `assigned ${relTime(task.assigned_at)}` : agent}>
    <span class="tb-avatar" aria-hidden="true">${name.slice(0, 1).toUpperCase()}</span>
    <span class="tb-who-name">${name}</span>
    ${by && html`<span class="tb-by">by ${by}</span>`}
  </span>`;
}

function StatePill({ task, numbers }) {
  const state = taskRowState(task);
  let text = STATE_LABELS[state];
  if (state === 'in-progress' && task.assigned_at) text = `in progress · ${relTime(task.assigned_at).replace(/ ago$/, '')}`;
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
  return { busy, run, api };
}

// RowActions are a row's buttons for its state. Each names its CLI verb.
function RowActions({ task, actions, onForm }) {
  const state = taskRowState(task);
  const b = (key, label, title, onClick, variant = '') => html`<${Button} key=${key} size="sm" variant=${variant || undefined}
    class=${`tb-act tb-act-${key}`} title=${title} busy=${actions.busy === `${task.id}:${key}`} disabled=${!!actions.busy}
    onClick=${onClick}>${label}<//>`;
  const go = (key, label, fn) => () => actions.run(`${task.id}:${key}`, label, fn);
  if (state === 'done' || state === 'dropped') {
    return b('reopen', 'Reopen', 'Put it back on the list where it was', go('reopen', 'Reopen', (api) => api.status(task.id, 'open')));
  }
  const edit = b('edit', 'Edit', 'taskboard edit: change the title or ref', () => onForm('edit'));
  const drop = b('drop', 'Drop', 'taskboard drop: take it off the board (park it in the tracker first)', () => onForm('drop'), 'ghost danger');
  if (state === 'blocked') {
    return [b('unblock', 'Unblock', 'taskboard unblock', go('unblock', 'Unblock', (api) => api.status(task.id, 'open'))), edit, drop];
  }
  if (task.stage === 'idea') {
    return [b('promote', '✓ Next', 'taskboard promote: to the end of the queue', go('promote', 'Promote', (api) => api.stage(task.id, 'next')), 'primary'), edit, drop];
  }
  return [
    b('done', 'Done', 'taskboard done', go('done', 'Done', (api) => api.status(task.id, 'done'))),
    b('block', 'Block', 'taskboard block: with a reason', () => onForm('block')),
    edit, drop,
  ];
}

// RowForm is the inline form under a row: edit its title and ref, or give
// the reason for a block or a drop. Enter submits, Escape closes.
function RowForm({ task, mode, actions, onClose }) {
  const [title, setTitle] = useState(task.title);
  const [ref, setRef] = useState(task.ref?.url || task.ref?.label || '');
  const [reason, setReason] = useState('');
  const submit = async () => {
    let ok;
    if (mode === 'edit') {
      const change = {};
      if (title.trim() !== task.title) change.title = title.trim();
      const before = task.ref?.url || task.ref?.label || '';
      if (ref.trim() !== before) {
        if (ref.trim()) change.ref = ref.trim();
        else change.clearRef = true;
      }
      if (!Object.keys(change).length) { onClose(); return; }
      ok = await actions.run(`${task.id}:form`, 'Edit', (api) => api.edit(task.id, change));
    } else if (mode === 'block') {
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
  const fields = mode === 'edit'
    ? html`<${Input} size="sm" class="tb-form-title" aria-label="Title" value=${title} maxLength=${200} autoFocus
        onInput=${(event) => setTitle(event.currentTarget.value)} onKeyDown=${keys} />
      <${Input} size="sm" class="tb-form-ref" aria-label="Ref" placeholder="ref: URL, owner/repo#12 or a tracker id" value=${ref}
        onInput=${(event) => setRef(event.currentTarget.value)} onKeyDown=${keys} />`
    : html`<${Input} size="sm" class="tb-form-reason" aria-label=${mode === 'block' ? 'Block reason' : 'Drop reason'} autoFocus
        placeholder=${mode === 'block' ? 'why is it blocked? (required)' : 'why drop it? (optional, e.g. "parked in awb")'}
        value=${reason} maxLength=${400} onInput=${(event) => setReason(event.currentTarget.value)} onKeyDown=${keys} />`;
  const label = mode === 'edit' ? 'Save' : mode === 'block' ? 'Block' : 'Drop';
  return html`<tr class="tb-form-row" data-task-form=${mode}><td></td><td colspan="5">
    <div class="tb-form">${fields}
      <${Button} size="sm" variant=${mode === 'drop' ? 'danger' : 'primary'} busy=${busy}
        disabled=${busy || (mode === 'block' && !reason.trim()) || (mode === 'edit' && !title.trim())} onClick=${() => void submit()}>${label}<//>
      <${Button} size="sm" variant="ghost" onClick=${onClose}>Cancel<//>
    </div>
  </td></tr>`;
}

function TaskRow({ task, n, numbers, group, snapshot, actions, drag, form, onForm }) {
  const state = taskRowState(task);
  const closed = state === 'done' || state === 'dropped';
  const over = drag?.over?.id !== task.id ? '' : drag.over.where === 'before' ? ' tb-drop-before' : ' tb-drop-after';
  const row = html`<tr class=${`tb-row tb-row-${state}${over}${drag?.dragging === task.id ? ' tb-dragging' : ''}`} data-task-id=${task.id}
      onDragOver=${closed ? undefined : (event) => drag?.overRow(event, task)} onDrop=${closed ? undefined : (event) => drag?.dropOnRow(event, task)}>
    <td class="tb-n">${!closed && actions && html`<span class="tb-handle" draggable="true" title="Drag to reorder (taskboard move)"
        aria-hidden="true" onDragStart=${(event) => drag.start(event, task)} onDragEnd=${drag.end}>⋮⋮</span>`}${n || ''}</td>
    <td class="tb-title-cell">
      <span class="tb-title">${task.title}</span>
      <span class="tb-id" title=${task.id}>${task.id}</span>
      <${DepChips} task=${task} numbers=${numbers} />
      ${task.status === 'blocked' && task.block_reason && html`<div class="tb-reason">blocked: “${task.block_reason}”</div>`}
    </td>
    <td><${RefChip} task=${task} /></td>
    <td><${Who} task=${task} group=${group} snapshot=${snapshot} /></td>
    <td><${StatePill} task=${task} numbers=${numbers} /></td>
    <td class="tb-actions">${actions && html`<${RowActions} task=${task} actions=${actions} onForm=${(mode) => onForm(task.id, mode)} />`}</td>
  </tr>`;
  if (!form || !actions) return row;
  return [row, html`<${RowForm} key=${`${task.id}-form`} task=${task} mode=${form} actions=${actions} onClose=${() => onForm(task.id, null)} />`];
}

// Both tables (the list and the closed tasks) share one column grid.
const COLUMNS = html`<colgroup><col class="tb-col-n" /><col /><col class="tb-col-ref" /><col class="tb-col-who" /><col class="tb-col-state" /><col class="tb-col-actions" /></colgroup>`;

const TASK_DRAG_TYPE = 'application/x-tclaude-task';

// useRowDrag reorders by dragging a row's handle onto another row (before
// or after it, by the pointer's half) or onto the Ideas band (demote to the
// top of the ideas). Its events stop here, so the group's own drags (agent
// rows, group reorder) never see them.
function useRowDrag(tasks, actions) {
  const dragged = useRef(null);
  const [over, setOver] = useState(null);
  const [dragging, setDragging] = useState('');
  const accept = (event) => {
    if (!dragged.current) return false;
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    return true;
  };
  const clear = () => { dragged.current = null; setOver(null); setDragging(''); };
  return {
    over, dragging,
    start(event, task) {
      event.stopPropagation();
      dragged.current = task;
      setDragging(task.id);
      event.dataTransfer?.setData(TASK_DRAG_TYPE, task.id);
      if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
      const row = event.currentTarget.closest?.('tr');
      if (row && event.dataTransfer?.setDragImage) event.dataTransfer.setDragImage(row, 12, 12);
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

// CaptureBox is the burst capture at the end of the list: Enter adds the
// line and keeps the focus for the next one; a pasted list adds one task per
// line; a line that is only an awb, Linear or GitHub id becomes a task with
// that ref, titled from the tracker. Everything lands at the end of the ideas
// (or the queue, with "capture as next"); drag to prioritise.
function CaptureBox({ group, actions, wizard }) {
  const [text, setText] = useState('');
  const [next, setNext] = useState(false);
  const [note, setNote] = useState('');
  const added = useRef(0);
  const sending = useRef(false);
  // putBack returns lines that did not land to the box, ahead of whatever
  // was typed meanwhile.
  const putBack = (lines) => setText((typed) => [lines.join('\n'), typed].filter(Boolean).join('\n'));
  // add takes the lines out of the box as it sends them, so the operator
  // keeps typing the next one; one capture is in flight at a time.
  const add = async (value, { fromBox = true } = {}) => {
    const lines = captureLines(value);
    if (!lines.length || sending.current) return;
    sending.current = true;
    if (fromBox) setText('');
    if (lines.length > 1 || isTrackerLine(lines[0])) setNote('looking up titles…');
    try {
      const out = await actions.run('capture', 'Capture', (api) => api.capture(group.name, lines.join('\n'), { next }));
      if (!out) {
        setNote('');
        putBack(lines);
        return;
      }
      added.current += out.created || 0;
      const where = next ? 'queued' : (wizard ? 'visions' : 'ideas');
      const more = importSummary(out);
      let failed = '';
      if (out.failed) {
        putBack(out.failed.remaining);
        failed = ` · stopped: ${out.failed.message}; ${out.failed.remaining.length} not sent, back in the box`;
      }
      setNote(`burst: ${added.current} ${where} added${more ? ` · ${more}` : ''}${failed}`);
    } finally {
      sending.current = false;
    }
  };
  const onKeyDown = (event) => {
    if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
      event.preventDefault();
      void add(event.currentTarget.value);
    } else if (event.key === 'Escape') {
      setText('');
      event.currentTarget.blur();
    }
  };
  const onPaste = (event) => {
    const pasted = event.clipboardData?.getData('text') || '';
    if (captureLines(pasted).length < 2) return;
    event.preventDefault();
    void add(pasted, { fromBox: false });
  };
  return html`<div class="tb-capture" data-taskboard-capture=${group.name}>
    <div class="tb-capture-head">
      <span class="ui-eyebrow">Capture · lands in ${next ? 'the queue' : 'ideas'}</span>
      <${Check} checked=${next} onChange=${(event) => setNext(event.currentTarget.checked)}>capture as next<//>
      ${note && html`<span class="tb-capture-note" role="status">${note}</span>`}
    </div>
    <${Textarea} size="sm" class="tb-capture-input" rows="2" aria-label=${wizard ? 'Capture a quest' : 'Capture a task'} value=${text}
      placeholder='next task… (a line that is only an awb, Linear or GitHub id becomes a ref: "tcl-fb0bfz", "LIN-231")'
      onInput=${(event) => setText(event.currentTarget.value)} onKeyDown=${onKeyDown} onPaste=${onPaste} />
    <div class="tb-capture-foot">
      <span class="ui-hint"><kbd>Enter</kbd> adds, stays here · paste a list → one task per line · <kbd>Shift</kbd>+<kbd>Enter</kbd> new line · <kbd>Esc</kbd> done</span>
      <${Button} size="sm" variant="primary" busy=${actions.busy === 'capture'} disabled=${!captureLines(text).length || !!actions.busy}
        onClick=${() => void add(text)}>Add<//>
    </div>
  </div>`;
}

// ImportPanel is "+ from awb/Linear…": paste tracker ids, one task per id,
// titled from the tracker (taskboard add --from-awb / --from-linear /
// --from-github).
function ImportPanel({ group, actions, onClose }) {
  const [kind, setKind] = useState('awb');
  const [ids, setIds] = useState('');
  const [next, setNext] = useState(false);
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
      <span class="tb-spacer"></span>
      <${Button} size="sm" variant="primary" busy=${actions.busy === 'import'} disabled=${!refs.length || !!actions.busy}
        onClick=${() => void submit()}>Import ${refs.length || ''}<//>
      <${Button} size="sm" variant="ghost" onClick=${onClose}>Close<//>
    </div>
  </div>`;
}

function HistoryLine({ entry, open, onToggle, wizard }) {
  const counts = entry.counts || {};
  const closed = entry.closed || [];
  const parts = [`${counts.done_this_week || 0} done this week`];
  if (counts.dropped) parts.push(`${counts.dropped} dropped`);
  return html`<div class="tb-history">
    ${parts.join(' · ')}
    ${closed.length > 0 && html` · <${Button} size="sm" variant="link" class="tb-history-toggle" aria-expanded=${open ? 'true' : 'false'}
      onClick=${onToggle}>${open ? '▾' : '▸'} ${wizard ? 'chronicle' : 'history'}<//>`}
    <span class="tb-history-note"> — ${wizard
      ? 'a quest leaves the board when it is fulfilled; it stays in the chronicle'
      : 'a task leaves the list when it is done or dropped; it stays in history'}</span>
  </div>`;
}

// TaskboardRows is the open section's list: the queue, the Ideas band, the
// capture box, the history line and, unfolded, the recently closed tasks.
// Without `actions` it is the read-only list.
export function TaskboardRows({ entry, group, snapshot, wizard = isWizardActive(), actions = null, importing = false, onImportClose = () => {} }) {
  const [history, setHistory] = useState(false);
  const [forms, setForms] = useState({});
  const tasks = entry.tasks || [];
  const drag = useRowDrag(tasks, actions);
  const queue = tasks.filter((task) => task.stage !== 'idea');
  const ideas = tasks.filter((task) => task.stage === 'idea');
  // Numbers run down the whole list, as the operator reads it.
  const numbers = new Map(tasks.map((task, i) => [task.id, i + 1]));
  const more = (entry.counts?.live || 0) - tasks.length;
  const name = group.name;
  const onForm = (id, mode) => setForms((current) => ({ ...current, [id]: mode }));
  const rowProps = { numbers, group, snapshot, actions, drag: actions ? drag : null, onForm };
  const ideasBand = (ideas.length > 0 || (actions && queue.length > 0)) && html`<tr class=${`tb-ideas-divider${drag.over?.id === IDEAS_BAND ? ' tb-drop-into' : ''}`}
      onDragOver=${actions ? drag.overIdeas : undefined} onDrop=${actions ? drag.dropOnIdeas : undefined}><td></td><td colspan="5">
    <span class="ui-eyebrow">${wizard ? 'Visions' : 'Ideas'} · ${ideas.length} · captured, not yet in the queue${actions ? ' — ✓ Next promotes; drag across this line to promote or demote' : ''}</span></td></tr>`;
  return html`<div class="tb-board" data-taskboard=${name}>
    ${importing && actions && html`<${ImportPanel} group=${group} actions=${actions} onClose=${onImportClose} />`}
    ${tasks.length ? html`<table class="ui-compact tb-table" onDragLeave=${actions ? drag.leaveTable : undefined}>${COLUMNS}
      <thead><tr><th class="tb-n">#</th><th>${wizard ? 'Quest · top is next' : 'Task · operator order, top is next'}</th>
        <th>Ref</th><th>Who</th><th>State</th><th></th></tr></thead>
      <tbody>
        ${queue.map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${numbers.get(task.id)} form=${forms[task.id]} ...${rowProps} />`)}
        ${ideasBand}
        ${ideas.map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${numbers.get(task.id)} form=${forms[task.id]} ...${rowProps} />`)}
      </tbody>
    </table>` : html`<div class="gf-empty ui-empty tb-empty">${wizard ? 'No quests on the board.' : 'No tasks on the board.'}${actions
      ? ' Capture one below.' : html` Add one with <code>tclaude agent taskboard add ${name} "…"</code>.`}</div>`}
    ${more > 0 && html`<div class="tb-more muted">+ ${more} more · <code>tclaude agent taskboard ls ${name}</code></div>`}
    ${actions && html`<${CaptureBox} group=${group} actions=${actions} wizard=${wizard} />`}
    <${HistoryLine} entry=${entry} open=${history} wizard=${wizard} onToggle=${() => setHistory(!history)} />
    ${history && (entry.closed || []).length > 0 && html`<table class="ui-compact tb-table tb-closed">${COLUMNS}<tbody>
      ${(entry.closed || []).map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${0} form=${null} ...${rowProps} />`)}
    </tbody></table>`}
  </div>`;
}

const VIEW_PREF = (group) => `tclaude.dash.groups.taskboard.view.${group}`;

// ViewSelector is the section's List | Lanes switch, remembered per group.
// Lanes arrive with a later slice; until then the switch shows the way.
function ViewSelector({ group }) {
  const view = dashPrefs.getItem(VIEW_PREF(group)) === 'lanes' ? 'lanes' : 'list';
  return html`<${Seg} class="tb-view" aria-label="Task board view">
    <${Button} size="sm" pressed=${view === 'list'}
      onClick=${() => dashPrefs.setItem(VIEW_PREF(group), 'list')}>☰ List<//>
    <${Button} size="sm" disabled title="Swim lanes come in a later slice">⇶ Lanes<//>
  <//>`;
}

// TaskboardSection is the group card's Task board: folded, one line of
// counts; it opens on its own when something needs the operator, until they
// fold or open it themselves (remembered per group). `api` is the writes'
// client; null draws the read-only list.
export function TaskboardSection({ group, snapshot, source: ownSource = taskboardContext, api: ownApi = taskboardApi, boards = nodeTaskboard }) {
  const { source, api } = boardOf(group, ownSource, ownApi, boards);
  const entry = useTaskboardEntry(group.name, source);
  const actions = useBoardActions(source, api);
  const [importing, setImporting] = useState(false);
  // Once the section has opened on its own (something needed the
  // operator), it stays open for this page, so clearing the last blocked
  // task never folds it under a form or the capture box in use. The
  // operator's own fold or open is remembered and wins as before.
  const autoOpened = useRef(false);
  if (!entry) return null;
  const wizard = isWizardActive();
  const counts = entry.counts || {};
  if ((counts.needs_you || 0) > 0) autoOpened.current = true;
  const head = api && html`<${ViewSelector} group=${group.name} />
    <${Button} size="sm" class="tb-import-open" pressed=${importing} title="taskboard add --from-awb / --from-linear / --from-github"
      onClick=${() => setImporting(!importing)}>+ from awb/Linear…<//>`;
  return html`<${FusedSection} group=${group.name} section="taskboard" label=${wizard ? 'Quest board' : 'Task board'}
      summary=${html`<${TaskboardCounts} counts=${counts} />`} auto=${autoOpened.current}
      actions=${head} force=${importing} attrs=${{ 'data-fused-taskboard': group.name }}>
    <${TaskboardRows} entry=${entry} group=${group} snapshot=${snapshot} wizard=${wizard} actions=${api ? actions : null}
      importing=${importing} onImportClose=${() => setImporting(false)} />
  <//>`;
}

function TaskboardCounts({ counts }) {
  const parts = taskboardCountParts(counts);
  return html`<span class="tb-counts" title=${taskboardCountsText(counts)}>
    <span class=${`gf-count${counts.live ? '' : ' zero'}`}>${counts.live || 0}</span>
    ${parts.map((part) => html`<span key=${part.key} class=${`tb-count tb-count-${part.key}`}>${` · ${part.text}`}</span>`)}
  </span>`;
}

// TaskboardSummaryChip carries the counts on the group's summary line, so a
// folded group still says what is on its board. Nothing when it is empty.
export function TaskboardSummaryChip({ group, source: ownSource = taskboardContext, boards = nodeTaskboard }) {
  const { source } = boardOf(group, ownSource, null, boards);
  const entry = useTaskboardEntry(group.name, source);
  if (!entry || !entry.counts?.live) return null;
  const parts = taskboardCountParts(entry.counts);
  const wizard = isWizardActive();
  return html`<span class="group-taskboard-chip" data-taskboard-chip=${group.name} title=${`${wizard ? 'Quest board' : 'Task board'}: ${taskboardCountsText(entry.counts)}`}>
    ${wizard ? 'quest board' : 'task board'}: ${parts.length
      ? parts.map((part, i) => html`<span key=${part.key} class=${`tb-count tb-count-${part.key}`}>${i ? ' · ' : ''}${part.text}</span>`)
      : `${entry.counts.live}`}
  </span>`;
}
