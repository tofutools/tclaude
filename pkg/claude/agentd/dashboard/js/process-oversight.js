// process-oversight.js -- a run's oversight in the run views (tk_e2fa2bd0):
// who oversees it, the concerns raised about it and where each waits on the
// escalation chain (overseer → the group's escalation agent → operator), and
// the run log in words.
//
// The run view (GET /v1/process/runs/{id}) carries `overseer` and `concerns`;
// the run's events carry the log. A concern with the operator shows its
// decision card's choices, answered here exactly as in Messages; one an agent
// holds the operator may still settle (continue, pause) or pass up.
//
// The desktop run focus (groups-run-focus.js) and the phone run sheet
// (phone-process-runs.js) both render these.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';

const html = htm.bind(h);

const VIA = { set: 'set on the run', template: "the template's choice", lead: "the group's lead", starter: 'started the run' };

// overseerText is the "overseen by" line of a run view.
export function overseerText(overseer, { wizard = false } = {}) {
  if (!overseer) return '';
  const who = overseer.agentId ? (overseer.name || overseer.agentId) : (wizard ? 'you, the keeper' : 'you (the operator)');
  const via = VIA[overseer.via] ? ` · ${VIA[overseer.via]}` : '';
  const up = overseer.escalationName ? ` · escalates to ${overseer.escalationName}` : '';
  return `${who}${via}${up}`;
}

export function openConcerns(view) {
  return (view?.concerns || []).filter((concern) => concern.state === 'open');
}

// concernsForYou counts the open concerns waiting on the operator's card.
export function concernsForYou(view) {
  return openConcerns(view).filter((concern) => concern.level === 'operator').length;
}

// actorName names an evidence actor ref for a reader.
export function actorName(ref, nameOf = () => '') {
  const value = String(ref || '');
  if (value === 'human:operator') return 'you';
  if (value.startsWith('tclaude:')) return 'the engine';
  if (value.startsWith('agent:')) {
    const id = value.slice('agent:'.length);
    return nameOf(id) || id;
  }
  if (value.startsWith('peer:')) return value.slice('peer:'.length);
  return value || 'someone';
}

function concernHolder(concern) {
  if (concern.level === 'operator') return 'you';
  return concern.holderName || concern.holder || concern.level;
}

const RESOLUTIONS = {
  continue: 'carry on', paused: 'pause the run', resume: 'resume', skip: 'skip the step',
  reassign: 'reassign the step', keep_paused: 'keep it paused', cancel: 'cancel the run',
};

function payloadOf(event) {
  const raw = event?.payload;
  if (raw && typeof raw === 'object') return raw;
  try { return JSON.parse(raw || '{}') || {}; } catch { return {}; }
}

function clip(text, limit = 140) {
  const value = String(text || '').trim().split('\n')[0];
  return value.length > limit ? `${value.slice(0, limit - 1)}…` : value;
}

// eventSentence turns one run event into a line of the run log. withStep
// false leaves out the step, for a history already under its step.
export function eventSentence(event, nameOf = () => '', { withStep = true } = {}) {
  const who = actorName(event.actor, nameOf);
  const payload = payloadOf(event);
  const step = event.nodeId && withStep ? ` at ${event.nodeId}` : '';
  switch (event.kind) {
    case 'concern_raised':
      return { icon: '⚑', tone: 'concern', text: `${who} raised concern #${payload.concern}${step}: ${clip(payload.reason)}` };
    case 'concern_escalated': {
      const to = payload.level === 'operator' ? 'you' : actorName(payload.holder, nameOf);
      const why = payload.why ? ` (${clip(payload.why, 100)})` : '';
      // The engine passes over a holder that did not answer; anyone else
      // passed it on themselves.
      const engine = !event.actor || String(event.actor).startsWith('tclaude:');
      const text = engine ? `concern #${payload.concern} passed to ${to}${why}` : `${who} passed concern #${payload.concern} to ${to}${why}`;
      return { icon: '↑', tone: 'concern', text };
    }
    case 'concern_resolved': {
      const note = payload.note ? `: ${clip(payload.note, 100)}` : '';
      return { icon: '✓', tone: 'settled', text: `${who} settled concern #${payload.concern}: ${RESOLUTIONS[payload.resolution] || payload.resolution}${note}` };
    }
    case 'overseer_set':
      return { icon: '👁', tone: '', text: payload.overseer ? `${who} made ${actorName(payload.overseer, nameOf)} the overseer` : `${who} gave the run back to its default overseer` };
    case 'run_paused':
      return { icon: '⏸', tone: 'concern', text: `${who} paused the run` };
    case 'run_resumed':
      return { icon: '▶', tone: '', text: `${who} resumed the run` };
    case 'block_marked':
      return { icon: '⏹', tone: 'concern', text: `${who} parked ${event.nodeId || payload.nodeId} (attempt ${payload.attempt}): ${clip(payload.reason)}` };
    case 'sent_back': {
      const reset = Array.isArray(payload.nodes) ? payload.nodes.length - 1 : 0;
      return { icon: '↶', tone: 'concern', text: `${who} sent the run back to ${event.nodeId || payload.nodeId}${reset > 0 ? ` (${reset} later step${reset === 1 ? '' : 's'} reset)` : ''}${payload.note ? `: ${clip(payload.note)}` : ''}` };
    }
    case 'run_canceled':
      return { icon: '✕', tone: 'concern', text: `${who} canceled the run${payload.note ? `: ${clip(payload.note)}` : ''}` };
    default:
      return { icon: '·', tone: '', text: `${String(event.kind || '').replace(/[_.]+/g, ' ')}${step}${event.actor ? ` · ${who}` : ''}` };
  }
}

function eventTime(iso) {
  const when = new Date(iso || '');
  return Number.isNaN(when.getTime()) ? '' : when.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// RunLog is the whole run's log, newest first.
export function RunLog({ events = [], nameOf, catchingUp = false, limit = 200 }) {
  const rows = events.slice(-limit).reverse();
  return html`<div class="pov-log" data-run-log>
    ${rows.length
    ? html`<ol class="pov-log-list">${rows.map((event) => {
      const line = eventSentence(event, nameOf);
      return html`<li key=${event.sequence} class=${line.tone ? `pov-log-${line.tone}` : undefined} data-run-log-kind=${event.kind}>
        <time>${eventTime(event.occurredAt)}</time><span class="pov-log-icon" aria-hidden="true">${line.icon}</span><span>${line.text}</span>
      </li>`;
    })}</ol>`
    : html`<div class="grf-muted">Nothing recorded yet.</div>`}
    ${catchingUp && html`<div class="grf-muted" role="status">Reading the rest of this run's history…</div>`}
  </div>`;
}

// ConcernCard is one open concern: who raised it, where it waits, and what
// the operator can do about it here.
export function ConcernCard({ concern, runId, api, onChanged, compact = false }) {
  const [busy, setBusy] = useState('');
  const [note, setNote] = useState('');
  const [error, setError] = useState('');
  const act = async (key, run) => {
    if (busy) return;
    setBusy(key);
    setError('');
    try {
      await run();
      setNote('');
      onChanged?.();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      setBusy('');
    }
  };
  const forYou = concern.level === 'operator';
  // Without its card (dismissed, or never posted) a concern with the operator
  // is still settled here: carry on or pause, as a holder would.
  const choices = forYou ? concern.choices || [] : [];
  const due = concern.answerBy ? eventTime(concern.answerBy) : '';
  return html`<li class=${`pov-concern${forYou ? ' pov-for-you' : ''}${compact ? ' pov-compact' : ''}`} data-run-concern=${concern.id}>
    <div class="pov-concern-head">
      <b>⚑ #${concern.id}</b>${concern.nodeId && html` <span class="pov-step">${concern.nodeId}</span>`}
      <span class="grf-muted">from ${concern.source === 'operator' ? 'you' : concern.raisedName || concern.raisedBy}</span>
      <span class=${`pov-holder${forYou ? ' pov-holder-you' : ''}`} data-concern-holder>${forYou ? 'waiting on you' : `with ${concernHolder(concern)}${due ? ` · until ${due}` : ''}`}</span>
    </div>
    <div class="pov-reason">${concern.reason}</div>
    <div class="pov-concern-actions">
      <input class="pov-note" type="text" maxlength="4096" aria-label=${`Note for concern ${concern.id} (optional)`} placeholder="note (optional)"
        value=${note} disabled=${!!busy} onInput=${(event) => setNote(event.currentTarget.value)} />
      ${choices.length
    ? choices.map((choice, i) => html`<${Button} key=${choice} size="sm" variant=${i === 0 ? 'primary' : undefined} disabled=${!!busy}
          data-concern-choice=${choice} onClick=${() => act(choice, () => api.pickConcern(concern.messageId, i + 1, note.trim()))}>${busy === choice ? '…' : choice}<//>`)
    : html`
        <${Button} size="sm" disabled=${!!busy} data-concern-answer="continue" title="Settle it: the run goes on (and resumes, if paused)"
          onClick=${() => act('continue', () => api.answerConcern(runId, concern.id, { action: 'continue', note: note.trim() }))}>${busy === 'continue' ? '…' : 'Carry on'}<//>
        <${Button} size="sm" disabled=${!!busy} data-concern-answer="pause" title="Settle it by holding the run"
          onClick=${() => act('pause', () => api.answerConcern(runId, concern.id, { action: 'pause', note: note.trim() }))}>${busy === 'pause' ? '…' : 'Pause run'}<//>
        ${!forYou && html`<${Button} size="sm" disabled=${!!busy} data-concern-answer="escalate" title="Pass it up the chain: to the group's escalation agent, else to you as a decision"
          onClick=${() => act('escalate', () => api.answerConcern(runId, concern.id, { action: 'escalate', note: note.trim() }))}>${busy === 'escalate' ? '…' : 'Pass it up'}<//>`}`}
    </div>
    ${error && html`<div class="grf-error ui-error" role="alert">${error}</div>`}
  </li>`;
}

// RaiseConcern is the operator's own "something is off with this run".
export function RaiseConcern({ runId, nodeId = '', nodeLabel = '', api, onChanged, onClose }) {
  const [reason, setReason] = useState('');
  const [aboutStep, setAboutStep] = useState(!!nodeId);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const submit = async () => {
    if (!reason.trim() || busy) return;
    setBusy(true);
    setError('');
    try {
      await api.raiseConcern(runId, { reason: reason.trim(), nodeId: aboutStep ? nodeId : '' });
      setReason('');
      onChanged?.();
      onClose?.();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      setBusy(false);
    }
  };
  return html`<div class="pov-raise" data-run-raise>
    <textarea rows="2" maxlength="4096" aria-label="What is wrong or stuck" placeholder="What is wrong or stuck? It goes to the run's overseer."
      value=${reason} disabled=${busy} onInput=${(event) => setReason(event.currentTarget.value)}></textarea>
    <div class="pov-raise-row">
      ${nodeId && html`<label class="grf-muted"><input type="checkbox" checked=${aboutStep} disabled=${busy}
        onChange=${(event) => setAboutStep(event.currentTarget.checked)} /> about ${nodeLabel || nodeId}</label>`}
      <${Button} size="sm" variant="primary" disabled=${busy || !reason.trim()} onClick=${() => void submit()}>${busy ? '…' : 'Raise'}<//>
      <${Button} size="sm" disabled=${busy} onClick=${onClose}>Cancel<//>
    </div>
    ${error && html`<div class="grf-error ui-error" role="alert">${error}</div>`}
  </div>`;
}

// SteerForm asks for the why behind a step being parked or the run being
// given up, and sends it.
function SteerForm({ kind, label, placeholder, confirm, required, send, onChanged, onClose }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const submit = async () => {
    if ((required && !text.trim()) || busy) return;
    setBusy(true);
    setError('');
    try {
      await send(text.trim());
      onChanged?.();
      onClose?.();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      setBusy(false);
    }
  };
  return html`<div class="pov-raise" data-run-steer=${kind}>
    <textarea rows="2" maxlength="1024" aria-label=${label} placeholder=${placeholder}
      value=${text} disabled=${busy} onInput=${(event) => setText(event.currentTarget.value)}></textarea>
    <div class="pov-raise-row">
      <${Button} size="sm" variant="danger" disabled=${busy || (required && !text.trim())} onClick=${() => void submit()}>${busy ? '…' : confirm}<//>
      <${Button} size="sm" disabled=${busy} onClick=${onClose}>Back<//>
    </div>
    ${error && html`<div class="grf-error ui-error" role="alert">${error}</div>`}
  </div>`;
}

// outAgentStep is the selected step's agent attempt that is out, if any:
// the one a person can park.
export function outAgentStep(view, nodeId) {
  if (!nodeId) return null;
  return (view?.commands || []).find((command) => command.kind === 'agent' && command.parkable && command.nodeId === nodeId) || null;
}

// RunOversight is the strip under a run's header: its overseer, a way to
// raise a concern, to park the selected step or give the run up, and its
// open concerns.
export function RunOversight({ view, runId, api, onChanged, nodeId = '', nodeLabel = '', nodeDone = false, wizard = false, compact = false }) {
  const [raising, setRaising] = useState(false);
  const [steering, setSteering] = useState('');
  if (!view) return null;
  const open = openConcerns(view);
  const terminal = ['completed', 'failed', 'canceled'].includes(view.status);
  // A run given up that is still draining takes no more steering.
  const steerable = !terminal && !view.draining;
  const out = steerable && outAgentStep(view, nodeId);
  const busy = raising || steering;
  return html`<section class=${`pov${compact ? ' pov-compact' : ''}`} aria-label="Oversight" data-run-oversight>
    <div class="pov-line">
      <span class="pov-overseer" data-run-overseer=${view.overseer?.agentId || view.overseer?.ref || ''}
        title="The overseer answers this run's concerns, and may pause, resume and reassign its steps">👁 ${wizard ? 'watched by' : 'overseen by'} <b>${overseerText(view.overseer, { wizard })}</b></span>
      ${open.length > 0 && html`<span class="pk-chip ui-tag pov-count" data-run-concern-count>⚑ ${open.length} open</span>`}
      ${!terminal && api?.raiseConcern && !busy && html`<${Button} size="sm" data-run-raise-open onClick=${() => setRaising(true)}>⚑ Raise a concern<//>`}
      ${out && api?.markBlocked && !busy && html`<${Button} size="sm" data-run-park-open
        title="Stop this step: its performer is released, and it waits on retry, skip or cancel"
        onClick=${() => setSteering('park')}>⏹ Park ${nodeLabel || nodeId}…<//>`}
      ${steerable && nodeDone && nodeId && !/[./]/.test(nodeId) && api?.sendBack && !busy && html`<${Button} size="sm" data-run-send-back-open
        title="Run this step again: everything after it is reset, work out there is withdrawn, and its outputs are asked for again"
        onClick=${() => setSteering('back')}>↶ Send back to ${nodeLabel || nodeId}…<//>`}
      ${steerable && api?.cancelRun && !busy && html`<${Button} size="sm" variant="danger" data-run-cancel-open
        title="Give the whole run up: work still out is withdrawn and it ends canceled"
        onClick=${() => setSteering('cancel')}>✕ Cancel ${wizard ? 'rite' : 'run'}…<//>`}
    </div>
    ${raising && html`<${RaiseConcern} runId=${runId} nodeId=${nodeId} nodeLabel=${nodeLabel} api=${api} onChanged=${onChanged} onClose=${() => setRaising(false)} />`}
    ${steering === 'park' && out && html`<${SteerForm} kind="park" label="Why this step cannot go on" required
      placeholder=${`Why can ${nodeLabel || nodeId} not go on? Its performer is told to stop.`} confirm="Park step"
      send=${(reason) => api.markBlocked(runId, { nodeId: out.nodeId, attempt: out.attempt, reason })}
      onChanged=${onChanged} onClose=${() => setSteering('')} />`}
    ${steering === 'back' && html`<${SteerForm} kind="back" label="Why the run goes back"
      placeholder=${`Why run ${nodeLabel || nodeId} again? (optional, recorded in its log) Everything after it is reset.`} confirm="Send back"
      send=${(note) => api.sendBack(runId, { nodeId, note })}
      onChanged=${onChanged} onClose=${() => setSteering('')} />`}
    ${steering === 'cancel' && html`<${SteerForm} kind="cancel" label="Why the run is given up"
      placeholder="Why is the run given up? (optional, recorded in its log)" confirm=${wizard ? 'Cancel the rite' : 'Cancel the run'}
      send=${(note) => api.cancelRun(runId, { note })}
      onChanged=${onChanged} onClose=${() => setSteering('')} />`}
    ${open.length > 0 && html`<ul class="pov-concerns">${open.map((concern) => html`<${ConcernCard} key=${concern.id}
      concern=${concern} runId=${runId} api=${api} onChanged=${onChanged} compact=${compact} />`)}</ul>`}
  </section>`;
}
