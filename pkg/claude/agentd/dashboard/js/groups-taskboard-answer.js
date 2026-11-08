// groups-taskboard-answer.js -- questions for the operator (tk_aa049a67,
// tk_8b2c86f7, docs/taskboard.md): an agent asks the operator with `taskboard
// ask-operator`, on a task (an entry of its activity) or on a task made for
// the question, and the operator answers here. QuestionCard is an open
// question on another task, in the operator's lane. QuestionTag marks a question
// on a card or row ("❓ pick one · 3"); AnswerDialog answers it: the options
// as radios (or checkboxes for a multi-select question) with their
// descriptions, the recommended one marked, a preview pane that follows the
// focused option, and free text, always available, with a pick or instead
// of one. Send answer is `taskboard answer`: the task closes as done and the
// agent that asked gets the answer.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Dialog } from './ui-dialog.js';
import { Button, Textarea } from './ui-components.js';

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
  return html`<div class="ui-card tb-card tb-question-card" data-question=${`${task.id}#${q.n}`}>
    <div class="tb-question-on">❓ ${askedBy || 'an agent'} asks on${' '}
      ${onOpen ? html`<button type="button" class="tb-question-task" title="Open the task" onClick=${(event) => { event.stopPropagation(); onOpen(task.id); }}>${task.title}</button>`
        : html`<span class="tb-question-task">${task.title}</span>`}</div>
    <div class="tb-question-title">${q.title || task.title}</div>
    <div class="tb-card-meta"><span class="ui-tag ui-tag-accent tb-question-tag">❓ Q${q.n} · ${questionKind(q)}</span>${
      q.waiting ? html`<span class="ui-tag ui-tag-warn tb-question-waiting" title="Its agent is waiting for the answer (ask-operator --wait)">⏳ waiting</span>` : null}${
      q.blocks && task.status === 'blocked' ? html`<span class="ui-tag ui-tag-warn" title="Asking blocked the task until the answer">blocks the task</span>` : null}</div>
    ${onAnswer && html`<div class="tb-card-answer"><${Button} size="sm" variant="primary" class="tb-answer-open"
      onClick=${(event) => { event.stopPropagation(); onAnswer(task.id, q.n); }}>Answer…<//></div>`}
  </div>`;
}

// AnswerDialog answers question (default: task's first open question).
// onSubmit({ picked, text }) runs the write and resolves true when it
// landed.
export function AnswerDialog({ task, question = null, askedBy = '', busy = false, onSubmit, onClose }) {
  const q = question || questionOf(task) || {};
  // A question task's question is the task: its brief is the context.
  const own = !!task.question && (q.n || 1) === (task.question.n || 1);
  const context = own ? task.body : q.body;
  const options = q.options || [];
  const [picked, setPicked] = useState([]);
  const [text, setText] = useState('');
  const [focus, setFocus] = useState(q.recommend || options[0]?.key || '');
  const textRef = useRef(null);
  const sending = useRef(false);
  const empty = !picked.length && !text.trim();
  const toggle = (key) => {
    setFocus(key);
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
  const shown = options.find((o) => o.key === focus);
  const previews = options.some((o) => o.preview);
  return html`<${Dialog} id="taskboard-answer-modal" dialogClass="cron-create-modal taskboard-answer-modal"
    title=${q.title || task.title || task.id} onClose=${onClose} onSubmitHotkey=${() => void submit()}
    busy=${busy} initialFocusRef=${options.length ? null : textRef} cancel="Cancel"
    footer=${html`<${Button} variant="primary" id="taskboard-answer-submit" busy=${busy}
      disabled=${busy || empty} onClick=${() => { void submit(); }}>${busy ? 'Sending…' : 'Send answer'}<//>`}>
    <div class="modal-meta tb-answer-meta">${task.id}${q.n ? ` · Q${q.n}` : ''}${!own ? ` on “${task.title}”` : ''}${askedBy ? ` · asked by ${askedBy}` : ''}${(q.waiting ?? task.waiting) ? ' · waiting for the answer' : ''}</div>
    ${context && html`<div class="tb-answer-context">${context}</div>`}
    ${options.length > 0 && html`<div class=${`tb-answer-options${previews ? ' tb-answer-has-preview' : ''}`}>
      <div class="tb-answer-list" role=${q.multi ? 'group' : 'radiogroup'} aria-label="Options">
        ${options.map((o) => html`<label key=${o.key} class=${`tb-answer-opt${picked.includes(o.key) ? ' tb-answer-on' : ''}`} data-answer-option=${o.key}
            onMouseEnter=${() => setFocus(o.key)}>
          <input type=${q.multi ? 'checkbox' : 'radio'} name="tb-answer" value=${o.key} checked=${picked.includes(o.key)} disabled=${busy}
            onFocus=${() => setFocus(o.key)} onChange=${() => toggle(o.key)} />
          <span><span class="tb-answer-label">${o.label}${o.key === q.recommend && html`<span class="ui-tag tb-answer-rec">recommended</span>`}</span>
            ${o.description && html`<span class="tb-answer-desc">${o.description}</span>`}</span>
        </label>`)}
      </div>
      ${previews && html`<div class="tb-answer-preview" aria-label="Preview">${shown?.preview || ''}</div>`}
    </div>`}
    <div class="cron-create-row tb-new-row"><span class="cron-create-label">${options.length ? 'Your words' : 'Answer'}</span>
      <span class="tb-new-field">
        <${Textarea} domRef=${textRef} class="tb-answer-text" rows=${3} aria-label="Answer in your own words" value=${text} maxLength=${4000} disabled=${busy}
          placeholder=${options.length ? 'optional: with your pick, or instead of one' : 'your answer'} onInput=${(event) => setText(event.currentTarget.value)} />
        <span class="ui-hint tb-new-hint">The answer goes to ${askedBy || 'the agent that asked'}${own ? ' and closes this task as done' : `, and is kept under Q${q.n} in the task's activity`} · Ctrl/Cmd+Enter sends</span>
      </span>
    </div>
  <//>`;
}

// AnswerFor is the AnswerDialog wired to the board's actions: the write
// runs as the board's other writes do (busy, refusal shown, board re-read).
// n names the question (0: the task's first open one).
export function AnswerFor({ task, n = 0, actions, askedBy = '', onClose }) {
  if (!task || !actions) return null;
  const q = questionOf(task, n);
  if (!q) return null;
  const busyKey = `${task.id}:answer`;
  // A question task's own question goes by its own route, which a node
  // from before per-task questions serves too.
  const which = task.question && (q.n || 1) === (task.question.n || 1) ? 0 : q.n;
  return html`<${AnswerDialog} key=${`${task.id}#${q.n || 0}`} task=${task} question=${q} askedBy=${askedBy} busy=${actions.busy === busyKey} onClose=${onClose}
    onSubmit=${({ picked, text }) => actions.run(busyKey, 'Answer', (api) => api.answer(task.id, picked, text, which))} />`;
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
