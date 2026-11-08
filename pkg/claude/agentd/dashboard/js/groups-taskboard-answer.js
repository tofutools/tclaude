// groups-taskboard-answer.js -- question tasks (tk_aa049a67,
// docs/taskboard.md): an agent asks the operator with `taskboard
// ask-operator`, and the operator answers here. QuestionTag marks a question
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

// questionOpen reports whether task is a question still to answer.
export function questionOpen(task) {
  return !!task?.question && !task.question.answer && (task.status === 'open' || task.status === 'blocked');
}

// questionKind is the tag's words: how the question is answered.
export function questionKind(question) {
  const n = question?.options?.length || 0;
  if (!n) return 'answer in words';
  return `${question.multi ? 'pick any' : 'pick one'} · ${n}`;
}

// QuestionTag marks a question; waiting says its asker is polling for the
// answer right now (ask-operator --wait).
export function QuestionTag({ task, askedBy = '' }) {
  const q = task?.question;
  if (!q) return null;
  if (q.answer) return html`<span class="ui-tag tb-question-tag tb-question-answered" title="Answered">❓ answered</span>`;
  return html`<span class="ui-tag ui-tag-accent tb-question-tag" title="A question for the operator (taskboard ask-operator)">❓ ${questionKind(q)}</span>${
    task.waiting ? html`<span class="ui-tag ui-tag-warn tb-question-waiting" title="Its agent is waiting for the answer (ask-operator --wait)">⏳ ${askedBy || 'its agent'} is waiting</span>` : null}`;
}

// AnswerDialog answers task's question. onSubmit({ picked, text }) runs the
// write and resolves true when it landed.
export function AnswerDialog({ task, askedBy = '', busy = false, onSubmit, onClose }) {
  const q = task.question || {};
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
    title=${task.title || task.id} onClose=${onClose} onSubmitHotkey=${() => void submit()}
    busy=${busy} initialFocusRef=${options.length ? null : textRef} cancel="Cancel"
    footer=${html`<${Button} variant="primary" id="taskboard-answer-submit" busy=${busy}
      disabled=${busy || empty} onClick=${() => { void submit(); }}>${busy ? 'Sending…' : 'Send answer'}<//>`}>
    <div class="modal-meta tb-answer-meta">${task.id}${askedBy ? ` · asked by ${askedBy}` : ''}${task.waiting ? ' · waiting for the answer' : ''}</div>
    ${task.body && html`<div class="tb-answer-context">${task.body}</div>`}
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
        <span class="ui-hint tb-new-hint">The answer goes to ${askedBy || 'the agent that asked'} and closes this task as done · Ctrl/Cmd+Enter sends</span>
      </span>
    </div>
  <//>`;
}

// AnswerFor is the AnswerDialog wired to the board's actions: the write
// runs as the board's other writes do (busy, refusal shown, board re-read).
export function AnswerFor({ task, actions, askedBy = '', onClose }) {
  if (!task || !actions) return null;
  return html`<${AnswerDialog} key=${task.id} task=${task} askedBy=${askedBy} busy=${actions.busy === `${task.id}:answer`} onClose=${onClose}
    onSubmit=${({ picked, text }) => actions.run(`${task.id}:answer`, 'Answer', (api) => api.answer(task.id, picked, text))} />`;
}

// canAnswer says whether the viewer is offered Answer on task: the
// operator's or an assigner's (the route refuses the agent that asked).
export function canAnswer(task, actions) {
  return questionOpen(task) && !!actions && typeof actions.api?.answer === 'function' && actions.api?.can?.assign !== false;
}
