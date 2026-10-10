import { h } from 'preact';
import { useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { remoteNodeID } from './skynet-model.js';
import { skynetState } from './skynet-state.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);

// The human inbox of the operator's other own nodes (tcl-99b5ir gap 2): each
// node this one trusts unrestricted serves its agents' human notifications and
// pending ask-human requests at /api/peer/{id}/human-inbox. The snapshot poll
// (refresh.js) is the only reader, at a relaxed cadence — never per-node full
// rate — so this module owns no timer. Everything a node returns is shown as
// text only.
export const INBOX_EVERY_MS = 60_000;
export const INBOX_HIDDEN_EVERY_MS = 300_000;

// remoteInbox maps a node ID to { label, messages, access_requests, error }.
export const remoteInbox = signal({});

let lastRead = -Infinity;

export function resetRemoteInboxForTest() {
  lastRead = -Infinity;
  remoteInbox.value = {};
}

// inboxPeers are the nodes worth asking: online and trusted unrestricted
// (anything less is refused by the peer anyway).
export function inboxPeers(fleet = skynetState.fleet.value) {
  return (fleet?.peers || []).filter((p) => p.level === 'unrestricted' && p.online);
}

// claimRemoteInboxRead returns the nodes this tick should read, claiming the
// slot, or [] — only on this node's own dashboard, at most every minute while
// visible and every five while hidden. Nodes that dropped out are forgotten.
export function claimRemoteInboxRead(now = Date.now(), { remote = remoteNodeID(), fleet = skynetState.fleet.value, hidden = !!globalThis.document?.hidden } = {}) {
  if (remote) return [];
  const peers = inboxPeers(fleet);
  const keep = new Set(peers.map((p) => p.id));
  const cur = remoteInbox.value;
  if (Object.keys(cur).some((id) => !keep.has(id))) remoteInbox.value = Object.fromEntries(Object.entries(cur).filter(([id]) => keep.has(id)));
  if (!peers.length) return [];
  if (now - lastRead < (hidden ? INBOX_HIDDEN_EVERY_MS : INBOX_EVERY_MS)) return [];
  lastRead = now;
  return peers;
}

const peerURL = (id, tail) => `/api/peer/${encodeURIComponent(id)}/${tail}`;

async function call(fetchImpl, url, body) {
  const init = { credentials: 'same-origin', cache: 'no-store' };
  if (body !== undefined) Object.assign(init, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const res = await fetchImpl(url, init);
  let data = null;
  try { data = await res.json(); } catch (_) { data = null; }
  if (!res.ok) {
    const err = new Error(data?.error || `HTTP ${res.status}`);
    err.status = res.status; err.code = data?.code || '';
    throw err;
  }
  return data;
}

// readRemoteInboxes reads each node without holding the snapshot tick; a
// failed read keeps that node's previous items and notes the error.
export async function readRemoteInboxes(peers, fetchImpl = (...a) => globalThis.fetch(...a)) {
  await Promise.all(peers.map(async (p) => {
    try {
      const data = await call(fetchImpl, peerURL(p.id, 'human-inbox'));
      const messages = Array.isArray(data?.messages) ? data.messages.filter((m) => m && !m.read) : [];
      const requests = Array.isArray(data?.access_requests) ? data.access_requests.filter(Boolean) : [];
      remoteInbox.value = { ...remoteInbox.value, [p.id]: { label: p.name || p.id, messages, access_requests: requests, error: '' } };
    } catch (e) {
      const prev = remoteInbox.value[p.id] || { messages: [], access_requests: [] };
      remoteInbox.value = { ...remoteInbox.value, [p.id]: { ...prev, label: p.name || p.id, error: e.code === 'permission' ? '' : String(e.message || e) } };
    }
  }));
}

export function createRemoteInboxActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  return Object.freeze({
    reply: (node, id, body) => call(fetchImpl, peerURL(node, 'human-inbox/reply'), { id, body }),
    markRead: (node, id) => call(fetchImpl, peerURL(node, 'human-inbox/read'), { id }),
    decide: (node, id, decision) => call(fetchImpl, peerURL(node, `human-inbox/access/${encodeURIComponent(id)}`), { decision: decision === 'approve' ? 'approve' : 'deny' }),
  });
}

// dropItem removes an answered item at once; the next read confirms it.
function dropItem(node, kind, id) {
  const cur = remoteInbox.value[node];
  if (!cur) return;
  const next = kind === 'request'
    ? { ...cur, access_requests: cur.access_requests.filter((r) => r.id !== id) }
    : { ...cur, messages: cur.messages.filter((m) => m.id !== id) };
  remoteInbox.value = { ...remoteInbox.value, [node]: next };
}

export function remoteInboxCounts(inbox = remoteInbox.value) {
  let requests = 0; let messages = 0;
  for (const n of Object.values(inbox)) { requests += n.access_requests?.length || 0; messages += n.messages?.length || 0; }
  return { requests, messages };
}

function errText(e) { return e?.message || String(e); }

// approveConsequence spells out what one remote approval does.
export function approveConsequence(r, node) {
  const who = r.title || r.agent_id || 'an agent';
  return `Approves ${r.perm} once for ${who} on ${node}${r.path ? `: the blocked call ${r.path} goes through now` : ''}${r.target_group ? ` (group ${r.target_group})` : ''}${r.target_conv_title ? ` (target ${r.target_conv_title})` : ''}. Nothing lasting is granted; "always allow" stays on ${node}'s own dashboard.`;
}

function InboxReader({ item, onClose, actions, confirm, toast }) {
  const { node, label, kind, data } = item;
  const [reply, setReply] = useState('');
  const [busy, setBusy] = useState(false);
  const done = (msg) => { dropItem(node, kind, data.id); toast(msg, false); onClose(); };
  const fail = (what) => (e) => { setBusy(false); toast(`${what} failed: ${errText(e)}`, true); };
  const send = () => { setBusy(true); actions.reply(node, data.id, reply.trim()).then(() => done(`Replied to ${data.from_title || data.from_agent} on ${label}`), fail('Reply')); };
  const markRead = () => { setBusy(true); actions.markRead(node, data.id).then(() => done(`Marked read on ${label}`), fail('Mark read')); };
  const approve = () => confirm({
    title: `Approve ${data.perm} on ${label}?`,
    body: approveConsequence(data, label),
    okLabel: 'Approve once',
    busyLabel: 'Approving…',
    action: () => actions.decide(node, data.id, 'approve'),
  }).then((r) => { if (r) done(`Approved once on ${label}`); }).catch(fail('Approve'));
  const deny = () => { setBusy(true); actions.decide(node, data.id, 'deny').then(() => done(`Denied on ${label}`), fail('Deny')); };
  return html`<${Overlay} id="remote-inbox-reader" labelledby="remote-inbox-title" onClose=${onClose}>
    ${kind === 'request' ? html`
      <h3 id="remote-inbox-title">🔐 ${data.perm} <span class="muted">@${label}</span></h3>
      <div class="muted">${data.title || data.agent_id || 'agent'} is waiting${data.deadline ? ` · auto-declines ${new Date(data.deadline).toLocaleTimeString()}` : ''}</div>
      ${data.path && html`<div><code>${data.path}</code></div>`}
      ${(data.target_group || data.target_conv_title) && html`<div class="muted">target: ${data.target_group ? `group ${data.target_group}` : ''}${data.target_conv_title ? ` ${data.target_conv_title}` : ''}</div>`}
      ${data.body && html`<pre class="remote-inbox-body">${data.body}</pre>`}
      <div class="muted">One answer for this one request. "Always allow" stays on ${label}'s own dashboard.</div>
      <div class="modal-buttons"><span class="spacer"></span>
        <button type="button" id="remote-inbox-deny" disabled=${busy} onClick=${deny}>Deny</button>
        <button type="button" id="remote-inbox-approve" class="primary" disabled=${busy} onClick=${approve}>Approve once…</button>
        <button type="button" onClick=${onClose}>Close</button></div>`
    : html`
      <h3 id="remote-inbox-title">${data.subject || '(no subject)'} <span class="muted">@${label}</span></h3>
      <div class="muted">from ${data.from_title || data.from_agent || 'unknown'}${data.group ? ` · ${data.group}` : ''} · ${data.created_at ? new Date(data.created_at).toLocaleString() : ''}</div>
      <pre class="remote-inbox-body">${data.body}${data.truncated ? '\n…' : ''}</pre>
      ${data.attachments?.length > 0 && html`<div class="muted">Attachments stay on ${label}: ${data.attachments.join(', ')}</div>`}
      ${data.replyable
        ? html`<textarea id="remote-inbox-reply" rows="3" placeholder=${`Reply to ${data.from_title || 'the agent'} on ${label}`} value=${reply} onInput=${(e) => setReply(e.currentTarget.value)}></textarea>`
        : html`<div class="muted">Answer this one on ${label}'s own dashboard.</div>`}
      <div class="modal-buttons"><span class="spacer"></span>
        <button type="button" id="remote-inbox-read" disabled=${busy} onClick=${markRead}>Mark read</button>
        ${data.replyable && html`<button type="button" id="remote-inbox-send" class="primary" disabled=${busy || !reply.trim()} onClick=${send}>Send reply</button>`}
        <button type="button" onClick=${onClose}>Close</button></div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation human-inbox ${kind === 'request' ? `decide ${label} ID approve|deny` : `reply ${label} ${data.id} --body …`}</code></div>
  </${Overlay}>`;
}

// RemoteInboxSection lists the other nodes' unread notifications and pending
// requests in the Messages sidebar; it renders nothing while there are none.
export function RemoteInboxSection({ inbox = remoteInbox.value, actions = defaultActions(), confirm = shellConfirm, toast = shellToast }) {
  const [open, setOpen] = useState(null);
  const nodes = Object.entries(inbox).filter(([, n]) => (n.access_requests?.length || 0) + (n.messages?.length || 0) > 0 || n.error);
  if (!nodes.length) return null;
  return html`<div class="remote-inbox" id="remote-inbox">
    <div class="mailbox-section">Other nodes</div>
    ${nodes.map(([id, n]) => html`<div class="remote-inbox-node" key=${id} data-node=${id}>
      ${n.error && html`<div class="mailbox-row nested muted" title=${n.error}>@${n.label}: unreachable</div>`}
      ${(n.access_requests || []).map((r) => html`<button type="button" class="mailbox-row remote-inbox-item pending" key=${`r${r.id}`} data-request=${r.id}
        title=${`${r.perm} for ${r.title || r.agent_id} on ${n.label}`} onClick=${() => setOpen({ node: id, label: n.label, kind: 'request', data: r })}>🔐 ${r.perm} <span class="muted">@${n.label}</span></button>`)}
      ${(n.messages || []).map((m) => html`<button type="button" class="mailbox-row remote-inbox-item" key=${`m${m.id}`} data-message=${m.id}
        title=${`${m.subject || ''} — from ${m.from_title || m.from_agent || ''} on ${n.label}`} onClick=${() => setOpen({ node: id, label: n.label, kind: 'message', data: m })}>✉ ${m.subject || m.from_title || 'notification'} <span class="muted">@${n.label}</span></button>`)}
    </div>`)}
    ${open && html`<${InboxReader} item=${open} actions=${actions} confirm=${confirm} toast=${toast} onClose=${() => setOpen(null)} />`}
  </div>`;
}

let shared = null;
function defaultActions() { return (shared ||= createRemoteInboxActions()); }
