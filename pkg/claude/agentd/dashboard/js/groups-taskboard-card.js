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
import { useContext, useEffect, useId, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { usePanelPlacement } from './pr-checks-hover.js';
import { useExclusivePopover, useLeaveGrace } from './hover-popover.js';
import { relTime } from './helpers.js';
import { MarkdownDocument } from './markdown-document.js';
import { useThumb, useMarkdownFiles } from './groups-taskboard-files.js';
import { TaskLink, taskPath } from './task-route.js';
import { PRChain } from './pr-chain.js';

const html = htm.bind(h);

// refsOf is every ref of a task, the first one first. A node from before
// tasks had several refs sends only `ref`.
export function refsOf(task) {
  if (Array.isArray(task?.refs)) return task.refs;
  return task?.ref ? [task.ref] : [];
}

// refKey is what tells two refs apart: the link, or the id with none.
export const refKey = (ref) => ref?.url || ref?.label || ref?.id || '';

// trackerStatusClass is an issue status's ui-tag variant, by the words its
// tracker uses: in progress needs attention (warn), in review is accent,
// anything else (done, backlog, …) is the plain tag.
export function trackerStatusClass(status) {
  const s = String(status || '').toLowerCase();
  if (/review/.test(s)) return 'ui-tag-accent';
  if (/progress|started|doing|active/.test(s)) return 'ui-tag-warn';
  return '';
}

// prOfRef is a PR ref in PRChain's shape, or null for any other ref.
export function prOfRef(ref) {
  if (!ref?.pr || !ref.url) return null;
  const { number, state, title, checks, reviews } = ref.pr;
  return { url: ref.url, number, state, title, checks, reviews };
}

// OneRef is one ref, shown by its kind with the richest display tclaude has:
// a GitHub/GitLab PR is the agent rows' PRChain (#N, CI, review, with their
// hover cards); an awb, Linear or GitHub issue is its id, with its status
// and title from its last lookup; anything else is a link.
export function OneRef({ of: ref }) {
  const pr = prOfRef(ref);
  if (pr) return html`<${PRChain} pr=${pr} className="tb-ref-pr" />`;
  const label = ref.label || ref.url || ref.kind;
  const tracker = ref.tracker;
  const title = [tracker?.title, `${ref.kind}${ref.url ? ` · ${ref.url}` : ''}`].filter(Boolean).join(' — ');
  // The store only keeps http(s) URLs; check again before making a link.
  const linked = ref.url && /^https?:\/\//i.test(ref.url);
  if (tracker || ref.kind === 'awb' || ref.kind === 'linear') {
    const id = linked
      ? html`<a class="tb-ref" href=${ref.url} target="_blank" rel="noopener noreferrer" title=${title}>${label}</a>`
      : html`<span class="tb-ref" title=${`${title} · not linked`}>${label}</span>`;
    return html`<span class="tb-ref-issue">${ref.kind !== 'url' && html`<span class="tb-ref-kind">${ref.kind}</span>`}${id}${tracker?.status
      && html`<span class=${`ui-tag tb-ref-status ${trackerStatusClass(tracker.status)}`.trim()}>${tracker.status}</span>`}</span>`;
  }
  if (linked) {
    return html`<a class="ui-tag ui-tag-accent tb-ref" href=${ref.url} target="_blank" rel="noopener noreferrer" title=${title}>🔗 ${label}</a>`;
  }
  // A bare tracker id with no link yet (no awb server configured, or a
  // Linear id not looked up): `edit --resolve-ref` links it.
  return html`<span class="ui-tag ui-tag-accent tb-ref" title=${`${title} · not linked`}>🔗 ${label}</span>`;
}

// RefChip is a task's refs, each by its kind (OneRef): every one on a Lanes
// card, the hover card and the edit form; the first and "+n" where `max` is
// 1 (the List's Ref column).
export function RefChip({ task, max = 0 }) {
  const refs = refsOf(task);
  if (!refs.length) return html`<span class="tb-ref-none">—</span>`;
  const shown = max > 0 ? refs.slice(0, max) : refs;
  const more = refs.length - shown.length;
  return html`<span class="tb-refs">${shown.map((ref) => html`<${OneRef} key=${refKey(ref)} of=${ref} />`)}${more > 0
    && html`<span class="tb-refs-more" title=${refs.slice(shown.length).map((r) => r.label || r.url).join(', ')}>+${more}</span>`}</span>`;
}

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
  if (task.assignee?.agent_id && !task.held) return 'in progress';
  return task.stage === 'idea' ? 'idea' : 'next';
}

function Thumb({ api, task, attachment }) {
  const url = useThumb(api, task.id, attachment);
  return url ? html`<img class="tb-card-thumb" src=${url} alt=${attachment.name} title=${attachment.name} />` : null;
}

// TaskCardPanel is the card itself.
function TaskCardPanel({ task, api, read, nameOf, panelID, headingID, panelRef, placement, chips, linked = false }) {
  const style = placement && !placement.hidden
    ? `top:${placement.top}px;left:${placement.left}px;max-height:${placement.maxHeight}px;`
      + (placement.placement === 'above' ? 'transform:translateY(-100%)' : '')
    : 'visibility:hidden';
  const files = task.attachments || [];
  const images = files.filter((f) => f.image).slice(0, 4);
  // The brief names its images among the task's files.
  const briefFiles = useMarkdownFiles(api, task, [], read?.body || '');
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
    ${linked && html`<div class="tb-task-panel-link">The title is the task's link: Ctrl/⌘-click opens a new tab.</div>`}
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
          : read.body ? html`<div class="tb-task-brief-clamp"><${MarkdownDocument} source=${read.body} attachments=${briefFiles} className="markdown-document tb-comment-md" /></div>`
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
  // On a board the title is the task's link (task-route.js): a plain click
  // opens the form here, a middle or Mod-click (or Copy link address) is
  // the browser's, a new tab at the task.
  const board = useContext(TaskLink);
  const href = onOpen && board ? taskPath({ taskId: task.id, node: board.node, group: board.group }) : '';
  const openForm = (event) => {
    if (href && (event.button || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey)) { event.stopPropagation(); return; }
    event.preventDefault?.();
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
    ${h(href ? 'a' : 'span', {
      role: onOpen && !href ? 'button' : undefined, tabindex: '0', href: href || undefined,
      class: `tb-title${onOpen ? ' tb-title-open' : ''}${extra ? ` ${extra}` : ''}`,
      draggable: href ? 'false' : undefined,
      'aria-describedby': open ? panelID : undefined,
      'aria-label': onOpen ? `${task.title}: opens the task` : undefined,
      onClick: onOpen ? openForm : undefined, onKeyDown: key, ...titleProps,
    }, task.title)}
    ${open && html`<${TaskCardPanel} task=${task} api=${api} read=${read} nameOf=${nameOf}
        panelID=${panelID} headingID=${headingID} panelRef=${panelRef} placement=${placement} chips=${chips} linked=${!!href} />`}
  </span>`;
}
