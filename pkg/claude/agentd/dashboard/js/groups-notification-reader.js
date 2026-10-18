import { Fragment, h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { relTime, shortAgentId } from './helpers.js';
import { humanNotificationMatchesSender } from './human-notification-attention.js';
import { openHumanNotifications } from './mail-bridge.js';
import { attachmentHref, bodilessNotice, messageAttachments, withPeerAttachments } from './human-attachments.js';
import { openHumanReplyModal } from './message-access-dialog-controller.js';
import { hasShownOverlay } from './overlay-stack.js';
import { announceNodeChanged, isPageNode, nodeFetch, nodeSnapshot, selfNodeID } from './node-api.js';
import { ImageAttachmentPreview } from './image-preview-overlay.js';
import { VideoAttachmentPreview } from './video-preview-overlay.js';
import { MarkdownAttachment } from './markdown-attachment.js';
import { remoteNodeID } from './fleet-model.js';
import { featureState, humanReadShared } from './peer-view-limits.js';
import { LockReason } from './lock-reason.js';
import { refreshPageNotifications } from './peer-notifications.js';
import { senderOnline } from './message-access-dialog-model.js';
import { MessageDownloadMenu } from './message-download-menu.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);
const readWrites = new Map();
// SAFE_AGENT admits only a plain agent ID into a terminal request.
const SAFE_AGENT = /^agt_[A-Za-z0-9]{4,64}$/;

// readerNode is where a notification lives. node names another node than
// the page's (a fused-view row; its requests go through nodeFetch(node) and
// its data is that node's snapshot), blank for the page's own. peer is the
// node that answers through its human-inbox: the page's peer, a fused peer,
// or '' for this node, whose /api/human-messages stays local on any page.
export function readerNode(sender, { self = selfNodeID(), remote = remoteNodeID() } = {}) {
  const node = sender?.node && !isPageNode(sender.node) ? sender.node : '';
  return { node, peer: node ? (node === self ? '' : node) : remote };
}

// readRoute is where a read toggle goes. A peer's notification is answered
// as that peer's Messages tab does: /api/human-inbox (under
// node.messages.mark-read).
function readRoute(id, read, peer) {
  if (!peer) return { url: '/api/human-messages/read', body: { id, read } };
  return { url: '/api/human-inbox/read', body: read ? { id } : { id, read: false } };
}

// shares reports whether a peer view takes feature from here: unrestricted
// trust holds every one.
function shares(peerView, feature) {
  return peerView?.level === 'unrestricted' || (feature === 'mark' ? humanReadShared(peerView) : featureState(feature, peerView) === 'shared');
}

// remoteNodeName is how this page names the peer it shows.
function remoteNodeName(remote) {
  return document.documentElement.dataset.remoteNodeName || remote.slice(0, 13);
}

async function postPeerJSON(url, body, fetchFn = fetch) {
  const res = await fetchFn(url, { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  if (!res.ok) {
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    throw Object.assign(new Error(data?.error || `HTTP ${res.status}`), { status: res.status });
  }
}

function attachmentSize(bytes) {
  const size = Number(bytes || 0);
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(size < 10 * 1024 ? 1 : 0)} KiB`;
  return `${(size / (1024 * 1024)).toFixed(size < 10 * 1024 * 1024 ? 1 : 0)} MiB`;
}

function LinkifiedBody({ text }) {
  const source = String(text ?? '');
  const urlRe = /https?:\/\/[^\s<>"']+/g;
  const parts = [];
  let last = 0;
  let match;
  while ((match = urlRe.exec(source)) !== null) {
    if (match.index > last) parts.push(source.slice(last, match.index));
    let url = match[0];
    let trail = '';
    for (;;) {
      const ch = url[url.length - 1];
      if (".,;:!?'\"".includes(ch)) {
        trail = ch + trail;
        url = url.slice(0, -1);
        continue;
      }
      if (ch === ')' || ch === ']') {
        const open = ch === ')' ? '(' : '[';
        if (url.split(ch).length > url.split(open).length) {
          trail = ch + trail;
          url = url.slice(0, -1);
          continue;
        }
      }
      break;
    }
    if (url) parts.push(html`<a href=${url} target="_blank" rel="noopener noreferrer">${url}</a>`);
    if (trail) parts.push(trail);
    last = match.index + match[0].length;
  }
  if (last < source.length) parts.push(source.slice(last));
  return html`<${Fragment}>${parts}</${Fragment}>`;
}

function senderMessages(sender, snapshot) {
  return (snapshot?.messages || [])
    .filter((message) => humanNotificationMatchesSender(message, sender));
}

// The snapshot carries only unread and recent notifications. The reader
// therefore loads the sender's history once and keeps every row it has
// shown, so a notification marked read stays open (and can be marked unread
// again) after the next snapshot drops it. Snapshot rows win: they carry the
// live read state.
const SENDER_HISTORY_PAGE = 50;

function mergedSenderMessages(sender, snapshot, known) {
  const byID = new Map(known);
  for (const message of senderMessages(sender, snapshot)) byID.set(message.id, message);
  for (const [id, message] of byID) known.set(id, message);
  return [...byID.values()].sort((a, b) => b.id - a.id);
}

function replaceMessageRead(state, id, read, fallback = null) {
  const snapshot = state.snapshot.value;
  const current = (snapshot?.messages || []).find((message) => message.id === id) || fallback;
  if (!snapshot || !current || current.read === read) return null;
  const inSnapshot = (snapshot.messages || []).some((message) => message.id === id);
  const messages = inSnapshot
    ? snapshot.messages.map((message) => (message.id === id ? { ...message, read } : message))
    : [...(snapshot.messages || []), { ...current, read }].sort((a, b) => b.id - a.id);
  state.publish({
    ...snapshot,
    messages,
    messages_unread: Math.max(0, Number(snapshot.messages_unread || 0) + (read ? -1 : 1)),
  });
  return current.read;
}

// persistHumanMessageRead writes a notification's read state, shown at once.
// One on another node than the page's (where.node) is not in the page's
// snapshot: show(read) shows it, and that node is read again once written.
export async function persistHumanMessageRead(state, id, read, onError, fallback = null, where = null, show = null) {
  const { node = '', peer = remoteNodeID() } = where || {};
  if (node) {
    if (!fallback || fallback.read === read) return;
    show?.(read);
    try {
      const route = readRoute(id, read, peer);
      await postPeerJSON(route.url, route.body, nodeFetch(node));
      announceNodeChanged(node);
    } catch (error) {
      show?.(!read);
      onError?.(error);
    }
    return;
  }
  const prior = replaceMessageRead(state, id, read, fallback);
  if (prior === null) return;
  let queue = readWrites.get(id);
  if (!queue) {
    queue = { tail: Promise.resolve(), confirmed: prior };
    readWrites.set(id, queue);
  }
  // A restricted peer's page lists its notifications at their own cadence:
  // the write shows there at once, ahead of any read still in flight.
  if (peer && !node) refreshPageNotifications(id, read, fallback);
  const request = queue.tail.catch(() => {}).then(async () => {
    const route = readRoute(id, read, peer);
    const response = await fetch(route.url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(route.body),
    });
    if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
  });
  queue.tail = request;
  try {
    await request;
    queue.confirmed = read;
    if (peer && !node) refreshPageNotifications();
  } catch (error) {
    const current = (state.snapshot.value?.messages || [])
      .find((message) => message.id === id);
    if (current?.read === read) replaceMessageRead(state, id, queue.confirmed);
    if (peer && !node) refreshPageNotifications(id, queue.confirmed, fallback);
    onError?.(error);
  } finally {
    if (queue.tail === request) readWrites.delete(id);
  }
}

function Attachment({ message }) {
  const attachments = messageAttachments(message);
  if (!attachments.length) return null;
  return html`<${Fragment}>${attachments.map((attachment, index) => {
    const href = attachmentHref(message, attachment);
    const filename = attachment.filename || 'attachment';
    return html`<div class="human-notification-drawer-attachment" key=${attachment.id || index}>
      <${ImageAttachmentPreview} messageID=${message.id} attachment=${attachment} siblings=${attachments} surface="drawer" />
      <${VideoAttachmentPreview} messageID=${message.id} attachment=${attachment} siblings=${attachments} surface="drawer" />
      <a class="human-notification-drawer-file-link" href=${href}
        download=${attachment.filename || ''} title=${`Download ${filename}`}>
        <span class="human-notification-drawer-file-icon" aria-hidden="true">▣</span>
        <span class="human-notification-drawer-file">
          <strong>${filename}</strong>
          <span title=${`${attachmentSize(attachment.size_bytes)} · ${attachment.content_type || 'file'}`}>
            ${attachmentSize(attachment.size_bytes)} · ${attachment.content_type || 'file'}</span>
        </span>
      </a>
      <a class="human-notification-drawer-download" href=${href}
        download=${attachment.filename || ''} title="Download this agent-published file">Download</a>
      <${MarkdownAttachment} messageID=${message.id} attachment=${attachment}
        siblings=${attachments} surface="drawer" />
    </div>`;
  })}</${Fragment}>`;
}

export function GroupsNotificationReader({
  descriptor, snapshot, state, actions, onSelect, onClose, closing = false, confirm = shellConfirm, toast = shellToast,
}) {
  const closeRef = useRef(null);
  const where = readerNode(descriptor.sender);
  const { node, peer } = where;
  // Another node's notification reads that node's snapshot (the fused view's).
  const nodeSnap = node ? nodeSnapshot(node) : snapshot;
  const senderKey = descriptor.sender.agent || descriptor.sender.conv || '';
  const knownKey = `${node}/${senderKey}`;
  // overrides holds the read state written to another node until it shows
  // (message IDs are per node, so it goes with the sender's rows).
  const known = useRef({ sender: null, rows: new Map(), overrides: new Map() });
  if (known.current.sender !== knownKey) known.current = { sender: knownKey, rows: new Map(), overrides: new Map() };
  const overrides = known.current.overrides;
  const [, setHistoryLoaded] = useState(0);
  useEffect(() => {
    if (!senderKey) return undefined;
    let live = true;
    const params = new URLSearchParams({ id: 'human', sender: senderKey, page_size: String(SENDER_HISTORY_PAGE) });
    // The history comes from the node the notification lives on.
    nodeFetch(node)(`/api/mailbox?${params.toString()}`, { credentials: 'same-origin' })
      .then((response) => (response.ok ? response.json() : null))
      .then((payload) => {
        if (!live || known.current.sender !== knownKey || !Array.isArray(payload?.messages)) return;
        // Its reply names what the node shares from here (a peer's view),
        // for a view that has no snapshot of that node (a terminal tab).
        if (payload.peer_view) known.current.peerView = payload.peer_view;
        for (const row of payload.messages) {
          if (!known.current.rows.has(row.id)) known.current.rows.set(row.id, row);
        }
        setHistoryLoaded((n) => n + 1);
      })
      .catch(() => {});
    return () => { live = false; };
  }, [knownKey]);
  const messages = mergedSenderMessages(descriptor.sender, nodeSnap, known.current.rows)
    .map((m) => {
      if (!overrides.has(m.id)) return m;
      const read = overrides.get(m.id);
      // The node's own read state caught up.
      if (m.read === read) { overrides.delete(m.id); return m; }
      return { ...m, read };
    });
  const found = messages.find((item) => item.id === descriptor.messageId) || messages[0];
  // A peer's files are read through this node's peer attachment routes; the
  // URLs it sends name its own, which on this origin would read this node's.
  const message = found && peer ? withPeerAttachments(peer, found) : found;
  const index = message ? messages.findIndex((item) => item.id === message.id) : -1;

  // Opening the reader deliberately does not mark the notification read.
  // Marking read is the operator's decision, made with the "Mark read" action
  // below, so a glance at a message never silently clears the attention marker.

  // Keyed on the open identity, not the launcher: a reopen that cancels an
  // in-flight exit reuses the mounted component, and the same launcher raising
  // the same message again must still move focus into the panel.
  useEffect(() => {
    closeRef.current?.focus({ preventScroll: true });
  }, [descriptor.openID, descriptor.launcher]);

  useEffect(() => {
    const onKeyDown = (event) => {
      if (event.key !== 'Escape' || hasShownOverlay()) return;
      // Already dismissed and sliding out: this Escape is not ours to eat, and
      // preventing it would deny it to whatever else the operator meant it for.
      if (closing) return;
      // The reader draws over the Terminals tab too, where a live xterm is
      // taking keys and calling preventDefault on the ones it forwards to the
      // PTY. Escape pressed inside a terminal belongs to that terminal, not to
      // this drawer.
      if (event.defaultPrevented) return;
      event.preventDefault();
      onClose(true);
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [onClose, closing]);

  if (!message) return null;
  const previous = messages[index - 1];
  const next = messages[index + 1];
  const senderID = descriptor.sender.agent || descriptor.sender.conv;
  const senderLabel = message.from_title || descriptor.sender.label || senderID;
  const created = message.created_at ? new Date(message.created_at).toLocaleString() : '';
  // What the peer does not share from here stays in place, disabled, with a
  // 🔒 saying why, as in its Messages tab.
  const peerName = !peer ? '' : node ? descriptor.sender.nodeName || node.slice(0, 13) : remoteNodeName(peer);
  const peerView = nodeSnap?.peer_view || known.current.peerView;
  const replyLock = peer && !shares(peerView, 'human.inbox.answer')
    ? `${peerName} keeps answering its notifications to its own operator: it needs unrestricted trust from ${peerName}.` : '';
  const markLock = peer && !shares(peerView, 'mark')
    ? `${peerName} shares its notifications for reading only: marking them read from here needs its node.messages.mark-read.` : '';
  // Deleting has no peer grant: unrestricted trust reaches the node's own
  // route, as its operator does.
  const deleteLock = peer && peerView?.level !== 'unrestricted'
    ? `${peerName} keeps deleting its notifications to its own operator: it needs unrestricted trust from ${peerName}.` : '';
  const senderAgent = message.from_agent || descriptor.sender.agent || '';
  const senderConv = message.from_conv || descriptor.sender.conv || '';
  const senderLive = nodeSnap ? senderOnline(nodeSnap, senderAgent, senderConv) : undefined;
  // The sender's terminal opens here: this node's as its row's jump does, a
  // peer's through the federation bridge (the peer decides whether it shares
  // it). This node's own, seen from a peer's page, raises nothing there.
  const terminalLock = peer
    ? (SAFE_AGENT.test(senderAgent) ? '' : 'This sender has no stable agent ID to open its terminal by.')
    : remoteNodeID() ? "This node's agents open their terminals from this node's own dashboard."
      : senderAgent || senderConv ? '' : 'This notification has no sending agent.';
  const openTerminal = () => {
    if (peer) {
      void import('./remote-terminal.js').then((m) => m.openRemoteTerminal({ instance: peer, agent: senderAgent, peerLabel: peerName, label: senderLabel, toast }));
      return;
    }
    void import('./agent-jump.js').then((m) => m.jumpAgent(senderAgent || senderConv, senderLabel, nodeSnap?.default_terminal === 'web', '', toast));
  };
  // A deleted notification leaves the reader on its neighbour, else closes it.
  const dropDeleted = () => {
    known.current.rows.delete(message.id);
    if (node) announceNodeChanged(node);
    else {
      const snap = state.snapshot.value;
      if (snap?.messages?.some((m) => m.id === message.id)) {
        state.publish({ ...snap, messages: snap.messages.filter((m) => m.id !== message.id),
          messages_unread: Math.max(0, Number(snap.messages_unread || 0) - (message.read ? 0 : 1)) });
      }
    }
    const neighbour = next || previous;
    if (neighbour) onSelect(neighbour.id); else onClose(true);
  };
  // A peer's own route through this node's proxy; this node's stays local on
  // any page (remote-node.js).
  const remove = () => confirm({
    title: 'Delete this notification?',
    body: `Permanently deletes this one notification${peer ? ` on ${peerName}` : ''}, read or unread. This cannot be undone.`,
    meta: `#${message.id}`, okLabel: 'Delete', busyLabel: 'Deleting…',
    action: () => postPeerJSON(peer ? `/api/peer/${encodeURIComponent(peer)}/human-messages/delete` : '/api/human-messages/delete', { id: message.id }),
  }).then((ok) => { if (ok) { toast(`Deleted${peer ? ` on ${peerName}` : ''}`, false); dropDeleted(); } },
    (e) => toast(`Delete failed: ${e?.message || e}`, true));

  return html`<aside class=${`human-notification-drawer${closing ? ' closing' : ''}`} role="dialog"
    data-message-id=${message.id}
    aria-modal="false" aria-labelledby="human-notification-drawer-subject">
    <header class="human-notification-drawer-header">
      <div>
        <span class="human-notification-drawer-kicker">Human notification</span>
        <span class=${`human-notification-drawer-state ${message.read ? 'read' : 'unread'}`}>
          ${message.read ? 'read' : 'unread'}
        </span>
      </div>
      <button ref=${closeRef} type="button" class="human-notification-drawer-close"
        onClick=${() => onClose(true)} aria-label="Close notification reader" title="Close (Esc)">×</button>
      <div class="human-notification-drawer-sender">
        <span class="human-notification-drawer-avatar" aria-hidden="true">${senderLabel.slice(0, 1).toUpperCase()}</span>
        <span><strong>${senderLabel}</strong>
          <small>${message.group || 'ungrouped'} · ${shortAgentId(descriptor.sender.agent, descriptor.sender.conv)}</small>
        </span>
      </div>
    </header>
    <nav class="human-notification-drawer-nav" aria-label="Notifications from this agent">
      <button type="button" disabled=${!previous}
        onClick=${() => previous && onSelect(previous.id)} aria-label="Newer notification" title="Newer notification">‹</button>
      <button type="button" disabled=${!next}
        onClick=${() => next && onSelect(next.id)} aria-label="Older notification" title="Older notification">›</button>
      <span>${index + 1} of ${messages.length} from this agent</span>
      <span class="human-notification-drawer-order">newest first</span>
    </nav>
    <div class="human-notification-drawer-announcement" role="status"
      aria-live="polite" aria-atomic="true">
      ${`Notification ${index + 1} of ${messages.length}: ${message.subject || '(no subject)'}`}
    </div>
    <div class="human-notification-drawer-scroll">
      <div class="human-notification-drawer-message">
        <h2 id="human-notification-drawer-subject">${message.subject || '(no subject)'}</h2>
        <div class="human-notification-drawer-date">${created}${created ? ` · ${relTime(message.created_at)}` : ''}</div>
        <div class="human-notification-drawer-body">${bodilessNotice(message)
          ? html`<span class="notification-bodiless">${bodilessNotice(message)}</span>`
          : html`<${LinkifiedBody} text=${message.body || ''} />`}</div>
        <${Attachment} message=${message} />
      </div>
    </div>
    <div class="human-notification-drawer-actions">
      <button type="button" class="primary human-notification-drawer-reply" disabled=${!!replyLock} onClick=${() => {
        // Keep the quick viewer mounted beneath the reply dialog so it remains
        // available after the reply is sent or the dialog is dismissed.
        openHumanReplyModal({
          id: message.id,
          agent: message.from_agent || descriptor.sender.agent || '',
          conv: message.from_conv || descriptor.sender.conv || '',
          label: senderLabel,
          subject: message.subject || '',
          // A peer's notification is answered through that peer, text only;
          // so is this node's from a peer's page, whose snapshot is not
          // this node's. Either's own snapshot knows the sender's liveness.
          ...(peer || node ? {
            nodeName: peerName || descriptor.sender.nodeName || '',
            // Without that node's snapshot the node itself refuses an offline sender.
            online: nodeSnap ? senderOnline(nodeSnap, message.from_agent || descriptor.sender.agent || '', message.from_conv || descriptor.sender.conv || '') : undefined,
            send: (body) => postPeerJSON(peer ? '/api/human-inbox/reply' : '/api/human-messages/reply', { id: message.id, body }, nodeFetch(node))
              .then(() => { if (node) announceNodeChanged(node); }),
          } : {}),
        });
      }}>Reply</button><${LockReason} why=${replyLock} />
      <button type="button" class="human-notification-drawer-terminal" disabled=${!!terminalLock || senderLive === false}
        title=${terminalLock ? undefined : senderLive === false ? 'Sending agent is offline — no terminal to open' : `Open ${senderLabel}'s terminal${peer ? ` on ${peerName} here` : ''}`}
        onClick=${openTerminal}>Terminal</button><${LockReason} why=${terminalLock} />
      ${!node && html`<button type="button" onClick=${() => {
        onClose(false);
        openHumanNotifications(senderID, message.id);
      }}>Open in Messages ↗</button>`}
      <button type="button" disabled=${!!markLock} onClick=${() =>
        persistHumanMessageRead(state, message.id, !message.read, actions.reportError, found, where, (read) => {
          overrides.set(message.id, read);
          setHistoryLoaded((n) => n + 1);
        })}>
        ${message.read ? 'Mark unread' : 'Mark read'}
      </button><${LockReason} why=${markLock} />
      <span class="grow"></span>
      <${MessageDownloadMenu} message=${message} node=${peer} toast=${toast} />
      <button type="button" class="danger human-notification-drawer-delete" disabled=${!!deleteLock}
        title=${deleteLock ? undefined : `Permanently delete this notification${peer ? ` on ${peerName}` : ''}`} onClick=${remove}>Delete</button><${LockReason} why=${deleteLock} />
      <button type="button" onClick=${() => onClose(true)}>Close</button>
    </div>
  </aside>`;
}
