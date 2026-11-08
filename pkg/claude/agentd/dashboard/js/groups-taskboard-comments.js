// groups-taskboard-comments.js -- a task's activity at the foot of the edit
// form (docs/taskboard.md "Comments and activity"): the changes and the
// comments, oldest first, and a composer that posts a comment at once (it
// does not wait for the form's Save). A comment is Markdown, rendered through
// markdown-document.js's allowlist, and may carry files; its author may edit
// or delete it. Everything goes through the board's api, so a linked node's
// task reads and takes comments the same way. A question for the operator
// (tk_8b2c86f7) is an entry of its own, with its answer right under it.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Textarea } from './ui-components.js';
import { relTime } from './helpers.js';
import { MarkdownDocument } from './markdown-document.js';
import { useAttachmentIntake } from './attachment-intake.js';
import { isPageNode, onPeerPage, selfNodeID } from './node-api.js';
import { taskFileLimits } from './taskboard-actions.js';
import { FilesField, StoredFiles, taskPastedName, useMarkdownFiles } from './groups-taskboard-files.js';
import { MarkdownEditor, submitKeyLabel } from './markdown-editor.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { SOURCE_MARK, questionSource } from './groups-taskboard-answer.js';

const html = htm.bind(h);

// COMMENT_AREA is where a paste or a drop is a comment's, not the task's:
// the composer and a comment being edited.
export const COMMENT_AREA = '.tb-comment-composer, .tb-comment-editor';

// COMMENT_MAX is a comment's size limit in UTF-8 bytes, as the server holds
// it; maxLength on the box counts characters, so the bytes are checked too.
export const COMMENT_MAX = 16 << 10;

// commentBytes is a comment's size as the server counts it.
export function commentBytes(text) {
  return new TextEncoder().encode(String(text || '').trim()).length;
}

const tooLong = (text) => commentBytes(text) > COMMENT_MAX;
const TOO_LONG = `over ${COMMENT_MAX >> 10} KiB: shorten it`;

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
const STAGE_NAME = { idea: 'Ideas', next: 'Next' };

// createdText is a created event's line: "created in Ideas, ref …", the stage
// by the name the board shows it under.
function createdText(detail) {
  const text = detail.replace(/^in (idea|next)\b/, (_, stage) => `in ${STAGE_NAME[stage]}`);
  return text.startsWith('in ') ? `created ${text}` : text;
}

// refText is a ref change's line: what the ref now reads, or that it went.
// An entry carries only the ref fields that changed.
function refText(changes) {
  const of = (field) => changes.find((c) => c.field === field);
  const url = of('ref_url');
  const label = of('ref_label');
  if (url && !url.after) return 'cleared ref';
  const now = label?.after || url?.after;
  if (now) return `ref → ${clip(now)}`;
  return label?.before ? 'cleared the ref label' : '';
}

export function changeText(entry) {
  const changes = entry.changes || [];
  // A ref edit's detail is only the changed fields' names ("kind, url,
  // label"); its changes say more. Other details read well as they are.
  const fromChanges = entry.kind === 'edited' && changes.some((c) => c.field.startsWith('ref_'));
  if (!fromChanges && (entry.detail_text || entry.detail)) {
    const detail = entry.detail_text || entry.detail;
    return entry.kind === 'created' ? createdText(detail) : detail;
  }
  const refs = changes.filter((c) => c.field.startsWith('ref_'));
  const parts = changes.filter((c) => !c.field.startsWith('ref_')).map((c) => {
    const field = FIELD_LABEL[c.field] || c.field;
    if (c.field === 'body') return 'edited the brief';
    if (!c.before) return `set ${field} to ${clip(c.after)}`;
    if (!c.after) return `cleared ${field}`;
    return `${field}: ${clip(c.before, 40)} → ${clip(c.after, 40)}`;
  });
  if (refs.length) {
    const ref = refText(refs);
    if (ref) parts.push(ref);
  }
  if (entry.action && !parts.length) return entry.action;
  return parts.join(' · ') || entry.detail_text || entry.detail || entry.kind;
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
    if (tooLong(body)) return;
    void run('Edit', () => api.editComment(taskId, comment.id, body));
  };
  const remove = async () => {
    const ok = await shellConfirm({ title: 'Delete comment?', body: 'The comment and its files are removed; the stream keeps a note that it was deleted.', okLabel: 'Delete' });
    if (ok) void run('Delete', () => api.deleteComment(taskId, comment.id));
  };
  const label = REASON_LABEL[comment.kind];
  // A comment shows the images it carries where its text names them.
  const files = useMarkdownFiles(api, { id: taskId, attachments: deleted ? [] : comment.attachments }, [], editing ?? comment.body);
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
            <${MarkdownEditor} compact class="tb-comment-edit-body" label="Edit comment" value=${editing} maxLength=${COMMENT_MAX} disabled=${busy}
              onInput=${setEditing} onSubmit=${save} attachments=${files} hint=${`${submitKeyLabel()} saves · Esc cancels`}
              onKeyDown=${(event) => {
                if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); setEditing(null); }
              }} />
            <span class="tb-comment-editor-acts">
              <${Button} size="sm" variant="primary" busy=${busy} disabled=${busy || !editing.trim() || tooLong(editing)} onClick=${save}>Save comment<//>
              ${tooLong(editing) && html`<span class="ui-hint tb-comment-too-long">${TOO_LONG}</span>`}
              <${Button} size="sm" variant="ghost" disabled=${busy} onClick=${() => setEditing(null)}>Cancel<//>
            </span>
          </div>`
        : html`<div class="tb-comment-body"><${MarkdownDocument} source=${comment.body} attachments=${files} className="markdown-document tb-comment-md" /></div>`}
    ${!deleted && comment.attachments?.length > 0 && html`<${StoredFiles} api=${api} taskId=${taskId} attachments=${comment.attachments}
      class="tb-comment-files" surface=${`comment-${comment.id}`} />`}
  </li>`;
}

// QUESTION_STATE says what became of a question that is not open.
const QUESTION_STATE = { answered: 'answered', withdrawn: 'withdrawn by its asker', closed: 'closed with the task' };

// The words of a mirrored question's entry, by where it was asked
// (questionSource).
const SOURCE_KIND = { '': 'asked the operator', approval: 'asked the operator for access', notify: 'asked the operator in Messages' };

// answerWords is an answer as the activity shows it: the picked labels,
// then the words.
export function answerWords(question) {
  const a = question?.answer;
  if (!a) return '';
  const labels = (a.picked || []).map((key) => (question.options || []).find((o) => o.key === key)?.label || key);
  return [labels.length ? `Picked ${labels.join(', ')}` : '', a.text ? `“${a.text}”` : ''].filter(Boolean).join(' — ');
}

// QuestionAnswerForm answers a question in the activity: the options (one,
// or any for a multi-select question) and words, as the Answer dialog does.
function QuestionAnswerForm({ question, busy, onSubmit, onCancel }) {
  const options = question.options || [];
  // An access request takes a pick only.
  const words = questionSource(question) !== 'approval';
  const [picked, setPicked] = useState([]);
  const [text, setText] = useState('');
  const empty = !picked.length && !text.trim();
  const toggle = (key) => {
    if (!question.multi) return setPicked([key]);
    setPicked(picked.includes(key) ? picked.filter((k) => k !== key) : [...picked, key]);
  };
  const submit = () => { if (!empty && !busy) void onSubmit({ picked, text: text.trim() }); };
  return html`<div class="tb-question-form">
    ${options.length > 0 && html`<div class="tb-answer-list" role=${question.multi ? 'group' : 'radiogroup'} aria-label="Options">
      ${options.map((o) => html`<label key=${o.key} class=${`tb-answer-opt${picked.includes(o.key) ? ' tb-answer-on' : ''}`} data-answer-option=${o.key}>
        <input type=${question.multi ? 'checkbox' : 'radio'} name=${`tb-q-${question.n}`} value=${o.key} checked=${picked.includes(o.key)} disabled=${busy}
          onChange=${() => toggle(o.key)} />
        <span><span class="tb-answer-label">${o.label}${o.key === question.recommend && html`<span class="ui-tag tb-answer-rec">recommended</span>`}</span>
          ${o.description && html`<span class="tb-answer-desc">${o.description}</span>`}</span>
      </label>`)}
    </div>`}
    ${words && html`<${Textarea} class="tb-answer-text" rows=${2} aria-label="Answer in your own words" value=${text} maxLength=${4000} disabled=${busy}
      placeholder=${options.length ? 'optional: with your pick, or instead of one' : 'your answer'}
      onInput=${(event) => setText(event.currentTarget.value)}
      onKeyDown=${(event) => {
        if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) { event.preventDefault(); event.stopPropagation(); submit(); }
        if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onCancel(); }
      }} />`}
    <span class="tb-comment-editor-acts">
      <${Button} size="sm" variant="primary" class="tb-question-send" busy=${busy} disabled=${busy || empty} onClick=${submit}>Send answer<//>
      <${Button} size="sm" variant="ghost" disabled=${busy} onClick=${onCancel}>Cancel<//>
    </span>
  </div>`;
}

// QuestionEntry is a question in a task's activity (tk_8b2c86f7): who
// asked, the question and its context, its options, and right under it the
// answer once given, or Answer… while it is open. Its asker withdraws an
// open question by deleting it.
function QuestionEntry({ api, task, entry, me, nameOf, canAnswerIt, onChanged }) {
  const comment = entry.comment;
  const q = comment.question;
  const [answering, setAnswering] = useState(false);
  const [busy, setBusy] = useState(false);
  const deleted = !!comment.deleted_at;
  const open = q.state === 'open' && (task.status === 'open' || task.status === 'blocked');
  const source = questionSource(q);
  // A mirrored question closes with its original; its asker does not withdraw it here.
  const mine = !!me && comment.author === me && open && !deleted && !source;
  // A mirrored question that ended unanswered keeps why as its answer's words.
  const ended = q.state === 'closed' && !!q.answer;
  const picked = new Set(q.answer?.picked || []);
  const run = async (what, fn) => {
    setBusy(true);
    try {
      await fn();
      setAnswering(false);
      onChanged();
    } catch (cause) {
      shellToast(`${what} failed: ${cause?.message || cause}`, true);
    } finally {
      setBusy(false);
    }
  };
  const withdraw = async () => {
    const ok = await shellConfirm({ title: `Withdraw Q${q.n}?`, body: 'The question leaves the operator\'s lane; the activity keeps a note of it.', okLabel: 'Withdraw' });
    if (ok) void run('Withdraw', () => api.deleteComment(task.id, comment.id));
  };
  // By number; a node from before per-task questions sends none, and is
  // answered through the question task's own route.
  const which = q.n || 0;
  return html`<li class=${`tb-activity-comment tb-activity-question tb-question-${q.state || 'open'}`} data-comment=${comment.id} data-question-n=${q.n}>
    <div class="tb-comment-head">
      <span class="tb-comment-author">${authorOf(comment, nameOf)}</span>
      <span class="tb-comment-kind" title=${SOURCE_MARK[source].also || null}>${SOURCE_MARK[source].icon} ${SOURCE_KIND[source]} · Q${q.n}</span>
      ${q.state !== 'open' && q.state !== 'answered' && !ended && html`<span class="tb-comment-kind tb-question-ended">${QUESTION_STATE[q.state] || q.state}</span>`}
      ${q.waiting && html`<span class="ui-tag ui-tag-warn tb-question-waiting">⏳ waiting</span>`}
      <span class="tb-activity-at" title=${comment.at}>${relTime(comment.at)}</span>
      ${mine && html`<span class="tb-comment-acts">
        <${Button} size="sm" variant="ghost" class="tb-question-withdraw" disabled=${busy} onClick=${() => void withdraw()}>Withdraw<//>
      </span>`}
    </div>
    <div class="tb-question-title">${q.title}</div>
    ${!deleted && comment.body && html`<div class="tb-comment-body"><${MarkdownDocument} source=${comment.body} className="markdown-document tb-comment-md" /></div>`}
    ${(q.options || []).length > 0 && html`<ul class="tb-question-opts">
      ${q.options.map((o) => html`<li key=${o.key} class=${picked.has(o.key) ? 'tb-question-picked' : ''}>${picked.has(o.key) ? '●' : '○'} ${o.label}${
        o.description ? html`<span class="tb-answer-desc">: ${o.description}</span>` : null}${
        o.key === q.recommend ? html` <span class="ui-tag tb-answer-rec">recommended</span>` : null}</li>`)}
    </ul>`}
    ${q.answer && html`<div class="tb-question-answer" data-answer-of=${q.n}>
      <div class="tb-comment-head"><span class="tb-comment-author">${nameOf ? nameOf(q.answer.answered_by) : q.answer.answered_by}</span>
        <span class="tb-comment-kind">${ended ? 'closed' : 'answered'} Q${q.n}</span>
        <span class="tb-activity-at" title=${q.answer.answered_at}>${relTime(q.answer.answered_at)}</span></div>
      <div class="tb-comment-body">${ended ? q.answer.text : answerWords(q)}</div>
    </div>`}
    ${open && canAnswerIt && !answering && html`<div class="tb-card-answer"><${Button} size="sm" variant="primary" class="tb-answer-open"
      onClick=${() => setAnswering(true)}>Answer…<//></div>`}
    ${open && answering && html`<${QuestionAnswerForm} question=${q} busy=${busy} onCancel=${() => setAnswering(false)}
      onSubmit=${(answer) => run('Answer', () => api.answer(task.id, answer.picked, answer.text, which))} />`}
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
  const composer = useRef(null);
  // A file pasted or dropped on the comment goes with it, linked where it
  // landed in the text.
  const intake = useAttachmentIntake({ setAttachments: setFiles, attachments: files, busy: () => sendingRef.current,
    limits: taskFileLimits(0), onReject: (text) => shellToast(text, true), onChange: () => { key.current = newCommentKey(); },
    nameFile: taskPastedName,
    onAdded: (added, event) => { if (event?.target?.closest?.('.md-ed')) composer.current?.insertFiles(added.map((a) => a.name), event); } });
  const composerFiles = useMarkdownFiles(api, null, files);

  useEffect(() => {
    if (typeof api?.activity !== 'function') { setStream({ state: 'failed', entries: [] }); return undefined; }
    let live = true;
    api.activity(task.id).then((out) => {
      if (live) setStream({ state: 'ready', entries: Array.isArray(out?.entries) ? out.entries : [], error: '' });
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
    if (tooLong(body)) return;
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

  // An answer shows right under its question, not at its own place.
  const asked = new Set(stream.entries.filter((entry) => entry.comment?.question).map((entry) => entry.comment.question.n));
  const entries = stream.entries.filter((entry) => entry.kind !== 'comment_edited' && !(entry.comment?.answers && asked.has(entry.comment.answers)));
  const comments = entries.filter((entry) => entry.comment).length;
  const canAnswerIt = typeof api?.answer === 'function' && api?.can?.assign !== false;
  return html`<div class="tb-activity" data-task-activity=${task.id}>
    ${stream.state === 'loading' && html`<div class="ui-hint">reading the activity…</div>`}
    ${stream.state === 'failed' && html`<div class="ui-hint">the activity could not be read${stream.error ? `: ${stream.error}` : ''}</div>`}
    ${stream.state === 'ready' && stream.error && html`<div class="ui-hint tb-activity-stale">the activity could not be read again (${stream.error}): it may miss the latest</div>`}
    ${entries.length > 0 && html`<ol class="tb-activity-list" ref=${list}>
      ${entries.map((entry, i) => (entry.comment?.question
        ? html`<${QuestionEntry} key=${`q-${entry.comment.id}`} api=${api} task=${task} entry=${entry} me=${me} nameOf=${nameOf}
            canAnswerIt=${canAnswerIt} onChanged=${() => setReads((n) => n + 1)} />`
        : entry.comment
        ? html`<${CommentEntry} key=${`c-${entry.comment.id}`} api=${api} taskId=${task.id} entry=${entry} me=${me} nameOf=${nameOf}
            onChanged=${() => setReads((n) => n + 1)} onEditing=${onEditing} />`
        : html`<${ChangeEntry} key=${entry.id || `e-${i}`} entry=${entry} nameOf=${nameOf} />`))}
    </ol>`}
    ${stream.state === 'ready' && !comments && html`<div class="ui-hint tb-activity-empty">no comments yet</div>`}
    <div class=${`tb-comment-composer${intake.dragOver ? ' files-drag-over' : ''}`} ...${intake.handlers}>
      <${MarkdownEditor} compact class="tb-comment-new" label="Comment" value=${draft} maxLength=${COMMENT_MAX} disabled=${sending}
        placeholder=${`Write a comment (Markdown) · ${submitKeyLabel()} sends`} editorRef=${composer} attachments=${composerFiles}
        modeKey="tclaude.dash.mdEditor.comment" onSubmit=${() => void send()}
        onInput=${(next) => {
          // The key names the comment as sent (trimmed): an edit that sends
          // the same text keeps it, so a retry after a lost answer is not a
          // second comment.
          if (next.trim() !== draft.trim()) key.current = newCommentKey();
          setDraft(next);
        }} />
      ${tooLong(draft) && html`<span class="ui-hint tb-comment-too-long">${TOO_LONG}</span>`}
      <span class="tb-comment-send-row">
        <${FilesField} intake=${intake} attachments=${files} busy=${sending} hint="files go with the comment: 📎, or paste into it" />
        <${Button} size="sm" variant="primary" id="taskboard-comment-send" busy=${sending} disabled=${sending || (!draft.trim() && !files.length) || tooLong(draft)}
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
