import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { PEER_ACTION_EVENT } from './peer-view-limits.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);


const SPAWN_POLL_MS = 2000;
const BRIEF_MAX = 8 * 1024;
// The peer's operator-message limits (dashboard_operator_message.go).
const MESSAGE_MAX = 16 * 1024;
const SUBJECT_MAX = 256;
const SAFE_AGENT_ID = /^agt_[A-Za-z0-9]{4,64}$/;

// AGENT_ACTIONS are the lifecycle actions a peer can grant; feature is the
// peer_view key that says whether it does. Move and teleport always bring
// the agent to this node.
export const AGENT_ACTIONS = Object.freeze([
  { id: 'stop', feature: 'lifecycle.stop', label: 'Stop' },
  { id: 'resume', feature: 'lifecycle.resume', label: 'Wake' },
  { id: 'restart', feature: 'lifecycle.restart', label: 'Restart' },
  { id: 'sandbox-restart', feature: 'lifecycle.sandbox-restart', label: 'Sandbox restart' },
  { id: 'retire', feature: 'lifecycle.retire', label: 'Retire' },
  { id: 'clone', feature: 'lifecycle.clone', label: 'Clone' },
  { id: 'move', feature: 'lifecycle.move', label: 'Move here' },
  { id: 'teleport', feature: 'lifecycle.teleport', label: 'Teleport here' },
]);

function errText(error) { return error?.message || String(error); }

const RECEIVE_SLUGS = new Set(['agents.receive', 'agents.teleport.receive']);

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
// peer's routes; the merged all-nodes view passes node to address
// /api/peer/{node}/* itself. /api/federation/links stays local and lists this
// node's groups (the move/teleport destinations).
export function createPeerActionActions({ fetchImpl = (...a) => globalThis.fetch(...a), node = '' } = {}) {
  const api = node ? `/api/peer/${encodeURIComponent(node)}/` : '/api/';
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
  const agent = (id, tail) => `${api}agents/${encodeURIComponent(id)}/${tail}`;
  return Object.freeze({
    stop: (id, force) => call('POST', agent(id, 'stop') + (force ? '?force=1' : '')),
    resume: (id) => call('POST', agent(id, 'resume')),
    restart: (id) => call('POST', agent(id, 'restart')),
    sandboxRestart: (id, direction) => call('POST', agent(id, 'sandbox-restart'), { action: direction === 'unlock' ? 'unlock' : 'restore' }),
    retire: (id) => call('POST', agent(id, 'retire'), {}),
    clone: (id, { followUp = '', noCopyConv = false } = {}) => call('POST', agent(id, 'clone'), { ...(followUp ? { follow_up: followUp } : {}), ...(noCopyConv ? { no_copy_conv: true } : {}) }),
    move: (id, group) => call('POST', agent(id, 'move'), { group }),
    teleport: (id, { group, note = '', clone = false }) => call('POST', agent(id, 'teleport'), { group, ...(note ? { note } : {}), ...(clone ? { clone: true } : {}) }),
    spawn: (group, { brief, name = '', role = '', profile = '' }) => call('POST', `${api}groups/${encodeURIComponent(group)}/spawn`, { brief, ...(name ? { name } : {}), ...(role ? { role } : {}), ...(profile ? { profile } : {}) }),
    spawnStatus: (id) => call('GET', `${api}spawn-requests/${encodeURIComponent(id)}`),
    message: (to, { subject = '', body }) => call('POST', `${api}operator-message`, { to, subject, body }),
    // localGroups lists this node's groups, marking those with a link that
    // receives agents from peer (an agents.receive or agents.teleport.receive
    // grant). Unscoped grants and unrestricted trust are not listed as links,
    // so the other groups stay selectable too.
    localGroups: async (peer) => ((await call('GET', '/api/federation/links'))?.groups || [])
      .filter((g) => g?.name)
      .map((g) => ({ name: g.name, receives: (g.federation_links || []).some((l) => l?.peer === peer && (l.slugs || []).some((x) => RECEIVE_SLUGS.has(x))) }))
      .sort((a, b) => (b.receives - a.receives) || a.name.localeCompare(b.name)),
  });
}

let sharedActions = null;
function defaultActions() { return (sharedActions ||= createPeerActionActions()); }

// consequence spells out what an agent action does on the peer.
// MOVE_LANDS / TELEPORT_LANDS say where an agent arriving here starts: this
// node decides, and the agent's path on the peer is only a hint.
const MOVE_LANDS = 'It waits in Fleet → Offers, where you choose its starting directory on accept: a matching Fleet repo, its own path if that exists here, or the group\'s default dir are offered.';
const TELEPORT_LANDS = 'Where this node lands teleports in that group automatically, it starts in the first fit (a matching Fleet repo, its own path if that exists here, the group\'s default dir, then the landing policy); otherwise it waits in Fleet → Offers for you to choose.';

export function consequence(action, { agent, node, force, group, clone, direction }) {
  switch (action) {
    case 'stop': return force
      ? `${agent} on ${node} is killed at once: its tmux session ends without a clean exit, and unsaved work in its turn is lost.`
      : `${agent} on ${node} is asked to exit cleanly and its tmux session ends. It can be woken again on ${node}.`;
    case 'retire': return `${agent} on ${node} is retired: its running session is asked to exit, and it is demoted to a plain conversation, leaving its groups and losing its permission grants (its worktree is kept). ${node}'s operator can reinstate it.`;
    case 'resume': return `${agent} starts again on ${node}, resuming its conversation under ${node}'s own launch configuration. Recreating a missing launch directory or switching a Codex drive is left to ${node}'s operator.`;
    case 'restart': return `${agent} is stopped and started again on ${node} with the same conversation, re-resolving its sandbox rules. ${node} refuses unless the agent is fully idle, with no background agents or shell commands.`;
    case 'sandbox-restart': return direction === 'unlock'
      ? `${agent} is restarted on ${node} with its sandbox OFF: full access to ${node}'s machine — its files, logins, keys and network — until its sandbox is restored. ${node} allows this only for a peer it trusts unrestricted, and refuses unless the agent is fully idle.`
      : `${agent} is restarted on ${node} under its normal sandbox configuration. ${node} refuses unless the agent is fully idle.`;
    case 'clone': return `A sibling of ${agent} starts on ${node}, inheriting its identity (groups, permissions, ownership). The original keeps running.`;
    case 'move': return `${agent} leaves ${node} and is offered to this node with its history, to land in group ${group}. Once it lands, the original on ${node} is retired. ${MOVE_LANDS}`;
    case 'teleport': return `${agent} teleports from ${node} to this node, continuing its history in group ${group}${clone ? '; the original keeps running on ' + node : '; the original on ' + node + ' is retired'}. ${TELEPORT_LANDS}`;
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
  const [direction, setDirection] = useState(req.direction === 'unlock' ? 'unlock' : 'restore');
  const [busy, setBusy] = useState(false);
  const toHere = action === 'move' || action === 'teleport';
  useEffect(() => {
    if (!toHere || groups) return;
    // Preselect only when exactly one group receives from this peer.
    actions.localGroups(req.nodeId).then((g) => { setGroups(g); const rx = g.filter((x) => x.receives); setGroup((cur) => cur || (rx.length === 1 ? rx[0].name : '')); }).catch((e) => setGroups({ error: errText(e) }));
  }, [toHere]);
  const label = req.label || req.agent;
  const run = () => {
    const text = consequence(action, { agent: label, node: req.node, force, group, clone: keep, direction });
    const send = () => {
      if (action === 'stop') return actions.stop(req.agent, force);
      if (action === 'resume') return actions.resume(req.agent);
      if (action === 'restart') return actions.restart(req.agent);
      if (action === 'sandbox-restart') return actions.sandboxRestart(req.agent, direction);
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
        toast(toHere ? `${title} offered: ${label} comes to group ${group} on this node (follow it with tclaude federation moves)${res?.source_repo ? `; repo hint ${res.source_repo}` : ''}` : `${title} sent to ${req.node} for ${label}`, false);
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
      ${action === 'sandbox-restart' && html`<label class="peer-action-opt">Sandbox
        <select id="peer-action-sandbox" value=${direction} onChange=${(e) => setDirection(e.currentTarget.value)}>
          <option value="restore">restore its normal sandbox</option>
          <option value="unlock">turn its sandbox OFF (unrestricted trust only)</option>
        </select></label>`}
      ${action === 'stop' && html`<label class="peer-action-opt"><input id="peer-action-force" type="checkbox" checked=${force} onChange=${(e) => setForce(e.currentTarget.checked)} /> Force kill (no clean exit)</label>`}
      ${action === 'clone' && html`
        <label class="peer-action-opt">Follow-up message for the clone <input id="peer-action-followup" value=${followUp} onInput=${(e) => setFollowUp(e.currentTarget.value)} /></label>
        <label class="peer-action-opt"><input type="checkbox" checked=${noCopy} onChange=${(e) => setNoCopy(e.currentTarget.checked)} /> Start without its conversation</label>`}
      ${toHere && html`
        <label class="peer-action-opt">Group on this node
          ${groups?.error ? html`<span class="fa-danger">${groups.error}</span>` : !groups ? html`<span class="muted">loading…</span>` : html`<select id="peer-action-group" value=${group} onChange=${(e) => setGroup(e.currentTarget.value)}>
            <option value="">pick a group…</option>
            ${groups.some((g) => g.receives) && html`<optgroup label=${`receives agents from ${req.node}`}>${groups.filter((g) => g.receives).map((g) => html`<option key=${g.name} value=${g.name}>${g.name}</option>`)}</optgroup>`}
            ${groups.some((g) => !g.receives) && html`<optgroup label=${`needs an agents.receive grant for ${req.node}`}>${groups.filter((g) => !g.receives).map((g) => html`<option key=${g.name} value=${g.name}>${g.name}</option>`)}</optgroup>`}
          </select>`}</label>
        ${Array.isArray(groups) && group && !groups.find((g) => g.name === group)?.receives && html`<div class="fa-warn">${group} has no listed grant receiving agents from ${req.node}; the agent lands only if an unscoped grant or unrestricted trust covers it.</div>`}
        ${action === 'teleport' && html`
          <label class="peer-action-opt">Note <input id="peer-action-note" value=${note} onInput=${(e) => setNote(e.currentTarget.value)} /></label>
          <label class="peer-action-opt"><input id="peer-action-keep" type="checkbox" checked=${keep} onChange=${(e) => setKeep(e.currentTarget.checked)} /> Keep the original running (clone over)</label>`}`}
      <div class="peer-action-consequence muted">${consequence(action, { agent: label, node: req.node, force, group: group || '…', clone: keep, direction })}</div>`}
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

// MessageDialog sends one operator message to a member of the peer's group,
// as `tclaude federation action message` does. The peer delivers it only to an
// agent it shares messaging for.
function MessageDialog({ req, members, actions, toast, onClose }) {
  const [to, setTo] = useState(members.length === 1 ? members[0].agent : '');
  const [subject, setSubject] = useState('');
  const [body, setBody] = useState('');
  const [busy, setBusy] = useState(false);
  const bytes = new TextEncoder().encode(body).length;
  const ok = to && body.trim() && bytes <= MESSAGE_MAX && [...subject].length <= SUBJECT_MAX;
  const who = members.find((m) => m.agent === to)?.label || to;
  const send = () => {
    setBusy(true);
    actions.message(to, { subject: subject.trim(), body })
      .then(() => { toast(`Message sent to ${who} on ${req.node}`, false); onClose(); })
      .catch((e) => toast(`Message failed: ${e?.status === 403 ? `${req.node} refused it — ${errText(e)}` : errText(e)}`, true))
      .finally(() => setBusy(false));
  };
  return html`<${Overlay} id="peer-message-modal" labelledby="peer-message-title" onClose=${onClose} blocked=${busy}>
    <h3 id="peer-message-title">Message ${req.group} on ${req.node}</h3>
    ${members.length === 0 ? html`<div class="muted">${req.node} shows no agents in ${req.group} to message.</div>` : html`
      <label class="peer-action-opt">To <select id="peer-message-to" value=${to} onChange=${(e) => setTo(e.currentTarget.value)}>
        <option value="">pick an agent…</option>
        ${members.map((m) => html`<option key=${m.agent} value=${m.agent}>${m.label}</option>`)}
      </select></label>
      <label class="peer-action-opt">Subject <input id="peer-message-subject" value=${subject} onInput=${(e) => setSubject(e.currentTarget.value)} /></label>
      <label class="peer-action-opt peer-action-brief">Message <textarea id="peer-message-body" rows="5" value=${body} onInput=${(e) => setBody(e.currentTarget.value)}></textarea></label>
      <span class=${bytes > MESSAGE_MAX ? 'fa-danger' : 'muted'}>${bytes} / ${MESSAGE_MAX} bytes · one agent at a time, delivered by ${req.node}</span>`}
    <div class="modal-buttons">
      <span class="spacer"></span>
      <button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      ${members.length > 0 && html`<button id="peer-message-send" type="button" class="primary" disabled=${busy || !ok} onClick=${send}>Send</button>`}
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude federation action message --node ${req.nodeId} --agent ${to || 'AGENT'} --body …</code></div>
  </${Overlay}>`;
}

// groupMembers lists the messageable agents of one of the peer's groups, as
// its snapshot shows them.
export function groupMembers(groups, name) {
  const g = (groups || []).find((x) => x?.name === name);
  return (g?.members || [])
    .filter((m) => SAFE_AGENT_ID.test(m?.agent_id || ''))
    .map((m) => ({ agent: m.agent_id, label: m.title || m.agent_id }));
}

// PeerActionDialog shows the dialog for one peer action request; req carries
// the node's name and ID. peerView says what the peer shares, groups are its
// snapshot's groups (message recipients).
export function PeerActionDialog({ req, peerView, groups, actions, confirm = shellConfirm, toast = shellToast, timers = globalThis, onClose }) {
  if (req.action === 'spawn') return html`<${SpawnDialog} req=${req} actions=${actions} confirm=${confirm} toast=${toast} timers=${timers} onClose=${onClose} />`;
  if (req.action === 'message') return html`<${MessageDialog} req=${req} members=${groupMembers(groups, req.group)} actions=${actions} toast=${toast} onClose=${onClose} />`;
  return html`<${AgentDialog} req=${req} shared=${sharedFeatures(peerView)} actions=${actions} confirm=${confirm} toast=${toast} onClose=${onClose} />`;
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
  const snap = snapshot?.value;
  return html`<${PeerActionDialog} req=${{ ...req, node, nodeId: remote.id }} peerView=${snap?.peer_view} groups=${snap?.groups} actions=${actions} confirm=${confirm} toast=${toast} timers=${timers} onClose=${() => setReq(null)} />`;
}
