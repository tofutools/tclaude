// phone-approval.js -- the phone's cards for what waits on the operator, in
// the shape of the Now mock: an agent's access request (amber edge, a
// countdown, Deny / +5 min / Approve), a decision put to the operator (purple
// edge, two options and ⋯ for the rest), and an agent asking (blue edge,
// Open). Now shows them; Access and Mail can reuse them.
//
// They answer through the same calls as the desktop companion cards
// (companion-approval.js), which the dock keeps using unchanged. Request
// bodies, questions and options are agent text, rendered as text.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { answerDecision, decideAccessRequest, secondsLeft } from './companion-approval.js';
import { postDismiss } from './decision-dismiss.js';
import { approvalContext, cardOptions, countdown, planDetails } from './phone-now-model.js';
import { openPhoneSheet, requestClosePhoneSheet } from './phone-sheet.js';
import { DiscardAsk, useDraftGuard } from './phone-draft-guard.js';
import { Button, Input } from './ui-components.js';
import { Avatar, ButtonRow, Card, SheetTitle, StatePill } from './phone-ui.js';
import { ContextMeter } from './context-meter.js';
import { useHold } from './phone-hold.js';

const html = htm.bind(h);

// Who a card is from: the agent's name, its group muted after it, and its
// context meter (context-meter.js) when its agent's state is known
// (ctxState).
function Who({ name, sub = '', ctxState = null }) {
  return html`<div class="phone-who"><span class="phone-who-name">${name}</span>${sub && html`<span class="phone-who-sub">${sub}</span>`}${ctxState && html`<${ContextMeter} state=${ctxState} />`}</div>`;
}

// useNow ticks once a second while the card shows, for its countdown.
function useNow(enabled) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!enabled) return undefined;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [enabled]);
  return now;
}

// PhoneApprovalCard is an agent's pending access request. Tapping its text
// opens the request in Mail (onMore), where its details and "Always allow"
// are. decide(id, decision) replaces the default call (Mail's controller,
// which updates its list at once).
export function PhoneApprovalCard({ request, fetchImpl, notify, from = '', group = '', ctxState = null, onMore = null, decide: decideWith = null, moreLabel = '' }) {
  const [busy, setBusy] = useState('');
  const now = useNow(!!request.deadline);
  const left = secondsLeft(request.deadline, now);
  const expired = left === 0;
  const plan = String(request.perm || '').startsWith('sudo.');
  const name = from || request.conv_title || 'An agent';
  async function decide(decision) {
    setBusy(decision);
    try {
      await (decideWith ? decideWith(request.id, decision) : decideAccessRequest(fetchImpl, request.id, decision));
      // An extension leaves the request pending: free the buttons again.
      if (decision === 'extend') setBusy('');
    } catch (error) {
      // A decide override that already told the operator marks its error told.
      if (!error?.told) notify(`That decision could not be recorded: ${error?.message || error}`, true);
      setBusy('');
    }
  }
  const context = approvalContext(request);
  // A plan's reason is its own line: the plan line already carries the
  // permissions and how long they last.
  const details = plan ? planDetails(request) : null;
  const reason = details?.ok ? details.reason : '';
  const text = html`<${Who} name=${name} sub=${group || request.target_group || ''} ctxState=${ctxState} /><div class="phone-card-line">${context}</div>
    ${reason && html`<div class="phone-card-line phone-plan-reason">“${reason}”</div>`}`;
  return html`<${Card} accent="key" class="phone-now-card" role="group" aria-label=${`${name} asks: ${context}${reason ? `, because: ${reason}` : ''}`}>
    <div class="phone-card-row">
      <${Avatar} bot glyph="🤖" badge="🔑" />
      ${onMore
        ? html`<button data-ui-exempt type="button" class="phone-card-text" aria-label=${moreLabel || `Open ${name}'s request in Mail`} onClick=${onMore}>${text}</button>`
        : html`<div class="phone-card-text">${text}</div>`}
      ${left !== null && html`<${StatePill} tone="key" title="Declined automatically when this runs out">${expired ? 'timing out' : countdown(left)}<//>`}
    </div>
    <${ButtonRow} buttons=${[
      { key: 'deny', label: busy === 'deny' ? 'Denying…' : 'Deny', tone: 'no', disabled: !!busy || expired, onClick: () => decide('deny') },
      { key: 'extend', label: '+5 min', title: 'Push the auto-decline back five minutes', disabled: !!busy || expired, onClick: () => decide('extend') },
      { key: 'approve', label: busy === 'approve' ? 'Approving…' : plan ? 'Approve plan' : 'Approve', tone: 'ok', disabled: !!busy || expired, onClick: () => decide('approve') },
    ]} />
  <//>`;
}

// DecisionAnswer answers a decision: a note to send with the pick, every
// option as a full-width button (the recommended one ★ and primary), and
// Dismiss. onDone(label) runs once it is sent, with the option's label, or ''
// for a dismissal. Now's ⋯ sheet and Mail's reader show it.
// onDraft(note, busy) tells a sheet holding it what would be lost and
// whether an answer is on its way, so it can guard its close.
export function DecisionAnswer({ decision, fetchImpl, notify, onDone, onDraft = null }) {
  const [busy, setBusy] = useState(0);
  const [note, setNote] = useState('');
  useEffect(() => { setBusy(0); setNote(''); onDraft?.('', false); }, [decision.id]);
  // Reported as it changes, not a render later: a Back right after a
  // keystroke must find the note.
  const draft = (nextNote, nextBusy) => onDraft?.(nextNote, !!nextBusy);
  const fetchWith = fetchImpl || ((...args) => globalThis.fetch(...args));
  const run = async (key, act, sent, failure) => {
    setBusy(key);
    draft(note, true);
    try {
      await act();
      onDone(sent);
    } catch (error) {
      notify(`${failure}: ${error?.message || error}`, true);
      setBusy(0);
      draft(note, false);
    }
  };
  return html`<${Input} class="phone-decision-note" placeholder="Add a note (optional)" aria-label="Note to send with your choice"
      value=${note} disabled=${!!busy} onInput=${(event) => { setNote(event.currentTarget.value); draft(event.currentTarget.value, busy); }} />
    <div class="phone-decision-options">
      ${decision.options.map((option, i) => {
        const recommended = i + 1 === decision.recommended;
        return html`<${Button} key=${option} class=${`phone-btn${recommended ? ' phone-btn-pri' : ''}`} disabled=${!!busy}
          title=${recommended ? 'The agent recommends this one' : undefined}
          onClick=${() => run(i + 1, () => answerDecision(fetchWith, decision.id, i + 1, note), option, 'That decision could not be sent')}>
          ${busy === i + 1 ? 'Sending…' : html`${recommended && html`<span class="phone-star" aria-label="recommended">★</span> `}${option}`}
        <//>`;
      })}
      <${Button} class="phone-btn" disabled=${!!busy} title="No longer relevant: take it off the list without answering; the agent is told"
        onClick=${() => run('dismiss', () => postDismiss(fetchWith, [decision.id]), '', 'That decision could not be dismissed')}>
        ${busy === 'dismiss' ? 'Dismissing…' : 'Dismiss'}<//>
    </div>`;
}

// DecisionSheet is a decision in full, behind its card's ⋯: the question, the
// agent's text and image, and its DecisionAnswer.
export function DecisionSheet({ decision, from = '', fetchImpl, notify, close }) {
  const image = (decision.attachments || []).find((a) => a.previewable);
  const question = decision.subject || 'Decision';
  // A note written and not sent is asked about before Back or ✕ drops it.
  const draft = useRef({ note: '', busy: false });
  const root = useRef(null);
  const guard = useDraftGuard(() => !!draft.current.note.trim(), () => draft.current.busy,
    () => root.current?.querySelector('.phone-decision-note'));
  // Close comes first: the sheet focuses its first button, and that must not
  // be an answer a stray Enter would send.
  return html`<div ref=${root} class="phone-decision-sheet">
    <div class="phone-sheet-head">
      <${SheetTitle}>${question}<//>
      <button data-ui-exempt type="button" class="phone-sheet-close" aria-label="Close" onClick=${requestClosePhoneSheet}>✕</button>
    </div>
    ${from && html`<p class="phone-sheet-sub">from ${from}</p>`}
    ${decision.body && decision.body !== decision.subject && html`<p class="phone-decision-body">${decision.body}</p>`}
    ${image && html`<a class="phone-decision-image" href=${image.url} target="_blank" rel="noopener"><img src=${image.url} alt=${image.filename || 'attached image'} /></a>`}
    <${DecisionAnswer} decision=${decision} fetchImpl=${fetchImpl} notify=${notify} onDone=${() => close()}
      onDraft=${(note, busy) => { draft.current = { note, busy }; }} />
    ${guard.asking && html`<${DiscardAsk} what="your note" guard=${guard} close=${close} />`}
  </div>`;
}

// PhoneDecisionCard is a decision an agent put to the operator: who asks, the
// question, the recommended option (★) and another as buttons, and ⋯ for the
// rest. A pick on the card goes without a note.
export function PhoneDecisionCard({ decision, fetchImpl, notify, from = '', steward = false }) {
  const [busy, setBusy] = useState(0);
  const question = decision.subject || 'Decision';
  async function choose(choice) {
    setBusy(choice);
    try {
      await answerDecision(fetchImpl, decision.id, choice);
    } catch (error) {
      notify(`That decision could not be sent: ${error?.message || error}`, true);
      setBusy(0);
    }
  }
  const more = () => openPhoneSheet({ label: question, Body: DecisionSheet, props: { decision, from, fetchImpl, notify } });
  return html`<${Card} accent="ask" class="phone-now-card" role="group" aria-label=${`Decision: ${question}`}>
    <div class="phone-card-row">
      ${steward ? html`<${Avatar} bot glyph="✦" />` : html`<${Avatar} bot glyph="🤖" badge="❓" />`}
      <div class="phone-card-text"><${Who} name=${from || 'An agent'} sub="decide" /><div class="phone-card-line">${question}</div></div>
    </div>
    <${ButtonRow} class="phone-decision-choices" buttons=${[
      ...cardOptions(decision).map((option) => ({
        key: `o${option.choice}`, disabled: !!busy, title: option.recommended ? 'The agent recommends this one' : undefined,
        label: busy === option.choice ? 'Sending…' : html`${option.recommended && html`<span class="phone-star" aria-label="recommended">★</span> `}${option.label}`,
        ariaLabel: option.recommended ? `${option.label} (recommended)` : undefined,
        onClick: () => choose(option.choice),
      })),
      { key: 'more', label: '⋯', grow: false, ariaLabel: 'All options, a note, or dismiss', disabled: !!busy, onClick: more },
    ]} />
  <//>`;
}

// AskCard is an agent waiting on the operator in its terminal: what it asks,
// and Open. A hold on it opens the agent's menu (onMenu).
export function AskCard({ agent, onOpen, onMenu = null }) {
  const { holding, handlers } = useHold(onMenu ? () => onMenu(agent) : null);
  return html`<${Card} accent="q" class=${`phone-now-card phone-holdable${holding ? ' phone-holding' : ''}`} role="group" aria-label=${`${agent.name} is asking`} ...${handlers}>
    <div class="phone-card-row">
      <${Avatar} bot glyph=${agent.steward ? '✦' : '🤖'} badge=${agent.steward ? '' : '❓'} />
      <div class="phone-card-text"><${Who} name=${agent.name} sub=${agent.group} ctxState=${agent.ctxState ?? null} />
        <div class="phone-card-line">${agent.detail ? `Asking: ${agent.detail}` : 'is asking you'}</div></div>
      <${Button} class="phone-btn" onClick=${() => onOpen(agent)}>Open<//>
    </div>
  <//>`;
}
