// phone-task-links.js -- the sections of a phone task page (phone-task-page.js)
// that point away from the task: its refs (a PR's state, CI and reviews; a
// tracker issue's status), the tasks it comes after, its epic (or, for an
// epic, its progress and tasks), and its process run. Each reads what the
// desktop's task view reads (groups-taskboard-card.js, -deps, -epics,
// process-task-run.js) and shows it as tappable phone rows.

import { h } from 'preact';
import htm from 'htm';
import { Card, Empty, ErrorNote, errorText, Section, StatePill } from './phone-ui.js';
import { prOfRef, refKey, refKindName, refsOf } from './groups-taskboard-card.js';
import { EpicProgress, childWhere, isEpic, progressParts } from './groups-taskboard-epics.js';
import { prReviewDecisionText } from './pr-review-model.js';
import { incompleteChecksText, isGitLabMRURL } from './pr-checks-hover.js';
import { prRef } from './pr-chain.js';
import { ciState } from './phone-agents-model.js';
import { taskStateLabel } from './phone-board-model.js';
import { processContext } from './process-context.js';
import { TaskRunLine, taskRunOf } from './process-task-run.js';

const html = htm.bind(h);

const httpURL = (url) => (/^https?:\/\//i.test(url || '') ? url : '');

// PR_TONE and CI_TONE colour a PR's state and its checks as StatePills.
const PR_TONE = Object.freeze({ open: 'work', draft: 'idle', merged: 'ask', closed: 'off' });
const CI_TONE = Object.freeze({ passing: 'work', failing: 'err', running: 'key', incomplete: 'idle' });
const CI_MARK = Object.freeze({ passing: '✓', failing: '✗', running: '…', incomplete: '?' });
const REVIEW_WORD = Object.freeze({ approved: 'approved', changes_requested: 'changes requested', review_required: 'review required' });
const REVIEW_TONE = Object.freeze({ approved: 'work', changes_requested: 'err', review_required: 'key' });

// refRow is one ref as a phone row: a PR with its state, CI and review
// decision; a tracker issue with its tracker, status and title; else a link.
// url is '' when the ref is not an http(s) link.
export function refRow(ref) {
  const url = httpURL(ref?.url);
  const pr = prOfRef(ref);
  if (pr) {
    const ci = ciState(pr.checks);
    const decision = pr.reviews?.decision || '';
    return {
      key: refKey(ref), kind: 'pr', url,
      label: `${isGitLabMRURL(pr.url) ? 'MR' : 'PR'} ${prRef(pr) || ''}`.trim(),
      title: pr.title || '',
      state: pr.state || '', stateTone: PR_TONE[pr.state] || 'idle',
      ci, ciText: ci ? `${CI_MARK[ci]} ${pr.checks?.total ? `${pr.checks.passed || 0}/${pr.checks.total}${pr.checks.incomplete ? '+' : ''}` : ci}` : '', ciTone: CI_TONE[ci] || 'idle',
      ciTitle: incompleteChecksText(pr.checks) || (ci ? `checks ${ci}` : ''),
      review: REVIEW_WORD[decision] || '', reviewTone: REVIEW_TONE[decision] || 'idle',
      reviewTitle: decision ? prReviewDecisionText(decision, isGitLabMRURL(pr.url)) : '',
    };
  }
  const tracker = ref?.tracker;
  const kindName = refKindName(ref?.kind);
  return {
    key: refKey(ref), kind: tracker || kindName ? 'issue' : 'link', url,
    label: ref?.label || ref?.url || ref?.kind || '',
    title: tracker?.title || '',
    kindName,
    state: tracker?.status || '', stateTone: /review/i.test(tracker?.status || '') ? 'ask' : /progress|started|doing|active/i.test(tracker?.status || '') ? 'key' : 'idle',
    note: url ? '' : (ref?.link_note ? `not linked: ${ref.link_note}` : 'not linked'),
  };
}

// boardTasks are the tasks the board knows: open, then recently closed.
const boardTasks = (entry) => [...(entry?.tasks || []), ...(entry?.closed || [])];

// depRows are the tasks this one comes after, each with where it stands. The
// board lists only its latest closed tasks, so a dependency it does not list
// shows its id: done when the task no longer waits on it (`unmet`), else not
// on the board. Every row opens: the task's own read finds it either way.
export function depRows(task, entry) {
  const known = boardTasks(entry);
  const unmet = new Set(task?.unmet || []);
  return (task?.depends_on || []).map((id) => {
    const dep = known.find((t) => t.id === id);
    if (dep) return { id, title: dep.title || id, state: taskStateLabel(dep, entry) };
    return { id, title: id, state: unmet.has(id) ? { text: 'not on the board', tone: 'off' } : { text: 'done', tone: 'idle' } };
  });
}

// epicOfTask is the epic a task is in, as the board lists it, or null; an
// epic it names that the board no longer shows is { id, title: id, gone }.
export function epicOfTask(task, entry) {
  const id = task?.parent_id;
  if (!id) return null;
  const epic = boardTasks(entry).find((t) => t.id === id);
  return epic ? { id, title: epic.title || id, progress: epic.progress || null } : { id, title: id, progress: null, gone: true };
}

const WHERE = Object.freeze({ idea: ['idea', 'idle'], next: ['next', 'idle'], progress: ['in progress', 'work'], column: ['in progress', 'work'], blocked: ['blocked', 'err'], done: ['done', 'idle'], dropped: ['dropped', 'off'] });

// childRows are an epic's tasks (the full read's children), each with where
// it stands.
export function childRows(children = []) {
  return children.map((child) => {
    const [text, tone] = WHERE[childWhere(child)] || ['', 'idle'];
    return { id: child.id, title: child.title || child.id, state: { text, tone } };
  });
}

// openPhoneRun opens a task's run in Automations' Runs (phone-process-runs.js).
export async function openPhoneRun(run) {
  const { openRunLink } = await import('./phone-process-runs.js');
  const { PROCESSES_SUBTAB, openAutomationsEntry } = await import('./automations-subnav.js');
  openRunLink({ selection: true, item: run.runId }, { open: () => openAutomationsEntry(PROCESSES_SUBTAB) });
}

// TaskRow is one tappable row: a title and a pill; `onTap` null is a row
// that does not open.
function TaskRow({ row, onTap, attr }) {
  const inner = html`<span class="phone-row-text"><span class="phone-task-row-title">${row.title}</span></span>
    ${row.state?.text && html`<${StatePill} tone=${row.state.tone}>${row.state.text}<//>`}`;
  return html`<li ...${attr}>${onTap
    ? html`<button data-ui-exempt type="button" class="phone-row" onClick=${onTap}>${inner}<span class="phone-row-chev" aria-hidden="true">›</span></button>`
    : html`<span class="phone-row">${inner}</span>`}</li>`;
}

// TaskRefs is the Refs section: one row per ref, a link where it has one.
export function TaskRefs({ task }) {
  const rows = refsOf(task).map(refRow);
  if (!rows.length) return null;
  return html`<${Section} title="Refs" count=${rows.length}>
    <${Card} class="phone-rows-card"><ul class="phone-rows phone-task-refs">
      ${rows.map((row) => {
        const head = html`<span class="phone-row-text">
            <span class="phone-task-ref-label">${row.kindName && html`<span class="phone-task-ref-kind">${row.kindName}</span> `}${row.label}</span>
            ${(row.title || row.note) && html`<span class="phone-task-ref-sub">${row.title || row.note}</span>`}
          </span>
          <span class="phone-task-ref-pills">
            ${row.state && html`<${StatePill} tone=${row.stateTone}>${row.state}<//>`}
            ${row.ciText && html`<${StatePill} tone=${row.ciTone} title=${row.ciTitle}>${row.ciText}<//>`}
            ${row.review && html`<${StatePill} tone=${row.reviewTone} title=${row.reviewTitle}>${row.review}<//>`}
          </span>`;
        return html`<li key=${row.key} data-task-ref=${row.kind}>${row.url
          ? html`<a class="phone-row phone-task-ref" href=${row.url} target="_blank" rel="noopener noreferrer">${head}</a>`
          : html`<span class="phone-row phone-task-ref">${head}</span>`}</li>`;
      })}
    </ul><//>
  <//>`;
}

// TaskAfter is the After section: the tasks this one waits on; a tap opens
// one the board lists.
export function TaskAfter({ task, entry, onOpen }) {
  const rows = depRows(task, entry);
  if (!rows.length) return null;
  return html`<${Section} title="After" count=${rows.length}>
    <${Card} class="phone-rows-card"><ul class="phone-rows phone-task-deps">
      ${rows.map((row) => html`<${TaskRow} key=${row.id} row=${row} attr=${{ 'data-task-dep': row.id }}
        onTap=${() => onOpen(row.id)} />`)}
    </ul><//>
  <//>`;
}

// TaskEpic is the Epic section: for a task in one, the epic (a tap opens
// it) with its progress; for an epic, its progress and its tasks, read with
// the page's full read (`full`, which carries `children`).
export function TaskEpic({ task, entry, full, failed = '', onOpen }) {
  if (isEpic(task)) {
    const progress = full?.progress || task.progress || {};
    const children = full ? childRows(full.children || []) : null;
    return html`<${Section} title="Epic" count=${progress.total ?? null}>
      <div class="phone-task-epic-progress" data-task-epic-progress=${task.id}>
        <${EpicProgress} epic=${{ progress }} large=${true} />
        <span class="phone-task-ref-sub">${progressParts(progress).join(' · ')}</span>
      </div>
      ${children === null ? (failed ? html`<${ErrorNote}>${errorText('its tasks', failed)}<//>` : html`<${Empty}>Loading…<//>`)
        : !children.length ? html`<${Empty}>No tasks in it yet.<//>`
        : html`<${Card} class="phone-rows-card"><ul class="phone-rows phone-task-children">
          ${children.map((row) => html`<${TaskRow} key=${row.id} row=${row} attr=${{ 'data-task-child': row.id }} onTap=${() => onOpen(row.id)} />`)}
        </ul><//>`}
    <//>`;
  }
  const epic = epicOfTask(task, entry);
  if (!epic) return null;
  const row = { title: epic.title, state: epic.gone ? { text: 'closed', tone: 'off' } : epic.progress ? { text: `${epic.progress.done || 0}/${epic.progress.total || 0} done`, tone: 'idle' } : null };
  return html`<${Section} title="Epic">
    <${Card} class="phone-rows-card"><ul class="phone-rows phone-task-parent">
      <${TaskRow} row=${row} attr=${{ 'data-task-epic': epic.id }} onTap=${epic.gone ? null : () => onOpen(epic.id)} />
    </ul><//>
  <//>`;
}

// TaskRun is the task's process run line, as on its board card; a tap opens
// the run. `context` is the process read model (the board keeps it fresh).
export function TaskRun({ task, nameOf, context = processContext.context.value }) {
  const run = taskRunOf(context, task?.id);
  if (!run) return null;
  return html`<div class="phone-task-run"><${TaskRunLine} run=${run} nameOf=${nameOf} onOpen=${(picked) => { void openPhoneRun(picked); }} /></div>`;
}
