import { h } from 'preact';
import { useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { Button } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { remoteNodeID } from './fleet-model.js';
import { fleetState } from './fleet-state.js';
import { nodeSnapshot } from './node-api.js';
import { featureState, humanReadShared } from './peer-view-limits.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { requestJSON } from './request-json.js';
import { errText } from './cell-format.js';

const html = htm.bind(h);

// The human inbox of the operator's other nodes (tcl-99b5ir gap 2), read from
// each node that shares it with this one. A node that trusts this one
// unrestricted serves its agents' human notifications and pending ask-human
// requests at /api/peer/{id}/human-inbox, as does one granting human.inbox.read
// within those groups; one granting only node.messages.read
// shares the notifications through its Messages mailbox (the human folder),
// and its requests stay on its own dashboard. The snapshot poll (refresh.js)
// is the only reader, at a relaxed cadence — never per-node full rate — so
// this module owns no timer. Everything a node returns is shown as text only.
export const INBOX_EVERY_MS = 60_000;
export const INBOX_HIDDEN_EVERY_MS = 300_000;
// A node that refused the read (it shares neither) is asked again only this
// rarely, unless what it sends elsewhere says it now shares.
export const INBOX_REFUSED_RETRY_MS = 30 * 60_000;
// A refused node whose last known view still says it shares (a fused snapshot
// or map summary from before the refusal) is asked again sooner.
export const INBOX_REFUSED_SHARED_RETRY_MS = 5 * 60_000;
const INBOX_MESSAGES = 50;

// remoteInbox maps a node ID to { label, messages, access_requests, error,
// answers, markable }: answers when it trusts this node unrestricted or grants
// human.inbox.answer (reply, decide), markable when it lets this node mark its
// notifications read.
export const remoteInbox = signal({});

let lastRead = -Infinity;
// heard maps a node ID to the peer_view of its last inbox reply; refusedAt to
// when it last refused the read.
let heard = new Map();
let refusedAt = new Map();

export function resetRemoteInboxForTest() {
  lastRead = -Infinity;
  heard = new Map();
  refusedAt = new Map();
  remoteInbox.value = {};
}

// peerViewOf is what a peer shares with this node, as its replies carry it
// (the fused view's snapshot, the map's summary, the last inbox read), or
// null before anything here has heard. It is the peer's trust in this node;
// p.level is the other direction, which the peer's routes ignore.
export function peerViewOf(id) {
  return nodeSnapshot(id)?.peer_view || fleetState.summaries.value?.[id]?.summary?.peer_view || heard.get(id) || null;
}

const unrestricted = (view) => view?.level === 'unrestricted';
const humanInbox = (view) => unrestricted(view) || featureState('human.inbox', view) === 'shared';
const answersInbox = (view) => unrestricted(view) || featureState('human.inbox.answer', view) === 'shared';
const sharesInbox = (view) => humanInbox(view) || featureState('node.messages', view) === 'shared';

// inboxPeers are the nodes worth asking: online, and sharing their
// notifications with this node. A node not heard from yet is asked, and after
// a refusal left alone for a while, even while an older view says it shares.
export function inboxPeers(fleet = fleetState.fleet.value, { now = Date.now(), viewOf = peerViewOf } = {}) {
  return (fleet?.peers || []).filter((p) => {
    if (!p.online) return false;
    const view = viewOf(p.id);
    if (view && !sharesInbox(view)) return false;
    // A refusal holds whatever an older view says, for a while.
    const wait = view ? INBOX_REFUSED_SHARED_RETRY_MS : INBOX_REFUSED_RETRY_MS;
    return !(now - (refusedAt.get(p.id) ?? -Infinity) < wait);
  });
}

// claimRemoteInboxRead returns the nodes this tick should read, claiming the
// slot, or [] — only on this node's own dashboard, at most every minute while
// visible and every five while hidden. Nodes that dropped out are forgotten.
export function claimRemoteInboxRead(now = Date.now(), { remote = remoteNodeID(), fleet = fleetState.fleet.value, hidden = !!globalThis.document?.hidden, viewOf } = {}) {
  if (remote) return [];
  const peers = inboxPeers(fleet, { now, viewOf });
  const keep = new Set(peers.map((p) => p.id));
  const cur = remoteInbox.value;
  if (Object.keys(cur).some((id) => !keep.has(id))) remoteInbox.value = Object.fromEntries(Object.entries(cur).filter(([id]) => keep.has(id)));
  if (!peers.length) return [];
  if (now - lastRead < (hidden ? INBOX_HIDDEN_EVERY_MS : INBOX_EVERY_MS)) return [];
  lastRead = now;
  return peers;
}

const peerURL = (id, tail) => `/api/peer/${encodeURIComponent(id)}/${tail}`;

// call reads url, or posts body to it.
function call(fetchImpl, url, body) {
  return requestJSON(url, { method: body === undefined ? 'GET' : 'POST', body, cache: 'no-store', fetchImpl });
}

// mailboxNotification is a Messages-folder row as the inbox lists it: its
// files by name, and no reply (that takes unrestricted trust).
function mailboxNotification(m) {
  const files = Array.isArray(m.attachments) ? m.attachments : m.attachment ? [m.attachment] : [];
  return { id: m.id, from_agent: m.from_agent, from_title: m.from_title, group: m.group, subject: m.subject, body: m.body,
    created_at: m.created_at, read: !!m.read, replyable: false, attachments: files.map((f) => f?.filename || f?.name || '').filter(Boolean) };
}

// readInbox reads one node through the route its trust in this node opens:
// human-inbox when unrestricted or it grants human.inbox.read, else its human
// mailbox folder, whose reply says what it shares (one sharing the human
// inbox is read through human-inbox then).
async function readInbox(fetchImpl, p) {
  if (!humanInbox(peerViewOf(p.id))) {
    const data = await call(fetchImpl, peerURL(p.id, `mailbox?id=human&unread=1&page=1&page_size=${INBOX_MESSAGES}`));
    const view = data?.peer_view || null;
    if (view) heard.set(p.id, view);
    if (!humanInbox(view)) {
      if (view && !sharesInbox(view)) throw Object.assign(new Error('not shared'), { code: 'permission' });
      const messages = (Array.isArray(data?.messages) ? data.messages : []).filter((m) => m && !m.read).map(mailboxNotification);
      return { messages, access_requests: [], answers: false, markable: humanReadShared(view) };
    }
  }
  const data = await call(fetchImpl, peerURL(p.id, 'human-inbox'));
  // The reply's own view says what it shares; an older node sends none and
  // serves this route only to unrestricted trust.
  const view = data?.peer_view || { ...heard.get(p.id), level: 'unrestricted' };
  heard.set(p.id, view);
  const answers = answersInbox(view);
  return {
    messages: Array.isArray(data?.messages) ? data.messages.filter((m) => m && !m.read).map((m) => answers ? m : { ...m, replyable: false }) : [],
    access_requests: Array.isArray(data?.access_requests) ? data.access_requests.filter(Boolean) : [],
    answers, markable: unrestricted(view) || humanReadShared(view),
  };
}

// readRemoteInboxes reads each node without holding the snapshot tick; a
// failed read keeps that node's previous items and notes the error.
export async function readRemoteInboxes(peers, fetchImpl = (...a) => globalThis.fetch(...a), now = Date.now()) {
  await Promise.all(peers.map(async (p) => {
    try {
      const got = await readInbox(fetchImpl, p);
      refusedAt.delete(p.id);
      remoteInbox.value = { ...remoteInbox.value, [p.id]: { label: p.name || p.id, ...got, error: '' } };
    } catch (e) {
      const refused = e.code === 'permission' || e.status === 403;
      if (refused) { refusedAt.set(p.id, now); heard.delete(p.id); }
      const prev = remoteInbox.value[p.id] || { messages: [], access_requests: [] };
      // A node that no longer shares its inbox with this one shows nothing.
      remoteInbox.value = { ...remoteInbox.value, [p.id]: refused
        ? { label: p.name || p.id, messages: [], access_requests: [], error: '' }
        : { ...prev, label: p.name || p.id, error: String(e.message || e) } };
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

// approveConsequence spells out what one remote approval does.
export function approveConsequence(r, node) {
  const who = r.title || r.agent_id || 'an agent';
  const sudo = String(r.perm || '').startsWith('sudo.')
    ? ` A sudo request grants the permissions it names for the time it asked for.`
    : '';
  return `Approves ${r.perm} once for ${who} on ${node}${r.path ? `: the blocked call ${r.path} goes through now` : ''}${r.target_group ? ` (group ${r.target_group})` : ''}${r.target_conv_title ? ` (target ${r.target_conv_title})` : ''}, with all its effects on ${node}.${sudo} No "always allow" rule is written; that stays on ${node}'s own dashboard.`;
}

function InboxReader({ item, onClose, actions, confirm, toast }) {
  const { node, label, kind, data, markable = true, answers = true } = item;
  // An older node sends no approvable or answerable flag; it serves only
  // unrestricted trust. A request this node may not answer (the answer grant
  // missing in one of the requester's groups) is answered where it is.
  const decides = answers && data.answerable !== false;
  const approvable = data.approvable !== false;
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
  return html`<${Dialog} id="remote-inbox-reader" labelledby="remote-inbox-title" onClose=${onClose}>
    ${kind === 'request' ? html`
      <h3 id="remote-inbox-title">🔐 ${data.perm} <span class="muted">@${label}</span></h3>
      <div class="muted">${data.title || data.agent_id || 'agent'} is waiting${data.deadline ? ` · auto-declines ${new Date(data.deadline).toLocaleTimeString()}` : ''}</div>
      ${data.path && html`<div><code>${data.path}</code></div>`}
      ${(data.target_group || data.target_conv_title) && html`<div class="muted">target: ${data.target_group ? `group ${data.target_group}` : ''}${data.target_conv_title ? ` ${data.target_conv_title}` : ''}</div>`}
      ${data.body && html`<pre class="remote-inbox-body">${data.body}</pre>`}
      <div class="muted">One answer for this one request; the call runs with all its effects. "Always allow" stays on ${label}'s own dashboard.</div>
      ${!decides ? html`<div class="muted">Answer this one on ${label}'s own dashboard.</div>`
        : !approvable && html`<div class="muted" id="remote-inbox-approve-reason">Approve stays on ${label}: ${data.approve_reason || 'not yours to approve'}</div>`}
      <div class="modal-buttons"><span class="spacer"></span>
        ${decides && html`<${Button} id="remote-inbox-deny" disabled=${busy} onClick=${deny}>Deny</${Button}>`}
        ${decides && html`<${Button} variant="primary" id="remote-inbox-approve" disabled=${busy || !approvable} title=${approvable ? '' : data.approve_reason || ''} onClick=${approve}>Approve once…</${Button}>`}
        <${Button} onClick=${onClose}>Close</${Button}></div>`
    : html`
      <h3 id="remote-inbox-title">${data.subject || '(no subject)'} <span class="muted">@${label}</span></h3>
      <div class="muted">from ${data.from_title || data.from_agent || 'unknown'}${data.group ? ` · ${data.group}` : ''} · ${data.created_at ? new Date(data.created_at).toLocaleString() : ''}</div>
      <pre class="remote-inbox-body">${data.body}${data.truncated ? '\n…' : ''}</pre>
      ${data.attachments?.length > 0 && html`<div class="muted">Attachments stay on ${label}: ${data.attachments.join(', ')}</div>`}
      ${data.replyable
        ? html`<textarea id="remote-inbox-reply" rows="3" placeholder=${`Reply to ${data.from_title || 'the agent'} on ${label}`} value=${reply} onInput=${(e) => setReply(e.currentTarget.value)}></textarea>`
        : html`<div class="muted">Answer this one on ${label}'s own dashboard.</div>`}
      <div class="modal-buttons"><span class="spacer"></span>
        ${markable && html`<${Button} id="remote-inbox-read" disabled=${busy} onClick=${markRead}>Mark read</${Button}>`}
        ${data.replyable && html`<${Button} variant="primary" id="remote-inbox-send" disabled=${busy || !reply.trim()} onClick=${send}>Send reply</${Button}>`}
        <${Button} onClick=${onClose}>Close</${Button}></div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation human-inbox ${kind === 'request' ? `decide ${label} ID approve|deny` : `reply ${label} ${data.id} --body …`}</code></div>
  </${Dialog}>`;
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
      ${(n.access_requests || []).map((r) => html`<button data-ui-exempt type="button" class="mailbox-row remote-inbox-item pending" key=${`r${r.id}`} data-request=${r.id}
        title=${`${r.perm} for ${r.title || r.agent_id} on ${n.label}`} onClick=${() => setOpen({ node: id, label: n.label, kind: 'request', data: r, answers: n.answers !== false })}>🔐 ${r.perm} <span class="muted">@${n.label}</span></button>`)}
      ${(n.messages || []).map((m) => html`<button data-ui-exempt type="button" class="mailbox-row remote-inbox-item" key=${`m${m.id}`} data-message=${m.id}
        title=${`${m.subject || ''} — from ${m.from_title || m.from_agent || ''} on ${n.label}`} onClick=${() => setOpen({ node: id, label: n.label, kind: 'message', data: m, markable: n.markable !== false })}>✉ ${m.subject || m.from_title || 'notification'} <span class="muted">@${n.label}</span></button>`)}
    </div>`)}
    ${open && html`<${InboxReader} item=${open} actions=${actions} confirm=${confirm} toast=${toast} onClose=${() => setOpen(null)} />`}
  </div>`;
}

let shared = null;
function defaultActions() { return (shared ||= createRemoteInboxActions()); }
