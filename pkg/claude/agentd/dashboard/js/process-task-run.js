// process-task-run.js -- a process run for a board task (tk_8aa88514), as the
// task card and the run card show it: the task's run found in the process
// context, the one line of its progress (step, round, who it waits on) with a
// segment bar, and the line of a task that will follow a process when it is
// started. Processes are opt-in: a task with no run and no process to follow
// shows nothing.
import { h } from 'preact';
import htm from 'htm';
import { runName, runState, runSteps, currentRunNode, normalizeNodeState } from './process-kit.js';

const html = htm.bind(h);

// taskRunOf is the latest run in the process context that is for taskID, a
// live one before a finished one; null when the task has none.
export function taskRunOf(context, taskID) {
  if (!taskID) return null;
  let best = null;
  for (const [id, run] of Object.entries(context?.runs || {})) {
    if (run?.scope?.task !== taskID) continue;
    const candidate = { ...run, runId: run.runId ?? id };
    const live = !isFinished(candidate);
    if (!best) { best = candidate; continue; }
    const bestLive = !isFinished(best);
    if (live !== bestLive) { if (live) best = candidate; continue; }
    if (String(candidate.startedAt || '') > String(best.startedAt || '')) best = candidate;
  }
  return best;
}

function isFinished(run) {
  const status = String(run?.status || '').toLowerCase();
  return status === 'completed' || status === 'failed' || status === 'canceled';
}

// taskRunProgress is what a task card says of its run: the process's name,
// its state, the current step (1-based, of total) and its label, the round
// of a loop, who it waits on, and one segment state per step.
export function taskRunProgress(run, nameOf = () => '') {
  const steps = runSteps(run);
  const node = currentRunNode(run);
  const state = runState(run);
  const index = node ? Math.max(1, steps.findIndex((step) => step.id === node.id) + 1) : 0;
  let waitingOn = '';
  if (node && state !== 'done' && state !== 'failed') {
    if (String(node.performer?.kind || '').toLowerCase() === 'human') waitingOn = 'you';
    else if (node.dispatch) waitingOn = node.dispatch.name || nameOf(node.dispatch.agentId) || '';
  }
  return {
    name: runName(run),
    state,
    index,
    total: steps.length,
    label: node?.label || node?.id || '',
    round: node?.round || 0,
    rounds: node?.rounds || 0,
    waitingOn,
    segments: steps.map((step) => normalizeNodeState(step.state)),
  };
}

const STATE_WORDS = Object.freeze({ done: 'done', failed: 'failed', waiting: 'waiting', running: 'running', planned: 'starting' });

// TaskRunLine is a task card's line for its run: ◈ name · step N label
// (round r) · waiting on X, then a segment per step. Clicking it opens the
// run.
export function TaskRunLine({ run, nameOf, onOpen, compact = false }) {
  if (!run) return null;
  const p = taskRunProgress(run, nameOf);
  const finished = p.state === 'done' || p.state === 'failed';
  const parts = [];
  if (finished) parts.push(p.state === 'done' ? 'completed' : 'failed');
  else if (p.index) parts.push(`step ${p.index}/${p.total} ${p.label}${p.round > 1 ? ` (round ${p.round})` : ''}`);
  else parts.push(STATE_WORDS[p.state] || p.state);
  if (p.waitingOn) parts.push(p.waitingOn === 'you' ? 'waiting on you' : `waiting on ${p.waitingOn}`);
  const title = `${p.name}: ${parts.join(' · ')}. Open the run.`;
  return html`<button type="button" data-ui-exempt class=${`tk-run tk-run-${p.state}${compact ? ' tk-run-compact' : ''}`}
    data-task-run=${run.runId} title=${title}
    onClick=${(event) => { event.stopPropagation(); onOpen?.(run); }}>
    <span class="tk-run-line"><span class="tk-run-glyph" aria-hidden="true">◈</span> <b>${p.name}</b> · ${parts.join(' · ')}</span>
    ${p.segments.length > 0 && html`<span class="tk-run-bar" aria-hidden="true">${p.segments.map((state, i) =>
      html`<i key=${i} class=${`tk-seg tk-seg-${state}${i + 1 === p.index && !finished ? ' tk-seg-current' : ''}`}></i>`)}</span>`}
  </button>`;
}

// taskProcessFollowed is the process a task will follow when it is started:
// its own choice, else the board's default way of working; '' for none.
export function taskProcessFollowed(task, boardDefault = '') {
  const own = String(task?.process || '');
  if (own === 'none') return '';
  return own || String(boardDefault || '');
}

// WillFollowLine is the line of a task not started yet that will run a
// process when it is: ◈ will follow X when started · change · no process.
// change picks another; no process opts the task out. Nothing shows for a
// task that follows none. compact (a narrow lanes card) is one line, the
// sentence in its title, and × for no process.
export function WillFollowLine({ task, boardDefault = '', nameOf = (id) => id, onChange, onNone, compact = false }) {
  const process = taskProcessFollowed(task, boardDefault);
  if (!process) return null;
  if (compact) {
    const name = nameOf(process) || process;
    return html`<span class="tk-follow tk-follow-compact" data-task-follow=${process} title=${`Will follow ${name} when started: the process starts when the task is taken or started`}>
      <span class="tk-follow-text"><span class="tk-run-glyph" aria-hidden="true">◈</span> <span class="tk-follow-name">${name}</span></span>
      ${onNone && html`<button type="button" data-ui-exempt class="tk-link tk-follow-x" data-task-follow-none aria-label="No process for this task" title="No process for this task"
        onClick=${(event) => { event.stopPropagation(); onNone(task); }}>×</button>`}
    </span>`;
  }
  return html`<span class="tk-follow" data-task-follow=${process}>
    <span class="tk-run-glyph" aria-hidden="true">◈</span> will follow <b>${nameOf(process) || process}</b> when started
    ${onChange && html` · <button type="button" data-ui-exempt class="tk-link" data-task-follow-change
      onClick=${(event) => { event.stopPropagation(); onChange(task); }}>change</button>`}
    ${onNone && html` · <button type="button" data-ui-exempt class="tk-link" data-task-follow-none
      onClick=${(event) => { event.stopPropagation(); onNone(task); }}>no process</button>`}
  </span>`;
}
