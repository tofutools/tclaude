// The operator's approval of an agent's access request: the card shared by the
// companion tab and the quick permissions panel, and the way to raise that
// panel. Dependency-light so a terminal tab can raise it too.
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { approvalSummary } from './companion-model.js';

const html = htm.bind(h);

// Surfaces ask for the panel by event, like the human-notification reader, so
// any of them can raise it over whatever tab is showing.
export const OPEN_ACCESS_PANEL_EVENT = 'tclaude:open-access-panel';

export function openAccessPanel({ agentId = '', launcher = null, documentRef = globalThis.document } = {}) {
  if (!documentRef) return false;
  documentRef.dispatchEvent(new CustomEvent(OPEN_ACCESS_PANEL_EVENT, { detail: { agentId, launcher } }));
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
export function ApprovalCard({ request, fetchImpl, notify, from = '' }) {
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
    <div class="companion-approval-actions">
      ${expired ? html`<span class="companion-approval-expired">Timing out…</span>` : html`<button type="button" class="companion-approval-extend" disabled=${!!busy}
        title="Push the auto-decline back five minutes" onClick=${() => decide('extend')}>+5 min</button>`}
      <button type="button" class="companion-approval-deny" disabled=${!!busy || expired} onClick=${() => decide('deny')}>${busy === 'deny' ? 'Denying…' : 'Deny'}</button>
      <button type="button" class="companion-primary" disabled=${!!busy || expired} onClick=${() => decide('approve')}>${busy === 'approve' ? 'Approving…' : summary.kind === 'plan' ? 'Approve plan' : 'Approve'}</button>
    </div>
  </div>`;
}

