// mail-parts.js — the Messages panes' building blocks, driven by props so
// this node's mail client (mail-island.js) and the fused Messages view
// (fused-mail.js) render the same folders, rows, pager, filters and reader
// headers. Each view keeps its own data and actions and configures these
// with what it can do: the fused view adds a node pill to each row and
// disables what the owning node does not allow from here.
import { Fragment, h } from 'preact';
import htm from 'htm';
import { focusRow } from './mail-keynav.js';
import { idTooltip, relTime, shortAgentId } from './helpers.js';
import { attachmentHref, bodilessNotice, messageAttachments } from './human-attachments.js';
import { ImageAttachmentPreview } from './image-preview-overlay.js';
import { VideoAttachmentPreview } from './video-preview-overlay.js';
import { MarkdownAttachment } from './markdown-attachment.js';
import { ROSTER_MAX } from './fused-mail-model.js';
import { allRecipientLabel, counterparty, senderLabel } from './mail-labels.js';

const html = htm.bind(h);

export function Progress({ progress }) {
  const pct = progress.total ? Math.round((progress.done / progress.total) * 100) : 0;
  return html`<${Fragment}><span class="mail-progress-label">${progress.verb} ${progress.done} / ${progress.total}…</span>
    <span class="mail-progress grow"><span class="mail-progress-fill" style=${`width:${pct}%`}></span></span></${Fragment}>`;
}

// LoadMore ends a server-paged folder list: "Show more" for the next page,
// or — once the list holds the most one roster read returns — a hint to
// narrow it with the filter instead.
export function LoadMore({ shown, total, busy, what, onMore, nested = false }) {
  if (shown >= ROSTER_MAX) {
    return html`<div class=${`mailbox-load-more capped${nested ? ' nested' : ''}`}>${shown} of ${total} shown · filter to narrow</div>`;
  }
  return html`<button type="button" class=${`mailbox-load-more${nested ? ' nested' : ''}`} disabled=${busy}
    title=${`Load the next page of ${what}`} onClick=${onMore}>Show more · ${shown} of ${total}</button>`;
}

const ATTACHMENT_ICONS = {
  file: html`<path d="m21.4 11.6-8.9 8.9a6 6 0 0 1-8.5-8.5l9.7-9.7a4 4 0 0 1 5.7 5.7l-9.7 9.7a2 2 0 0 1-2.8-2.8l8.9-8.9" />`,
  image: html`<rect x="3" y="3" width="18" height="18" rx="2" />
    <circle cx="8" cy="8" r="1.5" /><path d="m3 17 5-5 4 4 4-6 5 7" />`,
  video: html`<rect x="3" y="5" width="13" height="14" rx="2" /><path d="m16 10 5-3v10l-5-3z" />`,
};

export function MessageAttachmentIndicator({ attachments }) {
  if (!attachments.length) return null;
  const counts = { file: 0, image: 0, video: 0 };
  for (const attachment of attachments) {
    const mime = String(attachment.content_type || '').toLowerCase();
    // Type counts include media the browser may not be able to preview.
    const kind = attachment.video || mime.startsWith('video/') ? 'video'
      : attachment.previewable || mime.startsWith('image/') ? 'image' : 'file';
    counts[kind] += 1;
  }
  const kinds = Object.keys(counts).filter((kind) => counts[kind]);
  const label = kinds.map((kind) => `${counts[kind]} ${kind}${counts[kind] === 1 ? '' : 's'}`).join(', ');
  return html`<span class="mail-row-attachments" role="img" title=${label} aria-label=${label}>
    ${kinds.map((kind) => html`<span key=${kind} class="mail-row-attachment-type" data-type=${kind} aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"
        stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ATTACHMENT_ICONS[kind]}</svg>
      <span class="mail-row-attachment-count">${counts[kind]}</span>
    </span>`)}
  </span>`;
}

export function LinkifiedBody({ text }) {
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
      if (".,;:!?'\"".includes(ch)) { trail = ch + trail; url = url.slice(0, -1); continue; }
      if (ch === ')' || ch === ']') {
        const open = ch === ')' ? '(' : '[';
        if (url.split(ch).length > url.split(open).length) { trail = ch + trail; url = url.slice(0, -1); continue; }
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

function attachmentSize(bytes) {
  const size = Number(bytes || 0);
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(size < 10 * 1024 ? 1 : 0)} KiB`;
  return `${(size / (1024 * 1024)).toFixed(size < 10 * 1024 * 1024 ? 1 : 0)} MiB`;
}

export function HumanAttachment({ message }) {
  const attachments = messageAttachments(message);
  if (!attachments.length) return null;
  return html`<${Fragment}>${attachments.map((attachment, index) => html`
    <div class="mail-attachment" key=${attachment.id || index}>
      <${ImageAttachmentPreview} messageID=${message.id} attachment=${attachment} siblings=${attachments} surface="messages" />
      <${VideoAttachmentPreview} messageID=${message.id} attachment=${attachment} siblings=${attachments} surface="messages" />
      <span class="mail-attachment-label">Agent file</span>
      <a href=${attachmentHref(message, attachment)}
        download=${attachment.filename || ''} title="Download this agent-published file">⤓ ${attachment.filename || 'attachment'}</a>
      <span class="mail-attachment-size">${attachmentSize(attachment.size_bytes)}</span>
      <${MarkdownAttachment} messageID=${message.id} attachment=${attachment}
        siblings=${attachments} surface="messages" />
    </div>`)}</${Fragment}>`;
}

export function HeaderRow({ label, children }) {
  if (children == null || children === '') return null;
  return html`<div class="mail-hrow"><span class="mail-hlabel">${label}</span><span class="mail-hval">${children}</span></div>`;
}

export function RecipientNames({ recipients }) {
  if (!recipients?.length) return null;
  return html`<${Fragment}>${recipients.map((recipient, index) => html`<${Fragment} key=${recipient.conv_id || index}>
    ${index > 0 ? ', ' : ''}${recipient.title ? `${recipient.title} ` : ''}<span class="mail-cid"
      title=${idTooltip(recipient.agent_id, recipient.conv_id)}>${recipient.agent_id
        ? shortAgentId(recipient.agent_id, recipient.conv_id) : shortAgentId('', recipient.conv_id)}</span>
  </${Fragment}>`)}</${Fragment}>`;
}

// GroupLinkReader is a peer's cross-node link request (tcl-2ksi0k): one exact

export function messageDeliveryState(message) {
  if (message.forward_state === 'queued' || message.forward_state === 'handoff') return 'forwarded to ' + message.forwarded_to;
  if (message.forward_state === 'refused') return 'forwarding refused';
  if (message.forward_state === 'accepted') return 'delivered';
  if (message.nudge_discarded_at) return 'discarded while offline';
  if (message.delivered_at) return 'delivered';
  if (message.direction === 'out') return 'undelivered';
  return '';
}

// FolderRow is one sidebar folder: a lead control (checkbox, expand caret or
// spacer), then the folder button with its icon, name, tags, total and unread
// badge. total null leaves the count out.
export function FolderRow({
  id, icon, label, title, tags = null, total, totalTitle, unread = 0, active = false,
  lead = null, className = '', nested = false, disabled = false, onSelect,
}) {
  const cls = `mailbox-row${className ? ` ${className}` : ''}${nested ? ' nested' : ''}`;
  return html`<div class=${cls}>${lead || html`<span class="mail-box-check-spacer"></span>`}<button
    class=${`mailbox${active ? ' active' : ''}${unread ? ' has-unread' : ''}`}
    data-id=${id} title=${title} disabled=${disabled}
    aria-current=${active ? 'true' : undefined}
    onClick=${(event) => { focusRow(event.currentTarget); onSelect(); }}>
    <span class="mailbox-icon">${icon}</span>
    <span class="mailbox-name">${label}</span>
    ${tags}
    ${total !== null && html`<span class="mailbox-count" title=${totalTitle}>${total}</span>`}
    ${unread ? html`<span class="mailbox-unread">${unread > 99 ? '99+' : unread}</span>` : null}
  </button></div>`;
}

// FolderCaret expands or collapses a group folder's member folders.
export function FolderCaret({ expanded, disabled = false, group, onToggle }) {
  return html`<button type="button" class="mail-group-caret" data-group=${group} aria-expanded=${String(expanded)}
    disabled=${disabled}
    title=${expanded ? 'Collapse members' : 'Expand members'}
    onClick=${onToggle}>${expanded ? '▾' : '▸'}</button>`;
}

// SectionToggle is a foldable sidebar heading, such as "All agent mailboxes
// (12)" with its folded unread badge.
export function SectionToggle({ expanded, label, unread = 0, onToggle, className = '' }) {
  return html`<button type="button" class=${`mailbox-section mailbox-section-toggle${className ? ` ${className}` : ''}${unread ? ' has-unread' : ''}`}
    aria-expanded=${String(expanded)}
    title=${expanded ? 'Collapse the agent list' : 'Expand the agent list'}
    onClick=${onToggle}><span class="mailbox-section-caret">${expanded ? '▾' : '▸'}</span>
    ${label}
    ${unread > 0 && html` <span class="mailbox-unread">${unread > 99 ? '99+' : unread}</span>`}</button>`;
}

// MessageHead names a row's parties: sender → recipient in an aggregate
// folder (all, a group), else the other party with the direction.
export function MessageHead({ message, aggregate }) {
  if (aggregate) {
    const sender = senderLabel(message);
    return html`<${Fragment}>
      ${sender && html`<span class="mail-row-party" title=${message.from_conv ? idTooltip(message.from_agent, message.from_conv) : undefined}>${sender}</span>`}
      <span class="mail-row-arrow">→</span>
      <span class="mail-row-party" title=${message.to_conv ? idTooltip(message.to_agent, message.to_conv) : undefined}>${allRecipientLabel(message)}</span>
    </${Fragment}>`;
  }
  const outgoing = message.direction === 'out';
  const party = counterparty(message);
  const conv = outgoing ? message.to_conv : message.from_conv;
  const agent = outgoing ? message.to_agent : message.from_agent;
  return html`<${Fragment}><span class=${`mail-dir ${outgoing ? 'out' : 'in'}`} title=${outgoing ? 'sent' : 'received'}>${outgoing ? '→' : '←'}</span>
    ${party && html`<span class="mail-row-party" title=${conv ? idTooltip(agent, conv) : undefined}>${party}</span>`}</${Fragment}>`;
}

// MailRow is one message in the list: select checkbox, the row (unread dot,
// lead such as a node pill, parties, group, time, subject and attachments)
// and its delete button. canDelete false keeps the 🗑 in place, disabled,
// with deleteTitle saying why.
export function MailRow({
  id, message, kind, aggregate, preview, lead = null, active = false, checked = false, busy = false,
  onCheck, onSelect, onDelete, canDelete = true, deleteTitle = 'Delete this message',
}) {
  const unread = !message.read;
  return html`<div class="mail-row-wrap" data-key=${id} data-kind=${kind}>
    <input type="checkbox" class="mail-msg-check" data-id=${id} checked=${checked}
      disabled=${busy} title="Select message"
      onChange=${(event) => onCheck(event.currentTarget.checked)} />
    <button class=${`mail-row${active ? ' active' : ''}${unread ? ' unread' : ''}`}
      data-id=${id} aria-current=${active ? 'true' : undefined}
      onClick=${(event) => { focusRow(event.currentTarget); onSelect(); }}>
      <span class="mail-row-top">
        ${unread && html`<span class="mail-row-dot" title="unread">●</span>`}
        ${lead}
        <${MessageHead} message=${message} aggregate=${aggregate} />
        ${message.group && html`<span class="mail-row-group">${message.group}</span>`}
        <span class="mail-row-time">${relTime(message.created_at)}</span>
      </span>
      <span class="mail-row-subject-line">
        <span class="mail-row-subject">${preview}</span>
        <${MessageAttachmentIndicator} attachments=${messageAttachments(message)} />
      </span>
    </button>
    <button class="mail-row-del" data-id=${id}
      title=${deleteTitle} disabled=${busy || !canDelete}
      onClick=${onDelete}>🗑</button>
  </div>`;
}

// SelectAll ticks or clears every message on the page.
export function SelectAll({ checked, onChange }) {
  return html`<label title="Select / deselect every message on this page"><input type="checkbox" class="mail-select-all"
    checked=${checked} onChange=${(event) => onChange(event.currentTarget.checked)} /> all</label>`;
}

export const PAGE_SIZES = [25, 50, 100, 200];

// MailPager steps through a folder's pages and picks the page size. note
// follows the page position (the fused view says where its pages end).
export function MailPager({ id, hidden = false, page, pages, pageSize, onPage, onPageSize, note = '' }) {
  if (hidden) return html`<div id=${id} class="mail-pager" hidden></div>`;
  page = Math.min(page, pages);
  const atStart = page <= 1;
  const atEnd = page >= pages;
  return html`<div id=${id} class="mail-pager">
    ${pages > 1 && html`<${Fragment}><button title="First page" disabled=${atStart} onClick=${() => onPage(1)}>«</button>
      <button title="Previous page" disabled=${atStart} onClick=${() => onPage(page - 1)}>‹</button>
      <span class="mail-pager-pos">Page ${page} / ${pages}</span>
      <button title="Next page" disabled=${atEnd} onClick=${() => onPage(page + 1)}>›</button>
      <button title="Last page" disabled=${atEnd} onClick=${() => onPage(pages)}>»</button></${Fragment}>`}
    ${note && html`<span class="mail-pager-note muted">${note}</span>`}
    <span class="grow"></span><label class="mail-pager-size" title="Messages per page">
      <select class="mail-page-size" value=${pageSize} onChange=${(event) => onPageSize(Number(event.currentTarget.value))}>
        ${PAGE_SIZES.map(size => html`<option value=${size}>${size}</option>`)}</select> / page</label>
  </div>`;
}

// MailFilters is the folder filter above the sidebar and the message filter
// above the list, with its count and clear button.
export function MailFilters({
  boxId, boxPlaceholder, boxValue, onBoxInput, onBoxKeyDown,
  id, placeholder, value, onInput, onKeyDown, countId, countText, clearId, onClear,
}) {
  return html`<${Fragment}>
    <input id=${boxId} type="text" class="mail-sidebar-filter"
      placeholder=${boxPlaceholder}
      autocomplete="off" spellcheck=${false} value=${boxValue}
      onInput=${(event) => onBoxInput(event.currentTarget.value)}
      onKeyDown=${onBoxKeyDown} />
    <div class="mail-list-filter">
      <input id=${id} type="text"
        placeholder=${placeholder}
        autocomplete="off" spellcheck=${false} value=${value}
        onInput=${(event) => onInput(event.currentTarget.value)}
        onKeyDown=${onKeyDown} />
      <span class="filter-count" id=${countId}>${countText}</span>
      <button class="clear-filter" id=${clearId} title="Clear filter"
        aria-label="Clear message filter" onClick=${(event) => {
          const input = event.currentTarget.closest('.mail-list-filter')?.querySelector('input');
          onClear();
          input?.focus();
        }}>×</button>
    </div>
  </${Fragment}>`;
}

// MailFooterToggles are the sidebar's scope toggles. idPrefix keeps two
// mail clients on one page from sharing element IDs.
export function MailFooterToggles({ idPrefix, busy = false, showRetired, showEmpty, showPrevGens, onRetired, onEmpty, onPrevGens }) {
  return html`<${Fragment}>
    <label class="mail-sidebar-foot"
      title="Show folders for retired agents, and include their messages in the “All agent messages” firehose">
      <input type="checkbox" id=${`${idPrefix}-show-retired`} checked=${showRetired}
        disabled=${busy} onChange=${(event) => onRetired(event.currentTarget.checked)} /> show retired agents
    </label>
    <label class="mail-sidebar-foot" title="Show folders for agents that have never sent or received a message">
      <input type="checkbox" id=${`${idPrefix}-show-empty`} checked=${showEmpty}
        disabled=${busy} onChange=${(event) => onEmpty(event.currentTarget.checked)} /> show agents without messages
    </label>
    <label class="mail-sidebar-foot"
      title="Show folders for previous (superseded) generations of agents — past conversations left behind by a reincarnate / /clear. Only hides the folders in this list; no messages are hidden.">
      <input type="checkbox" id=${`${idPrefix}-show-prev-gens`} checked=${showPrevGens}
        disabled=${busy} onChange=${(event) => onPrevGens(event.currentTarget.checked)} /> show previous generations
    </label>
  </${Fragment}>`;
}

// MailGutters are the drag handles between the three panes (mail-resize.js).
export function MailGutters() {
  return html`<${Fragment}>
    <div class="mail-gutter" data-boundary="sidebar-list"
      title="Drag to resize · double-click to reset"></div>
    <div class="mail-gutter" data-boundary="list-reader"
      title="Drag to resize · double-click to reset"></div>
  </${Fragment}>`;
}

// MessageHeaders is the reader's subject line and From / To / Cc / Group /
// Date / Status block. extra adds rows after Date, such as the fused view's
// Node.
export function MessageHeaders({ message, extra = null }) {
  const when = message.created_at ? new Date(message.created_at).toLocaleString() : '';
  let to = message.to_recipients?.length ? html`<${RecipientNames} recipients=${message.to_recipients} />` : null;
  if (!to && (message.to_title || message.to_conv)) to = html`<${Fragment}>${message.to_title ? `${message.to_title} ` : ''}${message.to_conv && html`<span class="mail-cid"
    title=${idTooltip(message.to_agent, message.to_conv)}>${shortAgentId(message.to_agent, message.to_conv)}</span>`}</${Fragment}>`;
  const deliveryState = messageDeliveryState(message);
  const stateBits = html`<${Fragment}>${message.read ? 'read' : html`<span class="mail-state-unread">unread</span>`}${deliveryState
    ? html`<${Fragment}> · <span class=${deliveryState === 'delivered' ? '' : 'mail-state-pending'}>${deliveryState}</span></${Fragment}>` : null}</${Fragment}>`;
  return html`<div class="mail-reader-head"><div class="mail-subject">${message.subject || '(no subject)'} <span class="mail-id">#${message.id}</span></div>
    <div class="mail-headers">
      ${message.operator_authored
        ? html`<${HeaderRow} label="From">${senderLabel(message)}</${HeaderRow}>`
        : message.from_conv && html`<${HeaderRow} label="From">${message.from_title ? `${message.from_title} ` : ''}<span class="mail-cid"
          title=${idTooltip(message.from_agent, message.from_conv)}>${shortAgentId(message.from_agent, message.from_conv)}</span></${HeaderRow}>`}
      <${HeaderRow} label="To">${to}</${HeaderRow}>
      ${message.cc_recipients?.length > 0 && html`<${HeaderRow} label="Cc"><${RecipientNames} recipients=${message.cc_recipients} /></${HeaderRow}>`}
      <${HeaderRow} label="Group">${message.group || ''}</${HeaderRow}><${HeaderRow} label="Date">${when}</${HeaderRow}>
      ${extra}
      <${HeaderRow} label="Status">${stateBits}</${HeaderRow}>
    </div>
  </div>`;
}

// MessageBody is the reader's body: links clickable, everything else text.
export function MessageBody({ message }) {
  return html`<div class="mail-reader-body">${bodilessNotice(message)
    ? html`<span class="notification-bodiless">${bodilessNotice(message)}</span>`
    : html`<${LinkifiedBody} text=${message.body || ''} />`}</div>`;
}
