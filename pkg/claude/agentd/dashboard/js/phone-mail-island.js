// phone-mail-island.js — the Messages tab on a phone (data-layout="phone"):
// Mail, a one-column mail client in the shape of a phone mail app.
//
//   list   the top bar names the mailbox (Mail Human ▾ opens every mailbox,
//          agent folders too), with ✓✓ (all read) and 🔍 (search this
//          mailbox); a row of mailbox chips; one row per message: the other
//          party's avatar, sender, group, time, subject, a preview line and
//          attachment chips with their size. A pending decision is answered
//          on its row, a plain access request on its approval card. A swipe
//          right marks a row read or unread, a swipe left deletes it, and a
//          long press starts selecting. ✎ Write opens the message sheet.
//   reader a full-screen page pushed over the list (js/phone-page.js puts its
//          back button and actions in the shell's top bar and hides the
//          bottom nav): ⌖ (the sender's terminal), ✉̸ (mark unread) and 🗑 in
//          the bar; the subject, a sender row (Terminal ›, and Find ›: the
//          sender's row in Agents, agent-route.js; a terminal that cannot
//          open says why on a tap), the body, images inline (tap
//          to zoom, a history step of its own), and a reply bar that opens
//          the message sheet. Another node's mail, a peer's and a spawn
//          request keep the desktop reader, whose peer rules they need.
//
// It drives the same controller and state as the desktop panes (mail.js via
// mailController), so folders, paging, search, read state, history entries
// and every action behave the same; only the layout is the phone's.

import { Fragment, h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { mailLocation, registerMailController } from './mail-bridge.js';
import { toParent } from './nav-back.js';
import { MessageReader } from './mail-island.js';
import { PeerMailHost, awayTicket, peerOfMessage } from './peer-mail.js';
import { DecisionChoices, MailFooterToggles, LoadMore, MessageBody } from './mail-parts.js';
import { MessageAction } from './message-action.js';
import { allRecipientLabel, counterparty, senderLabel } from './mail-labels.js';
import { attachmentHref, attachmentSize, messageAttachments } from './human-attachments.js';
import { messageFileSource } from './attachment-source.js';
import { AttachmentViewer, viewerKind } from './attachment-viewer.js';
import { announceOverlay, registerOverlayRoute } from './overlay-routes.js';
import { relTime } from './helpers.js';
import { dashboardState } from './snapshot-store.js';
import { remoteNodeID } from './fleet-model.js';
import { companionAgents } from './companion-model.js';
import { createMessageAccessDialogActions } from './message-access-dialog-actions.js';
import { openPhoneCompose } from './phone-compose-sheet.js';
import { openPhoneSheet } from './phone-sheet.js';
import { DecisionAnswer, PhoneApprovalCard } from './phone-approval.js';
import { planDetails } from './phone-now-model.js';
import { shellToast } from './shell-state.js';
import { trustLock } from './lock-reason.js';
import { usePhoneBar, usePhonePage } from './phone-page.js';
import { NavToggle, navShown } from './phone-nav-toggle.js';
import { Button } from './ui-components.js';
import { Avatar, avatarHue, ChipRow, Empty, ErrorNote, errorText, SheetHead } from './phone-ui.js';
import { findAgentRoute } from './agent-route.js';

const html = htm.bind(h);

const HUMAN = 'human';
const ACCESS = 'access-requests';

// avatarHue (phone-ui.js) picks a name's avatar colour; re-exported for its callers here.
export { avatarHue };

// rowParty is who a list row is from (or, in a mixed folder, from → to).
export function rowParty(message, aggregate) {
  if (aggregate) return `${senderLabel(message) || 'system'} → ${allRecipientLabel(message)}`;
  return counterparty(message) || 'system';
}

// rowAvatar is whose avatar a row shows: the agent at the other end. Mail the
// operator wrote shows its recipient, not the operator.
export function rowAvatar(message, aggregate) {
  if (message.operator_authored) return allRecipientLabel(message);
  return aggregate ? senderLabel(message) : counterparty(message);
}

// rowLines are a row's subject and preview line: the subject, then the
// body's first line; a message without a subject leads with its body.
export function rowLines(message) {
  const lines = String(message.body || '').split('\n').map((line) => line.trim()).filter(Boolean);
  if (message.subject) return { subject: message.subject, preview: lines.find((line) => line !== message.subject) || '' };
  return { subject: lines[0] || '(no subject)', preview: lines[1] || '' };
}

// pendingDecision reports whether a message is a decision still waiting on
// the operator: a notification with options that nobody has answered or
// dismissed yet. Its row says so (Decide · N options ›) but never answers it:
// a tap opens the message, where the options are (an answer sent from the
// list was too easy to send by a tap meant to open it).
export function pendingDecision(message, folder) {
  return folder === HUMAN && Array.isArray(message.options) && message.options.length >= 2
    && !message.decision && !message.dismissed_at;
}

// accessInline reports whether an access request is a plain approve/deny
// that its card can answer; peer, link and proposal requests open the reader.
export function accessInline(request, controller) {
  return controller.accessIsPending(request) && !request.origin_peer
    && request.purpose !== 'group_link' && request.purpose !== 'peer_proposal';
}

// desktopReader says whether a message keeps the desktop reader on a phone:
// another node's mail, a peer operator's, an away answer and a peer's spawn
// request carry that node's or peer's rules, which the desktop reader holds.
export function desktopReader(message, folder) {
  if (remoteNodeID()) return true;
  if (folder !== HUMAN) return false;
  return !!peerOfMessage(message) || !!awayTicket(message) || String(message.group || '').startsWith('federation:');
}

const fileIcon = (file) => (String(file.content_type || '').startsWith('image/') || file.previewable ? '🖼' : '📄');
const fileLabel = (file) => [file.filename || 'file', attachmentSize(file.size_bytes)].filter(Boolean).join(' · ');

function AttachmentChips({ message }) {
  const files = messageAttachments(message);
  if (!files.length) return null;
  return html`<span class="phone-mail-files">${files.slice(0, 3).map((file, index) => html`<span key=${file.id ?? index}
    class="phone-mail-file">${fileIcon(file)} ${fileLabel(file)}</span>`)}
    ${files.length > 3 && html`<span class="phone-mail-file">+${files.length - 3}</span>`}</span>`;
}

// SWIPE_PX is how far a row travels before letting go acts; LONG_PRESS_MS
// how long a press holds to start selecting.
export const SWIPE_PX = 80;
export const LONG_PRESS_MS = 500;

// useRowGestures is a row's swipe and long press. onLeft / onRight act on a
// swipe past SWIPE_PX (null: that way does nothing), onHold on a long press.
// A swipe or hold swallows the click it would end in. Vertical drags stay
// the page's scroll (touch-action: pan-y on the row).
function useRowGestures({ onLeft, onRight, onHold }) {
  const [dx, setDx] = useState(0);
  // The offset as of the last move: a lift can come before a re-render.
  const travel = useRef(0);
  const slide = (x) => { travel.current = x; setDx(x); };
  const start = useRef(null);
  const swallow = useRef(false);
  const timer = useRef(null);
  const stopHold = () => { if (timer.current) { clearTimeout(timer.current); timer.current = null; } };
  useEffect(() => stopHold, []);
  const handlers = {
    onPointerDown: (event) => {
      if (event.button > 0) return;
      start.current = { x: event.clientX, y: event.clientY, swiping: false };
      swallow.current = false;
      stopHold();
      if (onHold) timer.current = setTimeout(() => { timer.current = null; swallow.current = true; start.current = null; onHold(); }, LONG_PRESS_MS);
    },
    onPointerMove: (event) => {
      const at = start.current;
      if (!at) return;
      const x = event.clientX - at.x;
      const y = event.clientY - at.y;
      if (Math.abs(x) > 8 || Math.abs(y) > 8) stopHold();
      if (!at.swiping) {
        if (Math.abs(y) > Math.abs(x) || Math.abs(x) < 12) return;
        at.swiping = true;
        event.currentTarget.setPointerCapture?.(event.pointerId);
      }
      const allowed = (x < 0 && onLeft) || (x > 0 && onRight) ? x : 0;
      slide(Math.max(-SWIPE_PX * 1.5, Math.min(SWIPE_PX * 1.5, allowed)));
    },
    onPointerUp: () => {
      stopHold();
      const at = start.current;
      start.current = null;
      if (!at?.swiping) return;
      swallow.current = true;
      const went = travel.current;
      slide(0);
      if (went <= -SWIPE_PX) onLeft?.();
      else if (went >= SWIPE_PX) onRight?.();
    },
    onPointerCancel: () => { stopHold(); start.current = null; slide(0); },
    onClickCapture: (event) => {
      if (!swallow.current) return;
      swallow.current = false;
      event.preventDefault();
      event.stopPropagation();
    },
    onContextMenu: (event) => { if (onHold) event.preventDefault(); },
  };
  return { dx, handlers };
}

// Swipeable is a list row with its swipe actions under it: right shows
// `right` (read / unread), left shows `left` (delete).
function Swipeable({ class: klass, id, left, right, onLeft, onRight, onHold, children }) {
  const { dx, handlers } = useRowGestures({ onLeft, onRight, onHold });
  return html`<li class=${klass} data-id=${id} ...${handlers}>
    ${dx !== 0 && html`<div class=${`phone-mail-swipe ${dx > 0 ? 'right' : 'left'}${Math.abs(dx) >= SWIPE_PX ? ' armed' : ''}`} aria-hidden="true">
      <span>${dx > 0 ? right : left}</span></div>`}
    <div class="phone-mail-slide" style=${dx ? `transform: translateX(${dx}px)` : undefined}>${children}</div>
  </li>`;
}

// rowActions are a message row's swipes and the reader's bar actions, by
// folder: a notification goes through the desktop's row handler (its
// read route and confirm), agent mail through the controller.
function rowActions(controller, folder) {
  const remote = !!remoteNodeID();
  if (remote) return null;
  const human = folder === HUMAN;
  const handle = (data) => import('./row-action-handler.js').then(({ handleRowAction }) => handleRowAction({ data }));
  return {
    setRead: (message, read) => (human
      ? handle({ act: read ? 'msg-mark-read' : 'msg-mark-unread', id: String(message.id) })
      : controller.setMessagesRead([message.id], read)),
    remove: (message) => controller.deleteOneMessage(message.id),
    deleteLock: human ? '' : trustLock('', 'deleting its messages'),
  };
}

function MessageRow({ message, folder, aggregate, open, controller, selecting, selected }) {
  const party = rowParty(message, aggregate);
  const { subject, preview } = rowLines(message);
  const unread = !message.read;
  const acts = rowActions(controller, folder);
  const select = () => controller.toggleMessageSelection(message.id, !selected);
  const tap = () => (selecting ? select() : open(message.id));
  return html`<${Swipeable} class=${`phone-mail-row${unread ? ' unread' : ''}${selected ? ' selected' : ''}`} id=${message.id}
    left="🗑 Delete" right=${unread ? '✓ Read' : '✉ Unread'}
    onLeft=${acts && !acts.deleteLock && !selecting ? () => acts.remove(message) : null}
    onRight=${acts && !selecting ? () => acts.setRead(message, unread) : null}
    onHold=${acts && !selecting ? select : null}>
    <button data-ui-exempt type="button" class="phone-mail-open" onClick=${tap}
      aria-pressed=${selecting ? (selected ? 'true' : 'false') : undefined}>
      ${selecting
        ? html`<span class=${`phone-mail-check${selected ? ' on' : ''}`} aria-hidden="true">${selected ? '✓' : ''}</span>`
        : html`<${Avatar} name=${rowAvatar(message, aggregate)} />`}
      <span class="phone-mail-text">
        <span class="phone-mail-top">
          <span class="phone-mail-party">${party}</span>
          ${message.group && html`<span class="phone-mail-group">· ${message.group}</span>`}
          <span class="phone-mail-time">${relTime(message.created_at)}</span>
        </span>
        <span class="phone-mail-subject">${subject}</span>
        ${preview && html`<span class="phone-mail-preview">${preview}</span>`}
        <${AttachmentChips} message=${message} />
        ${!selecting && pendingDecision(message, folder) && html`<span class="phone-mail-decide">Decide · ${message.options.length} options ›</span>`}
      </span>
    </button>
  <//>`;
}

// decideThrough is a PhoneApprovalCard's decide over Mail's controller, which
// toasts its own failure and resolves false: the card is told, so it frees
// its buttons without a second toast.
export function decideThrough(controller) {
  return async (id, decision) => {
    if (await controller.decideAccess(id, decision)) return;
    throw Object.assign(new Error('not recorded'), { told: true });
  };
}

function AccessRow({ request, open, controller }) {
  const who = controller.accessWho(request);
  if (accessInline(request, controller)) {
    return html`<li class="phone-mail-row phone-mail-card" data-id=${request.id}>
      <${PhoneApprovalCard} request=${request} from=${who} notify=${shellToast}
        decide=${decideThrough(controller)}
        onMore=${() => open(request.id)} moreLabel=${`Open ${who}'s request`} />
    </li>`;
  }
  const pending = controller.accessIsPending(request);
  return html`<li class=${`phone-mail-row${pending ? ' unread' : ''}`} data-id=${request.id}><div class="phone-mail-slide">
    <button data-ui-exempt type="button" class="phone-mail-open" onClick=${() => open(request.id)}>
      <${Avatar} bot glyph="🤖" badge="🔑" />
      <span class="phone-mail-text">
        <span class="phone-mail-top">
          <span class="phone-mail-party">${who}</span>
          <span class="phone-mail-group">· ${controller.accessStatusText(request)}</span>
          <span class="phone-mail-time">${relTime(pending ? request.created_at : (request.decided_at || request.created_at))}</span>
        </span>
        <span class="phone-mail-subject">${controller.accessSubject(request)}</span>
      </span>
    </button>
  </div></li>`;
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

// mailboxName is the open mailbox's name for the top bar.
export function mailboxName(current, controller) {
  const chip = chipsFor(controller.mailboxView({ unfiltered: true }), controller.mailboxLabel).find((c) => c.id === current.selected);
  if (chip) return chip.label;
  return controller.mailboxLabel(current.selectedMailbox || { kind: 'agent', id: current.selected });
}

// MailboxChips are folder filters over one list, not tabs over panels, so
// they are pressed toggle buttons in a group. An agent folder, picked from
// the title's ▾, shows as a chip of its own while open.
function MailboxChips({ current, controller }) {
  const chips = chipsFor(controller.mailboxView({ unfiltered: true }), controller.mailboxLabel);
  const agentFolder = !chips.some((chip) => chip.id === current.selected) && current.selected;
  const all = agentFolder ? [{ id: current.selected, label: mailboxName(current, controller), count: 0 }, ...chips] : chips;
  return html`<div class="phone-mail-chips"><${ChipRow} toggle label="Mailboxes" onSelect=${(id) => controller.selectMailbox(id, { navigate: true })}
    items=${all.map((chip) => ({ key: chip.id, label: chip.label, count: chip.count > 0 ? chip.count : null, pressed: chip.id === current.selected }))} /></div>`;
}

// MailboxSheet is the title's ▾: every mailbox, the agent folders with the
// desktop sidebar's filter and its retired / empty / previous-generation
// toggles. It reads the controller live, so it follows the roster.
export function MailboxSheet({ controller, close }) {
  const current = controller.state.view.value;
  const view = controller.mailboxView();
  // The roster lists agent folders only while the picker is open.
  useEffect(() => {
    controller.listAgents(true);
    return () => controller.listAgents(false);
  }, [controller]);
  const pick = (id) => { close(); controller.selectMailbox(id, { navigate: true }); };
  const chips = chipsFor(controller.mailboxView({ unfiltered: true }), controller.mailboxLabel);
  const item = (id, label, unread, lead) => html`<li key=${id}>
    <button data-ui-exempt type="button" class=${`phone-menu-item${id === current.selected ? ' on' : ''}`}
      aria-current=${id === current.selected ? 'true' : undefined} onClick=${() => pick(id)}>
      ${lead}<span class="phone-mail-folder">${label}</span>
      ${unread > 0 && html`<span class="ui-badge ui-badge-accent">${unread}</span>`}
    </button></li>`;
  return html`<div class="phone-mail-boxes">
    <h2 class="phone-sheet-title">Mailboxes</h2>
    <ul class="phone-menu">${chips.map((chip) => item(chip.id, chip.label, chip.count,
      html`<span class="phone-menu-icon" aria-hidden="true">${chip.id === HUMAN ? '🔔' : chip.id === ACCESS ? '🔑' : chip.id.startsWith('group:') ? '#' : '✉'}</span>`))}</ul>
    <${SheetHead}>Agents<//>
    <input name="filter-agent-mailboxes" class="ui-input phone-mail-search" type="search" placeholder="Filter agents" aria-label="Filter agent mailboxes"
      value=${current.boxQuery} onInput=${(event) => controller.setBoxQuery(event.currentTarget.value)} />
    <ul class="phone-menu">${view.agents.map((mailbox) => item(mailbox.id, controller.mailboxLabel(mailbox), mailbox.unread,
      html`<${Avatar} name=${controller.mailboxLabel(mailbox)} />`))}</ul>
    ${view.agents.length === 0 && html`<${Empty}>No agent mailboxes${current.boxQuery ? ' match' : ''}.<//>`}
    ${view.moreAgents > 0 && html`<${LoadMore} shown=${view.agents.length} total=${view.agentsTotal}
      busy=${current.busy} what="agent mailboxes" onMore=${controller.loadMoreAgents} />`}
    <${MailFooterToggles} idPrefix="phone-mail" busy=${current.busy}
      showRetired=${current.showRetired} showEmpty=${current.showEmpty} showPrevGens=${current.showPrevGens}
      onRetired=${controller.setShowRetired} onEmpty=${controller.setShowEmpty} onPrevGens=${controller.setShowPrevGens} />
  </div>`;
}

// The agents Write can address: every agent of this node, by name, with its
// group and whether it is live.
function writeRecipients() {
  return [...companionAgents(dashboardState.snapshot.value).values()]
    .filter((agent) => agent.agentId)
    .map((agent) => ({ agent: agent.agentId, label: agent.name, group: agent.group || '', online: !!agent.online }));
}

// SelectBar is the list while selecting: how many, and what to do to them.
function SelectBar({ current, controller, folder }) {
  const ids = [...current.selectedMsgs];
  const lock = folder === HUMAN ? '' : trustLock('', 'deleting its messages');
  return html`<div class="phone-mail-selectbar" role="toolbar" aria-label="Selected messages">
    <button data-ui-exempt type="button" class="phone-ib" aria-label="Stop selecting" onClick=${() => controller.clearMessageSelection()}>✕</button>
    <span class="phone-mail-selected">${ids.length} selected</span>
    ${folder !== HUMAN && html`<${Button} size="sm" class="phone-btn" disabled=${!ids.length} onClick=${() => controller.setMessagesRead(ids, true)}>✓ Read<//>`}
    <${Button} size="sm" class="phone-btn phone-btn-no" disabled=${!ids.length || !!lock} title=${lock || undefined}
      onClick=${() => controller.deleteSelectedMessages()}>🗑 Delete<//>
  </div>`;
}

function MailList({ current, controller, model, open, listScroll, active }) {
  const [searching, setSearching] = useState(!!current.messageQuery);
  const search = useRef(null);
  // Back from the reader returns to where the list was: Mail scrolls with
  // the page, and the reader started it at the top.
  useLayoutEffect(() => {
    if (listScroll.current) globalThis.scrollTo?.(0, listScroll.current);
    listScroll.current = 0;
  }, [listScroll]);
  // A search kept from before (the controller remembers it) stays in view.
  useEffect(() => { if (current.messageQuery) setSearching(true); }, [current.messageQuery]);
  useLayoutEffect(() => { if (searching && !current.messageQuery) search.current?.focus(); }, [searching]);
  const folder = current.selected;
  const human = folder === HUMAN;
  const aggregate = !!model.isAggregate;
  const selecting = !model.access && current.selectedMsgs.size > 0;
  usePhoneBar({
    tab: 'messages',
    menu: { sub: mailboxName(current, controller), label: 'Pick a mailbox', onClick: () => openPhoneSheet({ label: 'Mailboxes', Body: MailboxSheet, props: { controller } }) },
    actions: [
      ...(human && !remoteNodeID() ? [{ key: 'all-read', icon: '✓✓', label: 'Mark every notification read',
        onClick: () => import('./row-action-handler.js').then(({ handleRowAction }) => handleRowAction({ data: { act: 'msg-mark-all-read' } })) }] : []),
      { key: 'search', icon: '🔍', label: searching ? 'Close search' : 'Search this mailbox', onClick: () => {
        if (searching && current.messageQuery) controller.setMessageQuery('');
        setSearching(!searching);
      } },
    ],
  }, active);
  let rows;
  if (current.messageRequest?.phase === 'error') {
    rows = html`<${ErrorNote} onRetry=${controller.reloadMessagesPage}>${errorText('the messages', current.messageRequest.error)}<//>`;
  } else if (model.access) {
    const list = [...model.pendingAccess, ...model.handledAccess];
    rows = list.length === 0
      ? html`<${Empty}>${model.allAccess.length ? 'No access requests match.' : 'No access requests.'}<//>`
      : html`<ul class="phone-list phone-mail-list">${list.map((request) => html`<${AccessRow} key=${request.id}
          request=${request} open=${open} controller=${controller} />`)}</ul>`;
  } else {
    rows = model.messages.length === 0
      ? html`<${Empty}>${current.totalUnfiltered ? 'No messages match.' : 'This mailbox is empty.'}<//>`
      : html`<ul class="phone-list phone-mail-list">${model.messages.map((message) => html`<${MessageRow} key=${message.id}
          message=${message} folder=${folder} aggregate=${aggregate} open=${open} controller=${controller}
          selecting=${selecting} selected=${current.selectedMsgs.has(message.id)} />`)}</ul>`;
  }
  return html`<div class="phone-mail">
    ${selecting ? html`<${SelectBar} current=${current} controller=${controller} folder=${folder} />`
      : html`<${MailboxChips} current=${current} controller=${controller} />`}
    ${searching && html`<div class="phone-mail-tools">
      <input name="search-messages" ref=${search} class="ui-input phone-mail-search" type="search" placeholder=${`Search ${mailboxName(current, controller)}`}
        aria-label="Search messages" enterkeyhint="search"
        value=${current.messageQuery} onInput=${(event) => controller.setMessageQuery(event.currentTarget.value)} />
      <p class="phone-mail-count">${controller.messageCountText()}</p>
    </div>`}
    ${rows}
    ${!model.access && model.pages > 1 && html`<div class="phone-mail-pager">
      <${Button} size="sm" disabled=${current.page <= 1 || current.busy} onClick=${() => controller.goToPage(current.page - 1)}>‹ Newer<//>
      <span>${current.page} / ${model.pages}</span>
      <${Button} size="sm" disabled=${current.page >= model.pages || current.busy} onClick=${() => controller.goToPage(current.page + 1)}>Older ›<//>
    </div>`}
    ${!remoteNodeID() && html`<button data-ui-exempt type="button" class="phone-mail-write"
      onClick=${() => openPhoneCompose({}, { notify: shellToast, recipients: writeRecipients })}>✎ Write</button>`}
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

// sentLine is the sender row's second line: to whom, the group, how long ago.
export function sentLine(message, human) {
  const to = human || message.to_conv === 'human' ? 'to you' : `to ${allRecipientLabel(message)}`;
  return [to, message.group, message.created_at ? relTime(message.created_at) : ''].filter(Boolean).join(' · ');
}

// useFileRoute makes the open attachment viewer a location of its own (the
// `file` overlay param, its index): opening it is a history step, so the
// phone's Back closes the viewer and leaves the reader (B4).
function useFileRoute(count) {
  const [index, setIndex] = useState(null);
  const latest = useRef(index);
  latest.current = index;
  useEffect(() => registerOverlayRoute('file', {
    value: () => (latest.current === null ? '' : String(latest.current)),
    restore: (want) => {
      const next = want === '' || want === undefined ? null : Number(want);
      setIndex(next !== null && next >= 0 && next < count ? next : null);
    },
  }), [count]);
  const openAt = (i) => { setIndex(i); latest.current = i; announceOverlay({}); };
  const close = () => { if (latest.current === null) return; setIndex(null); latest.current = null; announceOverlay({ closed: true }); };
  return [index, openAt, close];
}

// PhoneAttachments are the reader's files: an image full width (tap to zoom
// in the viewer), any other file as a line, each with its name, size and
// ↓ save.
function PhoneAttachments({ message }) {
  const files = messageAttachments(message);
  const [open, openAt, close] = useFileRoute(files.length);
  if (!files.length) return null;
  const source = messageFileSource(message.id);
  const shown = open === null ? null : files[open];
  return html`<div class="phone-mail-attachments">
    ${files.map((file, index) => {
      const viewable = !!viewerKind(file);
      const save = html`<a class="phone-mail-save" href=${attachmentHref(message, file)} download=${file.filename || ''}>↓ save</a>`;
      return html`<figure key=${file.id ?? index} class="phone-mail-attachment">
        ${file.previewable && html`<button data-ui-exempt type="button" class="phone-mail-preview-img"
          aria-label=${`View ${file.filename || 'the image'}`} onClick=${() => openAt(index)}>
          <img src=${source.href(file)} alt="" loading="lazy" decoding="async" />
          <span class="phone-mail-zoom" aria-hidden="true">⤢ tap to zoom</span>
        </button>`}
        <figcaption class="phone-mail-caption">
          ${!file.previewable && viewable
            ? html`<button data-ui-exempt type="button" class="phone-mail-view" onClick=${() => openAt(index)}>${fileIcon(file)} ${file.filename || 'file'}</button>`
            : html`<span>${file.previewable ? '' : `${fileIcon(file)} `}${file.filename || 'file'}</span>`}
          ${file.size_bytes ? html` · <span>${attachmentSize(file.size_bytes)}</span>` : ''} · ${save}
        </figcaption>
      </figure>`;
    })}
    ${shown && html`<${AttachmentViewer} source=${source} attachment=${shown} attachments=${files} surface="messages" onClose=${close} routed=${true} />`}
  </div>`;
}

// openTerminal opens the sender's terminal (the phone's docked one).
const openTerminal = (agent, label) => import('./terminals-tab.js').then(({ openWebWindowPane }) => openWebWindowPane(agent, label, {}));

// findable says whether a message's sender has a row to find in Agents
// (Find ›, agent-route.js): an agent of this node. Mail a peer relayed, and
// a peer's page, have none here.
const findable = (item) => !!item.from_agent && !peerOfMessage(item) && !remoteNodeID();

// whyNotLive is what a tap on a sender's terminal says when it cannot open:
// the sender is offline (Find › to power it on), retired, deleted, or not
// known at all. The snapshot holds every agent that is not retired; a
// retired one is asked for (GET /api/retired, a contains filter).
export async function whyNotLive(item, sender, { snapshot = dashboardState.snapshot.value, fetchImpl = globalThis.fetch } = {}) {
  const id = item.from_agent || item.from_conv || '';
  if (!id) return 'Unknown sender: there is no agent to open.';
  const same = (m) => m && (m.agent_id === id || m.conv_id === id || (item.from_conv && m.conv_id === item.from_conv));
  if ([...(snapshot?.agents || []), ...(snapshot?.ungrouped || [])].some(same)) {
    return `${sender} is offline. Find › shows it in Agents, where you can power it on.`;
  }
  try {
    const response = await fetchImpl(`/api/retired?limit=20&q=${encodeURIComponent(id)}`, { credentials: 'same-origin' });
    const rows = response.ok ? (await response.json())?.rows || [] : null;
    if (rows?.some(same)) return `${sender} was retired.`;
    if (rows) return `${sender} no longer exists (deleted).`;
  } catch (_) { /* below */ }
  return `${sender} is not running.`;
}

// tryTerminal opens the sender's terminal, or says why it cannot.
const tryTerminal = (item, sender, live) => {
  if (live) openTerminal(item.from_agent, sender);
  else whyNotLive(item, sender).then((why) => shellToast(why, true));
};

// FindButton goes to the sender's row in Agents (agent-route.js): its group
// opened up, the row glowing. Never off: finding an offline sender to power
// it on is what it is for.
function FindButton({ item, sender }) {
  return html`<${Button} class="phone-btn phone-mail-find" aria-label=${`Find ${sender} in Agents`}
    onClick=${() => findAgentRoute(item.from_agent)}>Find ›<//>`;
}

// replyActions send a notification's reply, as the desktop reply dialog does.
let replies = null;

function PhoneReader({ current, controller, item, active, back }) {
  const folder = current.selected;
  const human = folder === HUMAN;
  const sender = human ? (item.from_title || item.from_conv || item.from_agent || 'the agent') : senderLabel(item);
  const live = !!item.from_agent && controller.senderOnline(item.from_agent, item.from_conv);
  const terminal = human && !!item.from_agent;
  const find = findable(item);
  const acts = rowActions(controller, folder);
  const canReply = human && !!(item.from_conv || item.from_agent);
  // The reply bar owns the bottom edge, with the ▾/▴ toggle for the nav
  // (js/phone-nav-toggle.js); a message with no reply bar keeps the nav.
  const nav = !canReply || navShown('mail');
  usePhonePage({
    title: '',
    onBack: back,
    hideNav: !nav,
    // The reader is a location of its own (/messages/<folder>/<id>).
    routed: true,
    actions: [
      ...(terminal ? [{ key: 'focus', icon: '⌖', label: live ? `Open ${sender}'s terminal` : `${sender} is offline`, muted: !live,
        onClick: () => tryTerminal(item, sender, live) }] : []),
      { key: 'read', icon: item.read ? '✉̸' : '✉', label: item.read ? 'Mark unread' : 'Mark read', onClick: () => acts.setRead(item, !item.read) },
      { key: 'delete', icon: '🗑', label: acts.deleteLock || 'Delete this message', danger: true, disabled: !!acts.deleteLock,
        onClick: () => acts.remove(item) },
    ],
  }, active);
  // A message opens at its top, wherever the list was scrolled to.
  useLayoutEffect(() => { globalThis.scrollTo?.(0, 0); }, [item.id]);
  const reply = () => {
    replies ||= createMessageAccessDialogActions({ notify: shellToast, refresh: () => controller.reloadMail() });
    openPhoneCompose({ agent: item.from_agent || item.from_conv, label: sender }, {
      title: `Reply to ${sender}`,
      hint: 'queued if it\'s busy',
      offline: controller.senderOnline(item.from_agent, item.from_conv) ? '' : `${sender} is offline`,
      send: ({ body, files }) => replies.replyHuman({ id: item.id, body, files, label: sender }),
    });
  };
  return html`<article class=${`phone-mail-reader phone-mail-read${canReply ? ' with-reply' : ''}${canReply && nav ? ' with-nav' : ''}`} data-kind=${controller.msgKind(item)}>
    <h1 class="phone-mail-title">${item.subject || '(no subject)'}</h1>
    <div class="phone-mail-sender">
      <${Avatar} name=${rowAvatar(item, folder === 'all')} />
      <div class="phone-mail-sender-text">
        <span class="phone-mail-sender-name">${human ? sender : rowParty(item, true)}</span>
        <span class="phone-mail-sender-line">${sentLine(item, human)}</span>
      </div>
      ${(terminal || find) && html`<div class="phone-mail-sender-acts">
        ${terminal && html`<${Button} class="phone-btn phone-mail-terminal" aria-disabled=${live ? undefined : 'true'}
          title=${live ? undefined : `${sender} is offline`} onClick=${() => tryTerminal(item, sender, live)}>Terminal ›<//>`}
        ${find && html`<${FindButton} item=${item} sender=${human ? sender : senderLabel(item)} />`}
      </div>`}
    </div>
    <${MessageBody} message=${item} agentMail=${!human} />
    <${PhoneAttachments} message=${item} />
    ${human && (pendingDecision(item, HUMAN) && (item.from_conv || item.from_agent)
      ? html`<div class="phone-mail-decision" role="group" aria-label="Decide">
          <${SheetHead}>Decide<//>
          <${DecisionAnswer} decision=${item} notify=${shellToast}
            onDone=${(sent) => { shellToast(sent ? `Sent “${sent}” to ${sender}` : `Dismissed; ${sender} is told`, false); controller.reloadMail(); }} />
        </div>`
      : html`<${DecisionChoices} message=${item} toast=${shellToast} onDone=${controller.reloadMail} />`)}
    ${human && html`<${MessageAction} message=${item} toast=${shellToast} onDone=${controller.reloadMail} onRefused=${controller.reloadMail} />`}
    ${canReply && html`<div class="phone-mail-replybar">
      <${NavToggle} kind="mail" />
      <button data-ui-exempt type="button" class="phone-mail-reply" onClick=${reply}>Reply to ${sender}…</button>
      <button data-ui-exempt type="button" class="phone-mail-send" aria-label=${`Reply to ${sender}`} onClick=${reply}>➤</button>
    </div>`}
  </article>`;
}

// DesktopReader is the desktop reader's content on a phone page: an access
// request, and mail that keeps the desktop reader (desktopReader).
function DesktopReader({ current, controller, model, item, active, back }) {
  const human = current.selected === HUMAN && !model.access;
  const sender = human ? (item.from_title || item.from_conv || item.from_agent || 'the agent') : '';
  const live = human && controller.senderOnline(item.from_agent, item.from_conv);
  const terminal = human && item.from_agent && !remoteNodeID();
  usePhonePage({
    title: '',
    onBack: back,
    hideNav: true,
    routed: true,
    actions: terminal ? [{
      key: 'terminal', icon: 'Terminal ›', label: live ? `Open ${sender}'s terminal` : `${sender} is offline`, muted: !live,
      onClick: () => tryTerminal(item, sender, live),
    }, ...(findable(item) ? [{
      key: 'find', icon: 'Find ›', label: `Find ${sender} in Agents`, onClick: () => findAgentRoute(item.from_agent),
    }] : [])] : [],
  }, active);
  useLayoutEffect(() => { globalThis.scrollTo?.(0, 0); }, [item.id]);
  return html`<div class=${`phone-mail-reader${model.access ? ' phone-mail-access-reader' : ''}`}>
    <${MessageReader} current=${current} controller=${controller} model=${withItem(model, item)} />
  </div>`;
}

// AccessReader is a plain pending access request on a phone: its card
// (countdown, Deny / +5 min / Approve), what it asks for, and Always allow
// where the request offers it. Anything else keeps the desktop reader.
function AccessReader({ controller, item, active, back }) {
  const who = controller.accessWho(item);
  // One Always allow at a time, and none once the request has timed out.
  const [always, setAlways] = useState(false);
  const allow = async (decision) => {
    setAlways(true);
    try { await controller.decideAccess(item.id, decision); } finally { setAlways(false); }
  };
  const lapsed = !controller.accessIsPending(item);
  usePhonePage({ title: '', onBack: back, hideNav: true, routed: true, actions: [] }, active);
  useLayoutEffect(() => { globalThis.scrollTo?.(0, 0); }, [item.id]);
  // A sudo plan's body is agentd's "Sudo request:" preview; read as facts
  // (what, how long, why) it replaces the raw body. A body not in that shape
  // stays raw.
  const plan = String(item.perm || '').startsWith('sudo.') ? planDetails(item) : null;
  const planned = !!plan?.ok;
  const rows = [
    ...(planned
      ? [['Permissions', plan.slugs.join(', '), true], ['For', plan.duration], ['Reason', plan.reason]]
      : [['Permission', item.perm, true]]),
    ['Endpoint', item.path, true], ['Group', item.target_group],
    ['Target', item.target_conv_title || item.target_conv_id], ['Scope', item.scope_display, true],
  ].filter(([, value]) => value);
  return html`<div class="phone-mail-reader phone-mail-read">
    <h1 class="phone-mail-title">${controller.accessSubject(item)}</h1>
    <${PhoneApprovalCard} request=${item} from=${who} notify=${shellToast} decide=${decideThrough(controller)} />
    <dl class="phone-mail-facts">${rows.map(([k, v, mono]) => html`<div key=${k}><dt>${k}</dt><dd class=${mono ? 'mono' : undefined}>${v}</dd></div>`)}
      ${item.body && !planned && html`<div><dt>${item.body_label || 'Body'}</dt><dd><pre class="phone-mail-pre">${item.body}</pre></dd></div>`}</dl>
    ${item.auto_grantable && html`<div class="phone-btns phone-mail-always">
      ${item.scope_display && html`<${Button} class="phone-btn phone-btn-grow" title=${`Approve now, and from now on for ${item.scope_display} only`}
        disabled=${always || lapsed} onClick=${() => allow('always_scoped')}>Always allow — ${item.scope_display}<//>`}
      <${Button} class="phone-btn phone-btn-grow" title="Approve now, and do not ask again for this permission"
        disabled=${always || lapsed} onClick=${() => allow('always')}>${item.scope_display ? 'Always allow — any scope' : 'Always allow'}<//>
    </div>`}
  </div>`;
}

function Reader(props) {
  const { current, controller, model, item } = props;
  if (model.access) return accessInline(item, controller) ? html`<${AccessReader} ...${props} />` : html`<${DesktopReader} ...${props} />`;
  if (desktopReader(item, current.selected)) return html`<${DesktopReader} ...${props} />`;
  return html`<${PhoneReader} ...${props} />`;
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
  // The reader's ‹ always leads to its folder's list (nav-back.js): a step
  // back when the list's own entry is the one under it, else the list in
  // place of the reader. The phone's Back keeps its meaning.
  const listScroll = useRef(0);
  const current = controller.state.view.value;
  const model = controller.messageView();
  const kept = useRef(null);
  const item = openedMessage(current, model, kept.current);
  kept.current = item ? { id: current.selectedMsgId, access: !!model.access, item } : null;
  const open = (id) => {
    listScroll.current = globalThis.scrollY || 0;
    controller.selectMessage(id, { navigate: true });
  };
  const back = () => {
    const here = mailLocation();
    toParent(here && { ...here, item: undefined }, () => controller.closeMessage());
  };
  const active = activeTab === 'messages';
  // The reader's reply to a peer operator's message opens its dialog through
  // PeerMailHost, as on the desktop.
  return html`<${Fragment}>
    ${item
      ? html`<${Reader} key=${`${model.access ? 'a' : 'm'}:${item.id}`} current=${current} controller=${controller} model=${model} item=${item}
          active=${active} back=${back} />`
      : html`<${MailList} current=${current} controller=${controller} model=${model} open=${open} listScroll=${listScroll} active=${active} />`}
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
