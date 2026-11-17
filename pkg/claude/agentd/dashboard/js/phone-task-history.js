// phone-task-history.js -- the phone task page's id, meta line and history
// entries (tk_2d942cb6 items 1, 2, 8 and 9), at parity with the desktop's
// edit form (groups-taskboard-edit.js) and its activity
// (groups-taskboard-comments.js), in the phone's compact style. Each is its
// own section component; phone-task-page.js places them.
//
// - TaskIdLine: the task's id with ⧉ to copy it.
// - TaskMetaLine: created by whom and when, assigned to whom by whom and
//   when, closed when, and who last edited it.
// - ChangeItem: a change entry in the words the desktop uses (changeText:
//   "status: open → done", "edited the brief").
// - QuestionItem: a question to the operator: Q-n, its title and context,
//   what became of it, its options (the one picked marked) and the answer
//   right under it. Answering stays the page's question card above.
// - historyEntries: the entries as the desktop lists them: no comment_edited
//   notes, and an answer under its question rather than in its own place.

import { h } from 'preact';
import htm from 'htm';
import { relTime } from './helpers.js';
import { MarkdownDocument } from './markdown-document.js';
import { changeText } from './groups-taskboard-comments.js';
import { SOURCE_MARK, answerWords, questionSource } from './groups-taskboard-answer.js';
import { writeClipboard } from './insecure-origin.js';
import { shellToast } from './shell-state.js';

const html = htm.bind(h);

// copyId copies a task's id and says whether it went.
export async function copyId(id, write = writeClipboard, toast = shellToast) {
  if (await write(id)) toast(`Copied ${id}`);
  else toast(`Could not copy ${id}`, true);
}

// TaskIdLine is the id, monospace, with ⧉ beside it.
export function TaskIdLine({ id, copy = copyId }) {
  return html`<p class="phone-task-id" data-task-id=${id}>
    <code>${id}</code>
    <button data-ui-exempt type="button" class="phone-task-id-copy" aria-label=${`Copy ${id}`} title="Copy the id"
      onClick=${() => void copy(id)}>⧉</button>
  </p>`;
}

// whoIs names an actor: "you" for the operator, an agent by its current
// name, anything else (a peer) by the name the server sent.
function whoIs(actor, name, nameOf) {
  const text = String(actor || '');
  if (!text) return name || '';
  if (text.startsWith('human:')) return nameOf(text);
  // An agent the roster knows goes by its name now; one it does not (retired)
  // by the name the server kept, not a clipped id.
  if (text.startsWith('agent:') && nameOf(text) !== text.slice(6, 18)) return nameOf(text);
  return name || nameOf(text);
}

// metaParts are the meta line's parts: the desktop form's line, then who
// last edited it (the board's own last_edited_*, as the desktop card says).
export function metaParts(task, nameOf) {
  const assignee = task.assignee?.agent_id;
  const created = task.created_by || task.created_at;
  const edited = task.last_edited_by && task.last_edited_at;
  return [
    created && `created${task.created_by ? ` by ${whoIs(task.created_by, task.created_by_name, nameOf)}` : ''}${task.created_at ? ` ${relTime(task.created_at)}` : ''}`,
    assignee && `assigned to ${task.assignee?.name || nameOf(`agent:${assignee}`)}${task.assigned_by ? ` by ${whoIs(task.assigned_by, '', nameOf)}` : ''}${task.assigned_at ? ` ${relTime(task.assigned_at)}` : ''}`,
    task.closed_at && `${task.status} ${relTime(task.closed_at)}`,
    edited && `last edited by ${whoIs(task.last_edited_by, task.last_edited_by_name, nameOf)} ${relTime(task.last_edited_at)}`,
  ].filter(Boolean);
}

// TaskMetaLine is the meta line, its parts joined by a dot.
export function TaskMetaLine({ task, nameOf }) {
  const parts = metaParts(task, nameOf);
  return html`<p class="phone-task-meta" data-task-meta>${parts.join(' · ')}</p>`;
}

// historyEntries is the activity as the desktop lists it: an answer shows
// under its question, and an edit's own note is not an entry.
export function historyEntries(entries = []) {
  const asked = new Set(entries.filter((entry) => entry.comment?.question).map((entry) => entry.comment.question.n));
  return entries.filter((entry) => entry.kind !== 'comment_edited' && !(entry.comment?.answers && asked.has(entry.comment.answers)));
}

// ChangeItem is one change: who, what in the desktop's words, when.
export function ChangeItem({ entry, nameOf }) {
  return html`<li class="phone-task-change" data-activity=${entry.kind}>
    <span class="phone-task-who">${whoIs(entry.actor, entry.actor_name, nameOf)}</span>
    <span class="phone-task-when">${relTime(entry.at)}</span>
    <span class="phone-task-what">${changeText(entry)}</span>
  </li>`;
}

// QUESTION_STATE says what became of a question that is not open.
const QUESTION_STATE = { answered: 'answered', withdrawn: 'withdrawn', closed: 'closed with the task' };
const SOURCE_KIND = { '': 'asked you', approval: 'asked you for access', notify: 'asked you in Messages' };

// QuestionItem is a question in the history (the desktop's QuestionEntry,
// read-only: the page's question card answers an open one).
export function QuestionItem({ entry, nameOf }) {
  const comment = entry.comment;
  const q = comment.question;
  const source = questionSource(q);
  const ended = q.state === 'closed' && !!q.answer;
  const state = q.state === 'open' ? 'open' : ended ? 'closed' : QUESTION_STATE[q.state] || q.state;
  const picked = new Set(q.answer?.picked || []);
  return html`<li class=${`phone-task-question phone-task-question-${q.state || 'open'}`} data-question-n=${q.n}>
    <span class="phone-task-who">${whoIs(comment.author, comment.author_name, nameOf)}</span>
    <span class="phone-task-when">${relTime(comment.at)}</span>
    <span class="phone-task-q-kind">${SOURCE_MARK[source]?.icon || '?'} ${SOURCE_KIND[source] || SOURCE_KIND['']} · Q${q.n} · <b>${state}</b>${q.waiting ? ' · ⏳ waiting' : ''}</span>
    <p class="phone-task-q-title">${q.title}</p>
    ${!comment.deleted_at && comment.body && html`<${MarkdownDocument} source=${comment.body} className="markdown-document phone-task-md" />`}
    ${(q.options || []).length > 0 && html`<ul class="phone-task-q-opts">
      ${q.options.map((o) => html`<li key=${o.key} class=${picked.has(o.key) ? 'phone-task-q-picked' : ''}>${picked.has(o.key) ? '●' : '○'} ${o.label || o.key}${
        o.key === q.recommend ? html` <span class="phone-task-q-rec">recommended</span>` : null}</li>`)}
    </ul>`}
    ${q.answer && html`<div class="phone-task-q-answer" data-answer-of=${q.n}>
      <span class="phone-task-who">${whoIs(q.answer.answered_by, '', nameOf)}</span>
      <span class="phone-task-when">${ended ? 'closed' : 'answered'} ${relTime(q.answer.answered_at)}</span>
      <p>${ended ? q.answer.text : answerWords(q)}</p>
    </div>`}
  </li>`;
}
