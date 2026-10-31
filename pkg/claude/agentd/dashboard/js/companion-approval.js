// The operator's approval of an agent's access request: the card shared by the
// companion tab and the quick permissions panel, and the way to raise that
// panel. Dependency-light so a terminal tab can raise it too.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { approvalSummary, stewardSummonBody, timeLeft } from './companion-model.js';
import { useDismissablePopover } from './hover-popover.js';

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

// StewardAccessControls open the dashboard's own permissions editor and sudo
// dialog for a Steward. They are operator actions in the browser: both save
// through the cookie-authed /api routes, which no agent reaches. Offline there
// is no one to change, so they stay in place and say why.
export function StewardAccessControls({ agent, openPermissions, openSudo }) {
  const offline = `${agent.name} is not running. Resume it to change its access.`;
  return html`
    ${openPermissions && html`<button type="button" class="companion-steward-perms" disabled=${!agent.online}
      aria-label=${`Edit ${agent.name}'s permissions`} title=${agent.online ? 'Permissions: edit its standing grants and denies' : offline}
      onClick=${() => openPermissions(agent.convId, agent.name)}>🛡</button>`}
    ${openSudo && html`<button type="button" class="companion-steward-sudo" disabled=${!agent.online}
      aria-label=${`Grant ${agent.name} sudo`} title=${agent.online ? 'Sudo: grant a time-bounded elevation' : offline}
      onClick=${() => openSudo(agent.agentId, agent.name)}>sudo</button>`}
  `;
}

// shutDownAgent retires an agent, with the default clean exit. For a Steward
// that is the whole lifecycle: its session ends, what it held as an instance
// goes with it so nothing lingers, and the next summon starts a fresh one
// because a retired agent is never reused. What the operator stored for all
// future Stewards stays. Its conversation stays in history.
export async function shutDownAgent(fetchImpl, agent) {
  const response = await fetchImpl(`/api/agents/${encodeURIComponent(agent.agentId)}/retire`, { method: 'POST', credentials: 'same-origin' });
  if (!response.ok) {
    const text = (await response.text?.()) || '';
    let message = text;
    try { message = JSON.parse(text).message || text; } catch (_) { /* plain text error */ }
    throw new Error(message || `${response.status}`);
  }
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

// StewardShutdown is the shut-down control of a Steward's title bar and of its
// detached window. It asks first, inline: this page and the detached one share
// no modal host, and the action ends a running conversation.
export function StewardShutdown({ agent, fetchImpl, onDown }) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const rootRef = useRef(null);
  useDismissablePopover(rootRef, asking, () => setAsking(false), { hold: () => busy || !!error });
  async function confirm() {
    setBusy(true);
    setError('');
    try {
      await shutDownAgent(fetchImpl, agent);
      setAsking(false);
      onDown(agent);
    } catch (cause) {
      setError(`Could not shut down: ${cause?.message || cause}`);
    } finally {
      setBusy(false);
    }
  }
  return html`<span ref=${rootRef} class="steward-shutdown">
    <button type="button" class="steward-shutdown-open" aria-label=${`Shut down ${agent.name}`} aria-expanded=${asking ? 'true' : 'false'}
      title="Shut down: end this conversation" onClick=${() => { setError(''); setAsking((value) => !value); }}>⏻</button>
    ${asking && html`<div class="steward-shutdown-confirm" role="alertdialog" aria-label=${`Shut down ${agent.name}?`}>
      <p><b>Shut down ${agent.name}?</b> This ends its conversation and what was given to this Steward only. Grants for all future Stewards are kept. You can start a new one afterwards.</p>
      ${error && html`<p class="steward-shutdown-error" role="alert">${error}</p>`}
      <div class="steward-shutdown-actions">
        <button type="button" disabled=${busy} onClick=${() => setAsking(false)}>Cancel</button>
        <button type="button" class="danger" disabled=${busy} onClick=${confirm}>${busy ? 'Shutting down…' : 'Shut down'}</button>
      </div>
    </div>`}
  </span>`;
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
      notify(access.sudoStored ? `Ended ${agent.name}'s sudo, and future Stewards no longer get what was stored.`
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
    ${access.sudo.length > 0 && html`<button type="button" disabled=${busy} title=${access.sudoStored ? 'Ends its temporary sudo, including what is stored for future Stewards. Permanent permissions stay.' : 'Ends its temporary sudo. Permanent permissions stay.'} onClick=${end}>${busy ? 'Ending…' : 'End sudo'}</button>`}
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
    ${onMore && request.auto_grantable && html`<button type="button" class="access-panel-more" onClick=${onMore}>Always allow? Open in Messages</button>`}
    <div class="companion-approval-actions">
      ${expired ? html`<span class="companion-approval-expired">Timing out…</span>` : html`<button type="button" class="companion-approval-extend" disabled=${!!busy}
        title="Push the auto-decline back five minutes" onClick=${() => decide('extend')}>+5 min</button>`}
      <button type="button" class="companion-approval-deny" disabled=${!!busy || expired} onClick=${() => decide('deny')}>${busy === 'deny' ? 'Denying…' : 'Deny'}</button>
      <button type="button" class="companion-primary" disabled=${!!busy || expired} onClick=${() => decide('approve')}>${busy === 'approve' ? 'Approving…' : summary.kind === 'plan' ? 'Approve plan' : 'Approve'}</button>
    </div>
  </div>`;
}

