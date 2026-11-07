// groups-taskboard-card.js -- a task's title with its hover card, on a List
// row's Name cell and a Lanes card: "what is this task" at a glance. It has
// the PR card's look (.ci-panel, usePanelPlacement) and hover intent, but not
// its stickiness: a board has many tasks, and a card that lingers covers the
// next ones. So the card shows only while the pointer is on the TITLE (or
// keyboard focus is), hides HOVER_CLOSE_MS after the pointer leaves it, never
// takes the pointer itself (pointer-events: none, so the tasks under it stay
// usable), and closes at once when another hover popover opens
// (useExclusivePopover). Nothing in it is clickable.
//
// The card opens at once with what the board already has: the title, the id,
// where it is (and why it is blocked), the assignee, who made it and when,
// the last change, the ref, the dependencies, the files and the 💬 count.
// The brief and the latest comment are not in the board's context; they are
// read through the board's api (so a linked node's task reads from its node)
// once the pointer has rested on the title, or at once for keyboard focus.
//
// A click on the title (or Enter on it) opens the task's edit form, on a
// List row and a Lanes card alike, and never pins the card; the title is a
// button, so a press on it is never a drag (row-drag.js).

import { h } from 'preact';
import { useEffect, useId, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { usePanelPlacement } from './pr-checks-hover.js';
import { useExclusivePopover, useLeaveGrace } from './hover-popover.js';
import { relTime } from './helpers.js';
import { MarkdownDocument } from './markdown-document.js';
import { useThumb } from './groups-taskboard-files.js';

const html = htm.bind(h);

// TASK_CARD_HOVER_MS is the hover intent before the card reads the brief and
// the latest comment: the PR card's (pr-link-hover.js), so sweeping down a
// column of titles reads nothing.
export const TASK_CARD_HOVER_MS = 500;

// details caches what a card read, by task and its last change, so a second
// hover paints at once.
const details = new Map();
const detailsKey = (task) => `${task.id}|${task.updated_at || ''}|${task.comment_count || 0}`;

const clip = (text, max) => {
  const s = String(text ?? '').replace(/\s+/g, ' ').trim();
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
};

// latestComment is the newest comment in a stream that is not deleted.
export function latestComment(entries = []) {
  for (let i = entries.length - 1; i >= 0; i -= 1) {
    const c = entries[i]?.comment;
    if (c && !c.deleted_at && c.body) return c;
  }
  return null;
}

function useTaskDetails(api, task, active) {
  const key = detailsKey(task);
  // state is keyed, so a task that changed since the last read shows
  // "reading…" rather than the old brief.
  const [state, setState] = useState(() => ({ key, value: details.get(key) || null }));
  useEffect(() => {
    if (!active) return undefined;
    const known = details.get(key);
    if (known) { setState({ key, value: known }); return undefined; }
    let live = true;
    const reads = [
      typeof api?.task === 'function' ? api.task(task.id).then((full) => full?.body || '', () => null) : Promise.resolve(null),
      (task.comment_count || 0) > 0 && typeof api?.activity === 'function'
        ? api.activity(task.id).then((out) => latestComment(out?.entries), () => null) : Promise.resolve(null),
    ];
    Promise.all(reads).then(([body, latest]) => {
      const next = { body, latest };
      // Only a complete read is kept; a failed one is tried again next time.
      if (body !== null && (latest !== null || !(task.comment_count > 0))) details.set(key, next);
      if (live) setState({ key, value: next });
    });
    return () => { live = false; };
  }, [active, key]);
  return state.key === key ? state.value : (details.get(key) || null);
}

const STATUS_WORD = { open: 'open', blocked: 'blocked', done: 'done', dropped: 'dropped' };

function whereText(task) {
  const status = task.status || 'open';
  if (status !== 'open') return STATUS_WORD[status] || status;
  if (task.assignee?.agent_id) return 'in progress';
  return task.stage === 'idea' ? 'idea' : 'next';
}

function Thumb({ api, task, attachment }) {
  const url = useThumb(api, task.id, attachment);
  return url ? html`<img class="tb-card-thumb" src=${url} alt=${attachment.name} title=${attachment.name} />` : null;
}

// TaskCardPanel is the card itself.
function TaskCardPanel({ task, api, read, nameOf, panelID, headingID, panelRef, placement, chips }) {
  const style = placement && !placement.hidden
    ? `top:${placement.top}px;left:${placement.left}px;max-height:${placement.maxHeight}px;`
      + (placement.placement === 'above' ? 'transform:translateY(-100%)' : '')
    : 'visibility:hidden';
  const files = task.attachments || [];
  const images = files.filter((f) => f.image).slice(0, 4);
  const deps = task.depends_on || [];
  const n = task.comment_count || 0;
  const who = (actor) => (nameOf ? nameOf(actor) : actor);
  const assignee = task.assignee?.agent_id ? (task.assignee.name || who(`agent:${task.assignee.agent_id}`)) : '';
  const edited = task.last_edited_at || task.updated_at;
  const editedBy = task.last_edited_by_name || (task.last_edited_by ? who(task.last_edited_by) : '');
  return html`<div ref=${panelRef} class="ci-panel prd-panel ui-popover tb-task-panel" style=${style} id=${panelID} role="tooltip"
      data-task-card=${task.id}>
    <div class="prd-title tb-task-panel-title" id=${headingID}>${task.title}</div>
    <div class="tb-task-panel-id"><code>${task.id}</code></div>
    <div class="prd-meta">
      <span class=${`tb-task-where tb-task-where-${whereText(task).replace(/\s+/g, '-')}`}>${whereText(task)}</span>
      ${assignee && html`<span>with ${assignee}</span>`}
    </div>
    ${task.status === 'blocked' && task.block_reason && html`<div class="tb-reason">blocked: “${task.block_reason}”</div>`}
    <div class="tb-task-panel-facts">
      <span>created ${task.created_by ? `by ${who(task.created_by)} ` : ''}${relTime(task.created_at)}</span>
      ${edited && html`<span>changed ${editedBy ? `by ${editedBy} ` : ''}${relTime(edited)}</span>`}
    </div>
    ${(task.ref || deps.length > 0) && html`<div class="tb-task-panel-links">${chips?.ref}${deps.length > 0 && chips?.deps}</div>`}
    <div class="tb-task-panel-brief">
      ${read === null ? html`<span class="ui-hint">reading…</span>`
        : read.body === null ? html`<span class="ui-hint">the brief could not be read</span>`
          : read.body ? html`<div class="tb-task-brief-clamp"><${MarkdownDocument} source=${read.body} className="markdown-document tb-comment-md" /></div>`
            : html`<span class="ui-hint">no brief</span>`}
    </div>
    ${(files.length > 0 || n > 0) && html`<div class="tb-task-panel-extra">
      ${files.length > 0 && html`<div class="tb-task-panel-files">
        <span>📎 ${files.length} file${files.length === 1 ? '' : 's'}</span>
        ${images.map((attachment) => html`<${Thumb} key=${attachment.id} api=${api} task=${task} attachment=${attachment} />`)}
      </div>`}
      ${n > 0 && html`<div class="tb-task-panel-comment">
        <span>💬 ${n}</span>
        ${read?.latest && html`<span class="tb-task-panel-latest"><b>${read.latest.author_name || who(read.latest.author)}:</b> ${clip(read.latest.body, 140)}</span>`}
      </div>`}
    </div>`}
  </div>`;
}

// keyboardFocus reports whether focus came from the keyboard: a click
// focuses the title too, and that must not hold the card open
// (pr-checks-hover.js has the same rule).
function keyboardFocus(target) {
  try {
    return target?.matches?.(':focus-visible') !== false;
  } catch {
    return true;
  }
}

// useTitleHover is the card's open state: the pointer on the title, with
// HOVER_CLOSE_MS before it hides, or keyboard focus on it.
function useTitleHover(rootRef) {
  const [hovered, setHovered] = useState(false);
  const [lingering, setLingering] = useState(false);
  const [focused, setFocused] = useState(false);
  const open = hovered || lingering || focused;
  const leave = useLeaveGrace(() => setLingering(false));
  // over is the pointer on the title: a leave after close() (a click that
  // opened the edit form) must not bring the card back for its grace.
  const over = useRef(false);
  // returning is set when the title opens the edit form: the dialog hands
  // focus back to the title as it closes, and that focus (keyboard-visible
  // after an Escape) must not open a card no pointer is on.
  const returning = useRef(false);
  const close = () => { over.current = false; leave.cancel(); setHovered(false); setLingering(false); setFocused(false); };
  useExclusivePopover(rootRef, open, close);
  useEffect(() => {
    if (!open) return undefined;
    const onVisibility = () => { if (document.visibilityState === 'hidden') close(); };
    window.addEventListener('blur', close);
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      window.removeEventListener('blur', close);
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [open]);
  const titleProps = {
    onMouseEnter: () => { over.current = true; leave.cancel(); setLingering(false); setHovered(true); },
    onMouseLeave: () => {
      setHovered(false);
      if (!over.current) return;
      over.current = false;
      setLingering(true);
      leave.schedule();
    },
    onFocus: (event) => {
      if (returning.current) { returning.current = false; return; }
      if (keyboardFocus(event.currentTarget)) setFocused(true);
    },
    onBlur: () => setFocused(false),
  };
  const handOff = () => { returning.current = true; close(); };
  return { open, hovered, focused, close, handOff, titleProps };
}

// TaskTitle is a task's title with its hover card; onOpen opens the edit
// form (null where the viewer may not edit: the title is plain then).
export function TaskTitle({ task, api, nameOf = null, onOpen = null, chips = null, class: extra = '' }) {
  const rootRef = useRef(null);
  const panelRef = useRef(null);
  const instanceID = useId();
  const { open, hovered, focused, close, handOff, titleProps } = useTitleHover(rootRef);
  const [dwelled, setDwelled] = useState(false);
  useEffect(() => {
    if (!hovered) { setDwelled(false); return undefined; }
    const timer = globalThis.setTimeout(() => setDwelled(true), TASK_CARD_HOVER_MS);
    return () => globalThis.clearTimeout(timer);
  }, [hovered]);
  const read = useTaskDetails(api, task, dwelled || focused);
  const { placement } = usePanelPlacement(rootRef, panelRef, open);
  const panelID = `tb-task-panel-${instanceID.replace(/[^a-zA-Z0-9_-]+/g, '-')}`;
  const headingID = `${panelID}-heading`;
  const openForm = (event) => {
    event.stopPropagation();
    handOff();
    onOpen();
  };
  const key = (event) => {
    if (event.key === 'Escape' && open) { event.preventDefault(); event.stopPropagation(); close(); return; }
    if (!onOpen || (event.key !== 'Enter' && event.key !== ' ')) return;
    event.preventDefault();
    openForm(event);
  };
  return html`<span ref=${rootRef} class=${`ci-hover tb-task-hover${open ? ' is-open' : ''}`}>
    <span role=${onOpen ? 'button' : undefined} tabindex="0" class=${`tb-title${onOpen ? ' tb-title-open' : ''}${extra ? ` ${extra}` : ''}`}
      aria-describedby=${open ? panelID : undefined}
      aria-label=${onOpen ? `${task.title}: opens the task` : undefined}
      onClick=${onOpen ? openForm : undefined} onKeyDown=${key} ...${titleProps}>${task.title}</span>
    ${open && html`<${TaskCardPanel} task=${task} api=${api} read=${read} nameOf=${nameOf}
        panelID=${panelID} headingID=${headingID} panelRef=${panelRef} placement=${placement} chips=${chips} />`}
  </span>`;
}
