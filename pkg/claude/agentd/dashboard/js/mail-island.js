import { Fragment, h, render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { registerMailController } from './mail-bridge.js';
import {
  enterFirstRow,
  focusRow,
  leaveFirstRow,
  movePaneFocus,
  moveRowSelection,
} from './mail-keynav.js';
import { idTooltip, relTime, shortAgentId, shortId } from './helpers.js';
import { dashboardState } from './snapshot-store.js';
import { hasShownOverlay } from './overlay-stack.js';
import {
  AgentMailAttachments, FolderCaret, FolderRow, HeaderRow, LoadMore, MessageAttachments, HumanActions, DecisionChoices, MailFilters, MailFooterToggles, MailGutters,
  MailPager, MailRow, MessageBody, MessageHeaders, Progress, SectionToggle, SelectAll,
} from './mail-parts.js';
import { MessageAction } from './message-action.js';
import { featureState, humanReadShared } from './peer-view-limits.js';
import { pageHumanRoute } from './mail-state.js';
import { refreshPageNotifications } from './peer-notifications.js';
import { AwayAnswer, PEER_MAIL_EVENT, PeerMailHost, awayTicket, peerLabelOf, peerOfMessage, replySubject } from './peer-mail.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { remoteNodeID } from './fleet-model.js';
import { ProposalReader } from './proposal-reader.js';
import { proposalTurn } from './proposals-model.js';

// openPeerMail opens the peer mail dialog (peer operator mail goes out
// through this node, so it is offered only on this node's own view).
function openPeerMail(detail) {
  document.dispatchEvent(new CustomEvent(PEER_MAIL_EVENT, { detail }));
}
import { TTL_CHOICES, USAGE_PERM, parseUSD, ttlText } from './peer-access.js';
import { LockReason, trustLock } from './lock-reason.js';
import { slugInfo } from './fleet-admin-model.js';
import { RemoteInboxSection } from './remote-inbox.js';
import { openFleetPage } from './fleet-move-drop.js';
import { createFleetAdminActions } from './fleet-admin-actions.js';

const html = htm.bind(h);

// The two list panes' arrow-key wiring (mail-keynav.js). Each names the row
// button ↑/↓ walk between, plus the wrapper that owns a row's out-of-button
// controls, so a move started from a row's checkbox or 🗑 continues from that
// row rather than restarting at the selection.
const SIDEBAR_ROWS = { rowSelector: '.mailbox', wrapSelector: '.mailbox-row' };
const LIST_ROWS = { rowSelector: '.mail-row', wrapSelector: '.mail-row-wrap' };

// Bare ←/→ move between the three mail panes (movePaneFocus).
function mailPanes(controller) {
  return [
    { selector: '#mail-sidebar', rows: SIDEBAR_ROWS },
    { selector: '#mail-list', rows: LIST_ROWS, select: (row) => controller.selectMessage(row.dataset.id) },
    { selector: '#mail-reader' },
  ];
}


function MailboxIcon({ mailbox }) {
  if (mailbox.kind === 'all') return '🗂';
  if (mailbox.kind === 'human') return '📬';
  if (mailbox.kind === 'access-requests') return '🔐';
  if (mailbox.kind === 'group') return '👥';
  return html`<span class=${`mail-dot ${mailbox.online ? 'online' : 'offline'}`}>●</span>`;
}

function MailboxRow({ mailbox, nested = false, prevGen = false, current, controller, expanded = false }) {
  const empty = mailbox.kind === 'agent' && !mailbox.total;
  const countTitle = mailbox.kind === 'group'
    ? `${mailbox.members || 0} member${mailbox.members === 1 ? '' : 's'} · ${mailbox.total} message${mailbox.total === 1 ? '' : 's'}`
    : mailbox.kind === 'access-requests'
      ? `${mailbox.unread || 0} pending · ${mailbox.total} total access request${mailbox.total === 1 ? '' : 's'}`
      : `${mailbox.in} received · ${mailbox.out} sent`;
  let lead = null;
  // Wiping is this node's operator's: a peer serves it under unrestricted
  // trust only.
  if (mailbox.kind === 'agent' && !nested && operatorHere()) {
    lead = html`<input type="checkbox" class="mail-box-check" data-conv=${mailbox.id}
      checked=${current.selectedBoxes.has(mailbox.id)} disabled=${current.busy} title="Select for bulk wipe"
      onChange=${(event) => controller.toggleBoxSelection(mailbox.id, event.currentTarget.checked)} />`;
  } else if (mailbox.kind === 'group') {
    lead = html`<${FolderCaret} expanded=${expanded} disabled=${current.busy} group=${mailbox.title}
      onToggle=${() => controller.toggleGroupExpand(mailbox.title)} />`;
  }
  return html`<${FolderRow} id=${mailbox.id} icon=${html`<${MailboxIcon} mailbox=${mailbox} />`}
    label=${controller.mailboxLabel(mailbox)} title=${controller.mailboxTitleAttr(mailbox)}
    className=${`${mailbox.retired ? 'retired' : ''}${empty ? ' empty-box' : ''}${prevGen ? ' prev-gen' : ''}`.trim()}
    nested=${nested} lead=${lead} active=${mailbox.id === current.selected} unread=${mailbox.unread}
    total=${mailbox.total} totalTitle=${countTitle} disabled=${current.busy}
    tags=${html`${mailbox.retired && html`<span class="mailbox-tag" title="This agent has been retired">retired</span>`}
      ${prevGen && html`<span class="mailbox-tag" title="A superseded past generation of this agent — a conversation left behind by a reincarnate / /clear">prev gen</span>`}`}
    onSelect=${() => controller.selectMailbox(mailbox.id, { navigate: true })} />`;
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
  // The flat agent list is server-paged, so its count and folded unread
  // badge come from the roster's totals, not the loaded page.
  const agentUnread = model.agentsExpanded ? 0 : model.agentsUnread;
  // ↑/↓ walk the folders in painted order — pinned, then each group with its
  // expanded members, then the agent folders. Section headings and expand
  // carets are not folders, so they are simply not on the path.
  const moveRow = (event) => moveRowSelection({
    event, ...SIDEBAR_ROWS, select: (row) => { controller.selectMailbox(row.dataset.id); },
  });
  const onKeyDown = (event) => {
    const client = event.currentTarget.closest('.mail-client');
    if (leaveFirstRow({
      event, ...SIDEBAR_ROWS, filter: client?.querySelector('#filter-mailboxes'),
    })) return;
    moveRow(event);
  };
  return html`<aside class="mail-sidebar" id="mail-sidebar" onKeyDown=${onKeyDown}>
    ${model.pinned.map(mailbox => html`<${MailboxRow} key=${mailbox.id} mailbox=${mailbox} current=${current} controller=${controller} />`)}
    ${model.groups.length > 0 && html`<div class="mailbox-section">${wizard ? 'Parties' : 'Groups'}</div>`}
    ${model.groups.map(group => html`<${Fragment} key=${group.mailbox.id}>
      <${MailboxRow} mailbox=${group.mailbox} current=${current} controller=${controller} expanded=${group.expanded} />
      ${group.expanded && (group.members.length > 0
        ? group.members.map(mailbox => html`<${MailboxRow} key=${mailbox.id} mailbox=${mailbox} nested=${true}
            prevGen=${model.prevGens.has(mailbox.id)} current=${current} controller=${controller} />`)
        : html`<div class="mailbox-row nested"><span class="mail-box-check-spacer"></span><div class="mailbox-nested-empty">no member folders shown</div></div>`)}
      ${group.expanded && group.membersTotal > group.members.length && html`<${LoadMore} nested=${true} shown=${group.members.length}
        total=${group.membersTotal} busy=${current.busy} what=${`${group.mailbox.title}'s member mailboxes`}
        onMore=${() => controller.loadMoreMembers(group.mailbox.title)} />`}
    </${Fragment}>`)}
    ${model.agentsTotal > 0 && (model.filtering
      ? html`<div class="mailbox-section">${wizard ? 'The Rookery' : 'All agent mailboxes'}</div>`
      : html`<${SectionToggle} expanded=${model.agentsExpanded} unread=${agentUnread} onToggle=${controller.toggleAgentsExpand}
          label=${`${wizard ? 'The Rookery' : 'All agent mailboxes'} (${model.agentsTotal})`} />`)}
    ${model.agentsTotal > 0 && model.agentsExpanded && html`<${Fragment}>
      ${operatorHere() && html`<div class="mailbox-section-help">Tick a mailbox to select it for bulk wipe; the 🗑 bar at the top of the sidebar then deletes every stored message in the ticked mailboxes.</div>`}
      ${model.agents.map(mailbox => html`<${MailboxRow} key=${mailbox.id} mailbox=${mailbox}
        prevGen=${model.prevGens.has(mailbox.id)} current=${current} controller=${controller} />`)}
      ${model.moreAgents > 0 && html`<${LoadMore} shown=${model.agents.length} total=${model.agentsTotal}
        busy=${current.busy} what="agent mailboxes" onMore=${controller.loadMoreAgents} />`}
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
      data-id=${request.id} aria-current=${active ? 'true' : undefined}
      onClick=${(event) => { focusRow(event.currentTarget); controller.selectMessage(request.id, { navigate: true }); }}>
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

function MessageRow({ message, current, aggregate, controller }) {
  return html`<${MailRow} id=${message.id} message=${message} kind=${controller.msgKind(message)} aggregate=${aggregate}
    preview=${controller.msgPreview(message)} active=${message.id === current.selectedMsgId}
    checked=${current.selectedMsgs.has(message.id)} busy=${current.busy}
    onCheck=${(checked) => controller.toggleMessageSelection(message.id, checked)}
    onSelect=${() => controller.selectMessage(message.id, { navigate: true })}
    onDelete=${() => controller.deleteOneMessage(message.id)}
    canDelete=${operatorHere()} deleteTitle=${trustLock('', 'deleting its messages') || 'Delete this message'} />`;
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
  // ↑/↓ move the open message one row up or down the page. The access folder's
  // "Recently handled" divider is not a row, so a move crosses it without a
  // wasted keypress.
  const moveRow = (event) => moveRowSelection({
    event, ...LIST_ROWS, select: (row) => controller.selectMessage(row.dataset.id),
  });
  const onKeyDown = (event) => {
    const client = event.currentTarget.closest('.mail-client');
    if (leaveFirstRow({
      event, ...LIST_ROWS, filter: client?.querySelector('#filter-messages'),
    })) return;
    moveRow(event);
  };
  if (model.access) {
    if (model.allAccess.length === 0) return html`<div class="mail-list" id="mail-list"><div class="empty">${wizard
      ? 'No petitions await. When a hero begs a boon beyond its station, it appears here for your judgement.'
      : 'No pending access requests. When an agent asks for access it can’t self-grant, it appears here for your decision.'}</div></div>`;
    if (model.pendingAccess.length + model.handledAccess.length === 0) return html`<div class="mail-list" id="mail-list"><div class="empty">${wizard
      ? 'No petitions match your seeking.' : 'No access requests match the filter.'}</div></div>`;
    // Pending rows, the divider, and handled rows are ONE keyed child list, not
    // three interpolations. This folder regroups itself: a request decided
    // elsewhere — the tray, another browser, or its auto-decline running out —
    // crosses from the pending group to the handled one on an ordinary refresh,
    // with nobody having touched the pane. Across separate child slots Preact
    // reconciles each slot on its own, so the moved row is unmounted and
    // remounted; that drops the focus ↑/↓ navigation rides on and the pane stops
    // answering the keyboard mid-flow. In one keyed list the row is reordered,
    // and the operator keeps their place.
    const accessRows = [
      ...model.pendingAccess.map(request => html`<${AccessRow} key=${request.id} request=${request} current=${current} controller=${controller} />`),
      ...(model.handledAccess.length > 0
        ? [html`<div key="__access_handled__" class="access-divider" data-key="__access_handled__">${wizard ? 'Judgements past' : 'Recently handled'}</div>`]
        : []),
      ...model.handledAccess.map(request => html`<${AccessRow} key=${request.id} request=${request} current=${current} controller=${controller} />`),
    ];
    return html`<div class="mail-list" id="mail-list" onKeyDown=${onKeyDown}>${accessRows}</div>`;
  }
  if (model.messages.length === 0) return html`<div class="mail-list" id="mail-list"><div class="empty">${current.totalUnfiltered
    ? (wizard ? 'No scrolls match your seeking.' : 'No messages match the filter.')
    : (wizard ? 'This roost holds no scrolls.' : 'This mailbox is empty.')}</div></div>`;
  return html`<div class="mail-list" id="mail-list" onKeyDown=${onKeyDown}>${model.messages.map(message => html`
    <${MessageRow} key=${message.id} message=${message} current=${current} aggregate=${model.isAggregate} controller=${controller} />`)}</div>`;
}

// Folder-wide actions (whole Human folder / whole agent inbox) share the
// bulk bar with the per-selection actions, after a divider. They stay
// reachable on an empty folder: the bar then carries only them (✉ peer is
// still useful with no notifications).
function FolderActions({ current, controller, solo = false }) {
  const human = current.selected === 'human';
  // A peer's page shows the same bar. Mark all read goes to the peer (the
  // human folder's through its human-inbox route, where the peer allows it;
  // an agent folder's without data-act, so the peer-view guard lets it
  // through to the peer, which decides). The rest stays the peer
  // operator's own, greyed in place.
  const remote = remoteNodeID();
  if (human && remote) {
    const name = remoteNodeName(remote);
    const canMark = humanReadShared(dashboardState.snapshot.value?.peer_view);
    const markAll = () => postPeerJSON('/api/human-inbox/read', { all: true }).then((res) => {
      shellToast(`marked ${res?.marked || 0} message(s) read on ${name}`);
      controller.reloadMail();
    }, (e) => shellToast(`Mark all read failed: ${e?.message || e}`, true));
    const clearLock = trustLock('', 'deleting its notifications');
    const clearRead = () => shellConfirm({
      title: 'Delete read messages?', body: `Permanently deletes every notification on ${name} that has been marked read. Unread ones are kept.`,
      okLabel: 'Delete read', busyLabel: 'Deleting…', action: () => postPeerJSON(pageHumanRoute('/api/human-messages/clear', remote), {}),
    }).then((res) => { if (res) { shellToast(`Deleted read notifications on ${name}`); controller.reloadMail(); } },
      (e) => shellToast(`Delete failed: ${e?.message || e}`, true));
    return html`<span class=${solo ? 'mail-folder-actions solo' : 'mail-folder-actions'}>
      <button id="mail-mark-all" disabled=${!canMark} onClick=${markAll}
        title=${canMark ? `Mark every human notification on ${name} read` : undefined}>✓ mark all read</button><${LockReason} why=${canMark ? '' : `${name} shares its notifications for reading only: marking them read from here needs its node.messages.mark-read.`} />
      <button id="mail-peer-compose" disabled title="Message a peer's operator from this node's own Messages">✉ peer</button>
      <button class="danger" id="mail-clear-read" disabled=${!!clearLock} onClick=${clearRead}
        title=${clearLock ? undefined : `Delete every human notification on ${name} that has been marked read`}>🗑 delete read</button><${LockReason} why=${clearLock} />
    </span>`;
  }
  return html`<span class=${solo ? 'mail-folder-actions solo' : 'mail-folder-actions'}>
    <button id="mail-mark-all" data-act="msg-mark-all-read"
      title="Mark every human notification read" hidden=${!human}>✓ mark all read</button>
    <button id="mail-peer-compose" title="Message a peer's operator or agents on a peer (federation)"
      hidden=${!human} onClick=${() => openPeerMail({})}>✉ peer</button>
    <button class="danger" id="mail-clear-read" data-act="msg-clear"
      title="Delete every human notification that has been marked read" hidden=${!human}>🗑 delete read</button>
    <button id="mail-agent-mark-all" data-act=${remote ? undefined : 'mail-agent-mark-all'}
      title="Mark every message this agent has received as read (on its behalf)"
      onClick=${controller.markAllAgentRead} hidden=${human}>✓ mark all read</button>
  </span>`;
}

function MessageBulkBar({ current, controller, model }) {
  // "All agent messages" and group folders span many inboxes, so no
  // folder-wide action applies there.
  const folderActions = !model.access && current.selected !== 'all' && !current.selected.startsWith('group:');
  if (model.access) return html`<div id="mail-bulk-bar" class="mail-bulk-bar" hidden></div>`;
  if (current.busy && current.progress?.where === 'bulk') {
    return html`<div id="mail-bulk-bar" class="mail-bulk-bar"><${Progress} progress=${current.progress} /></div>`;
  }
  if (!model.messages?.length) {
    if (!folderActions) return html`<div id="mail-bulk-bar" class="mail-bulk-bar" hidden></div>`;
    return html`<div id="mail-bulk-bar" class="mail-bulk-bar">
      <span class="grow"></span>
      <${FolderActions} current=${current} controller=${controller} solo />
    </div>`;
  }
  const count = current.selectedMsgs.size;
  const allChecked = model.messages.every(message => current.selectedMsgs.has(message.id));
  // Deleting a peer's mail is its operator's: unrestricted trust only.
  const deleteLock = trustLock('', 'deleting its messages');
  return html`<div id="mail-bulk-bar" class="mail-bulk-bar">
    <${SelectAll} checked=${allChecked} onChange=${controller.togglePageSelection} />
    <span class="grow">${count ? `${count} selected` : ''}</span>
    ${current.selected !== 'human' && html`<${Fragment}><button disabled=${count === 0} title="Mark the selected messages read"
      onClick=${() => controller.setMessagesRead([...current.selectedMsgs], true)}>✓ read</button>
      <button disabled=${count === 0} title="Mark the selected messages unread"
        onClick=${() => controller.setMessagesRead([...current.selectedMsgs], false)}>○ unread</button></${Fragment}>`}
    <button class="danger" disabled=${count === 0 || !!deleteLock}
      title=${deleteLock ? undefined : 'Delete the selected messages'}
      onClick=${controller.deleteSelectedMessages}>🗑 delete selected</button><${LockReason} why=${deleteLock} />
    ${folderActions && html`<${FolderActions} current=${current} controller=${controller} />`}
  </div>`;
}

function MessagePager({ current, controller, model }) {
  return html`<${MailPager} id="mail-pager" hidden=${model.access || !current.totalUnfiltered}
    page=${current.page} pages=${model.pages} pageSize=${current.pageSize}
    onPage=${controller.goToPage} onPageSize=${controller.setPageSize} />`;
}


// GroupLinkReader is a peer's cross-node link request (tcl-2ksi0k): one exact
// directed edge into one of this node's groups. Approving stores only that
// edge; "allow future" also grants the peer groups.link.accept for the
// destination group. It never grants the peer broader messaging.
function GroupLinkReader({ request, controller }) {
  const link = request.link || {};
  const peer = link.source_peer_label || request.conv_title || request.origin_peer || 'peer';
  // The feed also lists requests this node sent; those only show their state.
  const sent = request.proposal ? proposalTurn(request.proposal) === 'them' || (!!request.proposal.ours && !proposalTurn(request.proposal))
    : link.direction === 'out';
  const handled = !controller.accessIsPending(request) || sent;
  const outcome = handled ? controller.accessOutcome(request.status) : null;
  const mode = String(link.mode || 'members->members').replace('->', ' → ');
  // direction is relative to this node: "in" lands on our group (source is
  // the peer's), "out" lands on the peer's group (source is ours).
  const into = link.direction !== 'out';
  const src = into ? `${peer}:${link.source_group || '?'}` : (link.source_group || '?');
  const dst = into ? (link.dest_group || '?') : `${peer}:${link.dest_group || '?'}`;
  const who = link.mode === 'owners->members' ? 'owners' : 'members';
  return html`<${Fragment}>
    <div class="mail-reader-head">
      <div class="mail-subject">🌐 Cross-node link request${sent ? ' (sent)' : ''} <span class="mail-id">#${shortId(request.id)}</span></div>
      <div class="mail-headers">
        <${HeaderRow} label=${sent ? 'To peer' : 'From peer'}>${peer} ${request.origin_peer && html`<span class="mail-cid">${request.origin_peer}</span>`}</${HeaderRow}>
        <${HeaderRow} label="Status">${sent && request.status === 'pending'
          ? html`<span class="mail-state-pending">waiting for ${peer}'s operator · ${controller.accessCountdown(request.deadline)}</span>`
          : handled
          ? html`<span class=${`access-outcome ${outcome.cls}`}>${outcome.txt}</span>`
          : html`<span class="mail-state-pending">pending · auto-declines ${controller.accessCountdown(request.deadline)}</span>`}</${HeaderRow}>
        <${HeaderRow} label="Created">${request.created_at ? new Date(request.created_at).toLocaleString() : ''}</${HeaderRow}>
      </div>
    </div>
    <div class="mail-reader-body access-reader-body"><div class="access-meta">
      ${sent ? html`
        <div class="access-row"><span class="access-k">You asked</span><span class="access-v">${who} of <b>${src}</b> to message members of <b>${dst}</b> (${mode})</span></div>
        <div class="access-row"><span class="access-k">Cancel</span><span class="access-v">from Inter-group links (cancel request)</span></div>` : html`
        <div class="access-row"><span class="access-k">Wants</span><span class="access-v">${who} of <b>${src}</b> may message members of <b>${dst}</b></span></div>
        <div class="access-row"><span class="access-k">Edge</span><span class="access-v mono">${src} → ${dst} · ${mode}</span></div>
        <div class="access-row"><span class="access-k">Approving authorizes</span><span class="access-v">exactly this edge and mode, nothing broader.</span></div>
        <div class="access-row"><span class="access-k">Revoke</span><span class="access-v">any time from Inter-group links; takes effect on this node immediately</span></div>`}
    </div></div>
    <div class="mail-reader-actions access-reader-actions">
      ${handled ? html`<${Fragment}><span class=${`access-outcome ${outcome.cls}`}>${outcome.txt}</span>${request.decided_at && html`<span class="access-decided-at">${relTime(request.decided_at)}</span>`}</${Fragment}>`
        : html`<${Fragment}><span class="access-countdown" title="If you don't decide, this request is automatically declined.">${controller.accessCountdown(request.deadline)}</span>
          <span class="grow"></span>
          ${into && html`<button class="access-btn always" title=${`Approve, and let ${peer} link to ${link.dest_group} in future without asking (each link still has to be acknowledged)`}
            onClick=${() => controller.decideAccess(request.id, 'approve_future')}>Approve + allow future links into ${link.dest_group}</button>`}
          <button class="access-btn deny" onClick=${() => controller.decideAccess(request.id, 'deny')}>Decline</button>
          <button class="access-btn approve" onClick=${() => controller.decideAccess(request.id, 'approve')}>Approve</button></${Fragment}>`}
    </div>
  </${Fragment}>`;
}

function AccessReader({ request, controller }) {
  const wizard = document.body.classList.contains('wizard');
  if (request?.purpose === 'group_link') return html`<${GroupLinkReader} request=${request} controller=${controller} />`;
  if (request?.purpose === 'peer_proposal') return html`<${ProposalReader} key=${`${request.id}:${request.proposal?.revision}`} request=${request} controller=${controller} />`;
  if (!request) return html`<div class="empty">${wizard
    ? 'Choose a petition to weigh its boon.' : 'Select an access request to review its details.'}</div>`;
  const handled = !controller.accessIsPending(request);
  const outcome = handled ? controller.accessOutcome(request.status) : null;
  return html`<${Fragment}>
    <div class="mail-reader-head">
      <div class="mail-subject">${wizard ? 'Petition' : 'Access request'} <span class="mail-id">#${shortId(request.id)}</span></div>
      <div class="mail-headers">
        <${HeaderRow} label="From"><span
          title=${idTooltip(request.agent_id, request.current_conv_id || request.conv_id)}>${controller.accessWho(request)}</span></${HeaderRow}>
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
      ${request.scope_display && html`<div class="access-row"><span class="access-k">${wizard ? 'Bounds' : 'Scope'}</span><span class="access-v mono"
        title="What a scoped “always allow” would be limited to — the typed context this gate evaluates grants against">${request.scope_display}</span></div>`}
      ${request.origin_peer && html`<div class="access-row"><span class="access-k">Peer</span><span class="access-v">${request.conv_title || request.origin_peer} <span class="mail-cid">${request.origin_peer}</span></span></div>
        <div class="access-row"><span class="access-k">Asks for</span><span class="access-v">${request.group_id ? `group ${request.target_group || `#${request.group_id}`}` : slugInfo(request.perm)?.kind === 'node' ? 'node-wide' : 'every group on this node, including future ones'} · ${ttlText(request.grant_ttl_seconds) || 'permanent'}</span></div>
        ${request.grant_expires_at && html`<div class="access-row"><span class="access-k">Grant expires</span><span class="access-v">${new Date(request.grant_expires_at).toLocaleString()}</span></div>`}`}
      ${request.body && html`<div class="access-row access-body-row"><span class="access-k">${request.body_label || 'Body'}</span><pre class="access-body">${request.body}</pre></div>`}
    </div></div>
    <div class="mail-reader-actions access-reader-actions">
      ${handled ? html`<${Fragment}><span class=${`access-outcome ${outcome.cls}`}>${outcome.txt}</span>${request.decided_at && html`<span class="access-decided-at">${relTime(request.decided_at)}</span>`}</${Fragment}>`
        : request.origin_peer && request.perm === USAGE_PERM ? html`<${UsageAccessDecision} key=${request.id} request=${request} controller=${controller} />`
        : request.origin_peer ? html`<${PeerAccessDecision} request=${request} controller=${controller} />`
                : html`<${Fragment}><span class="access-countdown" title="If you don't decide, this request is automatically declined.">${controller.accessCountdown(request.deadline)}</span>
          <span class="grow"></span><button class="access-btn extend" title="Push the auto-decline back 5 minutes"
            onClick=${() => controller.decideAccess(request.id, 'extend')}>+5m</button>
          ${request.auto_grantable && request.scope_display && html`<button class="access-btn always scoped"
            title=${`Approve now AND remember this permission for this agent, but only for ${request.scope_display} — it will still ask for anything else`}
            onClick=${() => controller.decideAccess(request.id, 'always_scoped')}>${wizard
    ? `Grant ever after — ${request.scope_display}`
    : `Always allow — ${request.scope_display}`}</button>`}
          ${request.auto_grantable && html`<button class="access-btn always" title=${request.scope_display
    ? "Approve now AND remember this permission for this agent in EVERY context, not just this one"
    : "Approve now AND remember this permission for this agent, so it won't ask again"}
            onClick=${() => controller.decideAccess(request.id, 'always')}>${wizard
    ? 'Grant ever after'
    : request.scope_display ? 'Always allow — any scope' : 'Always allow'}</button>`}
          <button class="access-btn deny" onClick=${() => controller.decideAccess(request.id, 'deny')}>${wizard ? 'Refuse' : 'Decline'}</button>
          <button class="access-btn approve" onClick=${() => controller.decideAccess(request.id, 'approve')}>${wizard ? 'Grant' : 'Approve'}</button></${Fragment}>`}
    </div>
  </${Fragment}>`;
}

// PeerAccessDecision decides a peer operator's request for a peer grant on
// this node. Only the local operator decides these (never an away cover);
// approval can shorten the grant or narrow an any-group request to one shared
// group, never widen it, and there is no "always" for peers.
function PeerAccessDecision({ request, controller, loadLinks = defaultLinks }) {
  const [ttl, setTtl] = useState(String(request.grant_ttl_seconds ?? 3600));
  const [group, setGroup] = useState('');
  const [groups, setGroups] = useState([]);
  const info = slugInfo(request.perm);
  const kind = info?.kind || 'node';
  // Approval may shorten the grant, never lengthen it: offer the requested
  // lifetime and the shorter choices (all of them when permanent was asked).
  const asked = Number(request.grant_ttl_seconds ?? 3600);
  const ttlChoices = [...TTL_CHOICES.filter((c) => asked === 0 || (c.s !== 0 && c.s <= asked)),
    ...(TTL_CHOICES.some((c) => c.s === asked) ? [] : [{ s: asked, label: ttlText(asked) }])].sort((a, b) => (a.s || Infinity) - (b.s || Infinity));
  const narrowable = !request.group_id && kind === 'group';
  useEffect(() => {
    if (!narrowable) return;
    loadLinks().then((rows) => setGroups(rows.filter((g) => (g.federation_links || []).some((l) => l?.peer === request.origin_peer && l.kind === 'grant')).map((g) => ({ id: g.group_id, name: g.name }))))
      .catch(() => setGroups([]));
  }, [request.id]);
  const peer = request.conv_title || request.origin_peer;
  const where = request.group_id ? `in group ${request.target_group || `#${request.group_id}`}` : group ? `in group ${groups.find((g) => String(g.id) === group)?.name || group}` : kind === 'node' ? 'node-wide' : 'in every group on this node, including future ones';
  const approve = () => controller.decideAccess(request.id, 'approve', { grant_ttl_seconds: Number(ttl), ...(group ? { group_id: Number(group) } : {}) });
  return html`<${Fragment}><span class="access-countdown" title="If you don't decide, this request is automatically declined.">${controller.accessCountdown(request.deadline)}</span>
    <span class="access-peer-consequence">Approving lets ${peer} use <code>${request.perm}</code> (${info?.what || 'see the slug registry'}) ${where} ${Number(ttl) ? `for ${ttlText(Number(ttl))}` : 'permanently'}.${info?.warning ? html` <b class="fa-danger">${info.warning}</b>` : ''}</span>
    <span class="grow"></span>
    ${narrowable && groups.length > 0 && html`<select class="access-peer-group" title="Narrow to one shared group" value=${group} onChange=${(e) => setGroup(e.currentTarget.value)}>
      <option value="">every group, including future ones</option>${groups.map((g) => html`<option key=${g.id} value=${String(g.id)}>${g.name}</option>`)}</select>`}
    <select class="access-peer-ttl" title="How long the grant lasts" value=${ttl} onChange=${(e) => setTtl(e.currentTarget.value)}>
      ${ttlChoices.map((c) => html`<option key=${c.s} value=${String(c.s)}>${c.label}</option>`)}</select>
    <button class="access-btn extend" title="Push the auto-decline back 5 minutes" onClick=${() => controller.decideAccess(request.id, 'extend')}>+5m</button>
    <button class="access-btn deny" onClick=${() => controller.decideAccess(request.id, 'deny')}>Decline</button>
    <button class="access-btn approve" onClick=${approve}>Approve</button></${Fragment}>`;
}

// UsageAccessDecision decides a peer's usage request: billing its agents'
// model calls to one of this node's model gateways. The approver picks the
// gateway (unless one was asked for) and may lower the amount or shorten the
// time, never raise them.
function UsageAccessDecision({ request, controller }) {
  const gateways = request.usage_gateways || [];
  const [ttl, setTtl] = useState(String(request.grant_ttl_seconds ?? 3600));
  const [gateway, setGateway] = useState(request.http_proxy || gateways[0] || '');
  const [amount, setAmount] = useState(request.max_usd > 0 ? String(request.max_usd) : '');
  const asked = Number(request.grant_ttl_seconds ?? 3600);
  const ttlChoices = [...TTL_CHOICES.filter((c) => asked === 0 || (c.s !== 0 && c.s <= asked)),
    ...(TTL_CHOICES.some((c) => c.s === asked) ? [] : [{ s: asked, label: ttlText(asked) }])].sort((a, b) => (a.s || Infinity) - (b.s || Infinity));
  const usd = parseUSD(amount);
  const tooMuch = usd !== null && request.max_usd > 0 && (usd === 0 || usd > request.max_usd);
  const peer = request.conv_title || request.origin_peer;
  const approve = () => controller.decideAccess(request.id, 'approve', { grant_ttl_seconds: Number(ttl), http_proxy: gateway, max_usd: usd || 0 });
  const what = `${usd > 0 ? `up to $${usd.toFixed(2)}` : 'with no cap'} through ${gateway || 'any of your model gateways'}`;
  return html`<${Fragment}><span class="access-countdown" title="If you don't decide, this request is automatically declined.">${controller.accessCountdown(request.deadline)}</span>
    <span class="access-peer-consequence">Approving lets ${peer}'s agents bill their model calls to you, ${what}, ${Number(ttl) ? `for ${ttlText(Number(ttl))}` : 'permanently'}.${gateways.length === 0 ? html` <b class="fa-danger">This node has no enabled model gateway.</b>` : ''}${tooMuch ? html` <b class="fa-danger">At most $${Number(request.max_usd).toFixed(2)} was asked for.</b>` : ''}</span>
    <span class="grow"></span>
    ${request.http_proxy ? html`<span class="access-usage-gateway mono">${request.http_proxy}</span>` : html`<select class="access-usage-gateway" title="The model gateway it bills to" value=${gateway} onChange=${(e) => setGateway(e.currentTarget.value)}>
      ${gateways.map((g) => html`<option key=${g} value=${g}>${g}</option>`)}<option value="">any gateway</option></select>`}
    <input class="access-usage-amount" title="Dollars it may spend (blank: no cap)" inputmode="decimal" placeholder="no cap" size="7" value=${amount} onInput=${(e) => setAmount(e.currentTarget.value)} />
    <select class="access-peer-ttl" title="How long the share lasts" value=${ttl} onChange=${(e) => setTtl(e.currentTarget.value)}>
      ${ttlChoices.map((c) => html`<option key=${c.s} value=${String(c.s)}>${c.label}</option>`)}</select>
    <button class="access-btn extend" title="Push the auto-decline back 5 minutes" onClick=${() => controller.decideAccess(request.id, 'extend')}>+5m</button>
    <button class="access-btn deny" onClick=${() => controller.decideAccess(request.id, 'deny')}>Decline</button>
    <button class="access-btn approve" disabled=${usd === null || tooMuch || gateways.length === 0} onClick=${approve}>Share</button></${Fragment}>`;
}

async function defaultLinks() {
  const r = await fetch('/api/federation/links', { credentials: 'same-origin' });
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return (await r.json())?.groups || [];
}


function senderHarness(message) {
  const agent = message?.from_agent || '';
  const conv = message?.from_conv || '';
  const roster = dashboardState.snapshot.value?.agents || [];
  const sender = roster.find((entry) =>
    (agent && entry?.agent_id === agent) || (conv && entry?.conv_id === conv));
  return sender?.state?.harness || '';
}

// operatorHere reports whether this operator deletes and wipes the mail this
// page shows: this node's own, or a peer's that trusts it unrestricted.
function operatorHere() {
  return !remoteNodeID() || featureState('local_dashboard', dashboardState.snapshot.value?.peer_view) === 'shared';
}

// openRemote opens a peer agent's terminal here, through the federation
// bridge (remote-terminal.js), loaded on first use.
const openRemote = (opts) => import('./remote-terminal.js').then((m) => m.openRemoteTerminal(opts));

// remoteNodeName is how this page names the peer it shows.
function remoteNodeName(remote) {
  return document.documentElement.dataset.remoteNodeName || remote.slice(0, 13);
}

async function postPeerJSON(url, body) {
  const res = await fetch(url, { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  let data = null;
  try { data = await res.json(); } catch (_) { data = null; }
  if (!res.ok) { const e = new Error(data?.error || `HTTP ${res.status}`); e.status = res.status; throw e; }
  return data;
}

// peerHumanActions answer a notification on the peer this page shows: the
// page's fetch sends /api/human-inbox to that peer.
function peerHumanActions(remote, peerView) {
  const name = remoteNodeName(remote);
  return {
    name, exportNode: remote,
    canAnswer: featureState('human.inbox.answer', peerView) === 'shared', canMark: humanReadShared(peerView),
    // Deleting has no peer grant: unrestricted trust reaches the peer's own
    // route, as its operator does.
    canDelete: operatorHere(),
    // /api/human-messages stays on this node from a peer's page
    // (remote-node.js), so the peer's own route is named through its proxy.
    remove: (id) => postPeerJSON(pageHumanRoute('/api/human-messages/delete', remote), { id }),
    focus: (m) => openRemote({ instance: remote, agent: m.from_agent, peerLabel: name, label: m.from_title || '', toast: shellToast }),
    // The page's own list of its unread ones (a restricted peer's) follows.
    mark: (id, read) => postPeerJSON('/api/human-inbox/read', read ? { id } : { id, read: false })
      .then((res) => { refreshPageNotifications(id, read); return res; }),
    reply: (id, body) => postPeerJSON('/api/human-inbox/reply', { id, body }),
  };
}

// SPAWN_REQUEST_SUBJECT is the subject of a peer's spawn request awaiting
// this node's operator (federation_spawn.go notifyFederationSpawnRequest, and
// a failed automatic launch returned to pending).
const SPAWN_REQUEST_SUBJECT = /^remote spawn request #(\d+)( needs approval)?$/;

// ReviewSpawnRequest offers the review of the request a notification names
// while it still waits: the same peer's, and pending. A peer's mail can carry
// any subject, and an old notice outlives its decision.
function ReviewSpawnRequest({ id, instance, load = () => createFleetAdminActions().spawnRequests() }) {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    let live = true;
    setOpen(false);
    load().then((rows) => {
      if (live) setOpen((rows || []).some((r) => String(r?.id) === id && r?.instance === instance && r?.status === 'pending'));
    }, () => {});
    return () => { live = false; };
  }, [id, instance]);
  return open ? html`<button data-act="review-spawn-request" class="primary" title="Approve, change or deny it on ⚙ Fleet config → Spawn requests"
    onClick=${() => openFleetPage('spawns')}>review request…</button>` : null;
}

function MessageReader({ current, controller, model }) {
  const wizard = document.body.classList.contains('wizard');
  if (model.access) {
    if (model.allAccess.length === 0) return html`<div class="mail-reader" id="mail-reader"
      role="region" aria-label="Message reader" tabindex="0"><div class="empty">${wizard
      ? 'No petitions await judgement.' : 'No access requests to review.'}</div></div>`;
    const request = model.allAccess.find(item => item.id === String(current.selectedMsgId || ''));
    return html`<div class="mail-reader" id="mail-reader" data-kind=${request ? 'decree' : undefined}
      role="region" aria-label="Message reader" tabindex="0"><${AccessReader} request=${request} controller=${controller} /></div>`;
  }
  const message = model.messages.find(item => item.id === current.selectedMsgId);
  if (!message) return html`<div class="mail-reader" id="mail-reader"
    role="region" aria-label="Message reader" tabindex="0"><div class="empty">${wizard
    ? 'Unfurl a scroll to read it.' : 'Select a message to read.'}</div></div>`;
  const human = current.selected === 'human';
  const remote = remoteNodeID();
  const peer = human && !remote ? peerOfMessage(message) : '';
  const ticket = peer ? awayTicket(message) : '';
  // A peer's spawn request waits on this node's Spawn requests page.
  const spawnRequest = human && !remote && !message.from_conv && String(message.group || '').startsWith('federation:')
    ? SPAWN_REQUEST_SUBJECT.exec(message.subject || '') : null;
  let actions;
  if (human) {
    // A peer's notification is answered through its human-inbox routes, where
    // the peer allows it; this node's own routes would act on its own mail.
    const lead = !remote && html`<${Fragment}>
      ${spawnRequest && html`<${ReviewSpawnRequest} id=${spawnRequest[1]} instance=${String(message.group).slice('federation:'.length)} load=${controller.spawnRequests} />`}
      ${ticket && html`<${AwayAnswer} ticket=${ticket} peerLabel=${peerLabelOf(message)} confirm=${shellConfirm} toast=${shellToast} />`}
      ${peer && html`<button data-act="peer-mail-reply" title="Reply to this peer's operator"
        onClick=${() => openPeerMail({ peer, subject: replySubject(message.subject) })}>reply</button>`}</${Fragment}>`;
    actions = html`<${HumanActions} message=${message} senderLive=${controller.senderOnline(message.from_agent, message.from_conv)}
      harness=${remote ? '' : senderHarness(message)} toast=${shellToast} onDone=${controller.reloadMail} before=${lead}
      peer=${remote ? peerHumanActions(remote, dashboardState.snapshot.value?.peer_view) : null} />`;
  } else {
    const deleteLock = trustLock('', 'deleting its messages');
    actions = html`<div class="mail-reader-actions">
      <button onClick=${() => controller.setMessagesRead([message.id], !message.read)}
        data-id=${message.id} title=${message.read ? 'Mark this message unread for the recipient' : 'Mark this message read on the recipient’s behalf'}>${message.read ? 'mark unread' : 'mark read'}</button>
      <span class="spacer"></span>
      <button class="danger" data-id=${message.id} disabled=${!!deleteLock}
        onClick=${() => controller.deleteOneMessage(message.id)} title=${deleteLock ? undefined : 'Permanently delete this message'}>delete</button><${LockReason} why=${deleteLock} />
    </div>`;
  }
  return html`<div class="mail-reader" id="mail-reader" data-kind=${controller.msgKind(message)}
    role="region" aria-label="Message reader" tabindex="0">
    <${MessageHeaders} message=${message} />
    <${MessageBody} message=${message} agentMail=${!human} />
    ${human ? html`<${MessageAttachments} message=${message} />` : html`<${AgentMailAttachments} message=${message} />`}
    ${human && html`<${DecisionChoices} message=${message} toast=${shellToast} onDone=${controller.reloadMail} remote=${!!remote} />`}
    ${human && html`<${MessageAction} message=${message} toast=${shellToast} onDone=${controller.reloadMail} onRefused=${controller.reloadMail} remote=${!!remote} />`}
    ${actions}
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
  const enterMailboxList = (event) => {
    const client = event.currentTarget.closest('.mail-client');
    enterFirstRow({
      event, container: client?.querySelector('#mail-sidebar'), ...SIDEBAR_ROWS,
      select: (row) => controller.selectMailbox(row.dataset.id),
    });
  };
  const enterMessageList = (event) => {
    const client = event.currentTarget.closest('.mail-client');
    enterFirstRow({
      event, container: client?.querySelector('#mail-list'), ...LIST_ROWS,
      select: (row) => controller.selectMessage(row.dataset.id),
    });
  };
  return html`<div class="mail-client" onKeyDown=${(event) => !hasShownOverlay() && movePaneFocus(event, mailPanes(controller))}>
    <${PeerMailHost} toast=${shellToast} />
    <${MailFilters} boxId="filter-mailboxes" boxPlaceholder=${wizard ? 'Seek a hero…' : 'Filter mailboxes (name / id)'}
      boxValue=${current.boxQuery} onBoxInput=${controller.setBoxQuery} onBoxKeyDown=${enterMailboxList}
      id="filter-messages" placeholder=${wizard ? 'Search the scrolls…' : 'Filter messages (sender / recipient / subject / body)'}
      value=${current.messageQuery} onInput=${controller.setMessageQuery} onKeyDown=${enterMessageList}
      countId="filter-messages-count" countText=${controller.messageCountText()} clearId="filter-messages-clear"
      onClear=${() => controller.setMessageQuery('')} />
    <div class="mail-col mail-sidebar-col">
      <${RemoteInboxSection} />
      <${WipeBar} current=${current} controller=${controller} />
      ${current.mailboxRequest?.phase === 'error' && html`<div class="island-error mail-error" role="alert">
        Mailboxes failed to refresh: ${current.mailboxRequest.error}
        <button type="button" onClick=${controller.reloadMail}>Retry</button>
      </div>`}
      <${MailSidebar} current=${current} controller=${controller} />
      <${MailFooterToggles} idPrefix="mail" busy=${current.busy}
        showRetired=${current.showRetired} showEmpty=${current.showEmpty} showPrevGens=${current.showPrevGens}
        onRetired=${controller.setShowRetired} onEmpty=${controller.setShowEmpty} onPrevGens=${controller.setShowPrevGens} />
    </div>
    <div class="mail-col mail-list-col">
      <${MessageBulkBar} current=${current} controller=${controller} model=${messages} />
      <${MessageList} current=${current} controller=${controller} model=${messages} />
      <${MessagePager} current=${current} controller=${controller} model=${messages} />
    </div>
    <${MessageReader} current=${current} controller=${controller} model=${messages} />
    <${MailGutters} />
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
