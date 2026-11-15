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
import { ButtonRow, Card, ChipRow, Section } from './phone-ui.js';
import { usePhoneBar } from './phone-page.js';
import { openPhoneSheet } from './phone-sheet.js';
import { createTaskboardApi, localLimitHooks } from './taskboard-actions.js';
import { newCommentKey } from './groups-taskboard-comments.js';
import { Button } from './ui-components.js';
import {
  DONE, boardColumns, cardView, columnMoves, columnSections, defaultColumn, positionMoves, sectionPeers,
} from './phone-board-model.js';
import { TaskPage, boardWrite } from './phone-task-page.js';

const html = htm.bind(h);

// boardTask is the task whose page is open over the board, { id, group }, or
// null. Search opens a task by setting it (openBoardTask) with the Board up.
export const boardTask = signal(null);

export function openBoardTask(id, group) {
  boardTask.value = id && group ? { id, group } : null;
}

// boardPick is the group and column on screen, kept while the shell shows
// another view so coming back finds the board as it was left.
const boardPick = signal({ group: '', column: '' });

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
    <p class="phone-board-sheet-title">${task.title}</p>
    <h2 class="ui-eyebrow phone-menu-head">Move to</h2>
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
    <h2 class="ui-eyebrow phone-menu-head">Order</h2>
    <${ButtonRow} buttons=${[
      { key: 'top', label: '⤒ Top', disabled: !moves.top, onClick: go(() => api.move(task.id, moves.top), 'Could not move it') },
      { key: 'up', label: '↑ Up', disabled: !moves.up, onClick: go(() => api.move(task.id, moves.up), 'Could not move it') },
      { key: 'down', label: '↓ Down', disabled: !moves.down, onClick: go(() => api.move(task.id, moves.down), 'Could not move it') },
    ]} />
  </div>`;
}

// TextSheet is a sheet with one text box and a send button: a comment, an
// answer in words, a new task's title.
export function TextSheet({ title, label, placeholder = '', send = 'Send', multiline = true, onSubmit, close }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const field = useRef(null);
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
    <p class="phone-board-sheet-title">${title}</p>
    ${multiline
      ? html`<textarea ref=${field} class="ui-input phone-board-field" rows="4" aria-label=${label} placeholder=${placeholder}
          value=${text} onInput=${(event) => setText(event.currentTarget.value)}></textarea>`
      : html`<input ref=${field} class="ui-input phone-board-field" aria-label=${label} placeholder=${placeholder}
          value=${text} onInput=${(event) => setText(event.currentTarget.value)} />`}
    <div class="phone-btns">
      <${Button} class="phone-btn phone-btn-grow" onClick=${close}>Cancel<//>
      <${Button} type="submit" class="phone-btn phone-btn-grow phone-btn-pri" disabled=${busy || !text.trim()}>${busy ? 'Sending…' : send}<//>
    </div>
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

// TaskCard is one task on the board.
export function TaskCard({ task, view, onOpen, onMove, onComment, onAnswer }) {
  const q = view.question;
  return html`<${Card} accent=${view.accent} class="phone-board-card" data-task-id=${task.id}>
    <button data-ui-exempt type="button" class="phone-board-open" onClick=${onOpen}>
      <span class="phone-board-title">${view.title}</span>
      ${q && html`<span class="phone-board-asked">asked by ${q.asker || 'an agent'}${q.options.length ? ` · ${q.options.length} options` : ''}</span>`}
      ${view.note && html`<span class="phone-board-note">${view.note}</span>`}
    </button>
    ${view.tags.length > 0 && html`<div class="phone-board-tags">
      ${view.tags.map((tag) => html`<span key=${tag.key} class=${`phone-board-tag${tag.tone ? ` phone-board-tag-${tag.tone}` : ''}`}>${tag.text}</span>`)}
    </div>`}
    ${q?.inline
      ? html`<div class="phone-btns phone-board-options" role="group" aria-label=${`Answer: ${q.title}`}>
          ${q.options.map((option) => html`<${Button} key=${option.key} class="phone-btn" title=${option.recommended ? 'Recommended' : undefined}
            onClick=${() => onAnswer(q, option)}>${option.recommended && html`<span class="phone-board-star" aria-label="recommended">★ </span>`}${option.label}<//>`)}
        </div>`
      : !view.done && html`<div class="phone-btns">
          ${q && html`<${Button} class="phone-btn phone-btn-pri" onClick=${onOpen}>Answer…<//>`}
          <${Button} class="phone-btn" aria-haspopup="dialog" onClick=${onMove}>Move ▸<//>
          <${Button} class="phone-btn" aria-haspopup="dialog" onClick=${onComment}>Comment<//>
        </div>`}
  <//>`;
}

export function BoardView({ source, state, actions, fetchImpl = globalThis.fetch, api: given = null }) {
  const api = useMemo(() => given || createTaskboardApi({ fetchImpl: (...args) => fetchImpl(...args), limits: localLimitHooks() }), [given, fetchImpl]);
  useEffect(() => source.attach(), [source]);
  // The task page goes with the board: coming back to Board shows the board.
  useEffect(() => () => { boardTask.value = null; }, []);
  const status = source.status.value;
  const context = source.context.value;
  const groups = boardGroups(context);
  const pick = boardPick.value;
  const group = groups.find((g) => g.group === pick.group)?.group || groups[0]?.group || '';
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
    return html`<${TaskPage} key=${open.id} id=${open.id} group=${open.group} source=${source} state=${state} api=${api}
      onClose=${() => { boardTask.value = null; }} onMove=${(task, entry_) => openSheet('Move', MoveSheet, { task, entry: entry_, peers: sectionPeers(entry_, task) })} />`;
  }

  if (status === 'unavailable') return html`<p class="ui-empty phone-empty">Task boards are turned off.</p>`;
  if (!context) return html`<p class="ui-empty phone-empty">${status === 'error' ? `Could not read the boards: ${source.error.value}` : 'Loading…'}</p>`;
  if (!entry) return html`<p class="ui-empty phone-empty">No open tasks on any board.</p>`;

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
  return html`<div class="phone-board">
    <${ChipRow} label="Column" value=${column} onSelect=${(key) => setPick({ column: key })}
      items=${columns.map((c) => ({ key: c.key, label: c.label, count: c.count }))} />
    ${sections.map((section) => html`<${Section} key=${section.id} title=${section.label} count=${section.tasks.length}>
      ${section.tasks.length === 0
        ? null
        : section.tasks.map((task) => html`<${TaskCard} key=${task.id} task=${task} view=${cardView(task, names)}
            onOpen=${() => openBoardTask(task.id, group)}
            onAnswer=${answer(task)}
            onMove=${() => openSheet('Move', MoveSheet, { task, entry, peers: section.tasks })}
            onComment=${() => openSheet('Comment', TextSheet, {
              title: task.title, label: 'Comment', placeholder: 'Write a comment', send: 'Comment',
              onSubmit: (body) => boardWrite(source, () => api.comment(task.id, body, newCommentKey()), 'Could not comment'),
            })} />`)}
    <//>`)}
    ${column === DONE && sections[0].tasks.length === 0 && html`<p class="ui-empty phone-empty">Nothing done yet.</p>`}
  </div>`;
}
