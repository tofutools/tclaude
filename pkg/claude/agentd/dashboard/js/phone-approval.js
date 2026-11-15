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
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { answerDecision, decideAccessRequest, secondsLeft } from './companion-approval.js';
import { postDismiss } from './decision-dismiss.js';
import { approvalContext, cardOptions, countdown } from './phone-now-model.js';
import { openPhoneSheet } from './phone-sheet.js';
import { Button, Input } from './ui-components.js';
import { Avatar, ButtonRow, Card, StatePill } from './phone-ui.js';
import { contextHigh } from './phone-shell-model.js';

const html = htm.bind(h);

// Who a card is from: the agent's name, its group muted after it, and an
// amber tag when its context is nearly full (ctx, a percentage).
function Who({ name, sub = '', ctx = null }) {
  return html`<div class="phone-who"><span class="phone-who-name">${name}</span>${sub && html`<span class="phone-who-sub">${sub}</span>`}<${ContextTag} ctx=${ctx} /></div>`;
}

// ContextTag is "ctx 91%" in amber by an agent's name once its context is at
// CONTEXT_HIGH_PCT or over; nothing below that.
export function ContextTag({ ctx }) {
  if (!contextHigh({ ctx })) return null;
  return html`<span class="phone-ctx-tag" title=${`Context ${ctx}% full`}>ctx ${ctx}%</span>`;
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
export function PhoneApprovalCard({ request, fetchImpl, notify, from = '', group = '', ctx = null, onMore = null, decide: decideWith = null, moreLabel = '' }) {
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
  const text = html`<${Who} name=${name} sub=${group || request.target_group || ''} ctx=${ctx} /><div class="phone-card-line">${context}</div>`;
  return html`<${Card} accent="key" class="phone-now-card" role="group" aria-label=${`${name} asks: ${context}`}>
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

// DecisionSheet is a decision in full, behind its card's ⋯: the question, the
// agent's text and image, every option, a note to send with the pick, and
// Dismiss.
export function DecisionSheet({ decision, from = '', fetchImpl, notify, close }) {
  const [busy, setBusy] = useState(0);
  const [note, setNote] = useState('');
  const image = (decision.attachments || []).find((a) => a.previewable);
  const run = async (key, act, failure) => {
    setBusy(key);
    try {
      await act();
      close();
    } catch (error) {
      notify(`${failure}: ${error?.message || error}`, true);
      setBusy(0);
    }
  };
  const question = decision.subject || 'Decision';
  // Close comes first: the sheet focuses its first button, and that must not
  // be an answer a stray Enter would send.
  return html`<div class="phone-decision-sheet">
    <div class="phone-sheet-head">
      <h2 class="phone-sheet-title">${question}</h2>
      <button data-ui-exempt type="button" class="phone-sheet-close" aria-label="Close" onClick=${close}>✕</button>
    </div>
    ${from && html`<p class="phone-sheet-sub">from ${from}</p>`}
    ${decision.body && decision.body !== decision.subject && html`<p class="phone-decision-body">${decision.body}</p>`}
    ${image && html`<a class="phone-decision-image" href=${image.url} target="_blank" rel="noopener"><img src=${image.url} alt=${image.filename || 'attached image'} /></a>`}
    <${Input} class="phone-decision-note" placeholder="Add a note (optional)" aria-label="Note to send with your choice"
      value=${note} disabled=${!!busy} onInput=${(event) => setNote(event.currentTarget.value)} />
    <div class="phone-decision-options">
      ${decision.options.map((option, i) => {
        const recommended = i + 1 === decision.recommended;
        return html`<${Button} key=${option} class=${`phone-btn${recommended ? ' phone-btn-pri' : ''}`} disabled=${!!busy}
          title=${recommended ? 'The agent recommends this one' : undefined}
          onClick=${() => run(i + 1, () => answerDecision(fetchImpl, decision.id, i + 1, note), 'That decision could not be sent')}>
          ${busy === i + 1 ? 'Sending…' : html`${recommended && html`<span class="phone-star" aria-label="recommended">★</span> `}${option}`}
        <//>`;
      })}
      <${Button} class="phone-btn" disabled=${!!busy} title="No longer relevant: take it off the list without answering; the agent is told"
        onClick=${() => run('dismiss', () => postDismiss(fetchImpl, [decision.id]), 'That decision could not be dismissed')}>
        ${busy === 'dismiss' ? 'Dismissing…' : 'Dismiss'}<//>
    </div>
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
    <${ButtonRow} buttons=${[
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
// and Open.
export function AskCard({ agent, onOpen }) {
  return html`<${Card} accent="q" class="phone-now-card" role="group" aria-label=${`${agent.name} is asking`}>
    <div class="phone-card-row">
      <${Avatar} bot glyph=${agent.steward ? '✦' : '🤖'} badge=${agent.steward ? '' : '❓'} />
      <div class="phone-card-text"><${Who} name=${agent.name} sub=${agent.group} ctx=${agent.ctx ?? null} />
        <div class="phone-card-line">${agent.detail ? `Asking: ${agent.detail}` : 'is asking you'}</div></div>
      <${Button} class="phone-btn" onClick=${() => onOpen(agent)}>Open<//>
    </div>
  <//>`;
}
