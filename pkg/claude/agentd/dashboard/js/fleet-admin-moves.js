import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { receiverDecides, sentLanding } from './fleet-admin-landing.js';

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

// MOVE_ERRORS turn the move-agent refusals into what to do about them.
const MOVE_ERRORS = {
  unsupported_peer: (peer) => `${peer} has not advertised agent move support (it may run an older tclaude). Offer a copy from Offers instead.`,
  history_required: () => 'A move carries the conversation history, and this agent has no native history to send.',
  source: () => 'The agent is no longer an active agent on this node, or it restarted into a new conversation. Reload and pick it again.',
};

export function moveErrorText(error, peer) {
  const hint = MOVE_ERRORS[error?.code];
  return hint ? `${hint(peer)} (${errText(error)})` : errText(error);
}

// MoveAgentDialog is tclaude federation move-agent: the agent and its
// conversation history go to a peer, and the agent retires here once the peer
// confirms its copy is running.
export function MoveAgentDialog({ peers, agents, actions, confirm, onClose, onDone }) {
  const [form, setForm] = useState({ agent: agents[0]?.id || '', peer: peers[0]?.id || '', group: '' });
  const [findings, setFindings] = useState(null);
  const [allow, setAllow] = useState(false);
  const [error, setError] = useState('');
  const set = (k) => (e) => { setForm({ ...form, [k]: e.currentTarget.value }); setFindings(null); setAllow(false); };
  const peerLabel = peers.find((p) => p.id === form.peer)?.label || form.peer;
  const agentLabel = agents.find((a) => a.id === form.agent)?.label || form.agent;
  const send = () => {
    if (!form.agent || !form.peer) { setError('Pick an agent and a peer.'); return; }
    const group = form.group.trim();
    if (!group) { setError('Name the receiving group on the peer.'); return; }
    setError('');
    confirm({
      title: `Move ${agentLabel} to ${peerLabel}?`,
      body: `Sends ${agentLabel}'s configuration and its full conversation history — which may contain code, file contents and anything pasted into it — to ${peerLabel}, whose operator may start it in group ${group}. Once ${peerLabel} confirms its copy is running, ${agentLabel} is retired here: it stops running, leaves its groups and loses its grants on this node (its history and worktree stay), and mail peers send to its old address is refused. Until then you can abandon the move on this page, which keeps the agent here, but the history already offered to ${peerLabel} is not withdrawn and any copy it makes stays there. ${receiverDecides(peerLabel)}${allow ? ` The history includes suspected credentials (${findings.map((f) => `${f.kind} ×${f.count}`).join(', ')}).` : ''}`,
      okLabel: 'Move agent',
      busyLabel: 'Sending…',
      action: () => actions.moveAgent({ agent: form.agent, peer: form.peer, group, allow_flagged: allow }),
    }).then((r) => { if (r) onDone(`Move of ${agentLabel} to ${peerLabel} started${sentLanding(r, peerLabel)}`); })
      .catch((e) => {
        if (e?.code === 'flagged_credentials') { setFindings(e.body?.findings || []); setError('The history looks like it contains credentials. A move always carries the history: send it anyway, or clean up first.'); }
        else setError(moveErrorText(e, peerLabel));
      });
  };
  return html`<${Overlay} id="fleet-move-agent" labelledby="fleet-move-agent-title" onClose=${onClose}>
    <h3 id="fleet-move-agent-title">Move an agent to a peer</h3>
    ${!agents.length || !peers.length ? html`<div class="muted">${!agents.length ? 'This node has no agent to move.' : 'Trust a peer first.'}</div>` : html`
    <label class="fa-of-row"><span class="fa-k">agent</span><select id="fleet-move-agent-agent" value=${form.agent} onChange=${set('agent')}>
      ${agents.map((a) => html`<option key=${a.id} value=${a.id}>${a.label}</option>`)}</select></label>
    <label class="fa-of-row"><span class="fa-k">peer</span><select id="fleet-move-agent-peer" value=${form.peer} onChange=${set('peer')}>
      ${peers.map((p) => html`<option key=${p.id} value=${p.id}>${p.label}</option>`)}</select></label>
    <label class="fa-of-row"><span class="fa-k">group</span><input id="fleet-move-agent-group" value=${form.group} placeholder="receiving group on the peer (it must grant agents.receive)" autocomplete="off" onInput=${set('group')} /></label>
    <div class="muted">The agent retires here once the peer runs its copy. To keep it running here, offer a copy from Offers instead. Starting directory: the receiver chooses on accept.</div>`}
    ${findings && html`<div class="fa-plan"><ul>${findings.map((f) => html`<li class="fa-warn">${f.kind} ×${f.count}${f.locations?.length ? ` (${f.locations.slice(0, 3).join(', ')})` : ''}</li>`)}</ul></div>
      <label class="fa-of-check"><input id="fleet-move-agent-allow" type="checkbox" checked=${allow} onChange=${(e) => setAllow(e.currentTarget.checked)} /> send the history anyway</label>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation move-agent AGENT PEER --group … [--allow-flagged]</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      ${agents.length && peers.length ? html`<button id="fleet-move-agent-send" type="button" class="primary" disabled=${!!findings && !allow} onClick=${send}>Move…</button>` : ''}
      <button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

// MovesPage lists durable agent moves and teleports in both directions and
// carries this node's teleport freeze. Moves come from move-agent, the merged
// view and peer actions, or from this page's move dialog.
export function MovesPage({ view, agents = [], actions, confirm, toast, now = Date.now() }) {
  const [moving, setMoving] = useState(false);
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
      <button id="fleet-move-open" type="button" onClick=${() => setMoving(true)}>Move an agent to a peer…</button>
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
    <div class="muted fa-cli-note">Moves also start from a peer's view (Move here / Teleport here). CLI: <code>tclaude federation move-agent</code>, <code>tclaude federation moves</code>, <code>tclaude federation teleport</code>.</div>
    ${moving && html`<${MoveAgentDialog} peers=${view.trusted} agents=${agents} actions=${actions} confirm=${confirm} onClose=${() => setMoving(false)}
      onDone=${(msg) => { setMoving(false); toast(msg, false); setTick((n) => n + 1); }} />`}
  </div>`;
}
