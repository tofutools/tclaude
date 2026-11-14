// phone-process-runs.js -- process runs on a phone (data-layout="phone"):
// the Runs list Automations shows in place of the Processes templates, the
// run page pushed over it, and the gate cards Now shows for every run that
// waits on the operator.
//
// A phone reads and decides runs; it does not draw them or edit templates.
// The steps are a list rather than the desktop graph, and a gate's form is
// the desktop run inspector's DecisionForm, posted through the same run API,
// so verdicts, notes and the stale-attempt guard are the desktop's.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import {
  formatRunDuration, processContext, runDetailApi, withPerformerNames,
} from './process-context.js';
import { ProcessStepStrip, runName, runPauseState, runSteps, stateLabel } from './process-kit.js';
import { DecisionForm, useRunDetail } from './groups-run-focus.js';
import { usePhonePage } from './phone-page.js';
import { relTime } from './helpers.js';

const html = htm.bind(h);

// How many finished runs the list keeps under the live ones.
export const DONE_SHOWN = 10;

// phoneRuns sorts every group's runs into what waits on the operator, what is
// running and what finished, each run once, with the group it ran in. A run
// waiting on the operator is listed there only.
export function phoneRuns(context) {
  const runs = context?.runs || {};
  const seen = new Set();
  const pick = (ids, group) => (ids || []).filter((id) => {
    if (seen.has(id) || !runs[id]) return false;
    seen.add(id);
    return true;
  }).map((id) => ({ run: { ...runs[id], runId: runs[id].runId ?? id }, group }));
  const groups = context?.groups || [];
  const waiting = groups.flatMap((entry) => pick(entry.needsYou, entry.group || ''));
  const running = groups.flatMap((entry) => pick(entry.active, entry.group || ''));
  const done = groups.flatMap((entry) => pick(entry.done, entry.group || ''))
    .sort((a, b) => String(b.run.updatedAt || '').localeCompare(String(a.run.updatedAt || '')))
    .slice(0, DONE_SHOWN);
  return { waiting, running, done };
}

// runPosition is where a run stands, in words: the step it waits or works on
// and its place in the run, or how it ended.
export function runPosition(run) {
  const steps = runSteps(run);
  const waiting = steps.findIndex((step) => step.state === 'waiting');
  const at = waiting >= 0 ? waiting : steps.findIndex((step) => step.state === 'running' || step.state === 'failed');
  if (at >= 0) return `step ${at + 1}/${steps.length} · ${steps[at].label}`;
  const status = String(run?.status || '').toLowerCase();
  if (status && status !== 'running') {
    const when = relTime(run.updatedAt);
    return `${status}${when ? ` ${when}` : ''}`;
  }
  return steps.length ? `${steps.length} steps` : '';
}

// useProcessRuns keeps the shared process context fresh while a phone view
// shows it: once on mount, then on every snapshot poll. The source shares one
// read in flight, so views showing it at once cost one request.
export function useProcessRuns(source = processContext) {
  useEffect(() => {
    void source.refresh();
    const poll = () => void source.refresh();
    document.addEventListener('tclaude:snapshot', poll);
    return () => document.removeEventListener('tclaude:snapshot', poll);
  }, [source]);
  return { context: source.context.value, status: source.status.value, error: source.error.value };
}

function RunRow({ item, waiting, onOpen }) {
  const { run, group } = item;
  const paused = !!run.paused;
  const tag = waiting ? html`<span class="ui-tag ui-tag-accent">Decide</span>`
    : paused ? html`<span class="ui-tag">Paused</span>` : null;
  return html`<li class=${`phone-run-row${waiting ? ' waiting' : ''}`} data-run=${run.runId}>
    <button data-ui-exempt type="button" class="phone-run-open" onClick=${() => onOpen(run.runId)}>
      <span class="phone-run-text">
        <span class="phone-run-name">${runName(run)}</span>
        <span class="phone-run-sub">${[group, runPosition(run)].filter(Boolean).join(' · ')}</span>
        <${ProcessStepStrip} run=${run} />
      </span>
      ${tag}
    </button>
  </li>`;
}

function RunSection({ title, items, waiting = false, onOpen }) {
  if (!items.length) return null;
  return html`<div class="phone-run-section" role="region" aria-label=${title}>
    <h2 class="ui-eyebrow phone-section-title">${title}</h2>
    <ul class="phone-list">${items.map((item) => html`<${RunRow} key=${item.run.runId} item=${item} waiting=${waiting} onOpen=${onOpen} />`)}</ul>
  </div>`;
}

// RunsList is the Runs view: every group's runs, waiting on you first.
export function RunsList({ source = processContext, onOpen, desktopSite }) {
  const { context, status, error } = useProcessRuns(source);
  let body;
  if (status === 'unavailable') body = html`<p class="ui-empty phone-empty">Processes are not available on this node.</p>`;
  else if (!context) {
    body = html`<p class="ui-empty phone-empty">${status === 'error' ? `Could not read the runs: ${error}` : 'Loading…'}</p>`;
  } else {
    const { waiting, running, done } = phoneRuns(context);
    body = waiting.length + running.length + done.length === 0
      ? html`<p class="ui-empty phone-empty">No process runs.</p>`
      : html`<${RunSection} title="Waiting on you" items=${waiting} waiting onOpen=${onOpen} />
        <${RunSection} title="Running" items=${running} onOpen=${onOpen} />
        <${RunSection} title="Done" items=${done} onOpen=${onOpen} />`;
  }
  return html`<div class="phone-runs">
    ${body}
    <${DesktopOnly} what="Process templates and the editor are" desktopSite=${desktopSite} />
  </div>`;
}

// DesktopOnly says what a phone leaves to the desktop, with the way there.
export function DesktopOnly({ what, desktopSite }) {
  return html`<p class="phone-desktop-only">${what} desktop only.${desktopSite && html` <button data-ui-exempt type="button"
    class="phone-link" onClick=${desktopSite}>Desktop site ›</button>`}</p>`;
}

// Gates is a run's waiting decisions, each with its verdict form: what Now
// shows per run and what tops the run page.
function Gates({ run, detail, onDecide }) {
  const decisions = detail.view?.awaitingDecisions || [];
  if (!decisions.length) return null;
  const nodes = new Map(runSteps(run).map((node) => [String(node.id), node]));
  return decisions.map((decision) => {
    const node = nodes.get(String(decision.nodeId)) || { id: String(decision.nodeId), label: String(decision.nodeId) };
    return html`<div class="phone-gate" key=${`${node.id}:${decision.attempt || 0}`}>
      <div class="phone-gate-title">${node.label}</div>
      ${node.description && html`<p class="phone-gate-prompt">${node.description}</p>`}
      <${DecisionForm} key=${`${node.id}:${decision.attempt || 0}`} decision=${decision} node=${node} onDecide=${onDecide} />
    </div>`;
  });
}

function decider(api, runId, refresh, source) {
  return async (request) => {
    await api.decide(runId, request);
    refresh();
    await source?.refresh?.();
  };
}

// GateCard is one run waiting on the operator, on Now: which run, where, and
// the form to answer it.
export function GateCard({ item, api = runDetailApi, source = processContext, onOpen = null }) {
  const { run, group } = item;
  const [detail, refresh] = useRunDetail(api, run.runId);
  return html`<div class="phone-card phone-gate-card ui-card" data-run=${run.runId}>
    <div class="phone-gate-head">
      <span class="phone-run-name">${runName(run)}</span>
      <span class="phone-run-sub">${group}</span>
      ${onOpen && html`<button data-ui-exempt type="button" class="phone-link" onClick=${() => onOpen(run.runId)}>Run ›</button>`}
    </div>
    ${detail.view
      ? html`<${Gates} run=${run} detail=${detail} onDecide=${decider(api, run.runId, refresh, source)} />`
      : html`<p class="ui-empty">${detail.error ? `Could not read the run: ${detail.error}` : 'Loading…'}</p>`}
  </div>`;
}

// gateItems are the runs Now lists: those waiting on the operator.
export function gateItems(context) {
  return phoneRuns(context).waiting;
}

// RunPage is one run, pushed over the Runs list: its gates on top, then its
// steps in order with who performs them and how long they took.
export function RunPage({ runId, back, active = true, api = runDetailApi, source = processContext, nameOf = () => '' }) {
  const { context } = useProcessRuns(source);
  const raw = context?.runs?.[runId];
  const group = (context?.groups || []).find((entry) => [entry.needsYou, entry.active, entry.done].some((ids) => (ids || []).includes(runId)))?.group || '';
  const run = raw ? withPerformerNames({ ...raw, runId: raw.runId ?? runId }, nameOf) : null;
  const [detail, refresh] = useRunDetail(api, runId);
  const [busy, setBusy] = useState(false);
  const pause = run ? runPauseState(run) : { ok: false };
  const hold = async () => {
    if (busy) return;
    setBusy(true);
    try {
      await (pause.paused ? api.resume(runId) : api.pause(runId));
      refresh();
      await source.refresh();
    } finally {
      setBusy(false);
    }
  };
  usePhonePage({
    title: run ? runName(run) : 'Run',
    onBack: back,
    actions: run && pause.ok ? [{
      key: 'hold', icon: pause.paused ? 'Resume' : 'Pause', label: pause.why, disabled: busy, onClick: hold,
    }] : [],
  }, active);
  if (!run) {
    return html`<div class="phone-run-page"><p class="ui-empty phone-empty">${context ? 'This run is no longer listed.' : 'Loading…'}</p></div>`;
  }
  const steps = runSteps(run);
  const waitingOn = new Set((detail.view?.awaitingDecisions || []).map((decision) => String(decision.nodeId)));
  return html`<div class="phone-run-page">
    <${Gates} run=${run} detail=${detail} onDecide=${decider(api, runId, refresh, source)} />
    ${detail.error && html`<p class="ui-error-box" role="alert">Could not read the run: ${detail.error}</p>`}
    <p class="phone-run-facts">${[group, run.status, run.startedAt && `started ${relTime(run.startedAt)}`,
      run.startedAt && `${formatRunDuration(run.startedAt, Date.parse(run.status === 'running' ? '' : run.updatedAt || '') || Date.now())}`]
      .filter(Boolean).join(' · ')}</p>
    <h2 class="ui-eyebrow phone-section-title">Steps</h2>
    <ol class="phone-run-steps">${steps.map((step) => {
      const mine = waitingOn.has(String(step.id));
      const who = mine ? 'you' : (step.performer?.label || step.performer?.name || '');
      return html`<li key=${step.id} class=${`phone-run-step s-${step.state}${mine ? ' mine' : ''}`}>
        <span class="phone-run-dot" aria-hidden="true"></span>
        <span class="phone-run-step-label">${step.label}</span>
        <span class="phone-run-step-meta">${who || stateLabel(step.state)}</span>
      </li>`;
    })}</ol>
  </div>`;
}
