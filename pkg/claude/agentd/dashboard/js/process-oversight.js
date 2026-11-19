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

import { conditionWords, operandWords } from './process-rules.js';
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { RunCopyEditor } from './process-run-copy.js';
import { WaitingSteps } from './process-run-waits.js';

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
    case 'run_amended':
      return { icon: '✎', tone: 'concern', text: `${who} amended the run (#${payload.amendment}): ${amendSummaryText(payload.summary)}${payload.note ? ` · ${clip(payload.note)}` : ''}` };
    case 'decision_recorded':
      // A rule decision says what held and what it read, in plain words.
      if (event.actor === 'rule' && payload.rule) {
        const said = payload.rule.case >= 0 ? conditionWords(payload.rule.when) : 'no case held';
        const read = Object.entries(payload.rule.read || {}).map(([operand, value]) => `${operandWords(operand)} = ${value}`).join(' · ');
        return { icon: '⚙', tone: '', text: `the rule chose ${payload.verdict}${step}: ${said}${read ? ` (read ${read})` : ''}` };
      }
      return { icon: '·', tone: '', text: `${String(event.kind || '').replace(/[_.]+/g, ' ')}${step}${event.actor ? ` · ${who}` : ''}` };
    // A step's result: what failed and why, in words (a success is a dot).
    case 'program_observed': {
      const observation = payload.observation || {};
      if (observation.outcome !== 'failed') return { icon: '·', tone: '', text: `${event.nodeId || 'a command'} ${observation.outcome || 'finished'}${observation.exitCode ? ` (exit ${observation.exitCode})` : ''}` };
      return { icon: '✖', tone: 'concern', text: `${event.nodeId || 'a command'} failed${observation.error ? `: ${clip(observation.error)}` : ''}` };
    }
    // A message step: what it sent, and to whom (executor.MessageReport).
    case 'message_sent': {
      const observation = payload.observation || {};
      const message = payload.message || {};
      const reached = (message.delivered || []).map((d) => (d.key === 'operator' ? 'you' : d.name || d.key));
      const subject = message.subject ? ` “${clip(message.subject, 80)}”` : '';
      const to = reached.length ? ` to ${reached.join(', ')}` : '';
      if (observation.outcome !== 'failed') return { icon: '✉', tone: '', text: `${event.nodeId || 'a message'} sent${subject}${to}` };
      return { icon: '✖', tone: 'concern', text: `${event.nodeId || 'a message'} could not be sent${observation.error ? `: ${clip(observation.error)}` : ''}${reached.length ? ` (it reached ${reached.join(', ')})` : ''}` };
    }
    case 'message_resumed':
      return { icon: '✉', tone: '', text: `${event.nodeId || 'a message'}: sending again after a restart` };
    case 'agent_reported': {
      const observation = payload.observation || {};
      const verdict = observation.verdict ? ` ${observation.verdict}` : '';
      if (observation.outcome !== 'failed') return { icon: '·', tone: '', text: `${who} reported ${event.nodeId || 'a step'}${verdict || ' done'}${payload.note ? `: ${clip(payload.note, 100)}` : ''}` };
      return { icon: '✖', tone: 'concern', text: `${who} reported ${event.nodeId || 'a step'} failed${payload.note || observation.error ? `: ${clip(payload.note || observation.error)}` : ''}` };
    }
    // A step a person performs: opened, finished, or withdrawn with the run.
    case 'human_prepared':
      return { icon: '👤', tone: '', text: `${event.nodeId || 'a step'} waits on you${payload.command?.attempt > 1 ? ` (attempt ${payload.command.attempt})` : ''}` };
    case 'human_reported': {
      const outputs = Object.entries(payload.outputs || {}).map(([name, value]) => `${name} = ${clip(value, 60)}`).join(' · ');
      const note = payload.note ? `: ${clip(payload.note, 100)}` : '';
      if (payload.outcome === 'failed') return { icon: '✖', tone: 'concern', text: `${who} finished ${event.nodeId || 'a step'} as ${payload.choice}${note}` };
      return { icon: '✓', tone: '', text: `${who} finished ${event.nodeId || 'a step'} as ${payload.choice}${note}${outputs ? ` (${outputs})` : ''}` };
    }
    case 'human_withdrawn':
      return { icon: '✕', tone: 'concern', text: `${event.nodeId || 'a step'} was withdrawn: ${payload.reason || 'the run is ending'}` };
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
      <input name="note-optional" class="pov-note" type="text" maxlength="4096" aria-label=${`Note for concern ${concern.id} (optional)`} placeholder="note (optional)"
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
    <textarea name="what-is-wrong-or-stuck" rows="2" maxlength="4096" aria-label="What is wrong or stuck" placeholder="What is wrong or stuck? It goes to the run's overseer."
      value=${reason} disabled=${busy} onInput=${(event) => setReason(event.currentTarget.value)}></textarea>
    <div class="pov-raise-row">
      ${nodeId && html`<label class="grf-muted"><input name="process-oversight-input" type="checkbox" checked=${aboutStep} disabled=${busy}
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
    <textarea name="process-oversight-textarea" rows="2" maxlength="1024" aria-label=${label} placeholder=${placeholder}
      value=${text} disabled=${busy} onInput=${(event) => setText(event.currentTarget.value)}></textarea>
    <div class="pov-raise-row">
      <${Button} size="sm" variant="danger" disabled=${busy || (required && !text.trim())} onClick=${() => void submit()}>${busy ? '…' : confirm}<//>
      <${Button} size="sm" disabled=${busy} onClick=${onClose}>Back<//>
    </div>
    ${error && html`<div class="grf-error ui-error" role="alert">${error}</div>`}
  </div>`;
}

// amendSummaryText says what an amendment changes, by step.
export function amendSummaryText(summary = {}) {
  const parts = [];
  const add = (label, ids) => { if (ids?.length) parts.push(`${label} ${ids.join(', ')}`); };
  add('adds', summary.added);
  add('removes', summary.removed);
  add('changes', summary.changed);
  add('reroutes', summary.rerouted);
  if (summary.settings) parts.push('changes template settings');
  return parts.length ? parts.join('; ') : 'changes nothing';
}

const AMENDMENT_STATES = { proposed: 'waiting on you', applied: 'applied', rejected: 'rejected', stale: 'stale: the run moved on' };

// RunAmendments lists the changes made to this run's own copy of its
// template: what each changes, who and when, and a proposal's Apply/Reject.
export function RunAmendments({ view, api, onChanged, compact = false }) {
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const amendments = view?.amendments || [];
  if (!amendments.length) return null;
  const pick = async (amendment, choice) => {
    if (busy) return;
    setBusy(`${amendment.id}:${choice}`);
    setError('');
    try {
      if (choice === 'save') await api.saveAmendmentBack(view.id, amendment.id);
      else await api.pickConcern(amendment.messageId, choice, '');
      onChanged?.();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      setBusy('');
    }
  };
  return html`<div class=${`pov-amendments${compact ? ' pov-compact' : ''}`} data-run-amendments>
    <div class="pov-amend-title">✎ ${view.amended ? 'Amended: this run follows its own copy of the template' : 'Amendments proposed'}</div>
    <ul>${amendments.map((a) => html`<li key=${a.id} class=${`pov-amendment pov-amend-${a.state}`} data-run-amendment=${a.id}>
      <div class="pov-concern-head">
        <b>#${a.id}</b>
        <span class="grf-muted">by ${actorName(a.proposedBy, () => a.proposedName)} · ${eventTime(a.createdAt)}</span>
        <span class=${`pov-holder${a.state === 'proposed' && !a.outdated ? ' pov-holder-you' : ''}`} data-amendment-state>${a.outdated ? 'the run moved on: propose it again' : AMENDMENT_STATES[a.state] || a.state}${a.decidedName && a.state !== 'proposed' ? ` · ${a.decidedName}` : ''}</span>
      </div>
      <div class="pov-reason" data-amendment-summary>${amendSummaryText(a.summary)}</div>
      ${a.note && html`<div class="grf-muted">why: ${a.note}</div>`}
      ${a.savedTemplateRef && html`<div class="grf-muted" data-amendment-saved>saved back to the template: runs started since follow it</div>`}
      ${a.saveable && api?.saveAmendmentBack && html`<div class="pov-concern-actions">
        <${Button} size="sm" disabled=${!!busy} data-amendment-save-back
          title="Save this run's copy as the template's next version, so runs started from now on follow it (needs process.templates.manage)"
          onClick=${() => void pick(a, 'save')}>${busy === `${a.id}:save` ? '…' : '⤴ Save back to template'}<//>
      </div>`}
      ${a.state === 'proposed' && !a.outdated && a.messageId && api?.pickConcern && html`<div class="pov-concern-actions">
        <${Button} size="sm" variant="primary" disabled=${!!busy} data-amendment-apply onClick=${() => void pick(a, 1)}>${busy === `${a.id}:1` ? '…' : 'Apply amendment'}<//>
        <${Button} size="sm" disabled=${!!busy} data-amendment-reject onClick=${() => void pick(a, 2)}>${busy === `${a.id}:2` ? '…' : 'Reject'}<//>
      </div>`}
    </li>`)}</ul>
    ${error && html`<div class="grf-error ui-error" role="alert">${error}</div>`}
  </div>`;
}

// AmendRun edits the run's own copy of its template as YAML: Check says what
// it would change, then it is applied, or proposed to the operator.
export function AmendRun({ runId, api, onChanged, onClose }) {
  const [source, setSource] = useState(null);
  const [text, setText] = useState('');
  const [note, setNote] = useState('');
  const [checked, setChecked] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    api.runTemplate(runId).then((got) => {
      if (!live) return;
      setSource(got);
      setText(got.yaml || '');
    }).catch((cause) => live && setError(cause?.message || String(cause)));
    return () => { live = false; };
  }, [runId]);
  const run = async (dryRun) => {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      const got = await api.amendRun(runId, { yaml: text, note: note.trim(), dryRun, baseStateVersion: source?.stateVersion || 0 });
      if (dryRun) {
        setChecked(got);
      } else {
        onChanged?.();
        onClose?.();
      }
    } catch (cause) {
      setChecked(null);
      setError(cause?.message || String(cause));
    } finally {
      setBusy(false);
    }
  };
  const direct = checked ? checked.direct : source?.canAmendDirectly;
  return html`<div class="pov-raise pov-amend" data-run-amend>
    <div class="grf-muted">Change the steps this run has not reached yet. Only this run changes; the template stays as it is.</div>
    <textarea name="the-run-s-template-yaml" rows="14" spellcheck="false" class="pov-amend-yaml" aria-label="The run's template (YAML)" value=${text}
      disabled=${busy || !source} onInput=${(event) => { setText(event.currentTarget.value); setChecked(null); }}></textarea>
    <input name="why-optional" class="pov-note" type="text" maxlength="4096" aria-label="Why (optional)" placeholder="why (optional, shown to the operator and in the log)"
      value=${note} disabled=${busy} onInput=${(event) => setNote(event.currentTarget.value)} />
    ${checked && html`<div class="pov-amend-check" data-amend-check>This ${amendSummaryText(checked.summary)}.</div>`}
    <div class="pov-raise-row">
      ${checked
    ? html`<${Button} size="sm" variant="primary" disabled=${busy} data-amend-submit onClick=${() => void run(false)}>${busy ? '…' : direct ? 'Apply to this run' : 'Propose to the operator'}<//>`
    : html`<${Button} size="sm" variant="primary" disabled=${busy || !source} data-amend-check-open onClick=${() => void run(true)}>${busy ? '…' : 'Check'}<//>`}
      <${Button} size="sm" disabled=${busy} onClick=${onClose}>Back<//>
    </div>
    ${error && html`<div class="grf-error ui-error" role="alert">${error}</div>`}
  </div>`;
}

// BlockedSteps says, where the operator looks, which steps are blocked (parked,
// or out of retries) and why, and resolves one: Retry gives it another round,
// with an optional note its performer is handed (a park's reason is often a
// question), and Skip goes on past it. Cancel the run stays in the strip
// above. The CLI's resolve-blocked.
export function BlockedSteps({ view, runId, api, labelOf = (id) => id, onChanged, wizard = false }) {
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notes, setNotes] = useState({});
  const blocked = view?.blocked || [];
  if (!blocked.length) return null;
  const keyOf = (item) => `${item.nodeId}:${item.attempt}`;
  const resolve = async (item, action) => {
    if (busy) return;
    setBusy(`${item.nodeId}:${action}`);
    setError('');
    const note = action === 'retry' ? String(notes[keyOf(item)] || '').trim() : '';
    try {
      await api.resolveBlocked(runId, { nodeId: item.nodeId, attempt: item.attempt, action, ...(note ? { note } : {}) });
      onChanged?.();
    } catch (cause) {
      setError(cause?.message || String(cause));
      // Resolved elsewhere meanwhile (the CLI, a card): read the run again.
      onChanged?.();
    } finally {
      setBusy('');
    }
  };
  return html`<ul class="pov-blocked" data-run-blocked>${blocked.map((item) => {
    const label = labelOf(String(item.nodeId)) || item.nodeId;
    return html`<li key=${keyOf(item)} class="pov-blocked-item" data-run-blocked-node=${item.nodeId}>
      <div class="pov-reason"><b>⏹ ${label}</b> ${wizard ? 'is stayed' : 'is blocked'}${item.reason ? html`: <span data-run-blocked-reason>${item.reason}</span>` : ' and waits on you'}</div>
      ${api?.resolveBlocked && html`<div class="pov-concern-actions">
        <input class="pov-note" type="text" name="retry-note" maxlength="4096" data-run-blocked-note aria-label=${`Note for ${label}'s retry`}
          placeholder="answer or note for the retry (optional, handed to its performer)" value=${notes[keyOf(item)] || ''} disabled=${!!busy}
          onInput=${(event) => setNotes({ ...notes, [keyOf(item)]: event.currentTarget.value })} />
        <${Button} size="sm" variant="primary" disabled=${!!busy} data-run-blocked-retry
          title=${`Run ${label} again: its performer gets another round`}
          onClick=${() => void resolve(item, 'retry')}>${busy === `${item.nodeId}:retry` ? '…' : '↻ Retry'}<//>
        <${Button} size="sm" disabled=${!!busy} data-run-blocked-skip
          title=${`Go on past ${label} as if it was done`}
          onClick=${() => void resolve(item, 'skip')}>${busy === `${item.nodeId}:skip` ? '…' : '⤼ Skip this step'}<//>
      </div>`}
    </li>`;
  })}
    ${error && html`<li class="grf-error ui-error" role="alert">${error}</li>`}
  </ul>`;
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
export function RunOversight({ view, runId, api, onChanged, nodeId = '', nodeLabel = '', nodeDone = false, wizard = false, compact = false, labelOf = undefined }) {
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
      ${steerable && !compact && api?.amendRun && !busy && html`<${Button} size="sm" data-run-amend-open
        title="Change the steps this run has not reached yet, for this run only"
        onClick=${() => setSteering('amend-graph')}>✎ Amend run…<//>`}
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
    ${steering === 'amend-graph' && html`<${RunCopyEditor} runId=${runId} api=${api} onChanged=${onChanged}
      onClose=${() => setSteering('')} onYaml=${() => setSteering('amend')} />`}
    ${steering === 'amend' && html`<${AmendRun} runId=${runId} api=${api} onChanged=${onChanged} onClose=${() => setSteering('')} />`}
    ${steering === 'cancel' && html`<${SteerForm} kind="cancel" label="Why the run is given up"
      placeholder="Why is the run given up? (optional, recorded in its log)" confirm=${wizard ? 'Cancel the rite' : 'Cancel the run'}
      send=${(note) => api.cancelRun(runId, { note })}
      onChanged=${onChanged} onClose=${() => setSteering('')} />`}
    ${!terminal && html`<${BlockedSteps} view=${view} runId=${runId} api=${api} labelOf=${labelOf} onChanged=${onChanged} wizard=${wizard} />`}
    ${!terminal && html`<${WaitingSteps} view=${view} runId=${runId} api=${api} labelOf=${labelOf} onChanged=${onChanged} wizard=${wizard} compact=${compact} />`}
    <${RunAmendments} view=${view} api=${api} onChanged=${onChanged} compact=${compact} />
    ${open.length > 0 && html`<ul class="pov-concerns">${open.map((concern) => html`<${ConcernCard} key=${concern.id}
      concern=${concern} runId=${runId} api=${api} onChanged=${onChanged} compact=${compact} />`)}</ul>`}
  </section>`;
}
