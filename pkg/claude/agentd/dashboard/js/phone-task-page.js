// phone-task-page.js -- one task on a phone, pushed over the Board
// (phone-board.js): its title and state, its tags, any question to the
// operator with its options, the brief, its references (phone-task-links.js,
// with its epic, the tasks it comes after and its run), who worked on it
// (each a tap from their terminal), its files, the activity with its
// comments, and a comment box (phone-task-parts.js has the people, files and
// comment parts). The brief draws the images it names. It reads; the writes a phone makes are
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
import { ButtonRow, Card, Empty, ErrorNote, errorText, Section, StatePill } from './phone-ui.js';
import { Button } from './ui-components.js';
import { MarkdownDocument } from './markdown-document.js';
import { openQuestions } from './groups-taskboard-answer.js';
import { newCommentKey } from './groups-taskboard-comments.js';
import { relTime } from './helpers.js';
import { actorName, cardTags, taskStateLabel } from './phone-board-model.js';
import { shellToast } from './shell-state.js';
import { ChangeItem, QuestionItem, TaskIdLine, TaskMetaLine, historyEntries } from './phone-task-history.js';
import { TaskAfter, TaskEpic, TaskRefs, TaskRun } from './phone-task-links.js';
import { CommentBody, CommentHead, FilesSection, PeopleSection, PersonName, TaskBrief } from './phone-task-parts.js';

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

export function TaskPage({ id, group, source, state, api, actions = null, onClose, onMove, onOpenTask = () => {} }) {
  const [version, setVersion] = useState(0);
  const [comment, setComment] = useState('');
  const [sending, setSending] = useState(false);
  const commentKey = useRef(newCommentKey());
  const { entry, row } = liveRow(source.context.value, group, id);
  const read = useTaskRead(api, id, version);
  const task = row || read.task;
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
    return html`<div class="phone-task-page">${read.failed
      ? html`<${ErrorNote} onRetry=${() => setVersion((v) => v + 1)}>${errorText('the task', read.failed)}<//>`
      : html`<${Empty}>Loading…<//>`}</div>`;
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
  const all = read.activity?.entries || read.activity?.events || [];
  const entries = historyEntries(all);
  const nameOf = (actor) => actorName(actor, names);

  return html`<div class="phone-task-page">
    <h2 class="phone-task-page-title">${task.title}</h2>
    <${TaskIdLine} id=${task.id} />
    <${TaskMetaLine} task=${task} nameOf=${nameOf} />
    <div class="phone-board-tags">
      <${StatePill} tone=${state_.tone}>${state_.text}<//>
      ${cardTags(task, names).map((tag) => html`<span key=${tag.key} class=${`phone-board-tag${tag.tone ? ` phone-board-tag-${tag.tone}` : ''}`}>${tag.text}</span>`)}
    </div>
    <${TaskRun} task=${task} nameOf=${(agent) => names.get?.(agent)?.name || ''} />
    ${task.status === 'blocked' && task.block_reason && html`<p class="phone-board-note">${task.block_reason}</p>`}
    ${openQuestions(task).map((q) => html`<${QuestionBlock} key=${q.n || 0} task=${task} q=${q} names=${names} onAnswer=${answer} />`)}
    ${live && html`<${ButtonRow} buttons=${[{ key: 'move', label: 'Move ▸', onClick: () => onMove(task, entry) }]} />`}

    <${Section} title="Brief">
      ${body === null
        ? (read.failed ? html`<${ErrorNote}>${errorText('the brief', read.failed)}<//>` : html`<${Empty}>Loading…<//>`)
        : body.trim()
          ? html`<${TaskBrief} api=${api} task=${task} body=${body} />`
          : html`<${Empty}>No brief.<//>`}
    <//>

    <${TaskRefs} task=${task} />
    <${TaskEpic} task=${task} entry=${entry} full=${read.task} failed=${read.failed} onOpen=${onOpenTask} />
    <${TaskAfter} task=${task} entry=${entry} onOpen=${onOpenTask} />

    <${PeopleSection} task=${task} entries=${entries} names=${names} actions=${actions} />

    <${FilesSection} api=${api} task=${task} />

    <${Section} title="Activity" count=${entries.length}>
      ${read.activity === null
        ? (read.failed ? html`<${ErrorNote} onRetry=${() => setVersion((v) => v + 1)}>${errorText('the activity', read.failed)}<//>` : html`<${Empty}>Loading…<//>`)
        : html`<ol class="phone-task-activity">
          ${entries.map((ev, i) => (ev.comment?.question
            ? html`<${QuestionItem} key=${`q-${ev.comment.id}`} entry=${ev} nameOf=${nameOf} />`
            : ev.comment
              ? html`<li key=${`c-${ev.comment.id}`} class="phone-task-comment">
                <${PersonName} actor=${ev.comment.author} shown=${ev.comment.author_name} names=${names} actions=${actions} />
                <${CommentHead} comment=${ev.comment} />
                <span class="phone-task-when">${relTime(ev.at)}</span>
                <${CommentBody} api=${api} taskId=${task.id} comment=${ev.comment} />
              </li>`
              : html`<${ChangeItem} key=${ev.id || `e-${i}`} entry=${ev} nameOf=${nameOf}
                  who=${html`<${PersonName} actor=${ev.actor} shown=${ev.actor_name} names=${names} actions=${actions} />`} />`))}
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
