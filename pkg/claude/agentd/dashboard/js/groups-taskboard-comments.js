// groups-taskboard-comments.js -- a task's activity at the foot of the edit
// form (docs/taskboard.md "Comments and activity"): the changes and the
// comments, oldest first, and a composer that posts a comment at once (it
// does not wait for the form's Save). A comment is Markdown, rendered through
// markdown-document.js's allowlist, and may carry files; its author may edit
// or delete it. Everything goes through the board's api, so a linked node's
// task reads and takes comments the same way.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Textarea } from './ui-components.js';
import { relTime } from './helpers.js';
import { MarkdownDocument } from './markdown-document.js';
import { useAttachmentIntake } from './attachment-intake.js';
import { isPageNode, onPeerPage, selfNodeID } from './node-api.js';
import { taskFileLimits } from './taskboard-actions.js';
import { FilesField, StoredTile } from './groups-taskboard-files.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);

// COMMENT_AREA is where a paste or a drop is a comment's, not the task's:
// the composer and a comment being edited.
export const COMMENT_AREA = '.tb-comment-composer, .tb-comment-editor';

// COMMENT_MAX is a comment's size limit, as the server holds it.
export const COMMENT_MAX = 16 << 10;

// REASON_LABEL names a typed comment: a reason the task carries.
const REASON_LABEL = { block_reason: 'blocked', done_reason: 'done', drop_reason: 'dropped', report_note: 'report' };

// myAuthor is the author the viewer's comments carry on a board: this node's
// operator on its own board, and this node (peer:<instance>) on a linked
// node's, where the board names a peer's comments by its node. Only those
// comments offer Edit and Delete; the server holds the rule either way.
export function myAuthor(node = '', { self = selfNodeID(), peerPage = onPeerPage() } = {}) {
  const remote = isPageNode(node) ? peerPage : node !== self;
  if (!remote) return 'human:operator';
  return self ? `peer:${self}` : '';
}

// newCommentKey is a fresh idempotency key for one comment.
export function newCommentKey() {
  const uuid = globalThis.crypto?.randomUUID?.();
  return `dash-${uuid || `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`}`;
}

const clip = (text, max = 60) => {
  const s = String(text ?? '').replace(/\s+/g, ' ').trim();
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
};

// FIELD_LABEL names a changed field the way the form does.
const FIELD_LABEL = { title: 'title', body: 'brief', ref_url: 'ref', ref_label: 'ref', depends_on: 'dependencies', stage: 'stage',
  status: 'status', assignee: 'assignee', group: 'group' };

// changeText says what a change entry did, in one line.
export function changeText(entry) {
  if (entry.detail_text || entry.detail) return entry.detail_text || entry.detail;
  const changes = entry.changes || [];
  const parts = changes.map((c) => {
    const field = FIELD_LABEL[c.field] || c.field;
    if (c.field === 'body') return 'edited the brief';
    if (!c.before) return `set ${field} to ${clip(c.after)}`;
    if (!c.after) return `cleared ${field}`;
    return `${field}: ${clip(c.before, 40)} → ${clip(c.after, 40)}`;
  });
  if (entry.action && !parts.length) return entry.action;
  return parts.join(' · ') || entry.kind;
}

function authorOf(comment, nameOf) {
  return comment.author_name || (nameOf ? nameOf(comment.author) : comment.author);
}

function CommentEntry({ api, taskId, entry, me, nameOf, onChanged, onEditing = () => {} }) {
  const comment = entry.comment;
  const [editing, setEditing] = useState(null);
  // An open edit is unsaved input: the form asks before it closes.
  useEffect(() => { onEditing(comment.id, editing !== null && editing !== comment.body); }, [editing !== null && editing !== comment.body]);
  useEffect(() => () => onEditing(comment.id, false), []);
  const [busy, setBusy] = useState(false);
  const deleted = !!comment.deleted_at;
  const mine = !!me && comment.author === me && !deleted && comment.kind === 'comment';
  const run = async (what, fn) => {
    setBusy(true);
    try {
      await fn();
      setEditing(null);
      onChanged();
    } catch (cause) {
      shellToast(`${what} failed: ${cause?.message || cause}`, true);
    } finally {
      setBusy(false);
    }
  };
  const save = () => {
    const body = (editing || '').trim();
    if (!body || body === comment.body) { setEditing(null); return; }
    void run('Edit', () => api.editComment(taskId, comment.id, body));
  };
  const remove = async () => {
    const ok = await shellConfirm({ title: 'Delete comment?', body: 'The comment and its files are removed; the stream keeps a note that it was deleted.', okLabel: 'Delete' });
    if (ok) void run('Delete', () => api.deleteComment(taskId, comment.id));
  };
  const label = REASON_LABEL[comment.kind];
  return html`<li class=${`tb-activity-comment${deleted ? ' tb-comment-deleted' : ''}${mine ? ' tb-comment-mine' : ''}`} data-comment=${comment.id}>
    <div class="tb-comment-head">
      <span class="tb-comment-author">${authorOf(comment, nameOf)}</span>
      ${label && html`<span class="tb-comment-kind">${label}</span>`}
      <span class="tb-activity-at" title=${comment.at}>${relTime(comment.at)}</span>
      ${comment.edited_at && !deleted && html`<span class="tb-activity-at" title=${comment.edited_at}>· edited</span>`}
      ${mine && editing === null && html`<span class="tb-comment-acts">
        <${Button} size="sm" variant="ghost" class="tb-comment-edit" disabled=${busy} onClick=${() => setEditing(comment.body)}>Edit<//>
        <${Button} size="sm" variant="ghost" class="tb-comment-rm" disabled=${busy} onClick=${() => void remove()}>Delete<//>
      </span>`}
    </div>
    ${deleted
      ? html`<div class="tb-comment-body ui-hint">deleted</div>`
      : editing !== null
        ? html`<div class="tb-comment-editor">
            <${Textarea} class="tb-comment-edit-body" rows=${3} value=${editing} maxLength=${COMMENT_MAX} disabled=${busy}
              onInput=${(event) => setEditing(event.currentTarget.value)}
              onKeyDown=${(event) => {
                if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) { event.preventDefault(); event.stopPropagation(); save(); }
                if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); setEditing(null); }
              }} />
            <span class="tb-comment-editor-acts">
              <${Button} size="sm" variant="primary" busy=${busy} disabled=${busy || !editing.trim()} onClick=${save}>Save comment<//>
              <${Button} size="sm" variant="ghost" disabled=${busy} onClick=${() => setEditing(null)}>Cancel<//>
            </span>
          </div>`
        : html`<div class="tb-comment-body"><${MarkdownDocument} source=${comment.body} className="markdown-document tb-comment-md" /></div>`}
    ${!deleted && comment.attachments?.length > 0 && html`<div class="tb-files tb-comment-files">
      ${comment.attachments.map((attachment) => html`<${StoredTile} key=${attachment.id} api=${api} taskId=${taskId} attachment=${attachment} />`)}
    </div>`}
  </li>`;
}

function ChangeEntry({ entry, nameOf }) {
  const who = entry.actor_name || (nameOf ? nameOf(entry.actor) : entry.actor);
  return html`<li class="tb-activity-change" data-activity=${entry.kind}>
    <span class="tb-activity-who">${who}</span> <span class="tb-activity-what">${changeText(entry)}</span>
    <span class="tb-activity-at" title=${entry.at}>${relTime(entry.at)}</span>
  </li>`;
}

// TaskActivity is the stream and the composer. It reads the stream when it
// opens, after each comment, and when the task's comment count or update
// time moves (the board's poll). onDraft(dirty) tells the form a comment is
// being written, so closing it asks first.
export function TaskActivity({ api, task, me = '', nameOf = null, onDraft = () => {} }) {
  const [stream, setStream] = useState({ state: 'loading', entries: [] });
  const [reads, setReads] = useState(0);
  const [draft, setDraft] = useState('');
  const [files, setFiles] = useState([]);
  const [sending, setSending] = useState(false);
  const sendingRef = useRef(false);
  const key = useRef(newCommentKey());
  const list = useRef(null);
  const intake = useAttachmentIntake({ setAttachments: setFiles, attachments: files, busy: () => sendingRef.current,
    limits: taskFileLimits(0), onReject: (text) => shellToast(text, true), onChange: () => { key.current = newCommentKey(); } });

  useEffect(() => {
    if (typeof api?.activity !== 'function') { setStream({ state: 'failed', entries: [] }); return undefined; }
    let live = true;
    api.activity(task.id).then((out) => {
      if (live) setStream({ state: 'ready', entries: Array.isArray(out?.entries) ? out.entries : [] });
    }).catch((cause) => { if (live) setStream((before) => ({ ...before, state: before.entries.length ? 'ready' : 'failed', error: cause?.message || String(cause) })); });
    return () => { live = false; };
  }, [task.id, reads, task.comment_count, task.updated_at]);

  // The newest is at the bottom, by the composer: follow it.
  useEffect(() => {
    const el = list.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [stream.entries.length]);

  const [editingIds, setEditingIds] = useState(() => new Set());
  const onEditing = (id, on) => setEditingIds((before) => {
    if (before.has(id) === on) return before;
    const next = new Set(before);
    if (on) next.add(id); else next.delete(id);
    return next;
  });
  const dirty = draft.trim().length > 0 || files.length > 0 || editingIds.size > 0;
  useEffect(() => { onDraft(dirty); }, [dirty]);

  const send = async () => {
    const body = draft.trim();
    if (sendingRef.current) return;
    if (!body) {
      if (files.length) shellToast('A comment needs text: say what the files are', true);
      return;
    }
    sendingRef.current = true;
    setSending(true);
    try {
      await api.comment(task.id, body, key.current, files.map(({ file, name }) => ({ file, name })));
      for (const f of files) if (f.url) URL.revokeObjectURL(f.url);
      setFiles([]);
      setDraft('');
      key.current = newCommentKey();
      setReads((n) => n + 1);
    } catch (cause) {
      // The key stays: sending the same comment again is not a second one.
      shellToast(`Comment not sent: ${cause?.message || cause}`, true);
    } finally {
      sendingRef.current = false;
      setSending(false);
    }
  };

  const entries = stream.entries.filter((entry) => entry.kind !== 'comment_edited');
  const comments = entries.filter((entry) => entry.comment).length;
  return html`<div class="tb-activity" data-task-activity=${task.id}>
    ${stream.state === 'loading' && html`<div class="ui-hint">reading the activity…</div>`}
    ${stream.state === 'failed' && html`<div class="ui-hint">the activity could not be read${stream.error ? `: ${stream.error}` : ''}</div>`}
    ${entries.length > 0 && html`<ol class="tb-activity-list" ref=${list}>
      ${entries.map((entry, i) => (entry.comment
        ? html`<${CommentEntry} key=${`c-${entry.comment.id}`} api=${api} taskId=${task.id} entry=${entry} me=${me} nameOf=${nameOf}
            onChanged=${() => setReads((n) => n + 1)} onEditing=${onEditing} />`
        : html`<${ChangeEntry} key=${entry.id || `e-${i}`} entry=${entry} nameOf=${nameOf} />`))}
    </ol>`}
    ${stream.state === 'ready' && !comments && html`<div class="ui-hint tb-activity-empty">no comments yet</div>`}
    <div class=${`tb-comment-composer${intake.dragOver ? ' files-drag-over' : ''}`} ...${intake.handlers}>
      <${Textarea} class="tb-comment-new" rows=${3} value=${draft} maxLength=${COMMENT_MAX} disabled=${sending}
        placeholder="Write a comment (Markdown) · Ctrl+Enter sends"
        onInput=${(event) => { setDraft(event.currentTarget.value); key.current = newCommentKey(); }}
        onKeyDown=${(event) => {
          if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) { event.preventDefault(); event.stopPropagation(); void send(); }
        }} />
      <span class="tb-comment-send-row">
        <${FilesField} intake=${intake} attachments=${files} busy=${sending} hint="files go with the comment: 📎, or paste into it" />
        <${Button} size="sm" variant="primary" id="taskboard-comment-send" busy=${sending} disabled=${sending || (!draft.trim() && !files.length)}
          onClick=${() => void send()}>${sending ? 'Sending…' : 'Comment'}<//>
      </span>
    </div>
  </div>`;
}

// CommentCount is a task's 💬 count on its row or card: a click opens the
// edit form, whose foot holds the comments. Nothing shows without comments.
export function CommentCount({ task, onOpen = null }) {
  const n = task?.comment_count || 0;
  if (!n) return null;
  const title = `${n} comment${n === 1 ? '' : 's'}${onOpen ? ' · open' : ''}`;
  return onOpen
    ? html`<${Button} size="sm" variant="ghost" class="tb-comment-count" title=${title} aria-label=${title}
        onClick=${(event) => { event.stopPropagation(); onOpen(); }}>💬 ${n}<//>`
    : html`<span class="tb-comment-count" title=${title}>💬 ${n}</span>`;
}
