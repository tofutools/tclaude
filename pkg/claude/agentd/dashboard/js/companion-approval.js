// The operator's answers: the approval card for an agent's access request,
// shared by the companion tab, the quick permissions panel and the phone's Now
// view, the way to raise that panel, and the card for a decision an agent put
// to the operator. Dependency-light so a terminal tab or the phone shell can
// use them without the companion's window machinery.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { approvalSummary, stewardSummonBody, timeLeft } from './companion-model.js';
import { postDismiss } from './decision-dismiss.js';
import { useDismissablePopover } from './hover-popover.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// Surfaces ask for the panel by event, like the human-notification reader, so
// any of them can raise it over whatever tab is showing.
export const OPEN_ACCESS_PANEL_EVENT = 'tclaude:open-access-panel';

export function openAccessPanel({ agentId = '', launcher = null, returnFocus = null, documentRef = globalThis.document } = {}) {
  if (!documentRef) return false;
  documentRef.dispatchEvent(new CustomEvent(OPEN_ACCESS_PANEL_EVENT, { detail: { agentId, launcher, returnFocus } }));
  return true;
}

function secondsLeft(deadline, now) {
  const at = Date.parse(deadline || '');
  return Number.isFinite(at) ? Math.max(0, Math.round((at - now) / 1000)) : null;
}

// ApprovalCard is an agent's pending access request shown in its companion
// tab: a whole plan's temporary permissions (`tclaude agent sudo`) or one
// --ask-human command. Approve and Deny decide it the same way the Messages
// tab does. The request body is agent-supplied text and is rendered as text.
// endSudoAccess ends an agent's temporary sudo grants and nothing else: what it
// holds permanently is changed in the permissions editor.
export async function endSudoAccess(fetchImpl, agent) {
  const response = await fetchImpl(`/api/sudo?agent_id=${encodeURIComponent(agent.agentId)}`, { method: 'DELETE', credentials: 'same-origin' });
  if (!response.ok) throw new Error((await response.text?.()) || `${response.status}`);
}

async function refusal(response) {
  const text = (await response.text?.()) || '';
  try {
    const body = JSON.parse(text);
    return body.message || body.error || text;
  } catch (_) { return text || `${response.status}`; }
}

// shutDownAgent stops an agent and waits for it to be gone. A Steward keeps its
// seat, its conversation and what it holds, so it can be resumed, and starting a
// fresh one puts it on the roster. Retiring is what ends an agent for good.
export async function shutDownAgent(fetchImpl, agent) {
  const response = await fetchImpl(`/api/agents/${encodeURIComponent(agent.agentId)}/stop`, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ wait: true }),
  });
  if (!response.ok) throw new Error(await refusal(response));
}

// resumeAgent brings a stopped agent back on its own conversation. The daemon
// answers 200 either way and says how it went in action. The dashboard's wake
// has dialogs for a missing directory and a lost subscription, so the error says
// when one of them applies.
export async function resumeAgent(fetchImpl, agentId) {
  const response = await fetchImpl(`/api/agents/${encodeURIComponent(agentId)}/resume`, { method: 'POST', credentials: 'same-origin' });
  if (!response.ok) throw new Error(await refusal(response));
  const { action = '', detail = '' } = await response.json().catch(() => ({}));
  if (action === 'error' || action.startsWith('error:')) {
    const error = new Error(detail || action);
    error.recoverable = action === 'error:missing_cwd' || action === 'error:claude_subscription_lost';
    throw error;
  }
}

async function clearAgentContext(fetchImpl, agent) {
  const response = await fetchImpl(`/api/agents/${encodeURIComponent(agent.agentId)}/reincarnate`, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ mode: 'clear' }),
  });
  if (!response.ok) throw new Error(await refusal(response));
}

// requestStewardSummon asks for a Steward, from any page. The caller shows it.
export async function requestStewardSummon(fetchImpl) {
  const response = await fetchImpl('/api/scribe', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(stewardSummonBody()),
  });
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
  return result;
}

// ConfirmedAction is a control that asks first, in a small popover. askRef lets
// another control open the same question, and act hears who asked. With no
// trigger it has no button of its own: a cog's item asks through askRef.
function ConfirmedAction({ className = '', openClass = 'steward-shutdown-open', glyph, label, title, question, confirm, busyLabel, failure, act, askRef = null, trigger = true }) {
  const [asking, setAsking] = useState(false);
  if (askRef) askRef.current = (from = 'elsewhere') => { setError(''); setAsking(from); };
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const rootRef = useRef(null);
  useDismissablePopover(rootRef, asking, () => setAsking(false), { hold: () => busy || !!error });
  async function run() {
    setBusy(true);
    setError('');
    try {
      await act(asking);
      setAsking(false);
    } catch (cause) {
      setError(`${failure}: ${cause?.message || cause}`);
    } finally {
      setBusy(false);
    }
  }
  return html`<span ref=${rootRef} class=${`steward-shutdown ${className}`}>
    ${trigger && html`<button type="button" data-ui-exempt class=${openClass} aria-label=${label} aria-expanded=${asking ? 'true' : 'false'}
      title=${title} onClick=${() => { setError(''); setAsking((value) => (value ? false : 'here')); }}>${glyph}</button>`}
    ${asking && html`<div class="steward-shutdown-confirm" role="alertdialog" aria-label=${label}>
      <p>${question}</p>
      ${error && html`<p class="steward-shutdown-error ui-error" role="alert">${error}</p>`}
      <div class="steward-shutdown-actions">
        <${Button} size="sm" disabled=${busy} onClick=${() => setAsking(false)}>Cancel<//>
        <${Button} size="sm" variant="danger" disabled=${busy} onClick=${run}>${busy ? busyLabel : confirm}<//>
      </div>
    </div>`}
  </span>`;
}

// StewardShutdown is the shut-down control of a Steward's title bar and of its
// detached window. Asked through askRef, it was the window's close button, so
// onDown is told to take the tab off the window as a close does.
export function StewardShutdown({ agent, fetchImpl, onDown, askRef = null, trigger = true }) {
  return html`<${ConfirmedAction} glyph="⏻" label=${`Shut down ${agent.name}`} title="Shut down: stop it, keeping its conversation"
    question=${html`<b>Shut down ${agent.name}?</b> It stops and keeps its conversation and what it holds. Afterwards you can resume it or start fresh.`}
    confirm="Shut down" busyLabel="Shutting down…" failure="Could not shut down"
    askRef=${askRef} trigger=${trigger}
    act=${async (asked) => { await shutDownAgent(fetchImpl, agent); onDown(agent, { closing: asked === 'elsewhere' }); }} />`;
}

// ClearContext gives an agent a fresh conversation and nothing else: same agent,
// identity and permissions, with its startup context sent again. A shell has no
// context to clear, so the caller leaves this out for one.
export function ClearContext({ agent, fetchImpl, notify, askRef = null, trigger = true }) {
  return html`<${ConfirmedAction} askRef=${askRef} trigger=${trigger} className="companion-clear-context" openClass="companion-clear-open" glyph="⟲" label=${`Clear ${agent.name}'s context`} title="Clear context: same agent, fresh conversation"
    question=${html`<b>Clear ${agent.name}'s context?</b> It restarts on a new conversation as the same agent, with the same identity and permissions. It remembers nothing from this one, which stays in history.`}
    confirm="Clear context" busyLabel="Clearing…" failure="Could not clear context"
    act=${async () => { await clearAgentContext(fetchImpl, agent); notify(`Cleared ${agent.name}'s context.`); }} />`;
}

// ElevatedBand says an agent holds more than its usual access. End ends its sudo
// in one click, with no confirmation: it is temporary and the agent can ask again.
// Everything else on the band is listed only, and stays.
export function ElevatedBand({ agent, access, fetchImpl, notify }) {
  const [busy, setBusy] = useState(false);
  async function end() {
    setBusy(true);
    try {
      await endSudoAccess(fetchImpl, agent);
      notify(access.sudoStored ? `Ended ${agent.name}'s sudo, and future Stewards no longer get the stored sudo.`
        : `Ended ${agent.name}'s sudo.`);
    } catch (error) {
      notify(`Could not end ${agent.name}'s sudo: ${error?.message || error}`, true);
    } finally {
      setBusy(false);
    }
  }
  return html`<div class="companion-elevated" role="status">
    <span aria-hidden="true">🔓</span>
    <div class="companion-elevated-text">
      <b>Elevated access</b>
      ${access.sudo.map((grant) => html`<span key=${grant.id}><code>${grant.slug}</code> ${timeLeft(grant.remaining_seconds)} left${grant.carried ? ', all future Stewards: ending it ends it for them too' : ''}</span>`)}
      ${access.standing.map((slug) => html`<span key=${slug}><code>${slug}</code> ${access.stored?.has(slug) ? 'standing, all future Stewards' : 'standing'}</span>`)}
      ${access.inherited.map((slug) => html`<span key=${slug}><code>${slug}</code> from a group</span>`)}
    </div>
    ${access.sudo.length > 0 && html`<${Button} size="sm" disabled=${busy} title=${access.sudoStored ? 'Ends its temporary sudo, including what is stored for future Stewards. Permanent permissions stay.' : 'Ends its temporary sudo. Permanent permissions stay.'} onClick=${end}>${busy ? 'Ending…' : 'End sudo'}<//>`}
  </div>`;
}

export function ApprovalCard({ request, fetchImpl, notify, from = '', onMore = null }) {
  const [busy, setBusy] = useState('');
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  const summary = approvalSummary(request);
  const left = secondsLeft(request.deadline, now);
  const expired = left === 0;
  async function decide(decision) {
    setBusy(decision);
    try {
      const response = await fetchImpl(`/api/access-requests/${encodeURIComponent(request.id)}/decision`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ decision }),
      });
      if (!response.ok) throw new Error((await response.text?.()) || `${response.status}`);
      // An extension leaves the request pending: free the buttons again.
      if (decision === 'extend') setBusy('');
    } catch (error) {
      notify(`That decision could not be recorded: ${error?.message || error}`, true);
      setBusy('');
    }
  }
  return html`<div class=${`companion-approval ${summary.kind}`} role="group" aria-label=${summary.title}>
    <div class="companion-approval-head">
      <b>${summary.title}</b>${from && html`<span class="companion-approval-from">${from}</span>`}
      ${left !== null && html`<span class="companion-approval-timer" title="Declined automatically when this runs out">${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}</span>`}
    </div>
    <div class="companion-approval-slugs">${summary.slugs.map((slug) => html`<code key=${slug}>${slug}</code>`)}</div>
    <dl class="companion-approval-meta">
      ${request.target_group && html`<dt>Group</dt><dd>${request.target_group}</dd>`}
      ${request.target_conv_id && html`<dt>Target</dt><dd>${request.target_conv_title || request.target_conv_id}</dd>`}
      ${request.scope_display && html`<dt>Scope</dt><dd><code>${request.scope_display}</code></dd>`}
      ${request.path && !request.body && html`<dt>Endpoint</dt><dd><code>${request.method ? `${request.method} ` : ''}${request.path}</code></dd>`}
    </dl>
    ${request.body && html`<pre class="companion-approval-body">${request.body}</pre>`}
    ${onMore && request.auto_grantable && html`<button data-ui-exempt type="button" class="access-panel-more" onClick=${onMore}>Always allow? Open in Messages</button>`}
    <div class="companion-approval-actions">
      ${expired ? html`<span class="companion-approval-expired">Timing out…</span>` : html`<${Button} size="sm" variant="link" class="companion-approval-extend" disabled=${!!busy}
        title="Push the auto-decline back five minutes" onClick=${() => decide('extend')}>+5 min<//>`}
      <${Button} size="sm" variant="danger" class="companion-approval-deny" disabled=${!!busy || expired} onClick=${() => decide('deny')}>${busy === 'deny' ? 'Denying…' : 'Deny'}<//>
      <${Button} size="sm" variant="primary" class="companion-primary" disabled=${!!busy || expired} onClick=${() => decide('approve')}>${busy === 'approve' ? 'Approving…' : summary.kind === 'plan' ? 'Approve plan' : 'Approve'}<//>
    </div>
  </div>`;
}

// DecisionCard is a decision the agent put to the operator with
// `notify-human --option ...`: its question, any attached image, and one
// button per option. The pick (plus an optional note) goes back to the agent
// as mail, even if it is offline. All of it is agent text, rendered as text.
export function DecisionCard({ decision, fetchImpl, notify, from = '' }) {
  const [busy, setBusy] = useState(0);
  const [note, setNote] = useState('');
  const image = (decision.attachments || []).find((a) => a.previewable);
  async function choose(choice) {
    setBusy(choice);
    try {
      const response = await fetchImpl('/api/human-messages/reply', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: decision.id, choice, body: note.trim() }),
      });
      if (!response.ok) throw new Error((await response.text?.()) || `${response.status}`);
    } catch (error) {
      notify(`That decision could not be sent: ${error?.message || error}`, true);
      setBusy(0);
    }
  }
  async function dismiss() {
    setBusy('dismiss');
    try {
      await postDismiss(fetchImpl, [decision.id]);
    } catch (error) {
      notify(`That decision could not be dismissed: ${error?.message || error}`, true);
      setBusy(0);
    }
  }
  const question = decision.subject || 'Decision';
  return html`<div class="companion-decision" role="group" aria-label=${`Decision: ${question}`}>
    <div class="companion-approval-head"><b>${question}</b>${from && html`<span class="companion-decision-from">${from}</span>`}</div>
    ${decision.body && decision.body !== decision.subject && html`<p class="companion-decision-body">${decision.body}</p>`}
    <div class="companion-decision-options">
      ${decision.options.map((option, i) => {
        const recommended = i + 1 === decision.recommended;
        return html`<${Button} key=${option} size="sm" variant=${recommended ? 'primary' : ''} class=${recommended ? 'companion-primary' : ''} disabled=${!!busy}
          title=${recommended ? 'The agent recommends this one' : ''} onClick=${() => choose(i + 1)}>
          ${busy === i + 1 ? 'Sending…' : option}${recommended && busy !== i + 1 ? html` <span class="companion-decision-rec">★</span>` : ''}
        <//>`;
      })}
      <${Button} size="sm" class="companion-decision-dismiss" disabled=${!!busy} title="No longer relevant: take it off the list without answering; the agent is told"
        onClick=${dismiss}>${busy === 'dismiss' ? 'Dismissing…' : 'Dismiss'}<//>
    </div>
    <input class="companion-decision-note" type="text" placeholder="Add a note (optional)" value=${note} disabled=${!!busy}
      aria-label="Note to send with your choice" onInput=${(event) => setNote(event.currentTarget.value)} />
    ${image && html`<a class="companion-decision-image" href=${image.url} target="_blank" rel="noopener"><img src=${image.url} alt=${image.filename || 'attached image'} /></a>`}
  </div>`;
}
