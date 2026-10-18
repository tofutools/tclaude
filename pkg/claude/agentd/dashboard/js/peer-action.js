import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { PEER_ACTION_EVENT } from './peer-view-limits.js';
import { openRemoteAgentSpawn } from './agent-spawn-controller.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);


// The peer's operator-message limits (dashboard_operator_message.go).
const MESSAGE_MAX = 16 * 1024;
const SUBJECT_MAX = 256;
const SAFE_AGENT_ID = /^agt_[A-Za-z0-9]{4,64}$/;

function errText(error) { return error?.message || String(error); }

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
// /api/peer/{node}/* itself.
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
    move: (id, group) => call('POST', agent(id, 'move'), { group }),
    // moveDirect lets the receiving node (this one) land it at once when its
    // permissions allow; otherwise it waits as an offer (tcl-u4yodp).
    moveDirect: (id, body) => call('POST', agent(id, 'move'), body),
    // teleport asks the peer to teleport (or, with clone, copy) its agent to
    // this node: {group, clone?, note?}.
    teleport: (id, body) => call('POST', agent(id, 'teleport'), body),
    message: (to, { subject = '', body }) => call('POST', `${api}operator-message`, { to, subject, body }),
  });
}

let sharedActions = null;
function defaultActions() { return (sharedActions ||= createPeerActionActions()); }

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
    <p class="modal-hint">Delivers one immediate message to a single agent in “${req.group}”, sent by ${req.node} as you, the operator. ${req.node} grants messaging one agent at a time.</p>
    ${members.length === 0 ? html`<div class="cleanup-empty">${req.node} shows no agents in ${req.group} to message.</div>` : html`
      <label class="cron-create-row"><span class="cron-create-label">To</span><select id="peer-message-to" value=${to} onChange=${(e) => setTo(e.currentTarget.value)}>
        <option value="">pick an agent…</option>
        ${members.map((m) => html`<option key=${m.agent} value=${m.agent}>${m.label}</option>`)}
      </select></label>
      <label class="cron-create-row"><span class="cron-create-label">Subject</span><input id="peer-message-subject" type="text" value=${subject}
        placeholder="optional, shows in inbox listings" autocomplete="off" spellcheck="false" onInput=${(e) => setSubject(e.currentTarget.value)} /></label>
      <label class="cron-create-row"><span class="cron-create-label">Body</span><textarea id="peer-message-body" rows="4" value=${body}
        placeholder="message text (required)" spellcheck="false" onInput=${(e) => setBody(e.currentTarget.value)}></textarea></label>
      <p class=${bytes > MESSAGE_MAX ? 'cleanup-hint fa-danger' : 'cleanup-hint'}>${bytes} / ${MESSAGE_MAX} bytes</p>`}
    <div class="modal-buttons"><button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      <span class="spacer"></span>
      ${members.length > 0 && html`<button id="peer-message-send" type="button" class="primary" disabled=${busy || !ok} onClick=${send}>${busy ? 'Sending…' : 'Send'}</button>`}</div>
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

// RemoteSpawnOpener hands a spawn request to the regular spawn dialog in
// remote mode (agent-spawn-remote.js) and closes itself: a spawn on a peer
// uses the same dialog as here, with what the peer decides disabled.
function RemoteSpawnOpener({ req, snapshot, toast, openSpawn, onClose }) {
  useEffect(() => {
    void openSpawn({ nodeId: req.nodeId, node: req.node, groupName: req.group, snapshot, toast });
    onClose();
  }, []);
  return null;
}

// PeerActionDialog shows the dialog for one peer action request; req carries
// the node's name and ID. peerView says what the peer shares, groups are its
// snapshot's groups (message recipients), snapshot its whole snapshot (the
// spawn dialog's groups and harnesses).
export function PeerActionDialog({ req, peerView, groups, snapshot = null, actions, confirm = shellConfirm, toast = shellToast, openSpawn = openRemoteAgentSpawn, onClose }) {
  if (req.action === 'spawn') return html`<${RemoteSpawnOpener} req=${req} snapshot=${snapshot || { groups }} toast=${toast} openSpawn=${openSpawn} onClose=${onClose} />`;
  if (req.action === 'message') return html`<${MessageDialog} req=${req} members=${groupMembers(groups, req.group)} actions=${actions} toast=${toast} onClose=${onClose} />`;
  return null;
}

// PeerActionHost listens for peer action clicks on a peer view and shows the
// matching dialog. snapshot supplies peer_view (what the peer shares).
export function PeerActionHost({ snapshot, remote = globalThis.__tclaudeRemoteNode, actions = defaultActions(), confirm = shellConfirm, toast = shellToast, openSpawn = openRemoteAgentSpawn, doc = globalThis.document }) {
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
  return html`<${PeerActionDialog} req=${{ ...req, node, nodeId: remote.id }} peerView=${snap?.peer_view} groups=${snap?.groups} snapshot=${snap} actions=${actions} confirm=${confirm} toast=${toast} openSpawn=${openSpawn} onClose=${() => setReq(null)} />`;
}
