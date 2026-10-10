import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

function errText(error) { return error?.message || String(error); }

// ABANDONABLE are the outgoing move states the daemon lets the operator stop:
// retirement of the source has not started yet.
export const ABANDONABLE = Object.freeze(['awaiting_confirmation', 'confirmed', 'blocked', 'expired', 'declined']);
const FAILED = new Set(['blocked', 'expired', 'declined', 'failed']);

export function canAbandon(m) {
  return m?.direction === 'out' && ABANDONABLE.includes(m.state);
}

function shortAgent(id) {
  return id ? String(id).slice(0, 14) : '';
}

// MovesPage lists durable agent moves and teleports in both directions and
// carries this node's teleport freeze. Moves come from move-agent, the merged
// view and peer actions; this page only follows and abandons them.
export function MovesPage({ view, actions, confirm, toast, now = Date.now() }) {
  const [moves, setMoves] = useState(null);
  const [error, setError] = useState('');
  const [teleport, setTeleport] = useState(null);
  const [tick, setTick] = useState(0);
  const label = (id) => view.trusted.find((r) => r.id === id)?.label || id;

  useEffect(() => {
    let off = false;
    actions.moves().then((m) => { if (!off) { setMoves(m); setError(''); } }).catch((e) => { if (!off) setError(errText(e)); });
    actions.teleport().then((t) => { if (!off) setTeleport(t); }).catch((e) => { if (!off) setTeleport({ error: errText(e) }); });
    return () => { off = true; };
  }, [tick]);

  const frozen = teleport && !teleport.error ? !!teleport.disabled : null;
  const toggleTeleport = () => confirm({
    title: frozen ? 'Allow teleports again?' : 'Freeze teleports on this node?',
    body: frozen
      ? 'Agents may teleport from this node again, and peers\' agents may land here again where a group receives them.'
      : 'Agents here can no longer teleport to a peer, and teleports from peers can no longer land here, until you allow them again. Teleports already in flight stop at their next step. Moves started with move-agent are not affected.',
    okLabel: frozen ? 'Allow teleports' : 'Freeze teleports',
    busyLabel: 'Saving…',
    action: () => actions.setTeleport(!frozen),
  }).then((t) => { if (t) { setTeleport(t); toast(t.disabled ? 'Teleports are frozen' : 'Teleports are allowed', false); } })
    .catch((e) => toast(`Teleport switch failed: ${errText(e)}`, true));

  const abandon = (m) => confirm({
    title: `Abandon moving ${shortAgent(m.source_agent)} to ${label(m.peer)}?`,
    body: `This node stops moving the agent and does not retire it here; if it is already stopped (a paused-backup teleport), it stays stopped. If ${label(m.peer)} already created its copy, that copy stays there as an independent agent.`,
    okLabel: 'Abandon',
    busyLabel: 'Abandoning…',
    action: () => actions.abandonMove(m.id),
  }).then((r) => { if (r) { toast('Move abandoned', false); setTick((n) => n + 1); } })
    .catch((e) => toast(`Abandon failed: ${errText(e)}`, true));

  const list = Array.isArray(moves) ? [...moves].sort((a, b) => String(b.expires_at || '').localeCompare(String(a.expires_at || ''))) : [];
  return html`<div class="fa-moves">
    <div class="fa-run-settings" id="fleet-teleport">
      <span class="fa-k">Teleports</span>
      ${teleport?.error ? html`<span class="fa-danger">${teleport.error}</span>`
        : frozen == null ? html`<span class="muted">loading…</span>`
        : html`<span class=${frozen ? 'fa-warn' : 'muted'}>${frozen ? 'frozen: none leave or land here' : 'allowed (each still needs its grants)'}</span>
          <button id="fleet-teleport-toggle" type="button" class=${frozen ? '' : 'fa-danger'} onClick=${toggleTeleport}>${frozen ? 'Allow teleports…' : 'Freeze teleports…'}</button>`}
      <button type="button" class="fa-link" onClick=${() => setTick((n) => n + 1)}>refresh</button>
    </div>
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    ${!moves && !error ? html`<div class="empty">Loading…</div>` : list.length === 0 && !error ? html`<div class="empty">No agent moves or teleports.</div>` : html`
    <table class="fa-table" id="fleet-moves">
      <thead><tr><th></th><th>Kind</th><th>Agent</th><th>Peer</th><th>Group</th><th>State</th><th>Expires</th><th></th></tr></thead>
      <tbody>${list.map((m) => html`<tr key=${m.id} data-move=${m.id}>
        <td title=${m.direction === 'out' ? 'leaving this node' : 'arriving here'}>${m.direction === 'out' ? '⇢' : '⇠'}</td>
        <td>${m.teleport ? 'teleport' : 'move'}</td>
        <td><code title=${m.source_agent}>${shortAgent(m.source_agent)}</code>${m.target_agent ? html` → <code title=${m.target_agent}>${shortAgent(m.target_agent)}</code>` : ''}</td>
        <td>${label(m.peer)}</td>
        <td>${m.group || ''}</td>
        <td class=${FAILED.has(m.state) ? 'fa-danger' : ''} title=${m.last_error || ''}>${m.state}${m.last_error ? html` <span class="muted">— ${m.last_error}</span>` : ''}</td>
        <td class="muted">${m.expires_at && !String(m.expires_at).startsWith('0001-') ? (Date.parse(m.expires_at) < now ? 'expired' : new Date(m.expires_at).toLocaleString()) : ''}</td>
        <td class="fa-acts">${canAbandon(m) ? html`<button type="button" class="fa-danger" data-fa="abandon" onClick=${() => abandon(m)}>Abandon…</button>` : ''}</td>
      </tr>`)}</tbody>
    </table>`}
    <div class="muted fa-cli-note">Moves start from an agent (peer view: Move here / Teleport here) or <code>tclaude federation move-agent</code>. CLI: <code>tclaude federation moves</code>, <code>tclaude federation teleport</code>.</div>
  </div>`;
}
