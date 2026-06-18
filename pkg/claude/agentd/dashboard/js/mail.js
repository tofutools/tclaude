// mail.js — the Messages tab's mail client.
//
// An introspection + cleanup view over every mailbox agentd stores, so
// the operator can see what agents actually said to each other (and to
// the human) when something goes wrong between them — and prune that
// history. Three panes, the way a desktop mail client reads:
//
//   sidebar (#mail-sidebar) → mailbox list: a virtual "All agent
//                             messages" firehose, the "Human
//                             notifications" folder, then one folder per
//                             agent. Filtered by #filter-mailboxes (name
//                             / id / group); each agent row carries a
//                             checkbox for the bulk "wipe selected"
//                             action (#mail-wipe-bar).
//   list    (#mail-list)    → the selected folder's messages, newest
//                             first, filtered by #filter-messages. Each
//                             row has a select checkbox and a delete
//                             button; #mail-bulk-bar drives select-all +
//                             "delete selected".
//   reader  (#mail-reader)  → the selected message's headers + body,
//                             plus per-folder actions (human folder:
//                             mark-read / focus; every folder: delete).
//
// Read data comes from two cookie-authed GETs (dashboard_mailbox.go):
// /api/mailboxes for the sidebar roster and /api/mailbox?id=<all|human|
// conv> for a folder's messages. Mutations are the operator's authority
// (cookie + Origin): agent + "all" folders delete agent_messages via
// /api/mailbox/delete (by id) and /api/mailbox/wipe (by conv); the human
// folder keeps its /api/human-messages/* path (delete accepts an ids
// array for multi-select). The reader's human-only mark-read / focus
// actions still flow through row-actions.js's document handler.
//
// Bulk delete/wipe is split into many small batched requests (see
// runBatches) rather than one giant call: a progress bar fills in the
// bulk bar as each batch lands, and mail.busy freezes the refresh +
// handlers for the duration so nothing races the running op.

import { $, $$, esc, relTime, shortId } from './helpers.js';
import { dashPrefs } from './prefs.js';
// lastSnapshot lives in dashboard.js; confirmModal/toast live in
// refresh.js. Both are benign, TDZ-safe import cycles (see tabs.js):
// nothing here reads them at module top level — only inside handlers —
// and refresh.js's confirmModal/toast are hoisted function declarations.
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast } from './refresh.js';

const HUMAN_ID = 'human';
const ALL_ID = 'all';
const SELECTED_KEY = 'tclaude.dash.mail.mailbox';
const BOX_FILTER_KEY = 'tclaude.dash.mail.boxfilter';

// Bulk deletes are split into many small API calls rather than one giant
// request, so the operator watches the work advance and a huge selection
// can't block on a single long round-trip. DELETE_BATCH caps how many
// message ids go in one /api/mailbox/delete; WIPE_BATCH caps how many
// mailboxes go in one /api/mailbox/wipe (each conv can delete an unbounded
// number of rows server-side, so the per-call fan-out stays small).
const DELETE_BATCH = 50;
const WIPE_BATCH = 5;

// Module-local view state. selected survives across the 2s repaint
// (persisted) so the human's chosen folder is sticky; selectedMsgId (the
// open message) is per-session. selectedMsgs (checked rows) is
// per-folder — cleared on folder switch. selectedBoxes (checked agent
// mailboxes for the bulk wipe) persists across folder switches so the
// operator can tick several folders then wipe.
const mail = {
  mailboxes: [],
  selected: dashPrefs.getItem(SELECTED_KEY) || HUMAN_ID,
  messages: [],
  selectedMsgId: null,
  selectedMsgs: new Set(),
  selectedBoxes: new Set(),
  inflight: false,
  // busy is set while a batched delete/wipe runs: it freezes the 2s
  // refresh (so mail.messages isn't swapped out from under the running
  // op) and the tab's click/change handlers (so no second mutation
  // starts mid-run). progress, when set, drives the progress bar painted
  // into one of the bulk bars — {where:'bulk'|'wipe', verb, done, total}.
  busy: false,
  progress: null,
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
  // A running bulk op owns mail.messages / the selection sets; let it
  // finish before the background refresh swaps them out.
  if (mail.inflight || mail.busy) return;
  mail.inflight = true;
  try {
    await Promise.all([loadMailboxes(), loadMessages()]);
    pruneSelections();
    paintMail();
  } finally {
    mail.inflight = false;
  }
}

// reloadMail forces a roster+folder refresh after a mutation, bypassing
// the inflight guard so the operator sees the result immediately rather
// than waiting up to 2s for the next tick.
async function reloadMail() {
  if (mail.busy) return;
  await Promise.all([loadMailboxes(), loadMessages()]);
  pruneSelections();
  paintMail();
}

// pruneSelections drops checked ids/convs that no longer exist (deleted
// messages, vanished mailboxes) so the bulk-bar counts stay honest.
function pruneSelections() {
  const msgIds = new Set(mail.messages.map(m => m.id));
  for (const id of [...mail.selectedMsgs]) {
    if (!msgIds.has(id)) mail.selectedMsgs.delete(id);
  }
  const agentIds = new Set(
    mail.mailboxes.filter(mb => mb.kind === 'agent').map(mb => mb.id));
  for (const c of [...mail.selectedBoxes]) {
    if (!agentIds.has(c)) mail.selectedBoxes.delete(c);
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

// paintMail repaints all panes from cached state, applying the current
// filters. Sync — used by the filter inputs and after selection changes,
// with no server round-trip.
function paintMail() {
  paintBulkActions();
  paintSidebar();
  paintWipeBar();
  paintList();
  paintListBulkBar();
  paintReader();
}

function mailboxLabel(mb) {
  if (mb.kind === 'all') return 'All agent messages';
  if (mb.kind === 'human') return 'Human notifications';
  return mb.title || shortId(mb.id) || '(unknown)';
}

function mailboxIcon(mb) {
  if (mb.kind === 'all') return '🗂';
  if (mb.kind === 'human') return '📬';
  return `<span class="mail-dot ${mb.online ? 'online' : 'offline'}">●</span>`;
}

function mailboxMatchesFilter(mb, q) {
  if (!q) return true;
  return [mailboxLabel(mb), mb.id, mb.short, ...(mb.groups || [])]
    .some(s => (s || '').toLowerCase().includes(q));
}

function paintSidebar() {
  const el = $('#mail-sidebar');
  if (!el) return;
  const q = ($('#filter-mailboxes')?.value || '').toLowerCase();
  const boxes = mail.mailboxes.filter(mb => mailboxMatchesFilter(mb, q));
  if (!boxes.length) {
    el.innerHTML = mail.mailboxes.length
      ? '<div class="empty">No mailboxes match the filter.</div>'
      : '<div class="empty">No mailboxes.</div>';
    return;
  }
  el.innerHTML = boxes.map(mb => {
    const active = mb.id === mail.selected;
    const unread = mb.unread
      ? `<span class="mailbox-unread">${mb.unread > 99 ? '99+' : mb.unread}</span>`
      : '';
    const count = `<span class="mailbox-count" title="${mb.in} received · ${mb.out} sent">${mb.total}</span>`;
    const btn = `<button class="mailbox${active ? ' active' : ''}${mb.unread ? ' has-unread' : ''}"
      data-act="mailbox-select" data-id="${esc(mb.id)}" title="${esc(mailboxLabel(mb))}">
      <span class="mailbox-icon">${mailboxIcon(mb)}</span>
      <span class="mailbox-name">${esc(mailboxLabel(mb))}</span>
      ${count}${unread}
    </button>`;
    // Only real agent mailboxes are checkable for the bulk wipe — the
    // virtual "all" and "human" folders are special views. A spacer
    // keeps their labels aligned with the checkbox column.
    const lead = mb.kind === 'agent'
      ? `<input type="checkbox" class="mail-box-check" data-conv="${esc(mb.id)}"${mail.selectedBoxes.has(mb.id) ? ' checked' : ''} title="Select for bulk wipe" />`
      : '<span class="mail-box-check-spacer"></span>';
    return `<div class="mailbox-row">${lead}${btn}</div>`;
  }).join('');
}

// paintWipeBar shows the "wipe selected mailboxes" bar when one or more
// agent folders are ticked in the sidebar.
function paintWipeBar() {
  const bar = $('#mail-wipe-bar');
  if (!bar) return;
  if (mail.busy && mail.progress && mail.progress.where === 'wipe') {
    bar.hidden = false;
    bar.innerHTML = progressBarHTML(mail.progress);
    return;
  }
  const n = mail.selectedBoxes.size;
  bar.hidden = n === 0;
  if (n === 0) { bar.innerHTML = ''; return; }
  bar.innerHTML = `
    <span class="grow">${n} mailbox${n === 1 ? '' : 'es'} selected</span>
    <button data-act="mail-clear-box-sel" title="Clear selection">clear</button>
    <button class="danger" data-act="mail-wipe-selected" title="Delete every message in the selected mailboxes">🗑 wipe</button>`;
}

// counterparty returns the name to show in a non-aggregate message-list
// row — the OTHER party relative to the selected mailbox. For a received
// message that's the sender; for a sent one, the recipient.
function counterparty(m) {
  if (m.direction === 'out') {
    return m.to_title || shortId(m.to_conv) || '(unknown)';
  }
  return m.from_title || shortId(m.from_conv) || '(unknown sender)';
}

// allRecipientLabel names a row's recipient in the aggregate "all" view,
// collapsing a multicast to "first +N".
function allRecipientLabel(m) {
  if (m.to_title) return m.to_title;
  if (m.to_conv) return shortId(m.to_conv);
  const rs = m.to_recipients || [];
  if (rs.length) {
    const first = rs[0].title || shortId(rs[0].conv_id);
    return rs.length > 1 ? `${first} +${rs.length - 1}` : first;
  }
  return '(group)';
}

function msgMatchesFilter(m, q) {
  if (!q) return true;
  return [counterparty(m), m.from_title, m.to_title, m.from_conv, m.to_conv, m.group, m.subject, m.body]
    .some(s => (s || '').toLowerCase().includes(q));
}

function msgPreview(m) {
  if (m.subject) return m.subject;
  const firstNonBlank = (m.body || '').split('\n').find(l => l.trim() !== '');
  return firstNonBlank || '(no subject)';
}

// filteredMessages applies the message-list filter — shared by the list
// paint, the bulk bar, and select-all so they agree on "in view".
function filteredMessages() {
  const q = ($('#filter-messages')?.value || '').toLowerCase();
  return mail.messages.filter(m => msgMatchesFilter(m, q));
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
  const isAll = mail.selected === ALL_ID;
  el.innerHTML = filtered.map(m => {
    const active = m.id === mail.selectedMsgId;
    const unread = !m.read;
    const checked = mail.selectedMsgs.has(m.id) ? ' checked' : '';
    let head;
    if (isAll) {
      // The firehose has no "self" to be relative to — render from→to.
      head = `<span class="mail-row-party">${esc(m.from_title || shortId(m.from_conv) || '(unknown)')}</span>
        <span class="mail-row-arrow">→</span>
        <span class="mail-row-party">${esc(allRecipientLabel(m))}</span>`;
    } else {
      const arrow = m.direction === 'out'
        ? '<span class="mail-dir out" title="sent">→</span>'
        : '<span class="mail-dir in" title="received">←</span>';
      head = `${arrow}<span class="mail-row-party">${esc(counterparty(m))}</span>`;
    }
    const grp = m.group ? `<span class="mail-row-group">${esc(m.group)}</span>` : '';
    return `<div class="mail-row-wrap">
      <input type="checkbox" class="mail-msg-check" data-id="${m.id}"${checked} title="Select message" />
      <button class="mail-row${active ? ' active' : ''}${unread ? ' unread' : ''}"
        data-act="mail-open" data-id="${m.id}">
        <span class="mail-row-top">
          ${unread ? '<span class="mail-row-dot" title="unread">●</span>' : ''}
          ${head}
          ${grp}
          <span class="mail-row-time">${esc(relTime(m.created_at))}</span>
        </span>
        <span class="mail-row-subject">${esc(msgPreview(m))}</span>
      </button>
      <button class="mail-row-del" data-act="mail-msg-delete" data-id="${m.id}" title="Delete this message">🗑</button>
    </div>`;
  }).join('');
}

// paintListBulkBar drives the select-all checkbox + "delete selected"
// action over the messages currently in view.
function paintListBulkBar() {
  const bar = $('#mail-bulk-bar');
  if (!bar) return;
  if (mail.busy && mail.progress && mail.progress.where === 'bulk') {
    bar.hidden = false;
    bar.innerHTML = progressBarHTML(mail.progress);
    return;
  }
  const filtered = filteredMessages();
  if (!filtered.length) { bar.hidden = true; bar.innerHTML = ''; return; }
  bar.hidden = false;
  const n = mail.selectedMsgs.size;
  const allChecked = filtered.every(m => mail.selectedMsgs.has(m.id));
  bar.innerHTML = `
    <label title="Select / deselect every message in view">
      <input type="checkbox" class="mail-select-all"${allChecked ? ' checked' : ''} /> all
    </label>
    <span class="grow">${n ? `${n} selected` : ''}</span>
    <button class="danger" data-act="mail-del-selected" title="Delete the selected messages"${n ? '' : ' disabled'}>🗑 delete selected</button>`;
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

  // The human folder keeps mark-read + focus (its read-state is
  // meaningful); every folder gets delete. The human-folder delete
  // routes through row-actions.js's msg-delete; agent + "all" folders
  // route through this module's mail-msg-delete.
  let actions;
  if (mail.selected === HUMAN_ID) {
    const readBtn = m.read ? '' : `<button data-act="msg-mark-read" data-id="${m.id}" title="Mark this message read">mark read</button>`;
    const delBtn = `<button class="danger" data-act="msg-delete" data-id="${m.id}" title="Permanently delete this message">delete</button>`;
    actions = `<div class="mail-reader-actions">${humanFocusButton(m)}${readBtn}${delBtn}</div>`;
  } else {
    const delBtn = `<button class="danger" data-act="mail-msg-delete" data-id="${m.id}" title="Permanently delete this message">delete</button>`;
    actions = `<div class="mail-reader-actions">${delBtn}</div>`;
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
// is the only folder with read-state semantics.
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
    loadMessages().then(() => { pruneSelections(); paintMail(); });
    return;
  }
  mail.selected = id;
  mail.selectedMsgId = null;
  mail.selectedMsgs.clear();  // message selection is per-folder
  mail.messages = [];
  dashPrefs.setItem(SELECTED_KEY, id);
  paintMail();        // immediate feedback (active folder, empty list)
  loadMessages().then(() => { pruneSelections(); paintMail(); });
}

function selectMessage(id) {
  mail.selectedMsgId = Number(id);
  paintList();        // re-highlight the active row
  paintReader();
}

// --- mutations ------------------------------------------------------

// progressBarHTML renders the "Deleting 150 / 300…" label + a filling bar
// shown in a bulk bar while a batched op runs.
function progressBarHTML(p) {
  const pct = p.total ? Math.round((p.done / p.total) * 100) : 0;
  return `<span class="mail-progress-label">${esc(p.verb)} ${p.done} / ${p.total}…</span>
    <span class="mail-progress grow"><span class="mail-progress-fill" style="width:${pct}%"></span></span>`;
}

// chunk splits an array into runs of at most `size`.
function chunk(arr, size) {
  const out = [];
  for (let i = 0; i < arr.length; i += size) out.push(arr.slice(i, i + size));
  return out;
}

// runBatches drives a batched bulk operation: it splits `items` into
// chunks of `size` and runs `doBatch(chunk)` for each in series, painting
// a progress bar (in the `where` bulk bar) after every chunk so a large
// delete/wipe advances visibly instead of blocking on one request.
// doBatch returns the server-reported count handled by that chunk, or
// null on failure (already toasted) — a null aborts the remaining
// chunks. A single-chunk op skips the bar entirely, keeping the old
// quick-delete UX. While the run is in flight mail.busy freezes the 2s
// refresh and the tab handlers. Returns {deleted, handled, ok}.
async function runBatches(items, size, where, verb, doBatch) {
  const chunks = chunk(items, size);
  const showBar = chunks.length > 1;
  mail.busy = true;
  let deleted = 0, handled = 0, ok = true;
  if (showBar) {
    mail.progress = { where, verb, done: 0, total: items.length };
    paintWipeBar();
    paintListBulkBar();
  }
  try {
    for (const c of chunks) {
      const n = await doBatch(c);
      if (n === null) { ok = false; break; }
      deleted += n;
      handled += c.length;
      if (showBar) {
        mail.progress.done = handled;
        paintWipeBar();
        paintListBulkBar();
      }
    }
  } finally {
    mail.busy = false;
    mail.progress = null;
  }
  return { deleted, handled, ok };
}

// postDeleteMessages routes a delete by folder kind: the human folder
// mutates human_messages via its own endpoint; agent + "all" folders
// delete agent_messages rows. Returns the deleted count, or null on
// failure (already toasted).
async function postDeleteMessages(ids) {
  const url = mail.selected === HUMAN_ID
    ? '/api/human-messages/delete'
    : '/api/mailbox/delete';
  try {
    const r = await fetch(url, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids }),
    });
    if (!r.ok) { toast(`delete failed: ${await r.text()}`, true); return null; }
    const res = await r.json().catch(() => ({}));
    return res.deleted ?? ids.length;
  } catch (err) {
    toast(`delete failed: ${(err && err.message) || err}`, true);
    return null;
  }
}

async function deleteOneMessage(id) {
  if (mail.busy || !Number.isFinite(id)) return;
  const confirmed = await confirmModal({
    title: 'Delete this message?',
    body: 'Permanently deletes this one message. This cannot be undone.',
    meta: `#${id}`,
    okLabel: 'Delete',
  });
  if (!confirmed) return;
  const n = await postDeleteMessages([id]);
  if (n === null) return;
  mail.selectedMsgs.delete(id);
  if (mail.selectedMsgId === id) mail.selectedMsgId = null;
  toast('message deleted');
  await reloadMail();
}

async function deleteSelectedMessages() {
  if (mail.busy) return;
  const ids = [...mail.selectedMsgs];
  if (!ids.length) return;
  const confirmed = await confirmModal({
    title: `Delete ${ids.length} message${ids.length === 1 ? '' : 's'}?`,
    body: ids.length > DELETE_BATCH
      ? `Permanently deletes the selected messages, in batches of ${DELETE_BATCH}. This cannot be undone.`
      : 'Permanently deletes the selected messages. This cannot be undone.',
    okLabel: 'Delete',
  });
  if (!confirmed) return;
  const { deleted, handled, ok } = await runBatches(
    ids, DELETE_BATCH, 'bulk', 'Deleting',
    async batch => {
      const n = await postDeleteMessages(batch);
      if (n === null) return null;
      // Drop the handled ids from the selection as each batch lands, so a
      // mid-run failure leaves the selection pointing only at what's left.
      batch.forEach(id => mail.selectedMsgs.delete(id));
      if (mail.selectedMsgId != null && batch.includes(mail.selectedMsgId)) {
        mail.selectedMsgId = null;
      }
      return n;
    });
  if (handled) {
    toast(ok
      ? `deleted ${deleted} message${deleted === 1 ? '' : 's'}`
      : `deleted ${deleted} message${deleted === 1 ? '' : 's'}, then stopped on an error`,
      !ok);
  }
  await reloadMail();
}

async function wipeSelectedMailboxes() {
  if (mail.busy) return;
  const convs = [...mail.selectedBoxes];
  if (!convs.length) return;
  const names = convs.map(c => {
    const mb = mail.mailboxes.find(x => x.id === c);
    return (mb && (mb.title || mb.short)) || shortId(c);
  });
  const confirmed = await confirmModal({
    title: `Wipe ${convs.length} mailbox${convs.length === 1 ? '' : 'es'}?`,
    body: 'Permanently deletes every message where these agents are sender or recipient — including the copy in the other party’s mailbox. This cannot be undone.',
    meta: names.join(', '),
    okLabel: 'Wipe',
  });
  if (!confirmed) return;
  const { deleted, handled, ok } = await runBatches(
    convs, WIPE_BATCH, 'wipe', 'Wiping',
    async batch => {
      try {
        const r = await fetch('/api/mailbox/wipe', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ convs: batch }),
        });
        if (!r.ok) { toast(`wipe failed: ${await r.text()}`, true); return null; }
        const res = await r.json().catch(() => ({}));
        batch.forEach(c => mail.selectedBoxes.delete(c));
        return res.deleted || 0;
      } catch (err) {
        toast(`wipe failed: ${(err && err.message) || err}`, true);
        return null;
      }
    });
  if (handled) {
    mail.selectedMsgs.clear();
    mail.selectedMsgId = null;
    toast(ok
      ? `wiped ${deleted} message${deleted === 1 ? '' : 's'}`
      : `wiped ${deleted} message${deleted === 1 ? '' : 's'}, then stopped on an error`,
      !ok);
  }
  await reloadMail();
}

// --- wiring ---------------------------------------------------------

function initMail() {
  const sec = $('#tab-messages');
  if (sec) {
    // Delegated click handler scoped to the Messages tab. The human-
    // message mark-read / focus actions (msg-*) are handled by
    // row-actions.js's document-level handler; the data-act values here
    // don't overlap those.
    sec.addEventListener('click', e => {
      // A batched delete/wipe owns the view until it finishes — ignore
      // clicks (including the row delete buttons) so no second mutation
      // races the running one.
      if (mail.busy) return;
      const btn = e.target.closest('[data-act]');
      if (!btn || !sec.contains(btn)) return;
      const act = btn.getAttribute('data-act');
      if (act === 'mailbox-select') {
        selectMailbox(btn.getAttribute('data-id'));
      } else if (act === 'mail-open') {
        selectMessage(btn.getAttribute('data-id'));
      } else if (act === 'mail-msg-delete') {
        deleteOneMessage(Number(btn.getAttribute('data-id')));
      } else if (act === 'mail-del-selected') {
        deleteSelectedMessages();
      } else if (act === 'mail-wipe-selected') {
        wipeSelectedMailboxes();
      } else if (act === 'mail-clear-box-sel') {
        mail.selectedBoxes.clear();
        paintSidebar();
        paintWipeBar();
      }
    });
    // Checkbox toggles arrive as `change`, not `click` — and carry no
    // data-act, so row-actions.js's click handler leaves them alone (no
    // preventDefault) and they toggle normally. Selection state is the
    // source of truth; the 2s repaint re-derives `checked` from it.
    sec.addEventListener('change', e => {
      if (mail.busy) return;  // selection is frozen during a batched op
      const t = e.target;
      if (t.classList.contains('mail-msg-check')) {
        const id = Number(t.getAttribute('data-id'));
        if (t.checked) mail.selectedMsgs.add(id); else mail.selectedMsgs.delete(id);
        paintListBulkBar();
      } else if (t.classList.contains('mail-box-check')) {
        const conv = t.getAttribute('data-conv');
        if (t.checked) mail.selectedBoxes.add(conv); else mail.selectedBoxes.delete(conv);
        paintWipeBar();
      } else if (t.classList.contains('mail-select-all')) {
        const filtered = filteredMessages();
        if (t.checked) filtered.forEach(m => mail.selectedMsgs.add(m.id));
        else filtered.forEach(m => mail.selectedMsgs.delete(m.id));
        paintList();
        paintListBulkBar();
      }
    });
  }
  // Sidebar mailbox filter — name / short-id / group. Persisted like the
  // other tab filters, but scoped to the roster pane (the top filter bar
  // stays scoped to the open folder's messages).
  const boxFilter = $('#filter-mailboxes');
  if (boxFilter) {
    boxFilter.value = dashPrefs.getItem(BOX_FILTER_KEY) || '';
    boxFilter.addEventListener('input', () => {
      const v = boxFilter.value;
      if (v) dashPrefs.setItem(BOX_FILTER_KEY, v); else dashPrefs.removeItem(BOX_FILTER_KEY);
      paintSidebar();
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
