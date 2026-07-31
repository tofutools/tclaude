import { Fragment, h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { relTime, shortAgentId } from './helpers.js';
import { humanNotificationMatchesSender } from './human-notification-attention.js';
import { openHumanNotifications } from './mail-bridge.js';
import { openHumanReplyModal } from './message-access-dialog-controller.js';

const html = htm.bind(h);
const readWrites = new Map();

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

function replaceMessageRead(state, id, read) {
  const snapshot = state.snapshot.value;
  const current = (snapshot?.messages || []).find((message) => message.id === id);
  if (!snapshot || !current || current.read === read) return null;
  state.publish({
    ...snapshot,
    messages: snapshot.messages.map((message) =>
      message.id === id ? { ...message, read } : message),
    messages_unread: Math.max(0, Number(snapshot.messages_unread || 0) + (read ? -1 : 1)),
  });
  return current.read;
}

export async function persistHumanMessageRead(state, id, read, onError) {
  const prior = replaceMessageRead(state, id, read);
  if (prior === null) return;
  let queue = readWrites.get(id);
  if (!queue) {
    queue = { tail: Promise.resolve(), confirmed: prior };
    readWrites.set(id, queue);
  }
  const request = queue.tail.catch(() => {}).then(async () => {
    const response = await fetch('/api/human-messages/read', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, read }),
    });
    if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
  });
  queue.tail = request;
  try {
    await request;
    queue.confirmed = read;
  } catch (error) {
    const current = (state.snapshot.value?.messages || [])
      .find((message) => message.id === id);
    if (current?.read === read) replaceMessageRead(state, id, queue.confirmed);
    onError?.(error);
  } finally {
    if (queue.tail === request) readWrites.delete(id);
  }
}

function Attachment({ message }) {
  const attachment = message.attachment;
  if (!attachment) return null;
  const href = `/api/human-messages/${encodeURIComponent(message.id)}/attachment`;
  const filename = attachment.filename || 'attachment';
  return html`<div class="human-notification-drawer-attachment">
    <a class="human-notification-drawer-file-link" href=${href}
      download=${attachment.filename || ''} title=${`Download ${filename}`}>
      <span class="human-notification-drawer-file-icon" aria-hidden="true">▣</span>
      <span class="human-notification-drawer-file">
        <strong>${filename}</strong>
        <span>${attachment.content_type || 'file'} · ${attachmentSize(attachment.size_bytes)}</span>
      </span>
    </a>
    <a class="human-notification-drawer-download" href=${href}
      download=${attachment.filename || ''} title="Download this agent-published file">Download</a>
  </div>`;
}

export function GroupsNotificationReader({
  descriptor, snapshot, state, actions, onSelect, onClose,
}) {
  const closeRef = useRef(null);
  const messages = senderMessages(descriptor.sender, snapshot);
  const message = messages.find((item) => item.id === descriptor.messageId) || messages[0];
  const index = message ? messages.findIndex((item) => item.id === message.id) : -1;

  useEffect(() => {
    if (!message || message.read) return;
    void persistHumanMessageRead(state, message.id, true, actions.reportError);
  }, [message?.id]);

  useEffect(() => {
    closeRef.current?.focus({ preventScroll: true });
  }, [descriptor.launcher]);

  useEffect(() => {
    const onKeyDown = (event) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      onClose(true);
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [onClose]);

  if (!message) return null;
  const previous = messages[index - 1];
  const next = messages[index + 1];
  const senderID = descriptor.sender.agent || descriptor.sender.conv;
  const senderLabel = message.from_title || descriptor.sender.label || senderID;
  const created = message.created_at ? new Date(message.created_at).toLocaleString() : '';

  return html`<aside class="human-notification-drawer" role="dialog"
    data-message-id=${message.id}
    aria-modal="false" aria-labelledby="human-notification-drawer-subject">
    <header class="human-notification-drawer-header">
      <div>
        <span class="human-notification-drawer-kicker">Human notification</span>
        <span class=${`human-notification-drawer-state ${message.read ? 'read' : 'unread'}`}>
          ${message.read ? 'opened · read' : 'unread'}
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
        <div class="human-notification-drawer-body"><${LinkifiedBody} text=${message.body || ''} /></div>
        <${Attachment} message=${message} />
      </div>
    </div>
    <div class="human-notification-drawer-actions">
      <button type="button" class="primary human-notification-drawer-reply" onClick=${() => {
        onClose(false);
        openHumanReplyModal({
          id: message.id,
          agent: message.from_agent || descriptor.sender.agent || '',
          conv: message.from_conv || descriptor.sender.conv || '',
          label: senderLabel,
          subject: message.subject || '',
        });
      }}>Reply</button>
      <button type="button" onClick=${() => {
        onClose(false);
        openHumanNotifications(senderID, message.id);
      }}>Open in Messages ↗</button>
      <button type="button" onClick=${() =>
        persistHumanMessageRead(state, message.id, !message.read, actions.reportError)}>
        ${message.read ? 'Mark unread' : 'Mark read'}
      </button>
      <span class="grow"></span>
      <button type="button" onClick=${() => onClose(true)}>Close</button>
    </div>
  </aside>`;
}
