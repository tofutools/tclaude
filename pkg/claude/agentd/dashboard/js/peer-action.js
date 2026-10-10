import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { PEER_ACTION_EVENT } from './peer-view-limits.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);


const SPAWN_POLL_MS = 2000;
const BRIEF_MAX = 8 * 1024;

// AGENT_ACTIONS are the lifecycle actions a peer can grant; feature is the
// peer_view key that says whether it does. Move and teleport always bring
// the agent to this node.
export const AGENT_ACTIONS = Object.freeze([
  { id: 'stop', feature: 'lifecycle.stop', label: 'Stop' },
  { id: 'retire', feature: 'lifecycle.retire', label: 'Retire' },
  { id: 'clone', feature: 'lifecycle.clone', label: 'Clone' },
  { id: 'move', feature: 'lifecycle.move', label: 'Move here' },
  { id: 'teleport', feature: 'lifecycle.teleport', label: 'Teleport here' },
]);

function errText(error) { return error?.message || String(error); }

export function sharedFeatures(peerView) {
  return new Set(Array.isArray(peerView?.included) ? peerView.included : []);
}

export class PeerActionError extends Error {
  constructor(status, body) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code || '';
  }
}

// createPeerActionActions sends the peer action routes. On a peer view
// remote-node.js forwards /api/* to /api/peer/{id}/*, so these are the
// peer's routes; /api/federation/links stays local and lists this node's
// groups (the move/teleport destinations).
export function createPeerActionActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(method, url, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetchImpl(url, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) throw new PeerActionError(res.status, data);
    return data;
  }
  const agent = (id, tail) => `/api/agents/${encodeURIComponent(id)}/${tail}`;
  return Object.freeze({
    stop: (id, force) => call('POST', agent(id, 'stop') + (force ? '?force=1' : '')),
    retire: (id) => call('POST', agent(id, 'retire'), {}),
    clone: (id, { followUp = '', noCopyConv = false } = {}) => call('POST', agent(id, 'clone'), { ...(followUp ? { follow_up: followUp } : {}), ...(noCopyConv ? { no_copy_conv: true } : {}) }),
    move: (id, group) => call('POST', agent(id, 'move'), { group }),
    teleport: (id, { group, note = '', clone = false }) => call('POST', agent(id, 'teleport'), { group, ...(note ? { note } : {}), ...(clone ? { clone: true } : {}) }),
    spawn: (group, { brief, name = '', role = '', profile = '' }) => call('POST', `/api/groups/${encodeURIComponent(group)}/spawn`, { brief, ...(name ? { name } : {}), ...(role ? { role } : {}), ...(profile ? { profile } : {}) }),
    spawnStatus: (id) => call('GET', `/api/spawn-requests/${encodeURIComponent(id)}`),
    localGroups: async () => ((await call('GET', '/api/federation/links'))?.groups || []).map((g) => g?.name).filter(Boolean).sort(),
  });
}

let sharedActions = null;
function defaultActions() { return (sharedActions ||= createPeerActionActions()); }

// consequence spells out what an agent action does on the peer.
export function consequence(action, { agent, node, force, group, clone }) {
  switch (action) {
    case 'stop': return force
      ? `${agent} on ${node} is killed at once: its tmux session ends without a clean exit, and unsaved work in its turn is lost.`
      : `${agent} on ${node} is asked to exit cleanly and its tmux session ends. It can be woken again on ${node}.`;
    case 'retire': return `${agent} on ${node} is retired: demoted to a plain conversation, leaving its groups and losing its permission grants (its worktree is kept). ${node}'s operator can reinstate it.`;
    case 'clone': return `A sibling of ${agent} starts on ${node}, inheriting its identity (groups, permissions, ownership). The original keeps running.`;
    case 'move': return `${agent} leaves ${node} and is offered to this node with its history, to land in group ${group}. Once it lands, the original on ${node} is retired.`;
    case 'teleport': return `${agent} teleports from ${node} to this node, continuing its history in group ${group}${clone ? '; the original keeps running on ' + node : '; the original on ' + node + ' is retired'}.`;
    default: return '';
  }
}

function AgentDialog({ req, shared, actions, confirm, toast, onClose }) {
  const available = AGENT_ACTIONS.filter((a) => shared.has(a.feature));
  const [action, setAction] = useState(available.some((a) => a.id === req.action) ? req.action : available[0]?.id || '');
  const [force, setForce] = useState(false);
  const [followUp, setFollowUp] = useState('');
  const [noCopy, setNoCopy] = useState(false);
  const [groups, setGroups] = useState(null);
  const [group, setGroup] = useState('');
  const [note, setNote] = useState('');
  const [keep, setKeep] = useState(false);
  const [busy, setBusy] = useState(false);
  const toHere = action === 'move' || action === 'teleport';
  useEffect(() => {
    if (!toHere || groups) return;
    actions.localGroups().then((g) => { setGroups(g); setGroup((cur) => cur || g[0] || ''); }).catch((e) => setGroups({ error: errText(e) }));
  }, [toHere]);
  const label = req.label || req.agent;
  const run = () => {
    const text = consequence(action, { agent: label, node: req.node, force, group, clone: keep });
    const send = () => {
      if (action === 'stop') return actions.stop(req.agent, force);
      if (action === 'retire') return actions.retire(req.agent);
      if (action === 'clone') return actions.clone(req.agent, { followUp: followUp.trim(), noCopyConv: noCopy });
      if (action === 'move') return actions.move(req.agent, group);
      return actions.teleport(req.agent, { group, note: note.trim(), clone: keep });
    };
    const title = AGENT_ACTIONS.find((a) => a.id === action)?.label || action;
    setBusy(true);
    return confirm({ title: `${title}: ${label} on ${req.node}?`, body: text, okLabel: title, busyLabel: 'Sending…', action: send })
      .then((res) => {
        if (!res) return;
        toast(toHere ? `${title} offered: ${label} comes to group ${group} on this node (follow it with tclaude federation moves)` : `${title} sent to ${req.node} for ${label}`, false);
        onClose();
      })
      .catch((e) => toast(`${title} failed: ${e?.status === 403 ? `${req.node} refused it — ${errText(e)}` : errText(e)}`, true))
      .finally(() => setBusy(false));
  };
  const needGroup = toHere && !group;
  return html`<${Overlay} id="peer-action-modal" labelledby="peer-action-title" onClose=${onClose} blocked=${busy}>
    <h3 id="peer-action-title">${label} on ${req.node}</h3>
    ${available.length === 0 ? html`<div class="muted">${req.node} does not let you act on its agents.</div>` : html`
      <div class="peer-action-choices" role="radiogroup">
        ${available.map((a) => html`<label key=${a.id}><input type="radio" name="peer-action" value=${a.id} checked=${action === a.id} onChange=${() => setAction(a.id)} /> ${a.label}</label>`)}
      </div>
      ${action === 'stop' && html`<label class="peer-action-opt"><input id="peer-action-force" type="checkbox" checked=${force} onChange=${(e) => setForce(e.currentTarget.checked)} /> Force kill (no clean exit)</label>`}
      ${action === 'clone' && html`
        <label class="peer-action-opt">Follow-up message for the clone <input id="peer-action-followup" value=${followUp} onInput=${(e) => setFollowUp(e.currentTarget.value)} /></label>
        <label class="peer-action-opt"><input type="checkbox" checked=${noCopy} onChange=${(e) => setNoCopy(e.currentTarget.checked)} /> Start without its conversation</label>`}
      ${toHere && html`
        <label class="peer-action-opt">Group on this node
          ${groups?.error ? html`<span class="fa-danger">${groups.error}</span>` : !groups ? html`<span class="muted">loading…</span>` : html`<select id="peer-action-group" value=${group} onChange=${(e) => setGroup(e.currentTarget.value)}>${groups.map((g) => html`<option key=${g} value=${g}>${g}</option>`)}</select>`}</label>
        ${action === 'teleport' && html`
          <label class="peer-action-opt">Note <input id="peer-action-note" value=${note} onInput=${(e) => setNote(e.currentTarget.value)} /></label>
          <label class="peer-action-opt"><input id="peer-action-keep" type="checkbox" checked=${keep} onChange=${(e) => setKeep(e.currentTarget.checked)} /> Keep the original running (clone over)</label>`}`}
      <div class="peer-action-consequence muted">${consequence(action, { agent: label, node: req.node, force, group: group || '…', clone: keep })}</div>`}
    <div class="modal-buttons">
      <span class="spacer"></span>
      <button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      ${available.length > 0 && html`<button id="peer-action-submit" type="button" class="confirm-danger" disabled=${busy || needGroup} onClick=${run}>${AGENT_ACTIONS.find((a) => a.id === action)?.label || 'Send'}…</button>`}
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude federation action ${action} --node ${req.nodeId} --agent ${req.agent}</code></div>
  </${Overlay}>`;
}

function SpawnDialog({ req, actions, confirm, toast, onClose, timers }) {
  const [brief, setBrief] = useState('');
  const [name, setName] = useState('');
  const [role, setRole] = useState('');
  const [profile, setProfile] = useState('');
  const [busy, setBusy] = useState(false);
  const [request, setRequest] = useState(null);
  const pending = request && (request.status === 'pending' || request.status === 'launching');
  useEffect(() => {
    if (!pending) return undefined;
    let off = false;
    const t = timers.setTimeout(() => {
      actions.spawnStatus(request.id).then((r) => { if (!off) setRequest(r); })
        .catch((e) => { if (!off) setRequest({ ...request, status: 'unknown', reason: errText(e) }); });
    }, SPAWN_POLL_MS);
    return () => { off = true; timers.clearTimeout(t); };
  }, [request]);
  const bytes = new TextEncoder().encode(brief).length;
  const run = () => confirm({
    title: `Spawn an agent in ${req.group} on ${req.node}?`,
    body: `${req.node} starts a new agent in its group ${req.group} with your brief, under ${req.node}'s own launch policy for you (its profile, harness, model and directory; you cannot override them)${profile ? `, using profile ${profile} if ${req.node} allows it` : ''}. It counts against the live cap ${req.node} gave you.`,
    okLabel: 'Spawn',
    busyLabel: 'Sending…',
    action: () => actions.spawn(req.group, { brief, name: name.trim(), role: role.trim(), profile: profile.trim() }),
  }).then((r) => { if (r) setRequest(r); })
    .catch((e) => toast(`Spawn failed: ${e?.code === 'profile_not_allowed' ? `${req.node} does not allow that profile — ${errText(e)}` : e?.code === 'spawn_capacity' ? `${req.node} has no spawn capacity for you right now` : errText(e)}`, true));
  return html`<${Overlay} id="peer-spawn-modal" labelledby="peer-spawn-title" onClose=${onClose} blocked=${busy}>
    <h3 id="peer-spawn-title">Spawn in ${req.group} on ${req.node}</h3>
    ${request ? html`<div id="peer-spawn-status" class=${request.status === 'denied' || request.status === 'expired' || request.status === 'unknown' ? 'fa-danger' : ''}>
        Request ${request.id}: <b>${request.status}</b>${request.result_agent ? html` — agent <code>${request.result_agent}</code>` : ''}${request.reason ? ` — ${request.reason}` : ''}
        ${pending && html`<div class="muted">${req.node} decides under its launch policy; this updates on its own.</div>`}
      </div>` : html`
      <label class="peer-action-opt peer-action-brief">Brief <textarea id="peer-spawn-brief" rows="5" value=${brief} onInput=${(e) => setBrief(e.currentTarget.value)}></textarea></label>
      <span class=${bytes > BRIEF_MAX ? 'fa-danger' : 'muted'}>${bytes} / ${BRIEF_MAX} bytes</span>
      <label class="peer-action-opt">Name <input id="peer-spawn-name" value=${name} onInput=${(e) => setName(e.currentTarget.value)} /></label>
      <label class="peer-action-opt">Role <input id="peer-spawn-role" value=${role} onInput=${(e) => setRole(e.currentTarget.value)} /></label>
      <label class="peer-action-opt">Profile <input id="peer-spawn-profile" value=${profile} placeholder="${req.node}'s default" onInput=${(e) => setProfile(e.currentTarget.value)} /></label>`}
    <div class="modal-buttons">
      <span class="spacer"></span>
      <button type="button" onClick=${onClose}>${request ? 'Close' : 'Cancel'}</button>
      ${!request && html`<button id="peer-spawn-submit" type="button" class="confirm-danger" disabled=${busy || !brief.trim() || bytes > BRIEF_MAX} onClick=${() => { setBusy(true); run().finally(() => setBusy(false)); }}>Spawn…</button>`}
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude federation action spawn --node ${req.nodeId} --group ${req.group} --brief …</code>, then <code>tclaude federation action spawn-status --node ${req.nodeId} --job ID</code></div>
  </${Overlay}>`;
}

// PeerActionHost listens for peer action clicks on a peer view and shows the
// matching dialog. snapshot supplies peer_view (what the peer shares).
export function PeerActionHost({ snapshot, remote = globalThis.__tclaudeRemoteNode, actions = defaultActions(), confirm = shellConfirm, toast = shellToast, timers = globalThis, doc = globalThis.document }) {
  const [req, setReq] = useState(null);
  useEffect(() => {
    if (!remote?.id || !doc) return undefined;
    const on = (event) => setReq(event.detail || null);
    doc.addEventListener(PEER_ACTION_EVENT, on);
    return () => doc.removeEventListener(PEER_ACTION_EVENT, on);
  }, [remote?.id]);
  if (!req || !remote?.id) return null;
  const node = doc?.documentElement?.dataset?.remoteNodeName || remote.id.slice(0, 13);
  const full = { ...req, node, nodeId: remote.id };
  const close = () => setReq(null);
  if (req.action === 'spawn') return html`<${SpawnDialog} req=${full} actions=${actions} confirm=${confirm} toast=${toast} timers=${timers} onClose=${close} />`;
  return html`<${AgentDialog} req=${full} shared=${sharedFeatures(snapshot?.value?.peer_view)} actions=${actions} confirm=${confirm} toast=${toast} onClose=${close} />`;
}
