// phone-task-page.js -- one task on a phone, pushed over the Board
// (phone-board.js): its title and state, its tags, any question to the
// operator with its options, the brief, its references, the activity with
// its comments, and a comment box. It reads; the writes a phone makes are
// answering, commenting and Move ▸. Editing the task itself is the
// desktop's task form, which a phone never opens: nothing here takes the
// focus on its own, so the keyboard stays down until the operator taps a
// field.
//
// The page is a history step (phone-page.js, view 'board'): ‹ and Back close
// it onto the board it was opened from.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { phonePage, usePhonePage } from './phone-page.js';
import { ButtonRow, Card, Section, StatePill } from './phone-ui.js';
import { Button } from './ui-components.js';
import { MarkdownDocument } from './markdown-document.js';
import { openQuestions } from './groups-taskboard-answer.js';
import { newCommentKey } from './groups-taskboard-comments.js';
import { relTime } from './helpers.js';
import { actorName, cardTags, taskStateLabel } from './phone-board-model.js';
import { shellToast } from './shell-state.js';
import { ChangeItem, QuestionItem, TaskIdLine, TaskMetaLine, historyEntries } from './phone-task-history.js';

const html = htm.bind(h);

// write runs one board write, tells the operator if it was refused, and
// re-reads the board; resolves true when it landed.
export async function boardWrite(source, run, failText = 'Could not change the task') {
  try {
    await run();
    return true;
  } catch (cause) {
    shellToast(`${failText}: ${cause?.message || cause}`, true);
    return false;
  } finally {
    void source?.refresh?.();
  }
}

// liveRow is the task as the board's poll has it now (its open tasks, then
// its finished ones), or null.
function liveRow(context, group, id) {
  const entry = (context?.groups || []).find((g) => g.group === group);
  const row = [...(entry?.tasks || []), ...(entry?.closed || [])].find((task) => task.id === id) || null;
  return { entry, row };
}

// useTaskRead reads the task in full (its brief) and its activity, again
// whenever `version` changes (after a write here).
function useTaskRead(api, id, version) {
  const [read, setRead] = useState({ task: null, activity: null, failed: '' });
  useEffect(() => {
    let live = true;
    Promise.all([api.task(id), api.activity(id)])
      .then(([task, activity]) => { if (live) setRead({ task, activity, failed: '' }); })
      .catch((cause) => { if (live) setRead((was) => ({ ...was, failed: cause?.message || String(cause) })); });
    return () => { live = false; };
  }, [api, id, version]);
  return read;
}

// QuestionBlock is an open question with its options, and an answer in words.
function QuestionBlock({ task, q, names, onAnswer }) {
  const [text, setText] = useState('');
  const [picked, setPicked] = useState([]);
  const options = q.options || [];
  const toggle = (key) => setPicked((was) => (was.includes(key) ? was.filter((k) => k !== key) : [...was, key]));
  return html`<${Card} accent="q" class="phone-task-q" data-question=${q.n}>
    <p class="phone-board-title">${q.title || task.title}</p>
    <p class="phone-board-asked">asked by ${actorName(q.asked_by || task.assigned_by, names) || 'an agent'}${options.length ? ` · ${options.length} options` : ''}</p>
    ${q.body && html`<${MarkdownDocument} source=${q.body} className="markdown-document phone-task-md" />`}
    ${options.length > 0 && html`<div class="phone-btns phone-board-options" role="group" aria-label=${`Answer: ${q.title || task.title}`}>
      ${options.map((option) => html`<${Button} key=${option.key}
        class=${`phone-btn${q.multi && picked.includes(option.key) ? ' phone-btn-pri' : ''}`}
        aria-pressed=${q.multi ? String(picked.includes(option.key)) : undefined}
        title=${option.description || (option.key === q.recommend ? 'Recommended' : undefined)}
        onClick=${() => (q.multi ? toggle(option.key) : onAnswer(q, [option.key], ''))}>${option.key === q.recommend && html`<span class="phone-board-star" aria-label="recommended">★ </span>`}${option.label || option.key}<//>`)}
    </div>`}
    <form class="phone-task-answer" onSubmit=${async (event) => {
      event.preventDefault();
      if (!text.trim() && !picked.length) return;
      if (await onAnswer(q, picked, text.trim())) { setText(''); setPicked([]); }
    }}>
      <input name="answer-in-words" class="ui-input phone-board-field" aria-label="Answer in words" placeholder=${options.length ? 'Or answer in words' : 'Your answer'}
        value=${text} onInput=${(event) => setText(event.currentTarget.value)} />
      <${Button} type="submit" class="phone-btn phone-btn-pri" disabled=${!text.trim() && !picked.length}>Answer<//>
    </form>
  <//>`;
}

export function TaskPage({ id, group, source, state, api, onClose, onMove }) {
  const [version, setVersion] = useState(0);
  const [comment, setComment] = useState('');
  const [sending, setSending] = useState(false);
  const commentKey = useRef(newCommentKey());
  const { entry, row } = liveRow(source.context.value, group, id);
  const read = useTaskRead(api, id, version);
  // The board's live row over the full read: the read has what a row leaves
  // out (who created it, when it was assigned).
  const task = row ? { ...(read.task || {}), ...row } : read.task;
  const names = companionAgents(state.snapshot.value);

  // The page over the board. When it goes, a tab page it covered (Mail's
  // reader under the Board) comes back.
  const covered = useRef(phonePage.peek());
  // The bar names the board; the page's own heading carries the title,
  // whole, however long.
  usePhonePage({ view: 'board', title: group, onBack: onClose, covered: covered.current && covered.current.view !== 'board' ? covered.current : null });
  useEffect(() => () => {
    const was = covered.current;
    if (!phonePage.peek() && was && was.view !== 'board') phonePage.value = was;
  }, []);

  if (!task) {
    return html`<div class="phone-task-page"><p class="ui-empty phone-empty">${read.failed ? `Could not read the task: ${read.failed}` : 'Loading…'}</p></div>`;
  }
  const write = async (run, failText) => {
    const ok = await boardWrite(source, run, failText);
    setVersion((v) => v + 1);
    return ok;
  };
  const answer = (q, picked, text) => write(() => api.answer(task.id, picked, text, q.n || 0), 'Could not answer');
  const state_ = taskStateLabel(task, entry);
  const live = task.status === 'open' || task.status === 'blocked';
  // A brief the read has no body for is empty, not still loading.
  const body = read.task ? (read.task.body || '') : (typeof task.body === 'string' ? task.body : null);
  const refs = task.refs || (task.ref ? [task.ref] : []);
  const all = read.activity?.entries || read.activity?.events || [];
  const entries = historyEntries(all);
  const nameOf = (actor) => actorName(actor, names);

  return html`<div class="phone-task-page">
    <h2 class="phone-task-page-title">${task.title}</h2>
    <${TaskIdLine} id=${task.id} />
    <${TaskMetaLine} task=${task} nameOf=${nameOf} entries=${all} />
    <div class="phone-board-tags">
      <${StatePill} tone=${state_.tone}>${state_.text}<//>
      ${cardTags(task, names).map((tag) => html`<span key=${tag.key} class=${`phone-board-tag${tag.tone ? ` phone-board-tag-${tag.tone}` : ''}`}>${tag.text}</span>`)}
    </div>
    ${task.status === 'blocked' && task.block_reason && html`<p class="phone-board-note">${task.block_reason}</p>`}
    ${openQuestions(task).map((q) => html`<${QuestionBlock} key=${q.n || 0} task=${task} q=${q} names=${names} onAnswer=${answer} />`)}
    ${live && html`<${ButtonRow} buttons=${[{ key: 'move', label: 'Move ▸', onClick: () => onMove(task, entry) }]} />`}

    <${Section} title="Brief">
      ${body === null
        ? html`<p class="ui-empty">${read.failed ? 'Could not read the brief.' : 'Loading…'}</p>`
        : body.trim()
          ? html`<${MarkdownDocument} source=${body} className="markdown-document phone-task-md" />`
          : html`<p class="ui-empty">No brief.</p>`}
    <//>

    ${refs.length > 0 && html`<${Section} title="Refs" count=${refs.length}>
      <ul class="phone-list phone-task-refs">
        ${refs.map((ref) => html`<li key=${ref.url || ref.label}>
          ${/^https?:\/\//i.test(ref.url || '')
            ? html`<a href=${ref.url} target="_blank" rel="noopener noreferrer">${ref.pr?.number ? `PR #${ref.pr.number}` : ref.label || ref.url}</a>`
            : html`<span class="phone-task-ref-label">${ref.label || ref.url || ''}</span>`}
          ${ref.pr?.title && html`<span class="phone-task-ref-sub">${ref.pr.title}${ref.pr.checks?.total ? ` · ${ref.pr.checks.passed}/${ref.pr.checks.total} checks` : ''}</span>`}
        </li>`)}
      </ul>
    <//>`}

    <${Section} title="Activity" count=${entries.length}>
      ${read.activity === null
        ? html`<p class="ui-empty">${read.failed ? 'Could not read the activity.' : 'Loading…'}</p>`
        : html`<ol class="phone-task-activity">
          ${entries.map((ev, i) => (ev.comment?.question
            ? html`<${QuestionItem} key=${`q-${ev.comment.id}`} entry=${ev} nameOf=${nameOf} />`
            : ev.comment
              ? html`<li key=${`c-${ev.comment.id}`} class="phone-task-comment">
                <span class="phone-task-who">${ev.actor_name || actorName(ev.actor, names)}</span>
                <span class="phone-task-when">${relTime(ev.at)}</span>
                <${MarkdownDocument} source=${ev.comment.body || ''} className="markdown-document phone-task-md" />
              </li>`
              : html`<${ChangeItem} key=${ev.id || `e-${i}`} entry=${ev} nameOf=${nameOf} />`))}
        </ol>`}
    <//>

    <form class="phone-task-comment-box" onSubmit=${async (event) => {
      event.preventDefault();
      if (!comment.trim() || sending) return;
      setSending(true);
      const ok = await write(() => api.comment(task.id, comment.trim(), commentKey.current), 'Could not comment');
      setSending(false);
      if (ok) { setComment(''); commentKey.current = newCommentKey(); }
    }}>
      <textarea name="comment" class="ui-input phone-board-field" rows="3" aria-label="Comment" placeholder="Write a comment"
        value=${comment} onInput=${(event) => setComment(event.currentTarget.value)}></textarea>
      <${Button} type="submit" class="phone-btn phone-btn-pri" disabled=${sending || !comment.trim()}>${sending ? 'Sending…' : 'Comment'}<//>
    </form>
  </div>`;
}
