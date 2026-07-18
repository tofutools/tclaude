import { Fragment, h, render } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import { registerMailController } from './mail-bridge.js';
import { idTooltip, relTime, shortAgentId, shortId } from './helpers.js';
import { dashboardState } from './snapshot-store.js';

const html = htm.bind(h);

function Progress({ progress }) {
  const pct = progress.total ? Math.round((progress.done / progress.total) * 100) : 0;
  return html`<${Fragment}><span class="mail-progress-label">${progress.verb} ${progress.done} / ${progress.total}…</span>
    <span class="mail-progress grow"><span class="mail-progress-fill" style=${`width:${pct}%`}></span></span></${Fragment}>`;
}

function MailboxIcon({ mailbox }) {
  if (mailbox.kind === 'all') return '🗂';
  if (mailbox.kind === 'human') return '📬';
  if (mailbox.kind === 'access-requests') return '🔐';
  if (mailbox.kind === 'group') return '👥';
  return html`<span class=${`mail-dot ${mailbox.online ? 'online' : 'offline'}`}>●</span>`;
}

function MailboxRow({ mailbox, nested = false, prevGen = false, current, controller, expanded = false }) {
  const active = mailbox.id === current.selected;
  const empty = mailbox.kind === 'agent' && !mailbox.total;
  const cls = `mailbox-row${mailbox.retired ? ' retired' : ''}${empty ? ' empty-box' : ''}${prevGen ? ' prev-gen' : ''}${nested ? ' nested' : ''}`;
  const countTitle = mailbox.kind === 'group'
    ? `${mailbox.members || 0} member${mailbox.members === 1 ? '' : 's'} · ${mailbox.total} message${mailbox.total === 1 ? '' : 's'}`
    : mailbox.kind === 'access-requests'
      ? `${mailbox.unread || 0} pending · ${mailbox.total} total access request${mailbox.total === 1 ? '' : 's'}`
      : `${mailbox.in} received · ${mailbox.out} sent`;
  let lead = html`<span class="mail-box-check-spacer"></span>`;
  if (mailbox.kind === 'agent' && !nested) {
    lead = html`<input type="checkbox" class="mail-box-check" data-conv=${mailbox.id}
      checked=${current.selectedBoxes.has(mailbox.id)} disabled=${current.busy} title="Select for bulk wipe"
      onChange=${(event) => controller.toggleBoxSelection(mailbox.id, event.currentTarget.checked)} />`;
  } else if (mailbox.kind === 'group') {
    lead = html`<button type="button" class="mail-group-caret" data-group=${mailbox.title} aria-expanded=${String(expanded)}
      disabled=${current.busy}
      title=${expanded ? 'Collapse members' : 'Expand members'}
      onClick=${() => controller.toggleGroupExpand(mailbox.title)}>${expanded ? '▾' : '▸'}</button>`;
  }
  return html`<div class=${cls}>${lead}<button
    class=${`mailbox${active ? ' active' : ''}${mailbox.unread ? ' has-unread' : ''}`}
    data-id=${mailbox.id} title=${controller.mailboxTitleAttr(mailbox)} disabled=${current.busy}
    onClick=${() => controller.selectMailbox(mailbox.id)}>
    <span class="mailbox-icon"><${MailboxIcon} mailbox=${mailbox} /></span>
    <span class="mailbox-name">${controller.mailboxLabel(mailbox)}</span>
    ${mailbox.retired && html`<span class="mailbox-tag" title="This agent has been retired">retired</span>`}
    ${prevGen && html`<span class="mailbox-tag" title="A superseded past generation of this agent — a conversation left behind by a reincarnate / /clear">prev gen</span>`}
    <span class="mailbox-count" title=${countTitle}>${mailbox.total}</span>
    ${mailbox.unread ? html`<span class="mailbox-unread">${mailbox.unread > 99 ? '99+' : mailbox.unread}</span>` : null}
  </button></div>`;
}

function WipeBar({ current, controller }) {
  if (current.busy && current.progress?.where === 'wipe') {
    return html`<div id="mail-wipe-bar" class="mail-bulk-bar"><${Progress} progress=${current.progress} /></div>`;
  }
  const count = current.selectedBoxes.size;
  return html`<div id="mail-wipe-bar" class="mail-bulk-bar" hidden=${count === 0}>
    ${count > 0 && html`<${Fragment}><span class="grow">${count} mailbox${count === 1 ? '' : 'es'} selected</span>
      <button title="Clear selection" onClick=${controller.clearBoxSelection}>clear</button>
      <button class="danger" title="Delete every message in the selected mailboxes"
        onClick=${controller.wipeSelectedMailboxes}>🗑 wipe</button></${Fragment}>`}
  </div>`;
}

function MailSidebar({ current, controller }) {
  const model = controller.mailboxView();
  const wizard = document.body.classList.contains('wizard');
  if (model.empty) {
    return html`<aside class="mail-sidebar" id="mail-sidebar"><div class="empty">${model.hasRoster
      ? 'No mailboxes match the filter.' : 'No mailboxes.'}</div></aside>`;
  }
  const agentUnread = model.agentsExpanded ? 0 : model.agents.reduce((sum, mailbox) => sum + (mailbox.unread || 0), 0);
  return html`<aside class="mail-sidebar" id="mail-sidebar">
    ${model.pinned.map(mailbox => html`<${MailboxRow} key=${mailbox.id} mailbox=${mailbox} current=${current} controller=${controller} />`)}
    ${model.groups.length > 0 && html`<div class="mailbox-section">${wizard ? 'Parties' : 'Groups'}</div>`}
    ${model.groups.map(group => html`<${Fragment} key=${group.mailbox.id}>
      <${MailboxRow} mailbox=${group.mailbox} current=${current} controller=${controller} expanded=${group.expanded} />
      ${group.expanded && (group.members.length > 0
        ? group.members.map(mailbox => html`<${MailboxRow} key=${mailbox.id} mailbox=${mailbox} nested=${true}
            prevGen=${model.prevGens.has(mailbox.id)} current=${current} controller=${controller} />`)
        : html`<div class="mailbox-row nested"><span class="mail-box-check-spacer"></span><div class="mailbox-nested-empty">no member folders shown</div></div>`)}
    </${Fragment}>`)}
    ${model.agents.length > 0 && (model.filtering
      ? html`<div class="mailbox-section">${wizard ? 'The Rookery' : 'All agent mailboxes'}</div>`
      : html`<button type="button" class=${`mailbox-section mailbox-section-toggle${agentUnread ? ' has-unread' : ''}`}
          aria-expanded=${String(model.agentsExpanded)}
          title=${model.agentsExpanded ? 'Collapse the agent list' : 'Expand the agent list'}
          onClick=${controller.toggleAgentsExpand}><span class="mailbox-section-caret">${model.agentsExpanded ? '▾' : '▸'}</span>
          ${wizard ? 'The Rookery' : 'All agent mailboxes'} (${model.agents.length})
          ${agentUnread > 0 && html` <span class="mailbox-unread">${agentUnread > 99 ? '99+' : agentUnread}</span>`}</button>`)}
    ${model.agents.length > 0 && model.agentsExpanded && html`<${Fragment}>
      <div class="mailbox-section-help">Tick a mailbox to select it for bulk wipe; the 🗑 bar at the top of the sidebar then deletes every stored message in the ticked mailboxes.</div>
      ${model.agents.map(mailbox => html`<${MailboxRow} key=${mailbox.id} mailbox=${mailbox}
        prevGen=${model.prevGens.has(mailbox.id)} current=${current} controller=${controller} />`)}
    </${Fragment}>`}
  </aside>`;
}

function AccessRow({ request, current, controller }) {
  const handled = !controller.accessIsPending(request);
  const active = String(current.selectedMsgId || '') === request.id;
  const when = handled ? (request.decided_at || request.created_at) : request.created_at;
  const attention = controller.highlightedAccessRequest() === request.id;
  return html`<div class=${`mail-row-wrap access-row-wrap${handled ? ' handled' : ''}`}
    data-key=${request.id} data-kind="decree">
    <button class=${`mail-row access-row-item${active ? ' active' : ''}${handled ? '' : ' unread'}${attention ? ' access-attn' : ''}`}
      data-id=${request.id} onClick=${() => controller.selectMessage(request.id)}>
      <span class="mail-row-top">
        ${!handled && html`<span class="mail-row-dot" title="pending">●</span>`}
        <span class="mail-row-party" title=${idTooltip(request.agent_id, request.current_conv_id || request.conv_id)}>${controller.accessWho(request)}</span>
        <span class="mail-row-group">${controller.accessStatusText(request)}</span>
        <span class="mail-row-time">${relTime(when)}</span>
      </span>
      <span class="mail-row-subject">${controller.accessSubject(request)}</span>
    </button>
  </div>`;
}

function MessageHead({ message, aggregate, controller }) {
  if (aggregate) {
    const sender = controller.allSenderLabel(message);
    return html`<${Fragment}>
      ${sender && html`<span class="mail-row-party" title=${message.from_conv ? idTooltip(message.from_agent, message.from_conv) : undefined}>${sender}</span>`}
      <span class="mail-row-arrow">→</span>
      <span class="mail-row-party" title=${message.to_conv ? idTooltip(message.to_agent, message.to_conv) : undefined}>${controller.allRecipientLabel(message)}</span>
    </${Fragment}>`;
  }
  const outgoing = message.direction === 'out';
  const party = controller.counterparty(message);
  const conv = outgoing ? message.to_conv : message.from_conv;
  const agent = outgoing ? message.to_agent : message.from_agent;
  return html`<${Fragment}><span class=${`mail-dir ${outgoing ? 'out' : 'in'}`} title=${outgoing ? 'sent' : 'received'}>${outgoing ? '→' : '←'}</span>
    ${party && html`<span class="mail-row-party" title=${conv ? idTooltip(agent, conv) : undefined}>${party}</span>`}</${Fragment}>`;
}

function MessageRow({ message, current, aggregate, controller }) {
  const active = message.id === current.selectedMsgId;
  const unread = !message.read;
  return html`<div class="mail-row-wrap" data-key=${message.id} data-kind=${controller.msgKind(message)}>
    <input type="checkbox" class="mail-msg-check" data-id=${message.id} checked=${current.selectedMsgs.has(message.id)}
      disabled=${current.busy} title="Select message"
      onChange=${(event) => controller.toggleMessageSelection(message.id, event.currentTarget.checked)} />
    <button class=${`mail-row${active ? ' active' : ''}${unread ? ' unread' : ''}`}
      data-id=${message.id} onClick=${() => controller.selectMessage(message.id)}>
      <span class="mail-row-top">
        ${unread && html`<span class="mail-row-dot" title="unread">●</span>`}
        <${MessageHead} message=${message} aggregate=${aggregate} controller=${controller} />
        ${message.group && html`<span class="mail-row-group">${message.group}</span>`}
        <span class="mail-row-time">${relTime(message.created_at)}</span>
      </span>
      <span class="mail-row-subject">${controller.msgPreview(message)}</span>
    </button>
    <button class="mail-row-del" data-id=${message.id}
      title="Delete this message" disabled=${current.busy}
      onClick=${() => controller.deleteOneMessage(message.id)}>🗑</button>
  </div>`;
}

function MessageList({ current, controller, model }) {
  useEffect(() => {
    const id = controller.highlightedAccessRequest();
    if (!model.access || !id) return;
    const row = [...document.querySelectorAll('#mail-list .access-row-wrap')]
      .find(candidate => candidate.dataset.key === id);
    if (!row) return;
    row.scrollIntoView?.({ block: 'nearest' });
    controller.consumeAccessHighlight(id);
  });
  if (current.messageRequest?.phase === 'error') {
    return html`<div class="mail-list" id="mail-list"><div class="island-error mail-error" role="alert">
      Messages failed to load: ${current.messageRequest.error}
      <button type="button" onClick=${controller.reloadMessagesPage}>Retry</button>
    </div></div>`;
  }
  const wizard = document.body.classList.contains('wizard');
  if (model.access) {
    if (model.allAccess.length === 0) return html`<div class="mail-list" id="mail-list"><div class="empty">${wizard
      ? 'No petitions await. When a familiar begs a boon beyond its station, it appears here for your judgement.'
      : 'No pending access requests. When an agent asks for access it can’t self-grant, it appears here for your decision.'}</div></div>`;
    if (model.pendingAccess.length + model.handledAccess.length === 0) return html`<div class="mail-list" id="mail-list"><div class="empty">${wizard
      ? 'No petitions match your seeking.' : 'No access requests match the filter.'}</div></div>`;
    return html`<div class="mail-list" id="mail-list">
      ${model.pendingAccess.map(request => html`<${AccessRow} key=${request.id} request=${request} current=${current} controller=${controller} />`)}
      ${model.handledAccess.length > 0 && html`<div class="access-divider" data-key="__access_handled__">${wizard ? 'Judgements past' : 'Recently handled'}</div>`}
      ${model.handledAccess.map(request => html`<${AccessRow} key=${request.id} request=${request} current=${current} controller=${controller} />`)}
    </div>`;
  }
  if (model.messages.length === 0) return html`<div class="mail-list" id="mail-list"><div class="empty">${current.totalUnfiltered
    ? (wizard ? 'No scrolls match your seeking.' : 'No messages match the filter.')
    : (wizard ? 'This roost holds no scrolls.' : 'This mailbox is empty.')}</div></div>`;
  return html`<div class="mail-list" id="mail-list">${model.messages.map(message => html`
    <${MessageRow} key=${message.id} message=${message} current=${current} aggregate=${model.isAggregate} controller=${controller} />`)}</div>`;
}

function MessageBulkBar({ current, controller, model }) {
  if (model.access || (!model.messages?.length && !(current.busy && current.progress?.where === 'bulk'))) {
    return html`<div id="mail-bulk-bar" class="mail-bulk-bar" hidden></div>`;
  }
  if (current.busy && current.progress?.where === 'bulk') {
    return html`<div id="mail-bulk-bar" class="mail-bulk-bar"><${Progress} progress=${current.progress} /></div>`;
  }
  const count = current.selectedMsgs.size;
  const allChecked = model.messages.every(message => current.selectedMsgs.has(message.id));
  return html`<div id="mail-bulk-bar" class="mail-bulk-bar">
    <label title="Select / deselect every message on this page"><input type="checkbox" class="mail-select-all"
      checked=${allChecked} onChange=${(event) => controller.togglePageSelection(event.currentTarget.checked)} /> all</label>
    <span class="grow">${count ? `${count} selected` : ''}</span>
    ${current.selected !== 'human' && html`<${Fragment}><button disabled=${count === 0} title="Mark the selected messages read"
      onClick=${() => controller.setMessagesRead([...current.selectedMsgs], true)}>✓ read</button>
      <button disabled=${count === 0} title="Mark the selected messages unread"
        onClick=${() => controller.setMessagesRead([...current.selectedMsgs], false)}>○ unread</button></${Fragment}>`}
    <button class="danger" disabled=${count === 0} title="Delete the selected messages"
      onClick=${controller.deleteSelectedMessages}>🗑 delete selected</button>
  </div>`;
}

const PAGE_SIZES = [25, 50, 100, 200];
function MessagePager({ current, controller, model }) {
  if (model.access || !current.totalUnfiltered) return html`<div id="mail-pager" class="mail-pager" hidden></div>`;
  const page = Math.min(current.page, model.pages);
  const atStart = page <= 1;
  const atEnd = page >= model.pages;
  return html`<div id="mail-pager" class="mail-pager">
    ${model.pages > 1 && html`<${Fragment}><button title="First page" disabled=${atStart} onClick=${() => controller.goToPage(1)}>«</button>
      <button title="Previous page" disabled=${atStart} onClick=${() => controller.goToPage(page - 1)}>‹</button>
      <span class="mail-pager-pos">Page ${page} / ${model.pages}</span>
      <button title="Next page" disabled=${atEnd} onClick=${() => controller.goToPage(page + 1)}>›</button>
      <button title="Last page" disabled=${atEnd} onClick=${() => controller.goToPage(model.pages)}>»</button></${Fragment}>`}
    <span class="grow"></span><label class="mail-pager-size" title="Messages per page">
      <select class="mail-page-size" value=${current.pageSize} onChange=${(event) => controller.setPageSize(Number(event.currentTarget.value))}>
        ${PAGE_SIZES.map(size => html`<option value=${size}>${size}</option>`)}</select> / page</label>
  </div>`;
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

function HumanAttachment({ message }) {
  if (!message.attachment) return null;
  const attachment = message.attachment;
  return html`<div class="mail-attachment">
    <span class="mail-attachment-label">Agent file</span>
    <a href=${`/api/human-messages/${encodeURIComponent(message.id)}/attachment`}
      download=${attachment.filename || ''} title="Download this agent-published file">⤓ ${attachment.filename || 'attachment'}</a>
    <span class="mail-attachment-size">${attachmentSize(attachment.size_bytes)}</span>
  </div>`;
}

function HeaderRow({ label, children }) {
  if (children == null || children === '') return null;
  return html`<div class="mail-hrow"><span class="mail-hlabel">${label}</span><span class="mail-hval">${children}</span></div>`;
}

function RecipientNames({ recipients }) {
  if (!recipients?.length) return null;
  return html`<${Fragment}>${recipients.map((recipient, index) => html`<${Fragment} key=${recipient.conv_id || index}>
    ${index > 0 ? ', ' : ''}${recipient.title ? `${recipient.title} ` : ''}<span class="mail-cid"
      title=${idTooltip(recipient.agent_id, recipient.conv_id)}>${recipient.agent_id
        ? shortAgentId(recipient.agent_id, recipient.conv_id) : shortAgentId('', recipient.conv_id)}</span>
  </${Fragment}>`)}</${Fragment}>`;
}

function AccessReader({ request, controller }) {
  const wizard = document.body.classList.contains('wizard');
  if (!request) return html`<div class="empty">${wizard
    ? 'Choose a petition to weigh its boon.' : 'Select an access request to review its details.'}</div>`;
  const handled = !controller.accessIsPending(request);
  const outcome = handled ? controller.accessOutcome(request.status) : null;
  const callerState = request.caller_state === 'retired'
    ? (wizard ? 'banished' : 'retired')
    : request.caller_state === 'missing' ? (wizard ? 'lost to the mists' : 'metadata missing') : '';
  return html`<${Fragment}>
    <div class="mail-reader-head">
      <div class="mail-subject">${wizard ? 'Petition' : 'Access request'} <span class="mail-id">#${shortId(request.id)}</span></div>
      <div class="mail-headers">
        <${HeaderRow} label="From"><span class="mail-cid"
          title=${idTooltip(request.agent_id, request.current_conv_id || request.conv_id)}>${shortAgentId(request.agent_id, request.conv_id)}</span>
          ${request.conv_title ? ` · ${request.conv_title}` : ' · (title unavailable)'}${callerState ? ` · ${callerState}` : ''}</${HeaderRow}>
        <${HeaderRow} label=${wizard ? 'Calling incarnation' : 'Request generation'}><span class="mail-cid"
          title=${request.conv_id}>${shortAgentId('', request.conv_id)}</span></${HeaderRow}>
        ${request.current_conv_id && request.current_conv_id !== request.conv_id && html`<${HeaderRow}
          label=${wizard ? 'Current incarnation' : 'Current generation'}><span class="mail-cid"
            title=${request.current_conv_id}>${shortAgentId('', request.current_conv_id)}</span></${HeaderRow}>`}
        <${HeaderRow} label=${wizard ? 'State' : 'Status'}>${handled
          ? html`<span class=${`access-outcome ${outcome.cls}`}>${outcome.txt}</span>`
          : html`<span class="mail-state-pending">${controller.accessStatusText(request)}</span>`}</${HeaderRow}>
        <${HeaderRow} label=${wizard ? 'Raised' : 'Created'}>${request.created_at ? new Date(request.created_at).toLocaleString() : ''}</${HeaderRow}>
        ${!handled && html`<${HeaderRow} label=${wizard ? 'Sands' : 'Deadline'}>${controller.accessCountdown(request.deadline)}</${HeaderRow}>`}
        ${handled && request.decided_at && html`<${HeaderRow} label=${wizard ? 'Judged' : 'Decided'}>${new Date(request.decided_at).toLocaleString()}</${HeaderRow}>`}
        ${request.target_conv_id && html`<${HeaderRow} label=${wizard ? 'Quarry' : 'Target'}>${request.target_conv_title ? `${request.target_conv_title} ` : ''}
          <span class="mail-cid" title=${request.target_conv_id}>${shortAgentId('', request.target_conv_id)}</span></${HeaderRow}>`}
      </div>
    </div>
    <div class="mail-reader-body access-reader-body"><div class="access-meta">
      <div class="access-row"><span class="access-k">${wizard ? 'Boon' : 'Permission'}</span><span class="access-v mono">${request.perm}</span></div>
      ${request.path && html`<div class="access-row"><span class="access-k">${wizard ? 'Rite' : 'Endpoint'}</span><span class="access-v mono">${request.path}</span></div>`}
      ${request.target_group && html`<div class="access-row"><span class="access-k">${wizard ? 'Party' : 'Group'}</span><span class="access-v">${request.target_group}</span></div>`}
      ${request.target_conv_id && html`<div class="access-row"><span class="access-k">${wizard ? 'Quarry' : 'Target'}</span><span class="access-v">${request.target_conv_title || request.target_conv_id}</span></div>`}
      ${request.body && html`<div class="access-row access-body-row"><span class="access-k">${request.body_label || 'Body'}</span><pre class="access-body">${request.body}</pre></div>`}
    </div></div>
    <div class="mail-reader-actions access-reader-actions">
      ${handled ? html`<${Fragment}><span class=${`access-outcome ${outcome.cls}`}>${outcome.txt}</span>${request.decided_at && html`<span class="access-decided-at">${relTime(request.decided_at)}</span>`}</${Fragment}>`
        : html`<${Fragment}><span class="access-countdown" title="If you don't decide, this request is automatically declined.">${controller.accessCountdown(request.deadline)}</span>
          <span class="grow"></span><button class="access-btn extend" title="Push the auto-decline back 5 minutes"
            onClick=${() => controller.decideAccess(request.id, 'extend')}>+5m</button>
          ${request.auto_grantable && html`<button class="access-btn always" title="Approve now AND remember this permission for this agent, so it won't ask again"
            onClick=${() => controller.decideAccess(request.id, 'always')}>${wizard ? 'Grant ever after' : 'Always allow'}</button>`}
          <button class="access-btn deny" onClick=${() => controller.decideAccess(request.id, 'deny')}>${wizard ? 'Refuse' : 'Decline'}</button>
          <button class="access-btn approve" onClick=${() => controller.decideAccess(request.id, 'approve')}>${wizard ? 'Grant' : 'Approve'}</button></${Fragment}>`}
    </div>
  </${Fragment}>`;
}

export function messageDeliveryState(message) {
  if (message.nudge_discarded_at) return 'discarded while offline';
  if (message.delivered_at) return 'delivered';
  if (message.direction === 'out') return 'undelivered';
  return '';
}

function MessageReader({ current, controller, model }) {
  const wizard = document.body.classList.contains('wizard');
  if (model.access) {
    if (model.allAccess.length === 0) return html`<div class="mail-reader" id="mail-reader"><div class="empty">${wizard
      ? 'No petitions await judgement.' : 'No access requests to review.'}</div></div>`;
    const request = model.allAccess.find(item => item.id === String(current.selectedMsgId || ''));
    return html`<div class="mail-reader" id="mail-reader" data-kind=${request ? 'decree' : undefined}><${AccessReader} request=${request} controller=${controller} /></div>`;
  }
  const message = model.messages.find(item => item.id === current.selectedMsgId);
  if (!message) return html`<div class="mail-reader" id="mail-reader"><div class="empty">${wizard
    ? 'Unfurl a scroll to read it.' : 'Select a message to read.'}</div></div>`;
  const when = message.created_at ? new Date(message.created_at).toLocaleString() : '';
  let to = message.to_recipients?.length ? html`<${RecipientNames} recipients=${message.to_recipients} />` : null;
  if (!to && (message.to_title || message.to_conv)) to = html`<${Fragment}>${message.to_title ? `${message.to_title} ` : ''}${message.to_conv && html`<span class="mail-cid"
    title=${idTooltip(message.to_agent, message.to_conv)}>${shortAgentId(message.to_agent, message.to_conv)}</span>`}</${Fragment}>`;
  const deliveryState = messageDeliveryState(message);
  const stateBits = html`<${Fragment}>${message.read ? 'read' : html`<span class="mail-state-unread">unread</span>`}${deliveryState
    ? html`<${Fragment}> · <span class=${deliveryState === 'delivered' ? '' : 'mail-state-pending'}>${deliveryState}</span></${Fragment}>` : null}</${Fragment}>`;
  const human = current.selected === 'human';
  const fromTarget = message.from_agent || message.from_conv;
  const senderLive = controller.senderOnline(message.from_agent, message.from_conv);
  return html`<div class="mail-reader" id="mail-reader" data-kind=${controller.msgKind(message)}>
    <div class="mail-reader-head"><div class="mail-subject">${message.subject || '(no subject)'} <span class="mail-id">#${message.id}</span></div>
      <div class="mail-headers">
        ${message.operator_authored
          ? html`<${HeaderRow} label="From">${controller.allSenderLabel(message)}</${HeaderRow}>`
          : message.from_conv && html`<${HeaderRow} label="From">${message.from_title ? `${message.from_title} ` : ''}<span class="mail-cid"
            title=${idTooltip(message.from_agent, message.from_conv)}>${shortAgentId(message.from_agent, message.from_conv)}</span></${HeaderRow}>`}
        <${HeaderRow} label="To">${to}</${HeaderRow}>
        ${message.cc_recipients?.length > 0 && html`<${HeaderRow} label="Cc"><${RecipientNames} recipients=${message.cc_recipients} /></${HeaderRow}>`}
        <${HeaderRow} label="Group">${message.group || ''}</${HeaderRow}><${HeaderRow} label="Date">${when}</${HeaderRow}>
        <${HeaderRow} label="Status">${stateBits}</${HeaderRow}>
      </div>
    </div>
    <div class="mail-reader-body"><${LinkifiedBody} text=${message.body || ''} /></div>
    ${human && html`<${HumanAttachment} message=${message} />`}
    <div class="mail-reader-actions">
      ${human && message.from_conv && html`<${Fragment}><button data-act="msg-reply" data-id=${message.id} data-agent=${message.from_agent || ''}
        data-conv=${message.from_conv} data-label=${message.from_title || message.from_conv} data-subject=${message.subject || ''}
        title="Reply to this agent — opens a dialog to send your answer back">reply</button>
        <button data-act="msg-focus" data-id=${message.id} data-conv=${fromTarget} data-label=${message.from_title || message.from_conv}
          disabled=${!senderLive} title=${senderLive ? 'Focus this agent’s terminal window and mark the message read' : 'Sending agent is offline — no window to focus'}>focus</button></${Fragment}>`}
      <button data-act=${human ? (message.read ? 'msg-mark-unread' : 'msg-mark-read') : undefined}
        onClick=${human ? undefined : () => controller.setMessagesRead([message.id], !message.read)}
        data-id=${message.id} title=${message.read ? (human ? 'Mark this message unread' : 'Mark this message unread for the recipient')
          : (human ? 'Mark this message read' : 'Mark this message read on the recipient’s behalf')}>${message.read ? 'mark unread' : 'mark read'}</button>
      <button class="danger" data-act=${human ? 'msg-delete' : undefined} data-id=${message.id}
        onClick=${human ? undefined : () => controller.deleteOneMessage(message.id)} title="Permanently delete this message">delete</button>
    </div>
  </div>`;
}

export function MailApp({ controller }) {
  const activeTab = dashboardState.activeTab.value;
  useEffect(() => {
    if (activeTab === 'messages') controller.renderMailTab();
  }, [activeTab, controller]);
  useEffect(() => {
    const refreshReselectedTab = (event) => {
      if (event.detail?.tab === 'messages') controller.renderMailTab();
    };
    document.addEventListener('tclaude:tab-reselected', refreshReselectedTab);
    return () => document.removeEventListener('tclaude:tab-reselected', refreshReselectedTab);
  }, [controller]);
  const current = controller.state.view.value;
  const messages = controller.messageView();
  const wizard = document.body.classList.contains('wizard');
  return html`<div class="mail-client">
    <input id="filter-mailboxes" type="text" class="mail-sidebar-filter"
      placeholder=${wizard ? 'Seek a familiar…' : 'Filter mailboxes (name / id)'}
      autocomplete="off" spellcheck=${false} value=${current.boxQuery}
      onInput=${(event) => controller.setBoxQuery(event.currentTarget.value)} />
    <div class="mail-list-filter">
      <input id="filter-messages" type="text"
        placeholder=${wizard ? 'Search the scrolls…' : 'Filter messages (sender / recipient / subject / body)'}
        autocomplete="off" spellcheck=${false} value=${current.messageQuery}
        onInput=${(event) => controller.setMessageQuery(event.currentTarget.value)} />
      <span class="filter-count" id="filter-messages-count">${controller.messageCountText()}</span>
      <button class="clear-filter" id="filter-messages-clear" title="Clear filter"
        aria-label="Clear message filter" onClick=${() => {
          controller.setMessageQuery('');
          document.querySelector('#filter-messages')?.focus();
        }}>×</button>
      <button class="tool" id="mail-mark-all" data-act="msg-mark-all-read"
        title="Mark every human notification read" hidden=${current.selected !== 'human'}>✓ mark all read</button>
      <button class="tool" id="mail-clear-read" data-act="msg-clear"
        title="Delete every human notification that has been marked read" hidden=${current.selected !== 'human'}>🧹 clear read</button>
      <button class="tool" id="mail-agent-mark-all" data-act="mail-agent-mark-all"
        title="Mark every message this agent has received as read (on its behalf)"
        onClick=${controller.markAllAgentRead}
        hidden=${current.selected === 'human' || current.selected === 'all' || current.selected.startsWith('group:')}>✓ mark all read</button>
    </div>
    <div class="mail-col mail-sidebar-col">
      <${WipeBar} current=${current} controller=${controller} />
      ${current.mailboxRequest?.phase === 'error' && html`<div class="island-error mail-error" role="alert">
        Mailboxes failed to refresh: ${current.mailboxRequest.error}
        <button type="button" onClick=${controller.reloadMail}>Retry</button>
      </div>`}
      <${MailSidebar} current=${current} controller=${controller} />
      <label class="mail-sidebar-foot"
        title="Show folders for retired agents, and include their messages in the “All agent messages” firehose">
        <input type="checkbox" id="mail-show-retired" checked=${current.showRetired}
          disabled=${current.busy} onChange=${(event) => controller.setShowRetired(event.currentTarget.checked)} /> show retired agents
      </label>
      <label class="mail-sidebar-foot" title="Show folders for agents that have never sent or received a message">
        <input type="checkbox" id="mail-show-empty" checked=${current.showEmpty}
          disabled=${current.busy} onChange=${(event) => controller.setShowEmpty(event.currentTarget.checked)} /> show agents without messages
      </label>
      <label class="mail-sidebar-foot"
        title="Show folders for previous (superseded) generations of agents — past conversations left behind by a reincarnate / /clear. Only hides the folders in this list; no messages are hidden.">
        <input type="checkbox" id="mail-show-prev-gens" checked=${current.showPrevGens}
          disabled=${current.busy} onChange=${(event) => controller.setShowPrevGens(event.currentTarget.checked)} /> show previous generations
      </label>
    </div>
    <div class="mail-col mail-list-col">
      <${MessageBulkBar} current=${current} controller=${controller} model=${messages} />
      <${MessageList} current=${current} controller=${controller} model=${messages} />
      <${MessagePager} current=${current} controller=${controller} model=${messages} />
    </div>
    <${MessageReader} current=${current} controller=${controller} model=${messages} />
    <div class="mail-gutter" data-boundary="sidebar-list"
      title="Drag to resize · double-click to reset"></div>
    <div class="mail-gutter" data-boundary="list-reader"
      title="Drag to resize · double-click to reset"></div>
  </div>`;
}

export function mountMailIsland({ host, controller, registerCleanup }) {
  render(html`<${MailApp} controller=${controller} />`, host);
  const unregister = registerMailController(controller);
  const disposeLegacy = controller.initMail();
  registerCleanup(() => render(null, host));
  registerCleanup(unregister);
  registerCleanup(() => disposeLegacy?.());
}
