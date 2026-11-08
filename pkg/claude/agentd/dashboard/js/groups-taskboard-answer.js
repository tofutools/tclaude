// groups-taskboard-answer.js -- questions for the operator (tk_aa049a67,
// tk_8b2c86f7, docs/taskboard.md): an agent asks the operator with `taskboard
// ask-operator`, on a task (an entry of its activity) or on a task made for
// the question, and the operator answers here. QuestionCard is an open
// question on another task, in the operator's lane. QuestionTag marks a question
// on a card or row ("❓ pick one · 3"); AnswerDialog answers it: the
// question's context rendered as Markdown, its files in the attachment
// viewer, the options as radios (or checkboxes for a multi-select question)
// each with its description and preview, the recommended one marked, and
// free text, always available, with a pick or instead of one. TaskQuestions
// lists a task's questions in its edit form, where Answer… opens the same
// dialog. Send answer is `taskboard answer`: the task closes as done and the
// agent that asked gets the answer.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Dialog } from './ui-dialog.js';
import { Button, Textarea } from './ui-components.js';
import { MarkdownDocument } from './markdown-document.js';
import { StoredFiles, useMarkdownFiles } from './groups-taskboard-files.js';
import { shellToast } from './shell-state.js';

const html = htm.bind(h);

// taskQuestions are task's questions: every question asked on it
// (tk_8b2c86f7), or a question task's own from a node that sends only that.
export function taskQuestions(task) {
  if (Array.isArray(task?.questions) && task.questions.length) return task.questions;
  return task?.question ? [task.question] : [];
}

// isOpenQuestion reports whether q still waits on its answer: a node from
// before per-task questions sends no state.
export function isOpenQuestion(q) {
  if (!q) return false;
  return q.state ? q.state === 'open' : !q.answer;
}

// openQuestions are task's questions still to answer, while it is live.
export function openQuestions(task) {
  if (!(task?.status === 'open' || task?.status === 'blocked')) return [];
  return taskQuestions(task).filter(isOpenQuestion);
}

// questionOpen reports whether task has a question still to answer.
export function questionOpen(task) {
  return openQuestions(task).length > 0;
}

// questionOf is the question n of task (n 0: its first open one, else its
// first).
export function questionOf(task, n = 0) {
  const all = taskQuestions(task);
  if (n) return all.find((q) => q.n === n) || null;
  return openQuestions(task)[0] || all[0] || null;
}

// askerOf is the actor that asked task's question n (n 0: its first open
// one): fixed when it was asked, while assigned_by follows assign and
// unassign.
export function askerOf(task, n = 0) {
  return questionOf(task, n)?.asked_by || task?.assigned_by || '';
}

// questionSource says where a mirrored question was asked (tk_8b2c86f7):
// 'approval' for an --ask-human prompt, 'notify' for a notify-human
// message, '' for a question asked on the board.
export function questionSource(question) {
  const s = String(question?.source || '');
  if (s.startsWith('approval:')) return 'approval';
  if (s.startsWith('notify:')) return 'notify';
  return '';
}

// SOURCE_MARK is a question's mark, and the words for where it is answered too.
export const SOURCE_MARK = {
  '': { icon: '❓', also: '' },
  approval: { icon: '🔐', also: 'An access request: also in Messages → Access requests, and decided in either place' },
  notify: { icon: '📨', also: 'A notify-human message: also in Messages, and answered in either place' },
};

// questionKind is the tag's words: how the question is answered.
export function questionKind(question) {
  const n = question?.options?.length || 0;
  if (!n) return 'answer in words';
  return `${question.multi ? 'pick any' : 'pick one'} · ${n}`;
}

// QuestionTag marks a question task, or a task with questions open; waiting
// says an asker is polling for the answer right now (ask-operator --wait).
export function QuestionTag({ task, askedBy = '' }) {
  const all = taskQuestions(task);
  if (!all.length) return null;
  const open = openQuestions(task);
  const waiting = task.waiting ? html`<span class="ui-tag ui-tag-warn tb-question-waiting" title="Its agent is waiting for the answer (ask-operator --wait)">⏳ ${askedBy || 'its agent'} is waiting</span>` : null;
  if (task.question && !(task.questions?.length > 1)) {
    if (!open.length) return html`<span class="ui-tag tb-question-tag tb-question-answered" title="Answered">❓ answered</span>`;
    return html`<span class="ui-tag ui-tag-accent tb-question-tag" title="A question for the operator (taskboard ask-operator)">❓ ${questionKind(open[0])}</span>${waiting}`;
  }
  if (!open.length) return null;
  return html`<span class="ui-tag ui-tag-accent tb-question-tag" title="Questions on this task wait on the operator: see its activity">❓ ${open.length} asked you</span>${waiting}`;
}

// QuestionCard is an open question on a task the operator does not hold,
// in the operator's lane (tk_8b2c86f7): the task it is on, the question,
// how it is answered, and Answer…. onOpen opens the task's form.
export function QuestionCard({ task, question, askedBy = '', onAnswer = null, onOpen = null }) {
  const q = question;
  const source = questionSource(q);
  const mark = SOURCE_MARK[source];
  return html`<div class="ui-card tb-card tb-question-card" data-question=${`${task.id}#${q.n}`} data-question-source=${source || null}>
    <div class="tb-question-on" title=${mark.also || null}>${mark.icon} ${askedBy || 'an agent'} asks on${' '}
      ${onOpen ? html`<${Button} size="sm" variant="link" class="tb-question-task" title="Open the task" onClick=${(event) => { event.stopPropagation(); onOpen(task.id); }}>${task.title}<//>`
        : html`<span class="tb-question-task">${task.title}</span>`}</div>
    <div class="tb-question-title">${q.title || task.title}</div>
    <div class="tb-card-meta"><span class="ui-tag ui-tag-accent tb-question-tag">${mark.icon} Q${q.n} · ${questionKind(q)}</span>${
      source ? html`<span class="ui-tag tb-question-also" title=${mark.also}>also in Messages</span>` : null}${
      q.waiting ? html`<span class="ui-tag ui-tag-warn tb-question-waiting" title="Its agent is waiting for the answer (ask-operator --wait)">⏳ waiting</span>` : null}${
      q.blocks && task.status === 'blocked' ? html`<span class="ui-tag ui-tag-warn" title="Asking blocked the task until the answer">blocks the task</span>` : null}</div>
    ${onAnswer && html`<div class="tb-card-answer"><${Button} size="sm" variant="primary" class="tb-answer-open"
      onClick=${(event) => { event.stopPropagation(); onAnswer(task.id, q.n); }}>Answer…<//></div>`}
  </div>`;
}

// answerWords is an answer as the activity and the edit form show it: the
// picked labels, then the words.
export function answerWords(question) {
  const a = question?.answer;
  if (!a) return '';
  const labels = (a.picked || []).map((key) => (question.options || []).find((o) => o.key === key)?.label || key);
  return [labels.length ? `Picked ${labels.join(', ')}` : '', a.text ? `“${a.text}”` : ''].filter(Boolean).join(' — ');
}

// ownQuestion reports whether q is task's own question (a question task's):
// its brief is then the question's context, and its files the question's.
export function ownQuestion(task, q) {
  return !!task?.question && (q?.n || 1) === (task.question.n || 1);
}

// useQuestionTask is task as the dialog shows it: the board's context
// carries no brief, so a question task without one is read in full through
// the board's api, for its brief (the question's context) and its files.
function useQuestionTask(api, task, own) {
  const needs = own && !(typeof task.body === 'string' && task.body) && typeof api?.task === 'function';
  const [full, setFull] = useState({ state: needs ? 'loading' : 'ready', task: null });
  useEffect(() => {
    if (!needs) return undefined;
    let live = true;
    api.task(task.id).then((read) => { if (live) setFull({ state: 'ready', task: read || null }); })
      .catch(() => { if (live) setFull({ state: 'failed', task: null }); });
    return () => { live = false; };
  }, [task.id, needs]);
  // The files follow the live row (the board's poll); the brief, the read.
  const shown = full.task ? { ...full.task, attachments: task.attachments || full.task.attachments } : task;
  return { task: shown, state: full.state };
}

// AnswerDialog answers question (default: task's first open question), with
// all the operator needs to decide together: the question's context
// rendered as Markdown (its images inline), the files that go with it as
// tiles opening the attachment viewer (the question task's own, or the
// files of the task it is asked on), and every option with its label,
// description and preview, the recommended one marked. Free text is always
// there, with a pick or instead of one. api (the board's) reads a question
// task's brief and its files; without it the dialog shows what the row
// carries. onSubmit({ picked, text }) runs the write and resolves true when
// it landed.
export function AnswerDialog({ task, question = null, askedBy = '', busy = false, api = null, onSubmit, onClose }) {
  const q = question || questionOf(task) || {};
  const own = ownQuestion(task, q);
  const read = useQuestionTask(api, task, own);
  const shownTask = read.task;
  const context = own ? shownTask.body : q.body;
  const files = shownTask.attachments || [];
  const mdFiles = useMarkdownFiles(api, shownTask, [], context || '');
  const options = q.options || [];
  const recommended = options.find((o) => o.key === q.recommend);
  const source = questionSource(q);
  // An access request takes a pick only.
  const words = source !== 'approval';
  const [picked, setPicked] = useState([]);
  const [text, setText] = useState('');
  const textRef = useRef(null);
  const sending = useRef(false);
  const empty = !picked.length && !text.trim();
  const toggle = (key) => {
    if (!q.multi) return setPicked([key]);
    setPicked(picked.includes(key) ? picked.filter((k) => k !== key) : [...picked, key]);
  };
  const submit = async () => {
    if (empty || busy || sending.current) return;
    sending.current = true;
    try {
      if (await onSubmit({ picked, text: text.trim() })) onClose();
    } finally {
      sending.current = false;
    }
  };
  return html`<${Dialog} id="taskboard-answer-modal" dialogClass="cron-create-modal taskboard-answer-modal"
    title=${q.title || task.title || task.id} onClose=${onClose} onSubmitHotkey=${() => void submit()}
    busy=${busy} initialFocusRef=${options.length || !words ? null : textRef} cancel="Cancel" resizeKey="tclaude.dash.modalSize.taskboard-answer"
    footer=${html`<${Button} variant="primary" id="taskboard-answer-submit" busy=${busy}
      disabled=${busy || empty} onClick=${() => { void submit(); }}>${busy ? 'Sending…' : 'Send answer'}<//>`}>
    <div class="modal-meta tb-answer-meta">${task.id}${q.n ? ` · Q${q.n}` : ''}${!own ? ` on “${task.title}”` : ''}${askedBy ? ` · asked by ${askedBy}` : ''}${(q.waiting ?? task.waiting) ? ' · waiting for the answer' : ''}</div>
    ${context ? html`<div class="tb-answer-context"><${MarkdownDocument} source=${context} attachments=${mdFiles} className="markdown-document tb-answer-md" /></div>`
      : own && read.state === 'loading' ? html`<div class="ui-hint tb-answer-context-hint">reading the question…</div>`
      : own && read.state === 'failed' ? html`<div class="ui-hint tb-answer-context-hint">the question's brief could not be read</div>` : null}
    ${files.length > 0 && html`<div class="tb-answer-files" data-answer-files=${files.length}>
      <span class="ui-eyebrow">${own ? 'Attached' : 'Files on the task'} · ${files.length}</span>
      <${StoredFiles} api=${api} taskId=${task.id} attachments=${files} surface="taskboard-answer" />
    </div>`}
    ${options.length > 0 && html`<div class="tb-answer-options">
      <div class="tb-answer-head"><span class="ui-eyebrow">${q.multi ? `Pick any of ${options.length}` : `Pick one of ${options.length}`}</span>
        ${recommended && html`<span class="tb-answer-rec-line">★ ${askedBy || 'the agent'} recommends <strong>${recommended.label}</strong></span>`}</div>
      <div class="tb-answer-list" role=${q.multi ? 'group' : 'radiogroup'} aria-label="Options">
        ${options.map((o, i) => html`<label key=${o.key} class=${`tb-answer-opt${picked.includes(o.key) ? ' tb-answer-on' : ''}${o.key === q.recommend ? ' tb-answer-recommended' : ''}`} data-answer-option=${o.key}>
          <input type=${q.multi ? 'checkbox' : 'radio'} name="tb-answer" value=${o.key} checked=${picked.includes(o.key)} disabled=${busy}
            onChange=${() => toggle(o.key)} />
          <span class="tb-answer-body"><span class="tb-answer-label"><span class="tb-answer-n">${i + 1}.</span> ${o.label}${o.key === q.recommend && html`<span class="ui-tag tb-answer-rec">★ recommended</span>`}</span>
            ${o.description && html`<span class="tb-answer-desc">${o.description}</span>`}
            ${o.preview && html`<pre class="tb-answer-preview" aria-label=${`Preview of ${o.label}`}>${o.preview}</pre>`}</span>
        </label>`)}
      </div>
    </div>`}
    ${words ? html`<div class="cron-create-row tb-new-row"><span class="cron-create-label">${options.length ? 'Your words' : 'Answer'}</span>
      <span class="tb-new-field">
        <${Textarea} domRef=${textRef} class="tb-answer-text" rows=${3} aria-label="Answer in your own words" value=${text} maxLength=${4000} disabled=${busy}
          placeholder=${source === 'notify' && options.length ? 'optional: a note with your pick' : options.length ? 'optional: with your pick, or instead of one' : 'your answer'} onInput=${(event) => setText(event.currentTarget.value)} />
        <span class="ui-hint tb-new-hint">${answerHint(source, askedBy, own, q.n)} · Ctrl/Cmd+Enter sends</span>
      </span>
    </div>` : html`<span class="ui-hint tb-new-hint">${answerHint(source, askedBy, own, q.n)}</span>`}
  <//>`;
}

// answerHint says where the answer goes.
function answerHint(source, askedBy, own, n) {
  const who = askedBy || 'the agent that asked';
  if (source === 'approval') return `Decides ${who}'s access request, as Messages → Access requests does`;
  if (source === 'notify') return `The answer goes to ${who} as your reply in Messages, and is kept under Q${n}`;
  return `The answer goes to ${who}${own ? ' and closes this task as done' : `, and is kept under Q${n} in the task's activity`}`;
}

// AnswerFor is the AnswerDialog wired to the board's actions: the write
// runs as the board's other writes do (busy, refusal shown, board re-read).
// n names the question (0: the task's first open one).
export function AnswerFor({ task, n = 0, actions, askedBy = '', onClose }) {
  if (!task || !actions) return null;
  const q = questionOf(task, n);
  if (!q) return null;
  const busyKey = `${task.id}:answer`;
  // By number; a node from before per-task questions sends none, and is
  // answered through the question task's own route.
  const which = q.n || 0;
  return html`<${AnswerDialog} key=${`${task.id}#${q.n || 0}`} task=${task} question=${q} askedBy=${askedBy} busy=${actions.busy === busyKey} api=${actions.api} onClose=${onClose}
    onSubmit=${({ picked, text }) => actions.run(busyKey, 'Answer', (api) => api.answer(task.id, picked, text, which))} />`;
}

// AnswerWith is the AnswerDialog on the board's api alone, where the edit
// form opens it: its own busy state, a refusal as a toast (the dialog stays
// open), and onAnswered after the write landed.
export function AnswerWith({ task, n = 0, api, askedBy = '', onClose, onAnswered = () => {} }) {
  const [busy, setBusy] = useState(false);
  if (!task || !api) return null;
  const q = questionOf(task, n);
  if (!q) return null;
  const which = q.n || 0;
  const submit = async ({ picked, text }) => {
    setBusy(true);
    try {
      await api.answer(task.id, picked, text, which);
      shellToast(`Answered ${task.id}${which ? ` Q${which}` : ''}`);
      onAnswered();
      return true;
    } catch (cause) {
      shellToast(`Answer failed: ${cause?.message || cause}`, true);
      return false;
    } finally {
      setBusy(false);
    }
  };
  return html`<${AnswerDialog} key=${`${task.id}#${q.n || 0}`} task=${task} question=${q} askedBy=${askedBy} busy=${busy} api=${api}
    onClose=${onClose} onSubmit=${submit} />`;
}

// TaskQuestions is the edit form's Questions: every question on the task,
// the open ones first with Answer… (onAnswer(n)), the rest with what became
// of them. The full exchange stays in the task's Activity.
export function TaskQuestions({ task, nameOf = null, onAnswer = null }) {
  const all = taskQuestions(task);
  if (!all.length) return null;
  const live = task.status === 'open' || task.status === 'blocked';
  const isOpen = (q) => live && isOpenQuestion(q);
  const ordered = [...all.filter(isOpen), ...all.filter((q) => !isOpen(q))];
  const who = (actor) => (actor ? (nameOf ? nameOf(actor) : actor) : '');
  return html`<ul class="tb-edit-questions">
    ${ordered.map((q) => {
      const open = isOpen(q);
      const mark = SOURCE_MARK[questionSource(q)];
      const asker = who(q.asked_by || task.assigned_by);
      const said = answerWords(q);
      return html`<li key=${q.n || 0} class=${`tb-edit-question${open ? ' tb-edit-question-open' : ''}`} data-edit-question=${q.n || 0}>
        <span class="tb-edit-question-main">
          <span class="tb-question-title">${mark.icon} ${q.n ? `Q${q.n} · ` : ''}${q.title || task.title}</span>
          <span class="tb-edit-question-meta">${questionKind(q)}${asker ? ` · asked by ${asker}` : ''}${
            open && q.waiting ? ' · ⏳ waiting' : ''}${
            !open ? ` · ${q.answer ? (q.state === 'closed' ? 'closed' : 'answered') : q.state || 'closed'}` : ''}</span>
          ${!open && said && html`<span class="tb-edit-question-answer">${said}</span>`}
        </span>
        ${open && onAnswer && html`<${Button} size="sm" variant="primary" class="tb-answer-open" onClick=${() => onAnswer(q.n || 0)}>Answer…<//>`}
      </li>`;
    })}
  </ul>`;
}

// answerTarget parses the board's answering state, "tk_…" or "tk_…#n".
export function answerTarget(key) {
  const [id, n] = String(key || '').split('#');
  return { id, n: Number(n) || 0 };
}

// canAnswer says whether the viewer is offered Answer on task: the
// operator's or an assigner's (the route refuses the agent that asked).
export function canAnswer(task, actions) {
  return questionOpen(task) && !!actions && typeof actions.api?.answer === 'function' && actions.api?.can?.assign !== false;
}
