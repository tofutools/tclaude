// phone-board.js -- the phone shell's Board view: one group's task board, one
// column at a time (phone-board-model.js), as the approved mock draws it.
//
//   app bar   "Board webshop ▾" picks the group; ＋ adds a task to the column
//             on screen (Idea or Next; elsewhere to Next)
//   chips     Idea · Next · Started · the board's custom columns · Done
//   sections  Ready, Waiting on you, Queued, Blocked within the column
//   cards     title, tags (@agent, PR, refs, kind), Move ▸ and Comment; a
//             question to the operator answered in place with its options
//
// Move ▸ is a sheet (another column, up, down, to the top), never a drag.
// Tapping a card opens its phone page (phone-task-page.js) over the board, a
// history step of its own: Back and ‹ come back here, never to the
// desktop's task form, which a phone never opens.

import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { boardGroups } from './phone-shell-model.js';
import { ButtonRow, Card, ChipRow, Empty, ErrorNote, Section, SheetHead, SheetTitle, errorText } from './phone-ui.js';
import { usePhoneBar } from './phone-page.js';
import { openPhoneSheet, requestClosePhoneSheet } from './phone-sheet.js';
import { DiscardAsk, useDraftGuard } from './phone-draft-guard.js';
import { createTaskboardApi, localLimitHooks } from './taskboard-actions.js';
import { newCommentKey } from './groups-taskboard-comments.js';
import { Button } from './ui-components.js';
import {
  DONE, boardColumns, cardView, columnMoves, columnSections, defaultColumn, positionMoves, sectionPeers,
} from './phone-board-model.js';
import { TaskPage, boardWrite } from './phone-task-page.js';
import { openPhoneRun } from './phone-task-links.js';
import { processContext } from './process-context.js';
import { TaskRunLine, WillFollowLine, taskRunOf } from './process-task-run.js';

const html = htm.bind(h);

// boardTask is the task whose page is open over the board, { id, group }, or
// null. Search opens a task by setting it (openBoardTask) with the Board up.
export const boardTask = signal(null);

// openFrom opens task id from the page of `open` (its epic, a dependency):
// Back returns to `open`. closeTask is Back: the task it was opened from, or
// the board.
export function openFrom(open, id) {
  return { id, group: open.group, back: [...(open.back || []), open.id] };
}
export function closeTask(open) {
  const back = open?.back || [];
  return back.length ? { id: back[back.length - 1], group: open.group, back: back.slice(0, -1) } : null;
}

export function openBoardTask(id, group) {
  boardTask.value = id && group ? { id, group } : null;
}

// boardPick is the group and column on screen, kept while the shell shows
// another view so coming back finds the board as it was left.
const boardPick = signal({ group: '', column: '' });

// boardRequest counts asks to show the Board from a view outside the shell
// (Agents' "Board ›"); the shell (phone-shell-island.js) shows it on each.
export const boardRequest = signal(0);

// openBoardGroup shows the Board on one group's board.
export function openBoardGroup(group) {
  boardPick.value = { group: group || '', column: '' };
  boardTask.value = null;
  boardRequest.value += 1;
}

// moveTo is the write that puts task in column (phone-board-model.js
// columnMoves), as the desktop Lanes' drop does (groups-taskboard-lanes.js
// baseLaneDrop): a stage or custom column through the stage route, an
// agent's task kept held by it; Started starts its agent on it; Done closes
// it. Out of a custom column a task goes back to Next first, then is held or
// started so it lands where it was sent.
export async function moveTo(api, task, column) {
  if (column === DONE) return api.status(task.id, 'done');
  const agent = !!task.assignee?.agent_id && !task.assignee?.operator;
  const started = agent && !task.held;
  if (column.startsWith('col:')) return api.stage(task.id, column.slice(4));
  if (task.column) {
    if (column === 'idea') return agent ? api.hold(task.id, 'idea') : api.stage(task.id, 'idea');
    await api.stage(task.id, 'next');
    if (column === 'next' && started) return api.hold(task.id, 'next');
    if (column === 'started' && task.held) return api.start(task.id);
    return null;
  }
  if (column === 'started') return api.start(task.id);
  if (started) return api.hold(task.id, column);
  return api.stage(task.id, column);
}

// MoveSheet is Move ▸: the task's other columns, then its place among the
// cards of its section.
export function MoveSheet({ task, entry, peers, api, source, close }) {
  const columns = columnMoves(task, entry);
  const moves = positionMoves(peers, task);
  const go = (run, failText) => async () => {
    close();
    await boardWrite(source, run, failText);
  };
  return html`<div class="phone-board-move">
    <${SheetTitle}>${task.title}<//>
    <${SheetHead}>Move to<//>
    <ul class="phone-menu">
      ${columns.map((column) => html`<li key=${column.key}>
        <button data-ui-exempt type="button" class="phone-menu-item" disabled=${column.current || column.disabled}
          aria-current=${column.current ? 'true' : undefined}
          onClick=${go(() => moveTo(api, task, column.key), `Could not move it to ${column.label}`)}>
          <span class="phone-board-move-label">${column.label}</span>
          ${column.current && html`<small class="phone-board-move-why">here now</small>`}
          ${!column.current && column.why && html`<small class="phone-board-move-why">${column.why}</small>`}
        </button>
      </li>`)}
    </ul>
    <${SheetHead}>Order<//>
    <${ButtonRow} buttons=${[
      { key: 'top', label: '⤒ Top', disabled: !moves.top, onClick: go(() => api.move(task.id, moves.top), 'Could not move it') },
      { key: 'up', label: '↑ Up', disabled: !moves.up, onClick: go(() => api.move(task.id, moves.up), 'Could not move it') },
      { key: 'down', label: '↓ Down', disabled: !moves.down, onClick: go(() => api.move(task.id, moves.down), 'Could not move it') },
    ]} />
  </div>`;
}

// TextSheet is a sheet with one text box and a send button: a comment, an
// answer in words, a new task's title. A multi-line one asks before Back or
// Cancel drops what was written (phone-draft-guard.js); a one-line title is
// quick to type again.
export function TextSheet({ title, label, placeholder = '', send = 'Send', multiline = true, what = 'this', onSubmit, close }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const field = useRef(null);
  const guard = useDraftGuard(multiline && !!text.trim(), busy, () => field.current);
  // The operator asked to write: the field takes the focus (the sheet's own
  // focus goes to its first button, so after it).
  useEffect(() => {
    const timer = setTimeout(() => field.current?.focus(), 0);
    return () => clearTimeout(timer);
  }, []);
  const submit = async (event) => {
    event?.preventDefault?.();
    if (!text.trim() || busy) return;
    setBusy(true);
    const done = await onSubmit(text.trim());
    setBusy(false);
    if (done) close();
  };
  return html`<form class="phone-board-text" onSubmit=${submit}>
    <${SheetTitle}>${title}<//>
    ${multiline
      ? html`<textarea name="ui-input" ref=${field} class="ui-input phone-board-field" rows="4" aria-label=${label} placeholder=${placeholder}
          value=${text} onInput=${(event) => setText(event.currentTarget.value)}></textarea>`
      : html`<input name="ui-input" ref=${field} class="ui-input phone-board-field" aria-label=${label} placeholder=${placeholder}
          value=${text} onInput=${(event) => setText(event.currentTarget.value)} />`}
    ${guard.asking
      ? html`<${DiscardAsk} what=${what} guard=${guard} close=${close} />`
      : html`<${ButtonRow} buttons=${[
          { key: 'cancel', label: 'Cancel', onClick: requestClosePhoneSheet },
          { key: 'send', label: busy ? 'Sending…' : send, tone: 'pri', type: 'submit', disabled: busy || !text.trim() },
        ]} />`}
  </form>`;
}

// GroupSheet picks the board's group.
function GroupSheet({ groups, value, onPick, close }) {
  return html`<ul class="phone-menu">
    ${groups.map((g) => html`<li key=${g.group}>
      <button data-ui-exempt type="button" class="phone-menu-item" aria-current=${g.group === value ? 'true' : undefined}
        onClick=${() => { close(); onPick(g.group); }}>
        <span class="phone-board-move-label">${g.group}</span>
        <small class="phone-board-move-why">${g.live} open${g.waiting ? ` · ${g.waiting} waiting on you` : ''}</small>
      </button>
    </li>`)}
  </ul>`;
}

// TaskCard is one task on the board. `run` is the task's process run
// (tk_8aa88514), `follows` the board's default way of working.
export function TaskCard({ task, view, onOpen, onMove, onComment, onAnswer, run = null, follows = '', nameOf = () => '' }) {
  const q = view.question;
  return html`<${Card} accent=${view.accent} class="phone-board-card" data-task-id=${task.id}>
    <button data-ui-exempt type="button" class="phone-board-open" onClick=${onOpen}>
      <span class="phone-board-title">${view.title}</span>
      ${q && html`<span class="phone-board-asked">asked by ${q.asker || 'an agent'}${q.options.length ? ` · ${q.options.length} options` : ''}</span>`}
      ${view.note && html`<span class="phone-board-note">${view.note}</span>`}
    </button>
    ${run ? html`<${TaskRunLine} run=${run} compact=${true} nameOf=${nameOf} onOpen=${(picked) => { void openPhoneRun(picked); }} />`
    : !view.done && !task.started_at && task.status !== 'blocked' && task.kind !== 'epic' && html`<${WillFollowLine} task=${task} boardDefault=${follows} />`}
    ${view.tags.length > 0 && html`<div class="phone-board-tags">
      ${view.tags.map((tag) => html`<span key=${tag.key} class=${`phone-board-tag${tag.tone ? ` phone-board-tag-${tag.tone}` : ''}`}>${tag.text}</span>`)}
    </div>`}
    ${q?.inline
      ? html`<${ButtonRow} class="phone-board-options" label=${`Answer: ${q.title}`} buttons=${q.options.map((option) => ({
          key: option.key, grow: false, title: option.recommended ? 'Recommended' : undefined, onClick: () => onAnswer(q, option),
          label: html`${option.recommended && html`<span class="phone-board-star" aria-label="recommended">★ </span>`}${option.label}`,
        }))} />`
      : !view.done && html`<${ButtonRow} buttons=${[
          ...(q ? [{ key: 'answer', label: 'Answer…', tone: 'pri', grow: false, onClick: onOpen }] : []),
          { key: 'move', label: 'Move ▸', grow: false, attrs: { 'aria-haspopup': 'dialog' }, onClick: onMove },
          { key: 'comment', label: 'Comment', grow: false, attrs: { 'aria-haspopup': 'dialog' }, onClick: onComment },
        ]} />`}
  <//>`;
}

export function BoardView({ source, state, actions, fetchImpl = globalThis.fetch, api: given = null }) {
  const api = useMemo(() => given || createTaskboardApi({ fetchImpl: (...args) => fetchImpl(...args), limits: localLimitHooks() }), [given, fetchImpl]);
  useEffect(() => source.attach(), [source]);
  // The process context, for a task's run on its card: read with the board.
  useEffect(() => {
    void processContext.refresh();
    const poll = () => void processContext.refresh();
    document.addEventListener('tclaude:snapshot', poll);
    return () => document.removeEventListener('tclaude:snapshot', poll);
  }, []);
  // The task page goes with the board: coming back to Board shows the board.
  useEffect(() => () => { boardTask.value = null; }, []);
  const status = source.status.value;
  const context = source.context.value;
  const groups = boardGroups(context);
  const pick = boardPick.value;
  // A group the board lists, or one asked for by name (Agents' "Board ›")
  // whose board has nothing live yet; else the first.
  const asked = (context?.groups || []).some((g) => g.group === pick.group) ? pick.group : '';
  const group = groups.find((g) => g.group === pick.group)?.group || asked || groups[0]?.group || '';
  const entry = (context?.groups || []).find((g) => g.group === group) || null;
  const columns = entry ? boardColumns(entry) : [];
  const column = columns.find((c) => c.key === pick.column && pick.group === group)?.key || (entry ? defaultColumn(entry) : 'next');
  const setPick = (next) => { boardPick.value = { group, column, ...next }; };
  const open = boardTask.value;
  const answering = useRef(false);

  const openSheet = (label, Body, props) => openPhoneSheet({ label, Body, props: { api, source, ...props } });
  usePhoneBar({
    tab: 'board',
    menu: group ? {
      sub: group, label: 'pick a group',
      onClick: () => openSheet('Pick a group', GroupSheet, { groups, value: group, onPick: (g) => setPick({ group: g, column: '' }) }),
    } : null,
    actions: entry ? [{
      key: 'add', icon: '＋', label: 'Add a task',
      onClick: () => openSheet('Add a task', TextSheet, {
        title: `New task on ${group}`, label: 'Title', placeholder: 'What needs doing', send: 'Add', multiline: false,
        onSubmit: (title) => boardWrite(source, () => api.create(group, { title, stage: column === 'idea' ? 'idea' : 'next' }), 'Could not add the task'),
      }),
    }] : [],
  }, !open);

  if (open) {
    return html`<${TaskPage} key=${open.id} id=${open.id} group=${open.group} source=${source} state=${state} api=${api} actions=${actions}
      onClose=${() => { boardTask.value = closeTask(open); }} onOpenTask=${(id) => { boardTask.value = openFrom(open, id); }} onMove=${(task, entry_) => openSheet('Move', MoveSheet, { task, entry: entry_, peers: sectionPeers(entry_, task) })} />`;
  }

  if (status === 'unavailable') return html`<${Empty}>Task boards are turned off.<//>`;
  if (!context && status === 'error') return html`<${ErrorNote} onRetry=${() => void source.refresh()}>${errorText('the boards', source.error.value)}<//>`;
  if (!context) return html`<${Empty}>Loading…<//>`;
  if (!entry) return html`<${Empty}>No open tasks on any board.<//>`;

  const names = companionAgents(state.snapshot.value);
  // One answer at a time: a double tap must not send a second one.
  const answer = (task) => async (q, option) => {
    if (answering.current) return false;
    answering.current = true;
    try {
      return await boardWrite(source, () => api.answer(task.id, [option.key], '', q.n), 'Could not answer');
    } finally {
      answering.current = false;
    }
  };
  const sections = columnSections(entry, column);
  // The tasks' process runs (tk_8aa88514): none where processes are off.
  const runs = processContext.context.value;
  return html`<div class="phone-board">
    <${ChipRow} label="Column" value=${column} onSelect=${(key) => setPick({ column: key })}
      items=${columns.map((c) => ({ key: c.key, label: c.label, count: c.count }))} />
    ${sections.map((section) => html`<${Section} key=${section.id} title=${section.label} count=${section.tasks.length}>
      ${section.tasks.length === 0
        ? null
        : section.tasks.map((task) => html`<${TaskCard} key=${task.id} task=${task} view=${cardView(task, names)}
            run=${taskRunOf(runs, task.id)} follows=${entry?.process || ''} nameOf=${(id) => names.get?.(id)?.name || ''}
            onOpen=${() => openBoardTask(task.id, group)}
            onAnswer=${answer(task)}
            onMove=${() => openSheet('Move', MoveSheet, { task, entry, peers: section.tasks })}
            onComment=${() => openSheet('Comment', TextSheet, {
              title: task.title, label: 'Comment', placeholder: 'Write a comment', send: 'Comment', what: 'this comment',
              onSubmit: (body) => boardWrite(source, () => api.comment(task.id, body, newCommentKey()), 'Could not comment'),
            })} />`)}
    <//>`)}
    ${column === DONE && sections[0].tasks.length === 0 && html`<${Empty}>Nothing done yet.<//>`}
  </div>`;
}
