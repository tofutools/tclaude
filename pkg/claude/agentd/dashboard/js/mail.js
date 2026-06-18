// mail.js — the Messages tab's mail client.
//
// A read-only introspection view over every mailbox agentd stores, so
// the operator can see what agents actually said to each other (and to
// the human) when something goes wrong between them. Three panes, the
// way a desktop mail client reads:
//
//   sidebar (#mail-sidebar) → mailbox list, one per agent + the
//                             "Human notifications" folder, with unread
//                             badges.
//   list    (#mail-list)    → the selected mailbox's messages, newest
//                             first, filtered by the filter-bar input.
//   reader  (#mail-reader)  → the selected message's headers + body,
//                             plus (human folder only) the existing
//                             mark-read / delete / focus actions.
//
// Data comes from two cookie-authed GET endpoints (see
// dashboard_mailbox.go): /api/mailboxes for the sidebar roster and
// /api/mailbox?id=<conv|human> for a folder's messages. Viewing an
// agent mailbox never mutates that agent's read-state — this is
// introspection, not inbox management; the only mutating actions are on
// the human folder, reusing the /api/human-messages/* handlers wired in
// row-actions.js (data-act="msg-mark-read" / "msg-delete" /
// "msg-mark-all-read" / "msg-clear" / "msg-focus").

import { $, $$, esc, relTime, shortId } from './helpers.js';
import { dashPrefs } from './prefs.js';
// lastSnapshot lives in dashboard.js — imported back for the online set
// that gates the human-folder "focus" button. A benign, TDZ-safe cycle
// (see tabs.js): no top-level code here reads it.
import { lastSnapshot } from './dashboard.js';

const HUMAN_ID = 'human';
const SELECTED_KEY = 'tclaude.dash.mail.mailbox';

// Module-local view state. selected survives across the 2s repaint
// (persisted) so the human's chosen folder is sticky; selectedMsgId is
// per-session (an id that vanishes from the list just clears the
// reader).
const mail = {
  mailboxes: [],
  selected: dashPrefs.getItem(SELECTED_KEY) || HUMAN_ID,
  messages: [],
  selectedMsgId: null,
  inflight: false,
};

function mailTabActive() {
  const sec = $('#tab-messages');
  return !!sec && sec.classList.contains('active');
}

// onlineConvs is the set of conv-ids with a live tmux window — focus is
// only meaningful for those. Cross-referenced from the snapshot already
// in memory, so no extra round-trip.
function onlineConvs() {
  const set = new Set();
  const snap = lastSnapshot || {};
  (snap.agents || []).concat(snap.ungrouped || [])
    .forEach(a => { if (a && a.online) set.add(a.conv_id); });
  return set;
}

// --- data loading ---------------------------------------------------

async function loadMailboxes() {
  try {
    const r = await fetch('/api/mailboxes', { credentials: 'same-origin' });
    if (!r.ok) return;
    const data = await r.json();
    mail.mailboxes = data.mailboxes || [];
  } catch { /* transient; keep the last roster painted */ }
}

async function loadMessages() {
  const id = mail.selected;
  try {
    const r = await fetch(`/api/mailbox?id=${encodeURIComponent(id)}`,
      { credentials: 'same-origin' });
    if (!r.ok) { mail.messages = []; return; }
    const data = await r.json();
    // Guard against a stale response landing after the human switched
    // folders mid-flight.
    if (mail.selected !== id) return;
    mail.messages = data.messages || [];
  } catch { mail.messages = []; }
}

// loadMail refreshes both the roster and the open folder, then repaints.
// Guarded so overlapping 2s ticks don't stack fetches.
async function loadMail() {
  if (mail.inflight) return;
  mail.inflight = true;
  try {
    await Promise.all([loadMailboxes(), loadMessages()]);
    paintMail();
  } finally {
    mail.inflight = false;
  }
}

// --- rendering ------------------------------------------------------

// renderMailTab is the entry the refresh loop calls every 2s and on tab
// activation: repaint from cache (cheap, keeps the filter responsive),
// and pull fresh data when the tab is actually being viewed.
function renderMailTab() {
  paintMail();
  if (mailTabActive()) loadMail();
}

// paintMail repaints all three panes from cached state, applying the
// current filter. Sync — used by the filter input and after selection
// changes, with no server round-trip.
function paintMail() {
  paintBulkActions();
  paintSidebar();
  paintList();
  paintReader();
}

function mailboxLabel(mb) {
  if (mb.kind === 'human') return 'Human notifications';
  return mb.title || shortId(mb.id) || '(unknown)';
}

function paintSidebar() {
  const el = $('#mail-sidebar');
  if (!el) return;
  el.innerHTML = mail.mailboxes.map(mb => {
    const active = mb.id === mail.selected;
    const icon = mb.kind === 'human'
      ? '📬'
      : `<span class="mail-dot ${mb.online ? 'online' : 'offline'}">●</span>`;
    const unread = mb.unread
      ? `<span class="mail-unread">${mb.unread > 99 ? '99+' : mb.unread}</span>`
      : '';
    const count = `<span class="mail-count" title="${mb.in} received · ${mb.out} sent">${mb.total}</span>`;
    return `<button class="mailbox${active ? ' active' : ''}${mb.unread ? ' has-unread' : ''}"
      data-act="mailbox-select" data-id="${esc(mb.id)}" title="${esc(mailboxLabel(mb))}">
      <span class="mailbox-icon">${icon}</span>
      <span class="mailbox-name">${esc(mailboxLabel(mb))}</span>
      ${count}${unread}
    </button>`;
  }).join('') || '<div class="empty">No mailboxes.</div>';
}

// counterparty returns the name to show in the message-list row — the
// OTHER party relative to the selected mailbox. For a received message
// that's the sender; for a sent one, the recipient.
function counterparty(m) {
  if (m.direction === 'out') {
    return m.to_title || shortId(m.to_conv) || '(unknown)';
  }
  return m.from_title || shortId(m.from_conv) || '(unknown sender)';
}

function msgMatchesFilter(m, q) {
  if (!q) return true;
  return [counterparty(m), m.from_title, m.to_title, m.group, m.subject, m.body]
    .some(s => (s || '').toLowerCase().includes(q));
}

function msgPreview(m) {
  if (m.subject) return m.subject;
  const firstNonBlank = (m.body || '').split('\n').find(l => l.trim() !== '');
  return firstNonBlank || '(no subject)';
}

function paintList() {
  const el = $('#mail-list');
  if (!el) return;
  const q = ($('#filter-messages')?.value || '').toLowerCase();
  const filtered = mail.messages.filter(m => msgMatchesFilter(m, q));

  const total = mail.messages.length;
  const countEl = $('#filter-messages-count');
  if (countEl) {
    countEl.textContent = q
      ? `${filtered.length} / ${total}`
      : `${total} message${total === 1 ? '' : 's'}`;
  }

  if (!filtered.length) {
    el.innerHTML = total
      ? '<div class="empty">No messages match the filter.</div>'
      : '<div class="empty">This mailbox is empty.</div>';
    return;
  }
  el.innerHTML = filtered.map(m => {
    const active = m.id === mail.selectedMsgId;
    const unread = !m.read;
    const arrow = m.direction === 'out'
      ? '<span class="mail-dir out" title="sent">→</span>'
      : '<span class="mail-dir in" title="received">←</span>';
    const grp = m.group ? `<span class="mail-row-group">${esc(m.group)}</span>` : '';
    return `<button class="mail-row${active ? ' active' : ''}${unread ? ' unread' : ''}"
      data-act="mail-open" data-id="${m.id}">
      <span class="mail-row-top">
        ${unread ? '<span class="mail-row-dot" title="unread">●</span>' : ''}
        ${arrow}
        <span class="mail-row-party">${esc(counterparty(m))}</span>
        ${grp}
        <span class="mail-row-time">${esc(relTime(m.created_at))}</span>
      </span>
      <span class="mail-row-subject">${esc(msgPreview(m))}</span>
    </button>`;
  }).join('');
}

// recipientNames renders a decorated recipients array ([{conv_id,title}])
// as "name <abcd1234>, …" for the reader headers.
function recipientNames(rs) {
  if (!rs || !rs.length) return '';
  return rs.map(r => r.title
    ? `${esc(r.title)} <span class="mail-cid">${esc(shortId(r.conv_id))}</span>`
    : `<span class="mail-cid">${esc(shortId(r.conv_id))}</span>`).join(', ');
}

function readerHeaderRow(label, valueHTML) {
  if (!valueHTML) return '';
  return `<div class="mail-hrow"><span class="mail-hlabel">${label}</span><span class="mail-hval">${valueHTML}</span></div>`;
}

// humanFocusButton renders the "focus" action for a human-folder
// message, disabled when the sending agent has no live window. Reuses
// the msg-focus handler in row-actions.js (jump + mark read).
function humanFocusButton(m) {
  if (!m.from_conv) return '';
  const focusable = onlineConvs().has(m.from_conv);
  const label = m.from_title || m.from_conv;
  return `<button data-act="msg-focus" data-id="${m.id}" data-conv="${esc(m.from_conv)}" data-label="${esc(label)}"${focusable ? '' : ' disabled'} title="${focusable ? 'Focus this agent’s terminal window and mark the message read' : 'Sending agent is offline — no window to focus'}">focus</button>`;
}

function paintReader() {
  const el = $('#mail-reader');
  if (!el) return;
  const m = mail.messages.find(x => x.id === mail.selectedMsgId);
  if (!m) {
    el.innerHTML = '<div class="empty">Select a message to read.</div>';
    return;
  }
  const when = m.created_at ? new Date(m.created_at).toLocaleString() : '';
  const fromHTML = m.from_title
    ? `${esc(m.from_title)} <span class="mail-cid">${esc(shortId(m.from_conv))}</span>`
    : (m.from_conv ? `<span class="mail-cid">${esc(shortId(m.from_conv))}</span>` : '');
  // To: prefer the full recipients array (multicasts carry every
  // addressee); fall back to the single to_conv for a plain 1:1.
  let toHTML = recipientNames(m.to_recipients);
  if (!toHTML && (m.to_title || m.to_conv)) {
    toHTML = m.to_title
      ? `${esc(m.to_title)} <span class="mail-cid">${esc(shortId(m.to_conv))}</span>`
      : `<span class="mail-cid">${esc(shortId(m.to_conv))}</span>`;
  }
  const stateBits = [];
  stateBits.push(m.read ? 'read' : '<span class="mail-state-unread">unread</span>');
  if (m.delivered_at) stateBits.push('delivered');
  else if (m.direction === 'out') stateBits.push('<span class="mail-state-pending">undelivered</span>');

  // Human-folder messages keep the mark-read / delete actions (and
  // focus); agent folders are read-only introspection.
  let actions = '';
  if (mail.selected === HUMAN_ID) {
    const readBtn = m.read ? '' : `<button data-act="msg-mark-read" data-id="${m.id}" title="Mark this message read">mark read</button>`;
    const delBtn = `<button class="danger" data-act="msg-delete" data-id="${m.id}" title="Permanently delete this message">delete</button>`;
    actions = `<div class="mail-reader-actions">${humanFocusButton(m)}${readBtn}${delBtn}</div>`;
  }

  el.innerHTML = `
    <div class="mail-reader-head">
      <div class="mail-subject">${esc(m.subject || '(no subject)')} <span class="mail-id">#${m.id}</span></div>
      <div class="mail-headers">
        ${readerHeaderRow('From', fromHTML)}
        ${readerHeaderRow('To', toHTML)}
        ${readerHeaderRow('Cc', recipientNames(m.cc_recipients))}
        ${readerHeaderRow('Group', m.group ? esc(m.group) : '')}
        ${readerHeaderRow('Date', esc(when))}
        ${readerHeaderRow('Status', stateBits.join(' · '))}
      </div>
    </div>
    <div class="mail-reader-body">${esc(m.body || '')}</div>
    ${actions}`;
}

// paintBulkActions shows the filter-bar "mark all read" / "clear read"
// buttons only on the human folder — they act on human_messages, which
// is the only folder the dashboard is allowed to mutate.
function paintBulkActions() {
  const human = mail.selected === HUMAN_ID;
  const markAll = $('#mail-mark-all');
  const clearRead = $('#mail-clear-read');
  if (markAll) markAll.hidden = !human;
  if (clearRead) clearRead.hidden = !human;
}

// --- selection ------------------------------------------------------

function selectMailbox(id) {
  if (!id || id === mail.selected) {
    // Re-click on the active folder: just refresh it.
    loadMessages().then(paintMail);
    return;
  }
  mail.selected = id;
  mail.selectedMsgId = null;
  mail.messages = [];
  dashPrefs.setItem(SELECTED_KEY, id);
  paintMail();        // immediate feedback (active folder, empty list)
  loadMessages().then(paintMail);
}

function selectMessage(id) {
  mail.selectedMsgId = Number(id);
  paintList();        // re-highlight the active row
  paintReader();
}

// --- wiring ---------------------------------------------------------

function initMail() {
  // Delegated click handler scoped to the Messages tab. The human-
  // message mutation actions (msg-*) are handled by row-actions.js's
  // own document-level handler; here we only own folder/message
  // selection, which has no overlap with those data-act values.
  const sec = $('#tab-messages');
  if (sec) {
    sec.addEventListener('click', e => {
      const btn = e.target.closest('[data-act]');
      if (!btn || !sec.contains(btn)) return;
      const act = btn.getAttribute('data-act');
      if (act === 'mailbox-select') {
        selectMailbox(btn.getAttribute('data-id'));
      } else if (act === 'mail-open') {
        selectMessage(btn.getAttribute('data-id'));
      }
    });
  }
  // Load immediately when the human switches TO the Messages tab, rather
  // than waiting up to 2s for the next snapshot tick. bindTabs (in
  // refresh.js) toggles the .active class on the same click; this
  // listener fires after, so mailTabActive() inside renderMailTab sees
  // the freshly-set class.
  $$('nav button[data-tab="messages"]').forEach(b =>
    b.addEventListener('click', renderMailTab));
}

export { renderMailTab, paintMail, initMail };
