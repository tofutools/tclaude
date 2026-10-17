import { h, Fragment } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { openBreakGlassModal } from './message-access-dialog-controller.js';

const html = htm.bind(h);

export function liveBreakGlass(snapshot) {
  if (snapshot?.peer_view) return [];
  return (snapshot?.break_glass || []).filter(grant => Date.parse(grant.expires_at) > Date.now());
}
export function breakGlassRemaining(grant) {
  const seconds = Math.max(0, Math.ceil((Date.parse(grant.expires_at) - Date.now()) / 1000));
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.ceil(seconds / 60)}m`;
  return `${Math.floor(seconds / 3600)}h${Math.ceil(seconds % 3600 / 60)}m`;
}

export function BreakGlassBadge({ member, snapshot }) {
  const grant = liveBreakGlass(snapshot).find(row => row.agent_id === member.agent_id);
  if (!grant) return null;
  return html`<button class="break-glass-pill" title=${`Can execute unsandboxed as you until ${new Date(grant.expires_at).toLocaleString()} — ${grant.reason || 'no reason supplied'}`}
    data-act="break-glass" data-agent=${member.agent_id}>🚨 ${breakGlassRemaining(grant)}</button>`;
}

export function BreakGlassIndicator({ state, banner = false }) {
  const snapshot = state.snapshot.value;
  const [dismissed, setDismissed] = useState(false);
  const [,tick] = useState(0);
  const [error,setError] = useState('');
  const grants = liveBreakGlass(snapshot);
  useEffect(() => {
    if (!grants.length) return;
    const timer = setInterval(() => tick(value => value + 1), 1000);
    return () => clearInterval(timer);
  }, [grants.length]);
  if (!grants.length) return null;
  if (!banner) return html`<button class="break-glass-pill break-glass-header" onClick=${() => { setDismissed(false); openBreakGlassModal('', 'runs'); }}>🚨 BREAK-GLASS · ${grants.length} live</button>`;
  if (dismissed) return null;
  const revokeAll = async () => {
    try {
      const response = await fetch('/api/break-glass/revoke', { method:'POST', credentials:'same-origin', headers:{'Content-Type':'application/json'}, body:JSON.stringify({target:'all'}) });
      if (!response.ok) throw new Error(await response.text());
    } catch (err) { setError(err.message); }
  };
  return html`<${Fragment}><div class="break-glass-banner" role="alert">
    <strong>🚨 Break-glass is live</strong><span> — these agents can run unsandboxed shell commands as you:</span>
    ${grants.map(grant => html`<button key=${grant.id} class="break-glass-pill" onClick=${() => openBreakGlassModal(grant.agent_id)} title=${`Expires ${new Date(grant.expires_at).toLocaleString()}`}>${grant.conv_title || grant.agent_id} · ${breakGlassRemaining(grant)} left</button>`)}
    <span class="spacer"></span><button onClick=${() => openBreakGlassModal('', 'runs')}>view runs</button>
    <button onClick=${revokeAll}>revoke all</button><button aria-label="Dismiss break-glass banner for this page session" onClick=${() => setDismissed(true)}>×</button>
    ${error && html`<span>${error}</span>`}
  </div></${Fragment}>`;
}
