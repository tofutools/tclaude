// phone-mail-island.js — the Messages tab on a phone (data-layout="phone"):
// Mail, a one-column mail client in the shape of a phone mail app.
//
//   list   a row of mailbox chips (Human, All agent mail, Access requests,
//          each group, and Agents… for one agent's folder), a search box,
//          and one row per message: sender, group, time, subject, a preview
//          line and attachment chips. A pending decision or access request
//          is answered right on its row. ✎ Write opens the message dialog.
//   reader a full-screen page pushed over the list (js/phone-page.js puts its
//          back button and actions in the shell's top bar and hides the
//          bottom nav), showing the desktop reader's content, with a reply
//          bar at the bottom.
//
// It drives the same controller and state as the desktop panes (mail.js via
// mailController), so folders, paging, search, read state, history entries
// and every action behave the same; only the layout is the phone's. The
// reader body is the desktop MessageReader itself.

import { Fragment, h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { registerMailController } from './mail-bridge.js';
import { MessageReader } from './mail-island.js';
import { PeerMailHost } from './peer-mail.js';
import { DecisionChoices, MailFooterToggles, LoadMore } from './mail-parts.js';
import { allRecipientLabel, counterparty, senderLabel } from './mail-labels.js';
import { messageAttachments } from './human-attachments.js';
import { relTime } from './helpers.js';
import { dashboardState } from './snapshot-store.js';
import { remoteNodeID } from './fleet-model.js';
import { openHumanReplyModal, openMessageCreateModal } from './message-access-dialog-controller.js';
import { shellToast } from './shell-state.js';
import { usePhonePage } from './phone-page.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

const HUMAN = 'human';
const ACCESS = 'access-requests';

// avatarHue picks one of the eight avatar colours for a name, stably.
export function avatarHue(name) {
  let hash = 0;
  for (const ch of String(name || '')) hash = (hash * 31 + ch.codePointAt(0)) >>> 0;
  return hash % 8;
}

function Avatar({ name }) {
  const initial = String(name || '?').trim().charAt(0).toUpperCase() || '?';
  return html`<span class="phone-mail-avatar" data-hue=${avatarHue(name)} aria-hidden="true">${initial}</span>`;
}

// rowParty is who a list row is from (or, in a mixed folder, from → to).
export function rowParty(message, aggregate) {
  if (aggregate) return `${senderLabel(message) || 'system'} → ${allRecipientLabel(message)}`;
  return counterparty(message) || 'system';
}

// rowLines are a row's subject and preview line: the subject, then the
// body's first line; a message without a subject leads with its body.
export function rowLines(message) {
  const lines = String(message.body || '').split('\n').map((line) => line.trim()).filter(Boolean);
  if (message.subject) return { subject: message.subject, preview: lines.find((line) => line !== message.subject) || '' };
  return { subject: lines[0] || '(no subject)', preview: lines[1] || '' };
}

// pendingDecision reports whether a row can answer its decision inline: a
// notification with options that nobody has answered or dismissed yet.
export function pendingDecision(message, folder) {
  return folder === HUMAN && Array.isArray(message.options) && message.options.length >= 2
    && !message.decision && !message.dismissed_at;
}

// accessInline reports whether an access request is a plain approve/decline
// that its row can answer; peer, link and proposal requests open the reader.
export function accessInline(request, controller) {
  return controller.accessIsPending(request) && !request.origin_peer
    && request.purpose !== 'group_link' && request.purpose !== 'peer_proposal';
}

function AttachmentChips({ message }) {
  const files = messageAttachments(message);
  if (!files.length) return null;
  return html`<span class="phone-mail-files">${files.slice(0, 3).map((file, index) => html`<span key=${file.id ?? index}
    class="phone-mail-file">${String(file.content_type || '').startsWith('image/') ? '🖼' : '📄'} ${file.filename || 'file'}</span>`)}
    ${files.length > 3 && html`<span class="phone-mail-file">+${files.length - 3}</span>`}</span>`;
}

function MessageRow({ message, folder, aggregate, open, controller }) {
  const party = rowParty(message, aggregate);
  const { subject, preview } = rowLines(message);
  const unread = !message.read;
  return html`<li class=${`phone-mail-row${unread ? ' unread' : ''}`} data-id=${message.id}>
    <button data-ui-exempt type="button" class="phone-mail-open" onClick=${() => open(message.id)}>
      <${Avatar} name=${aggregate ? senderLabel(message) : party} />
      <span class="phone-mail-text">
        <span class="phone-mail-top">
          <span class="phone-mail-party">${party}</span>
          ${message.group && html`<span class="phone-mail-group">${message.group}</span>`}
          <span class="phone-mail-time">${relTime(message.created_at)}</span>
        </span>
        <span class="phone-mail-subject">${subject}</span>
        ${preview && html`<span class="phone-mail-preview">${preview}</span>`}
        <${AttachmentChips} message=${message} />
      </span>
    </button>
    ${pendingDecision(message, folder) && html`<div class="phone-mail-inline">
      <${DecisionChoices} message=${message} toast=${shellToast} onDone=${controller.reloadMail} remote=${!!remoteNodeID()} />
    </div>`}
  </li>`;
}

function AccessRow({ request, open, controller }) {
  const pending = controller.accessIsPending(request);
  const who = controller.accessWho(request);
  return html`<li class=${`phone-mail-row${pending ? ' unread' : ''}`} data-id=${request.id}>
    <button data-ui-exempt type="button" class="phone-mail-open" onClick=${() => open(request.id)}>
      <${Avatar} name=${who} />
      <span class="phone-mail-text">
        <span class="phone-mail-top">
          <span class="phone-mail-party">${who}</span>
          <span class="phone-mail-group">${controller.accessStatusText(request)}</span>
          <span class="phone-mail-time">${relTime(pending ? request.created_at : (request.decided_at || request.created_at))}</span>
        </span>
        <span class="phone-mail-subject">${controller.accessSubject(request)}</span>
      </span>
    </button>
    ${accessInline(request, controller) && html`<div class="phone-mail-inline phone-mail-access">
      <${Button} size="sm" variant="danger" onClick=${() => controller.decideAccess(request.id, 'deny')}>Decline<//>
      <${Button} size="sm" variant="success" onClick=${() => controller.decideAccess(request.id, 'approve')}>Approve<//>
    </div>`}
  </li>`;
}

// chipsFor are the mailbox chips: Human, All agent mail and Access requests
// (whichever the roster offers), then each group.
const PINNED = { human: 'Human', all: 'All agent mail', [ACCESS]: 'Access' };
export function chipsFor(view, label) {
  const order = Object.keys(PINNED);
  const pinned = view.pinned.filter((mailbox) => PINNED[mailbox.kind])
    .sort((a, b) => order.indexOf(a.kind) - order.indexOf(b.kind));
  return [
    ...pinned.map((mailbox) => ({ id: mailbox.id, label: PINNED[mailbox.kind], count: mailbox.unread || 0 })),
    ...view.groups.map((group) => ({ id: group.mailbox.id, label: label(group.mailbox), count: group.mailbox.unread || 0 })),
  ];
}

// MailboxChips are folder filters over one list, not tabs over panels, so
// they are pressed toggle buttons in a group.
function MailboxChips({ current, controller, onAgents, onPick }) {
  const view = controller.mailboxView({ unfiltered: true });
  const chips = chipsFor(view, controller.mailboxLabel);
  const agentFolder = !chips.some((chip) => chip.id === current.selected) && current.selected;
  return html`<div class="phone-mail-chips" role="group" aria-label="Mailboxes">
    ${chips.map((chip) => html`<button data-ui-exempt key=${chip.id} type="button"
      class=${`phone-mail-chip${chip.id === current.selected ? ' on' : ''}`} aria-pressed=${chip.id === current.selected}
      onClick=${() => { onPick(); controller.selectMailbox(chip.id, { navigate: true }); }}>${chip.label}${chip.count > 0 && html` <span class="phone-mail-chip-count">${chip.count}</span>`}</button>`)}
    <button data-ui-exempt type="button" class=${`phone-mail-chip${agentFolder ? ' on' : ''}`}
      aria-pressed=${!!agentFolder} onClick=${onAgents}>${agentFolder ? `${controller.mailboxLabel(current.selectedMailbox || { kind: 'agent', id: current.selected })} ▾` : 'Agents ▾'}</button>
  </div>`;
}

// AgentFolders lists the agent folders to pick one from, with the desktop
// sidebar's filter and its retired / empty / previous-generation toggles.
function AgentFolders({ current, controller, onPicked }) {
  const view = controller.mailboxView();
  return html`<div class="phone-mail-agents">
    <input class="ui-input phone-mail-search" type="search" placeholder="Filter agents" aria-label="Filter agent mailboxes"
      value=${current.boxQuery} onInput=${(event) => controller.setBoxQuery(event.currentTarget.value)} />
    <ul class="phone-list">
      ${view.agents.map((mailbox) => html`<li key=${mailbox.id}>
        <button data-ui-exempt type="button" class="phone-menu-item" onClick=${() => { controller.selectMailbox(mailbox.id, { navigate: true }); onPicked(); }}>
          <${Avatar} name=${controller.mailboxLabel(mailbox)} />
          <span class="phone-mail-folder">${controller.mailboxLabel(mailbox)}</span>
          ${mailbox.unread > 0 && html`<span class="ui-badge ui-badge-accent">${mailbox.unread}</span>`}
        </button>
      </li>`)}
    </ul>
    ${view.agents.length === 0 && html`<p class="ui-empty phone-empty">No agent mailboxes${current.boxQuery ? ' match' : ''}.</p>`}
    ${view.moreAgents > 0 && html`<${LoadMore} shown=${view.agents.length} total=${view.agentsTotal}
      busy=${current.busy} what="agent mailboxes" onMore=${controller.loadMoreAgents} />`}
    <${MailFooterToggles} idPrefix="phone-mail" busy=${current.busy}
      showRetired=${current.showRetired} showEmpty=${current.showEmpty} showPrevGens=${current.showPrevGens}
      onRetired=${controller.setShowRetired} onEmpty=${controller.setShowEmpty} onPrevGens=${controller.setShowPrevGens} />
  </div>`;
}

function MailList({ current, controller, model, open, listScroll }) {
  const [agents, setAgents] = useState(false);
  // Back from the reader returns to where the list was: Mail scrolls with
  // the page, and the reader started it at the top.
  useLayoutEffect(() => {
    if (listScroll.current) globalThis.scrollTo?.(0, listScroll.current);
    listScroll.current = 0;
  }, [listScroll]);
  // The roster lists agent folders only while the picker is open.
  useEffect(() => {
    controller.listAgents(agents);
    return () => controller.listAgents(false);
  }, [agents, controller]);
  const folder = current.selected;
  const aggregate = !!model.isAggregate;
  let rows;
  if (current.messageRequest?.phase === 'error') {
    rows = html`<div class="ui-error-box" role="alert">Messages failed to load: ${current.messageRequest.error}
      <${Button} size="sm" onClick=${controller.reloadMessagesPage}>Retry<//></div>`;
  } else if (model.access) {
    const list = [...model.pendingAccess, ...model.handledAccess];
    rows = list.length === 0
      ? html`<p class="ui-empty phone-empty">${model.allAccess.length ? 'No access requests match.' : 'No access requests.'}</p>`
      : html`<ul class="phone-list phone-mail-list">${list.map((request) => html`<${AccessRow} key=${request.id}
          request=${request} open=${open} controller=${controller} />`)}</ul>`;
  } else {
    rows = model.messages.length === 0
      ? html`<p class="ui-empty phone-empty">${current.totalUnfiltered ? 'No messages match.' : 'This mailbox is empty.'}</p>`
      : html`<ul class="phone-list phone-mail-list">${model.messages.map((message) => html`<${MessageRow} key=${message.id}
          message=${message} folder=${folder} aggregate=${aggregate} open=${open} controller=${controller} />`)}</ul>`;
  }
  const human = folder === HUMAN;
  return html`<div class="phone-mail">
    <${MailboxChips} current=${current} controller=${controller} onAgents=${() => setAgents((value) => !value)} onPick=${() => setAgents(false)} />
    ${agents ? html`<${AgentFolders} current=${current} controller=${controller} onPicked=${() => setAgents(false)} />` : html`<${Fragment}>
      <div class="phone-mail-tools">
        <input class="ui-input phone-mail-search" type="search" placeholder="Search mail" aria-label="Search messages"
          value=${current.messageQuery} onInput=${(event) => controller.setMessageQuery(event.currentTarget.value)} />
        ${human && !remoteNodeID() && html`<${Button} size="sm" data-act="msg-mark-all-read" title="Mark every human notification read">✓ all read<//>`}
      </div>
      <p class="phone-mail-count">${controller.messageCountText()}</p>
      ${rows}
      ${!model.access && model.pages > 1 && html`<div class="phone-mail-pager">
        <${Button} size="sm" disabled=${current.page <= 1 || current.busy} onClick=${() => controller.goToPage(current.page - 1)}>‹ Newer<//>
        <span>${current.page} / ${model.pages}</span>
        <${Button} size="sm" disabled=${current.page >= model.pages || current.busy} onClick=${() => controller.goToPage(current.page + 1)}>Older ›<//>
      </div>`}
    </${Fragment}>`}
    <button data-ui-exempt type="button" class="phone-mail-write" onClick=${() => openMessageCreateModal({})}>✎ Write</button>
  </div>`;
}

// openedMessage is the message or access request the reader shows, or null.
// `kept` is the one it showed last: a refresh that pushes the open message
// onto the next page (new mail arriving above it) keeps it open, until the
// selection itself changes.
export function openedMessage(current, model, kept = null) {
  const id = current.selectedMsgId;
  if (id === null || id === undefined || id === '') return null;
  const found = model.access
    ? model.allAccess.find((request) => request.id === String(id))
    : model.messages.find((message) => message.id === id);
  if (found) return found;
  return kept && String(kept.id) === String(id) && !!kept.access === !!model.access ? kept.item : null;
}

// withItem is the page model with the open item in it: the desktop reader
// looks it up there, and a kept item may have left the page.
export function withItem(model, item) {
  if (model.access) {
    return model.allAccess.includes(item) ? model : { ...model, allAccess: [...model.allAccess, item] };
  }
  return model.messages.includes(item) ? model : { ...model, messages: [...model.messages, item] };
}

function Reader({ current, controller, model, item, active, back }) {
  const human = current.selected === HUMAN && !model.access;
  const sender = human ? (item.from_title || item.from_conv || item.from_agent || 'the agent') : '';
  const live = human && controller.senderOnline(item.from_agent, item.from_conv);
  const terminal = human && item.from_agent && !remoteNodeID();
  usePhonePage({
    title: '',
    onBack: back,
    hideNav: true,
    actions: terminal ? [{
      key: 'terminal', icon: 'Terminal ›', label: live ? `Open ${sender}'s terminal` : `${sender} is offline`, disabled: !live,
      onClick: () => import('./terminals-tab.js').then(({ openWebWindowPane }) => openWebWindowPane(item.from_agent, sender, {})),
    }] : [],
  }, active);
  // A message opens at its top, wherever the list was scrolled to.
  useLayoutEffect(() => { globalThis.scrollTo?.(0, 0); }, [item.id]);
  const canReply = human && (item.from_conv || item.from_agent) && !remoteNodeID();
  return html`<div class=${`phone-mail-reader${canReply ? ' with-reply' : ''}`}>
    <${MessageReader} current=${current} controller=${controller} model=${withItem(model, item)} />
    ${canReply && html`<div class="phone-mail-replybar">
      <button data-ui-exempt type="button" class="phone-mail-reply" onClick=${() => openHumanReplyModal({
        id: item.id, agent: item.from_agent || '', conv: item.from_conv || '', label: sender, subject: item.subject || '',
      })}>Reply to ${sender}…</button>
    </div>`}
  </div>`;
}

export function PhoneMailApp({ controller }) {
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
  // The reader opened from this list pushed a history entry over it, so its
  // back button is the browser's Back (and the OS back gesture does the
  // same). A reader reached any other way (a deep link, an attention jump)
  // has no list entry under it: back then closes it in place.
  const pushed = useRef(null);
  const listScroll = useRef(0);
  const current = controller.state.view.value;
  const model = controller.messageView();
  const kept = useRef(null);
  const item = openedMessage(current, model, kept.current);
  kept.current = item ? { id: current.selectedMsgId, access: !!model.access, item } : null;
  // Once the reader is gone, however it went (the OS back gesture, a
  // delete), its entry is no longer the one under a later reader.
  if (!item) pushed.current = null;
  const open = (id) => {
    pushed.current = String(id);
    listScroll.current = globalThis.scrollY || 0;
    controller.selectMessage(id, { navigate: true });
  };
  const back = () => {
    if (pushed.current === String(current.selectedMsgId) && globalThis.history?.length > 1) {
      pushed.current = null;
      globalThis.history.back();
      return;
    }
    pushed.current = null;
    controller.closeMessage();
  };
  // The reader's reply to a peer operator's message opens its dialog through
  // PeerMailHost, as on the desktop.
  return html`<${Fragment}>
    ${item
      ? html`<${Reader} current=${current} controller=${controller} model=${model} item=${item}
          active=${activeTab === 'messages'} back=${back} />`
      : html`<${MailList} current=${current} controller=${controller} model=${model} open=${open} listScroll=${listScroll} />`}
    <${PeerMailHost} toast=${shellToast} />
  </${Fragment}>`;
}

export function mountPhoneMailIsland({ host, controller, registerCleanup }) {
  render(html`<${PhoneMailApp} controller=${controller} />`, host);
  const unregister = registerMailController(controller);
  const disposeLegacy = controller.initMail();
  registerCleanup(() => render(null, host));
  registerCleanup(unregister);
  registerCleanup(() => disposeLegacy?.());
}
