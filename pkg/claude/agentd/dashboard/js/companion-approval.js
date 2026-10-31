// The operator's approval of an agent's access request: the card shared by the
// companion tab and the quick permissions panel, and the way to raise that
// panel. Dependency-light so a terminal tab can raise it too.
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { approvalSummary, timeLeft } from './companion-model.js';

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
// endElevatedAccess revokes what an agent holds itself. One failure must not
// leave the other live, so both are tried.
export async function endElevatedAccess(fetchImpl, agent, access) {
  const send = async (url, options) => {
    const response = await fetchImpl(url, { credentials: 'same-origin', ...options });
    if (!response.ok) throw new Error((await response.text?.()) || `${response.status}`);
  };
  const results = await Promise.allSettled([
    access.sudo.length && send(`/api/sudo?agent_id=${encodeURIComponent(agent.agentId)}`, { method: 'DELETE' }),
    access.standing.length && send('/api/permissions', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ conv: agent.convId, overrides: Object.fromEntries(access.standing.map((slug) => [slug, 'default'])) }),
    }),
  ]);
  const failed = results.find((result) => result.status === 'rejected');
  if (failed) throw failed.reason;
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
      onClick=${() => openSudo(agent.agentId)}>sudo</button>`}
  `;
}

// ElevatedBand says an agent holds more than its usual access and ends it in
// one click. No confirmation: taking access away is safe, and it can ask again.
export function ElevatedBand({ agent, access, fetchImpl, notify }) {
  const [busy, setBusy] = useState(false);
  async function end() {
    setBusy(true);
    try {
      await endElevatedAccess(fetchImpl, agent, access);
      notify(access.inherited.length
        ? `Ended what ${agent.name} held itself. Group access remains: change it in Groups.`
        : `${agent.name} is back to its usual access.`);
    } catch (error) {
      notify(`Could not end ${agent.name}'s elevated access: ${error?.message || error}`, true);
    } finally {
      setBusy(false);
    }
  }
  return html`<div class="companion-elevated" role="status">
    <span aria-hidden="true">🔓</span>
    <div class="companion-elevated-text">
      <b>Elevated access</b>
      ${access.sudo.map((grant) => html`<span key=${grant.id}><code>${grant.slug}</code> ${timeLeft(grant.remaining_seconds)} left</span>`)}
      ${access.standing.map((slug) => html`<span key=${slug}><code>${slug}</code> standing</span>`)}
      ${access.inherited.map((slug) => html`<span key=${slug}><code>${slug}</code> from a group</span>`)}
    </div>
    ${(access.sudo.length > 0 || access.standing.length > 0) && html`<button type="button" disabled=${busy} onClick=${end}>${busy ? 'Ending…' : 'End now'}</button>`}
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

