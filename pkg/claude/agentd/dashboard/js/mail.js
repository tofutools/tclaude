// mail.js — the Messages tab's mail client.
//
// An introspection + cleanup view over every mailbox agentd stores, so
// the operator can see what agents actually said to each other (and to
// the human) when something goes wrong between them — and prune that
// history. Three panes, the way a desktop mail client reads:
//
//   sidebar (#mail-sidebar) → mailbox list: a virtual "All agent
//                             messages" firehose, the "Human
//                             notifications" folder, a "Groups" section
//                             (one aggregate folder per group, each
//                             expandable to reveal its member agent
//                             folders nested beneath it), then an "Agents"
//                             section with one folder per agent. Filtered
//                             by #filter-mailboxes (name / id / group);
//                             each agent row carries a checkbox for the
//                             bulk "wipe selected" action
//                             (#mail-wipe-bar). Retired agents are
//                             hidden (and their traffic dropped from the
//                             "all" firehose) until the #mail-show-retired
//                             footer toggle opts them back in
//                             (include_retired, server-side). Agents with
//                             an empty mailbox are likewise hidden until
//                             the #mail-show-empty toggle opts them in
//                             (include_empty, server-side, roster only).
//   list    (#mail-list)    → the selected folder's messages, newest
//                             first, filtered by #filter-messages. Each
//                             row has a select checkbox and a delete
//                             button; #mail-bulk-bar drives select-all +
//                             "delete selected".
//   reader  (#mail-reader)  → the selected message's headers + body,
//                             plus per-folder actions (human folder:
//                             mark-read / focus; agent + "all" folders:
//                             mark-read/unread toggle; every folder:
//                             delete).
//
// Read data comes from two cookie-authed GETs (dashboard_mailbox.go):
// /api/mailboxes for the sidebar roster and /api/mailbox?id=<all|human|
// conv> for a folder's messages. Mutations are the operator's authority
// (cookie + Origin): agent + "all" folders delete agent_messages via
// /api/mailbox/delete (by id) and /api/mailbox/wipe (by conv), and set
// their read-state via /api/mailbox/mark-read (by id, or whole-folder by
// conv) — the operator repairing a stuck agent's inbox on its behalf; the
// human folder keeps its /api/human-messages/* path (delete accepts an ids
// array for multi-select). The reader's human-only mark-read / focus
// actions still flow through row-actions.js's document handler.
//
// Bulk delete/wipe is split into many small batched requests (see
// runBatches) rather than one giant call: a progress bar fills in the
// bulk bar as each batch lands, and mail.busy freezes the refresh +
// handlers for the duration so nothing races the running op.

import { $, $$, esc, relTime, shortId, withPreservedFocus } from './helpers.js';
import { dashPrefs } from './prefs.js';
// lastSnapshot lives in dashboard.js; confirmModal/toast live in
// refresh.js. Both are benign, TDZ-safe import cycles (see tabs.js):
// nothing here reads them at module top level — only inside handlers —
// and refresh.js's confirmModal/toast are hoisted function declarations.
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast } from './refresh.js';

const HUMAN_ID = 'human';
const ALL_ID = 'all';
// Group folders are keyed "group:<name>" (the server's groupMailboxPrefix).
// They're aggregate views like the "all" firehose — every row renders
// from→to, there's no per-folder "mark all read" (a group isn't a single
// recipient), and they aren't wipe-checkable (wipe is per-conv).
const GROUP_PREFIX = 'group:';
const SELECTED_KEY = 'tclaude.dash.mail.mailbox';
const BOX_FILTER_KEY = 'tclaude.dash.mail.boxfilter';
const PAGE_SIZE_KEY = 'tclaude.dash.mail.pagesize';
// Whether the sidebar lists retired-agent folders (and the "all" firehose
// counts their traffic). Off by default — a retired agent is demoted, so
// its folder is clutter until the operator asks for it.
const SHOW_RETIRED_KEY = 'tclaude.dash.mail.showretired';
// Whether the sidebar lists agent folders with an empty mailbox (no sent
// or received mail). Off by default — a never-messaged agent's folder is
// clutter until the operator asks for it. Roster-only: an empty mailbox
// has no messages, so unlike retired it never touches the firehose.
const SHOW_EMPTY_KEY = 'tclaude.dash.mail.showempty';
// Per-group sidebar expand state: a group row can expand to reveal its
// member agent folders nested beneath it. Keyed by group name under a
// mail-specific prefix so it stays independent of the Groups tab's own
// tclaude.dash.group.<name> card-expand flags. Default collapsed.
const GROUP_EXPAND_PREFIX = 'tclaude.dash.mail.groupexp.';

// Page sizes the selector offers. The default (50) is what a fresh
// dashboard uses until the operator picks one; every value stays at or
// below the server's maxMailboxPageSize cap (500).
const PAGE_SIZES = [25, 50, 100, 200];
const DEFAULT_PAGE_SIZE = 50;

// SEARCH_DEBOUNCE_MS delays the server reload after a keystroke so a fast
// typist fires one query, not one per character.
const SEARCH_DEBOUNCE_MS = 220;

function initialPageSize() {
  const n = parseInt(dashPrefs.getItem(PAGE_SIZE_KEY) || '', 10);
  return PAGE_SIZES.includes(n) ? n : DEFAULT_PAGE_SIZE;
}

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
// open message) is per-session. selectedMsgs (checked rows) persists
// across page navigation so a batched bulk delete can span pages (it is
// cleared only on folder switch or a search change, where the message
// universe itself changes); select-all ticks the current page into it.
// selectedBoxes (checked agent mailboxes for the bulk wipe) persists
// across folder switches so the operator can tick several folders then
// wipe.
//
// Pagination + search are server-side: messages holds only the current
// page; page/pageSize/total/totalUnfiltered come back with each fetch.
// reqSeq tokens the in-flight fetch so a slow earlier response can't
// clobber a newer page/search.
const mail = {
  mailboxes: [],
  selected: dashPrefs.getItem(SELECTED_KEY) || HUMAN_ID,
  // showRetired drives the include_retired param on both fetches. Sticky
  // (persisted) so the operator's choice survives a reload.
  showRetired: dashPrefs.getItem(SHOW_RETIRED_KEY) === '1',
  // showEmpty drives the include_empty param on the roster fetch only.
  // Sticky so the operator's choice survives a reload.
  showEmpty: dashPrefs.getItem(SHOW_EMPTY_KEY) === '1',
  messages: [],
  selectedMsgId: null,
  selectedMsgs: new Set(),
  selectedBoxes: new Set(),
  inflight: false,
  page: 1,
  pageSize: initialPageSize(),
  total: 0,
  totalUnfiltered: 0,
  reqSeq: 0,
  searchTimer: null,
  // busy is set while a batched delete/wipe runs: it freezes the 2s
  // refresh (so mail.messages isn't swapped out from under the running
  // op) and the tab's click/change handlers (so no second mutation
  // starts mid-run). progress, when set, drives the progress bar painted
  // into one of the bulk bars — {where:'bulk'|'wipe', verb, done, total}.
  busy: false,
  progress: null,
};

// pageCount derives the number of pages from the live total + page size
// (at least 1, even for an empty folder).
function pageCount() {
  return Math.max(1, Math.ceil(mail.total / mail.pageSize));
}

// currentSearch reads the live message-filter text (server-side search
// term). Trimmed so trailing spaces don't count as a query.
function currentSearch() {
  return ($('#filter-messages')?.value || '').trim();
}

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
    const params = new URLSearchParams();
    if (mail.showRetired) params.set('include_retired', '1');
    if (mail.showEmpty) params.set('include_empty', '1');
    const qs = params.toString();
    const r = await fetch(`/api/mailboxes${qs ? '?' + qs : ''}`, { credentials: 'same-origin' });
    if (!r.ok) return;
    const data = await r.json();
    mail.mailboxes = data.mailboxes || [];
  } catch { /* transient; keep the last roster painted */ }
}

async function loadMessages() {
  const id = mail.selected;
  const q = currentSearch();
  const seq = ++mail.reqSeq;
  const params = new URLSearchParams({
    id,
    q,
    page: String(mail.page),
    page_size: String(mail.pageSize),
  });
  // The "all" firehose and group folders honour include_retired
  // server-side — a group folder hides retired members' traffic by
  // default, like the firehose. Sending it for a specific agent folder is
  // harmless (a retired folder the operator opened explicitly still shows
  // all of its mail).
  if (mail.showRetired) params.set('include_retired', '1');
  try {
    const r = await fetch(`/api/mailbox?${params.toString()}`,
      { credentials: 'same-origin' });
    if (!r.ok) { if (seq === mail.reqSeq) clearMessages(); return; }
    const data = await r.json();
    // Guard against a stale response landing after the human switched
    // folders / page / search mid-flight (reqSeq is the newest request).
    if (mail.selected !== id || seq !== mail.reqSeq) return;
    mail.messages = data.messages || [];
    // Trust the server's clamped page (a stale page past the last one
    // comes back pulled to the last page after a delete).
    if (typeof data.page === 'number') mail.page = data.page;
    if (typeof data.page_size === 'number') mail.pageSize = data.page_size;
    mail.total = data.total || 0;
    mail.totalUnfiltered = data.total_unfiltered || 0;
  } catch { if (seq === mail.reqSeq) clearMessages(); }
}

// clearMessages empties the current page and its totals together, so a
// transient fetch failure doesn't leave the pager reading "Page 1 / 3"
// over an empty list. Self-heals on the next tick / reload.
function clearMessages() {
  mail.messages = [];
  mail.total = 0;
  mail.totalUnfiltered = 0;
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

// pruneSelections drops checked mailboxes that no longer exist so the
// wipe-bar count stays honest. Message selections are NOT pruned against
// the current page: selectedMsgs spans pages, and mail.messages is only
// the page in view, so pruning here would silently drop every off-page
// pick. A genuinely-deleted message left in the set is harmless — the
// batched delete no-ops it server-side — and folder / search changes
// clear the set outright.
function pruneSelections() {
  const agentIds = new Set(
    mail.mailboxes.filter(mb => mb.kind === 'agent').map(mb => mb.id));
  for (const c of [...mail.selectedBoxes]) {
    if (!agentIds.has(c)) mail.selectedBoxes.delete(c);
  }
  // Snap an orphaned folder selection back to the firehose: if the open
  // folder (agent OR group) is no longer in the roster — e.g. a retired
  // conv persisted as the selection while retired agents are hidden (the
  // initial-load twin of setShowRetired's toggle-time snap), a deleted
  // agent, or a renamed/deleted group — leaving it selected would show its
  // mail with no matching sidebar row. Checked against the WHOLE roster
  // (not just agentIds) so a valid group folder is kept. Guarded on a
  // loaded roster (the pinned all / human folders mean a real roster always
  // has entries) so a transient empty fetch can't bounce a valid selection;
  // clearMessages lets the next load fill in the firehose.
  const validFolder = mail.mailboxes.some(mb => mb.id === mail.selected);
  if (mail.mailboxes.length
      && mail.selected !== ALL_ID && mail.selected !== HUMAN_ID
      && !validFolder) {
    mail.selected = ALL_ID;
    mail.selectedMsgId = null;
    mail.selectedMsgs.clear();
    mail.page = 1;
    clearMessages();
    dashPrefs.setItem(SELECTED_KEY, ALL_ID);
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
//
// Wrapped in withPreservedFocus because this is the mail tab's single
// repaint chokepoint, and several callers reach it ASYNCHRONOUSLY —
// loadMail()/reloadMail() repaint after their fetch resolves, i.e. after
// refresh.js's own synchronous focus restore has already run. Without
// this wrap a Tab-navigating user (stepping the mailbox sidebar or the
// message list) was bounced to the top each time fresh mail data landed.
// The sidebar/list rows carry data-act + data-id, so they restore by
// signature; the static filter inputs above the panes are never rebuilt
// and keep their focus untouched.
function paintMail() {
  withPreservedFocus(() => {
    paintBulkActions();
    paintSidebar();
    paintWipeBar();
    paintList();
    paintListBulkBar();
    paintPager();
    paintReader();
  });
}

// reloadMessagesPage refetches the current folder's page (current
// search + page + size) and repaints. Used by the pager, the page-size
// selector, and the debounced search. Bypasses the inflight guard — it's
// an explicit operator action that should land promptly — but defers to a
// running bulk op, which owns mail.messages / the selection until it ends.
async function reloadMessagesPage() {
  if (mail.busy) return;
  await loadMessages();
  pruneSelections();
  paintMail();
}

// scheduleMailReload debounces a server reload after the operator types
// in the message filter.
function scheduleMailReload() {
  if (mail.searchTimer) clearTimeout(mail.searchTimer);
  mail.searchTimer = setTimeout(() => {
    mail.searchTimer = null;
    reloadMessagesPage();
  }, SEARCH_DEBOUNCE_MS);
}

// onMailSearchChanged is the message-filter hook (called by refresh.js's
// bindFilter when #filter-messages changes). A new search resets to page
// 1 and clears the selection (the search changes the message universe, so
// a cross-page selection made under the old query no longer makes sense),
// then reloads from the server — pagination has to span the whole
// filtered folder, so the filter can't be a client-side repaint. Repaints
// the bulk bar immediately so it feels responsive while the debounced
// fetch is in flight.
function onMailSearchChanged() {
  mail.page = 1;
  mail.selectedMsgs.clear();
  paintListBulkBar();
  scheduleMailReload();
}

// goToPage navigates to a 1-based page (clamped) and reloads. No-op when
// already there. The message selection persists across pages (a bulk
// delete can span them), so it is left untouched.
function goToPage(n) {
  const target = Math.min(Math.max(n, 1), pageCount());
  if (target === mail.page) return;
  mail.page = target;
  reloadMessagesPage();
}

// setPageSize switches how many messages a page holds, persists the
// choice, resets to page 1, and reloads. Selection persists (same
// messages, just regrouped into pages).
function setPageSize(n) {
  if (!PAGE_SIZES.includes(n) || n === mail.pageSize) return;
  mail.pageSize = n;
  mail.page = 1;
  dashPrefs.setItem(PAGE_SIZE_KEY, String(n));
  reloadMessagesPage();
}

// isSelectedRetired reports whether the open folder is a retired-agent
// folder, per the current roster. Used to decide whether hiding retired
// folders would strand the selection.
function isSelectedRetired() {
  const mb = mail.mailboxes.find(x => x.id === mail.selected);
  return !!(mb && mb.retired);
}

// setShowRetired flips the "show retired agents" toggle, persists it, and
// re-fetches so the roster + "all" firehose reflect the new scope. When
// hiding retired folders with one currently open, it snaps the selection
// back to the firehose first — otherwise the operator would be left
// reading a folder that just vanished from the sidebar.
function setShowRetired(on) {
  if (on === mail.showRetired || mail.busy) return;
  mail.showRetired = on;
  if (on) dashPrefs.setItem(SHOW_RETIRED_KEY, '1');
  else dashPrefs.removeItem(SHOW_RETIRED_KEY);
  if (!on && isSelectedRetired()) {
    mail.selected = ALL_ID;
    mail.selectedMsgId = null;
    mail.selectedMsgs.clear();
    mail.page = 1;
    dashPrefs.setItem(SELECTED_KEY, ALL_ID);
  }
  reloadMail();
}

// isGroupFolder reports whether the open folder is a group folder
// ("group:<name>") — an aggregate of every member's traffic, rendered
// from→to like the "all" firehose rather than relative to one agent.
function isGroupFolder() {
  return (mail.selected || '').startsWith(GROUP_PREFIX);
}

// isGroupExpanded reports whether a group row is expanded to show its
// nested member folders. Keyed by group name; default collapsed.
function isGroupExpanded(name) {
  return dashPrefs.getItem(GROUP_EXPAND_PREFIX + name) === '1';
}

// toggleGroupExpand flips a group row's expand state, persists it (a set
// '1' / removeItem pair, like the other sticky sidebar toggles), and
// repaints the sidebar so the nested member folders appear / vanish. Pure
// view state — no server round-trip; the roster already carries each
// group's member_convs.
function toggleGroupExpand(name) {
  if (!name) return;
  if (isGroupExpanded(name)) dashPrefs.removeItem(GROUP_EXPAND_PREFIX + name);
  else dashPrefs.setItem(GROUP_EXPAND_PREFIX + name, '1');
  paintSidebar();
}

// isSelectedEmpty reports whether the open folder is an empty-mailbox
// agent folder (total 0), per the current roster. Used to decide whether
// hiding empty folders would strand the selection.
function isSelectedEmpty() {
  const mb = mail.mailboxes.find(x => x.id === mail.selected);
  return !!(mb && mb.kind === 'agent' && !mb.total);
}

// setShowEmpty flips the "show agents without messages" toggle, persists
// it, and re-fetches so the roster reflects the new scope. When hiding
// empty folders with one currently open, it snaps the selection back to
// the firehose first — same stranding guard as setShowRetired. (Only the
// roster narrows; the firehose is unaffected, since an empty mailbox has
// no messages.)
function setShowEmpty(on) {
  if (on === mail.showEmpty || mail.busy) return;
  mail.showEmpty = on;
  if (on) dashPrefs.setItem(SHOW_EMPTY_KEY, '1');
  else dashPrefs.removeItem(SHOW_EMPTY_KEY);
  if (!on && isSelectedEmpty()) {
    mail.selected = ALL_ID;
    mail.selectedMsgId = null;
    mail.selectedMsgs.clear();
    mail.page = 1;
    dashPrefs.setItem(SELECTED_KEY, ALL_ID);
  }
  reloadMail();
}

function mailboxLabel(mb) {
  if (mb.kind === 'all') return 'All agent messages';
  if (mb.kind === 'human') return 'Human notifications';
  if (mb.kind === 'group') return mb.title || '(group)';
  return mb.title || shortId(mb.id) || '(unknown)';
}

function mailboxIcon(mb) {
  if (mb.kind === 'all') return '🗂';
  if (mb.kind === 'human') return '📬';
  if (mb.kind === 'group') return '👥';
  return `<span class="mail-dot ${mb.online ? 'online' : 'offline'}">●</span>`;
}

function mailboxMatchesFilter(mb, q) {
  if (!q) return true;
  return [mailboxLabel(mb), mb.id, mb.short, ...(mb.groups || [])]
    .some(s => (s || '').toLowerCase().includes(q));
}

// mailboxRowHTML renders one sidebar row — a pinned ("all"/"human"), group,
// or agent folder. nested=true marks a member-agent row shown beneath an
// expanded group: it indents and drops the bulk-wipe checkbox (the
// canonical checkbox lives on the flat Agents row for that same conv).
function mailboxRowHTML(mb, nested = false) {
  const active = mb.id === mail.selected;
  const unread = mb.unread
    ? `<span class="mailbox-unread">${mb.unread > 99 ? '99+' : mb.unread}</span>`
    : '';
  // A group folder has no per-direction tally — show member count +
  // message count instead of the agent folder's "received · sent".
  const countTitle = mb.kind === 'group'
    ? `${mb.members || 0} member${mb.members === 1 ? '' : 's'} · ${mb.total} message${mb.total === 1 ? '' : 's'}`
    : `${mb.in} received · ${mb.out} sent`;
  const count = `<span class="mailbox-count" title="${esc(countTitle)}">${mb.total}</span>`;
  // Retired folders only appear when the toggle is on; tag them so they
  // read as demoted rather than a live agent.
  const tag = mb.retired ? '<span class="mailbox-tag" title="This agent has been retired">retired</span>' : '';
  const btn = `<button class="mailbox${active ? ' active' : ''}${mb.unread ? ' has-unread' : ''}"
    data-act="mailbox-select" data-id="${esc(mb.id)}" title="${esc(mailboxLabel(mb))}">
    <span class="mailbox-icon">${mailboxIcon(mb)}</span>
    <span class="mailbox-name">${esc(mailboxLabel(mb))}</span>
    ${tag}${count}${unread}
  </button>`;
  // Lead column. A flat agent row gets the bulk-wipe checkbox; a group row
  // gets an expand caret (toggles its nested member folders); everything
  // else — the pinned folders and the nested member rows — gets a spacer so
  // every label stays aligned under the checkbox column. data-group (not
  // data-name) keys the caret so focusSignature can restore focus to it
  // across the 2s repaint.
  let lead;
  if (mb.kind === 'agent' && !nested) {
    lead = `<input type="checkbox" class="mail-box-check" data-conv="${esc(mb.id)}"${mail.selectedBoxes.has(mb.id) ? ' checked' : ''} title="Select for bulk wipe" />`;
  } else if (mb.kind === 'group') {
    const expanded = isGroupExpanded(mb.title);
    lead = `<button type="button" class="mail-group-caret" data-act="mailbox-toggle-group" data-group="${esc(mb.title)}" aria-expanded="${expanded ? 'true' : 'false'}" title="${expanded ? 'Collapse members' : 'Expand members'}">${expanded ? '▾' : '▸'}</button>`;
  } else {
    lead = '<span class="mail-box-check-spacer"></span>';
  }
  // Empty-mailbox agent folders only appear when the toggle is on; dim them
  // (like retired) so they read as low-priority opt-in clutter. The "0"
  // count is their tag, so no extra label. Retired and empty are disjoint in
  // practice (a retired folder always has the mail that put it in the
  // roster), so the two row classes never both apply.
  // NB: the modifier is `empty-box`, not `empty` — a bare `.empty` is the
  // global empty-state placeholder class (centered, 24px padding), which
  // would otherwise hijack the row's layout.
  const empty = mb.kind === 'agent' && !mb.total;
  const cls = `mailbox-row${mb.retired ? ' retired' : ''}${empty ? ' empty-box' : ''}${nested ? ' nested' : ''}`;
  return `<div class="${cls}">${lead}${btn}</div>`;
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
  // The server orders the roster [all, human, groups…, agents…]. Render by
  // explicit section rather than walking kind transitions, so a group can
  // expand INLINE into its member folders without breaking the "Agents"
  // divider that follows. A one-line divider heads the Groups and Agents
  // sections; the pinned "all"/"human" folders need none.
  const pinned = boxes.filter(mb => mb.kind === 'all' || mb.kind === 'human');
  const groups = boxes.filter(mb => mb.kind === 'group');
  const agents = boxes.filter(mb => mb.kind === 'agent');
  // Index the filtered agent folders so an expanded group nests the SAME
  // folders its members map to — selecting a nested row opens the identical
  // conv folder as the flat Agents entry. A member hidden by the retired /
  // empty / text filters simply doesn't nest, exactly as it's absent from
  // the flat list.
  const agentById = new Map(agents.map(mb => [mb.id, mb]));

  let html = pinned.map(mb => mailboxRowHTML(mb)).join('');
  if (groups.length) {
    html += '<div class="mailbox-section">Groups</div>';
    for (const g of groups) {
      html += mailboxRowHTML(g);
      if (!isGroupExpanded(g.title)) continue;
      const members = (g.member_convs || [])
        .map(id => agentById.get(id))
        .filter(Boolean);
      html += members.length
        ? members.map(mb => mailboxRowHTML(mb, true)).join('')
        : '<div class="mailbox-row nested"><span class="mail-box-check-spacer"></span>'
          + '<div class="mailbox-nested-empty">no members with messages</div></div>';
    }
  }
  if (agents.length) {
    html += '<div class="mailbox-section">Agents</div>';
    html += agents.map(mb => mailboxRowHTML(mb)).join('');
  }
  el.innerHTML = html;
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

// allSenderLabel names a row's sender in the aggregate "all" view, or ''
// when there is no real sender to name. A message with no originating conv
// (from_conv empty, so no resolvable title either) was sent by the
// human/operator — e.g. from a spawn dialog — not an agent; we render it as
// a bare "→ recipient" rather than a misleading "(unknown)". A genuinely
// unresolvable but real conv still falls back to its short conv-id, so only
// the truly-sender-less rows lose the party. Mirrors the reader pane, which
// already omits the "From" header entirely for these.
function allSenderLabel(m) {
  return m.from_title || shortId(m.from_conv);
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

function msgPreview(m) {
  if (m.subject) return m.subject;
  const firstNonBlank = (m.body || '').split('\n').find(l => l.trim() !== '');
  return firstNonBlank || '(no subject)';
}

// filteredMessages is the set of messages currently in view — with
// server-side search + pagination that is exactly the current page
// (mail.messages). Shared by the list paint, the bulk bar, and select-all
// so they agree on "in view" (= this page).
function filteredMessages() {
  return mail.messages;
}

function paintList() {
  const el = $('#mail-list');
  if (!el) return;
  const q = currentSearch();
  const filtered = mail.messages;

  // total = rows matching the search across the whole folder;
  // totalUnfiltered = rows in the folder regardless of search. Show
  // "matching / all" while searching, else a plain count.
  const total = mail.total;
  const totalUnfiltered = mail.totalUnfiltered;
  const countEl = $('#filter-messages-count');
  if (countEl) {
    countEl.textContent = q
      ? `${total} / ${totalUnfiltered}`
      : `${totalUnfiltered} message${totalUnfiltered === 1 ? '' : 's'}`;
  }

  if (!filtered.length) {
    el.innerHTML = totalUnfiltered
      ? '<div class="empty">No messages match the filter.</div>'
      : '<div class="empty">This mailbox is empty.</div>';
    return;
  }
  // Both the "all" firehose and a group folder are aggregates with no
  // single "self" to be relative to, so they render from→to rather than a
  // received/sent arrow.
  const isAggregate = mail.selected === ALL_ID || isGroupFolder();
  el.innerHTML = filtered.map(m => {
    const active = m.id === mail.selectedMsgId;
    const unread = !m.read;
    const checked = mail.selectedMsgs.has(m.id) ? ' checked' : '';
    let head;
    if (isAggregate) {
      // The firehose has no "self" to be relative to — render from→to. A
      // sender-less row (human/operator) drops the empty party and reads as a
      // bare "→ recipient" rather than "(unknown) → recipient".
      const sender = allSenderLabel(m);
      const fromHTML = sender
        ? `<span class="mail-row-party">${esc(sender)}</span>`
        : '';
      head = `${fromHTML}<span class="mail-row-arrow">→</span>
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
// action over the messages currently in view. With server-side
// pagination "in view" is this page, so select-all ticks just this page —
// but the selection persists across pages, so the operator can walk pages
// ticking more and then delete the lot in one batched op.
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
  // Agent + "all" folders gain a read/unread toggle over the selection (the
  // operator clearing several of a stuck agent's messages at once); the human
  // folder keeps its own mark-read path (the filter-bar "mark all read"), so
  // its bulk bar stays delete-only.
  const readBtns = mail.selected !== HUMAN_ID
    ? `<button data-act="mail-mark-read-selected" title="Mark the selected messages read"${n ? '' : ' disabled'}>✓ read</button>
       <button data-act="mail-mark-unread-selected" title="Mark the selected messages unread"${n ? '' : ' disabled'}>○ unread</button>`
    : '';
  bar.innerHTML = `
    <label title="Select / deselect every message on this page">
      <input type="checkbox" class="mail-select-all"${allChecked ? ' checked' : ''} /> all
    </label>
    <span class="grow">${n ? `${n} selected` : ''}</span>
    ${readBtns}
    <button class="danger" data-act="mail-del-selected" title="Delete the selected messages"${n ? '' : ' disabled'}>🗑 delete selected</button>`;
}

// paintPager renders the footer under the message list: a page-size
// selector (always shown for a non-empty folder so the operator can tune
// it) plus first/prev/«position»/next/last navigation when the folder
// spans more than one page. Hidden entirely for an empty folder.
function paintPager() {
  const bar = $('#mail-pager');
  if (!bar) return;
  if (!mail.totalUnfiltered) { bar.hidden = true; bar.innerHTML = ''; return; }
  bar.hidden = false;
  const pages = pageCount();
  const page = Math.min(mail.page, pages);
  const sizeOpts = PAGE_SIZES.map(sz =>
    `<option value="${sz}"${sz === mail.pageSize ? ' selected' : ''}>${sz}</option>`).join('');
  const sizeSel = `<label class="mail-pager-size" title="Messages per page">
      <select class="mail-page-size">${sizeOpts}</select> / page
    </label>`;
  let nav = '';
  if (pages > 1) {
    const atStart = page <= 1;
    const atEnd = page >= pages;
    nav = `
      <button data-act="mail-page-first" title="First page"${atStart ? ' disabled' : ''}>«</button>
      <button data-act="mail-page-prev" title="Previous page"${atStart ? ' disabled' : ''}>‹</button>
      <span class="mail-pager-pos">Page ${page} / ${pages}</span>
      <button data-act="mail-page-next" title="Next page"${atEnd ? ' disabled' : ''}>›</button>
      <button data-act="mail-page-last" title="Last page"${atEnd ? ' disabled' : ''}>»</button>`;
  }
  bar.innerHTML = `${nav}<span class="grow"></span>${sizeSel}`;
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
    // Agent + "all" folders: an explicit operator toggle of the row's
    // read-state (set on the recipient's behalf — repairing a stuck agent's
    // inbox), plus delete.
    const readBtn = m.read
      ? `<button data-act="mail-msg-mark-read" data-id="${m.id}" data-read="0" title="Mark this message unread for the recipient">mark unread</button>`
      : `<button data-act="mail-msg-mark-read" data-id="${m.id}" data-read="1" title="Mark this message read on the recipient’s behalf">mark read</button>`;
    const delBtn = `<button class="danger" data-act="mail-msg-delete" data-id="${m.id}" title="Permanently delete this message">delete</button>`;
    actions = `<div class="mail-reader-actions">${readBtn}${delBtn}</div>`;
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

// paintBulkActions shows the message-filter row's bulk read actions. The
// human folder gets "mark all read" / "clear read" (over human_messages); a
// single agent folder gets its own "mark all read" (marks every message that
// agent has RECEIVED read, on its behalf — clearing a stuck agent's inbox).
// The "all" firehose gets neither: "mark all read" across every conv's
// traffic is not a meaningful operator action.
function paintBulkActions() {
  const human = mail.selected === HUMAN_ID;
  // A group folder is an aggregate, not a single agent's inbox — exclude it
  // from agentFolder so it never shows the per-folder "mark all read" (that
  // marks one conv's received mail; a group has no single recipient).
  const agentFolder = !human && mail.selected !== ALL_ID && !isGroupFolder();
  const markAll = $('#mail-mark-all');
  const clearRead = $('#mail-clear-read');
  const agentMarkAll = $('#mail-agent-mark-all');
  if (markAll) markAll.hidden = !human;
  if (clearRead) clearRead.hidden = !human;
  if (agentMarkAll) agentMarkAll.hidden = !agentFolder;
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
  mail.page = 1;              // a new folder starts at its first page
  mail.total = 0;
  mail.totalUnfiltered = 0;
  if (mail.searchTimer) { clearTimeout(mail.searchTimer); mail.searchTimer = null; }
  dashPrefs.setItem(SELECTED_KEY, id);
  paintMail();        // immediate feedback (active folder, empty list)
  loadMessages().then(() => { pruneSelections(); paintMail(); });
}

function selectMessage(id) {
  mail.selectedMsgId = Number(id);
  paintList();        // re-highlight the active row
  paintReader();
}

// openMailbox brings the Messages tab forward and selects a folder — the
// deep-link target for the Groups-tab cog menus' "view messages" items (an
// agent's conv-id, or "group:<name>"). The synthetic nav click activates
// the tab (bindTabs) and fires renderMailTab.
//
// The roster is refreshed (awaited) BEFORE selectMailbox so the target
// folder is present when selectMailbox → pruneSelections runs: a deep link
// from the Groups tab can target a folder the Messages-tab roster hasn't
// loaded since it last changed (e.g. a group created while the operator was
// on the Groups tab), and pruneSelections snaps any selection not in the
// cached roster back to "all" — which would bounce the deep link. A failed
// roster refresh leaves the cache as-is; selectMailbox still loads the
// folder directly (the server resolves it regardless of the roster).
async function openMailbox(id) {
  if (!id) return;
  const navBtn = $('nav button[data-tab="messages"]');
  if (navBtn) navBtn.click();
  await loadMailboxes();
  selectMailbox(id);
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
  // Snapshot the selection up front: runBatches iterates this captured
  // list, not the live Set, so a background refresh landing during the
  // confirm wait (before mail.busy is set) can prune the Set without
  // affecting the in-flight op — the per-batch delete just no-ops.
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

// setMessagesRead marks the given message ids read (read=true) or unread
// (read=false) on the recipient's behalf — the operator repairing a stuck
// agent's inbox read-state. Non-destructive and reversible, so no confirm.
// Reloads so the unread dots + the sidebar badge update.
//
// Unlike deleteSelectedMessages this sends the whole selection in one
// request rather than batching: a mark is a single cheap UPDATE (no
// per-row work to watch advance), and the server's 256KB body cap bounds
// the id list well above any realistic selection (~21k ids) — an
// over-large body just 4xx's and surfaces as a toast. No progress bar, so
// mail.busy is intentionally left unset (matching deleteOneMessage).
async function setMessagesRead(ids, read) {
  ids = ids.filter(Number.isFinite);
  if (mail.busy || !ids.length) return;
  const verb = read ? 'read' : 'unread';
  try {
    const r = await fetch('/api/mailbox/mark-read', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids, read }),
    });
    if (!r.ok) { toast(`mark ${verb} failed: ${await r.text()}`, true); return; }
    const res = await r.json().catch(() => ({}));
    const n = res.marked || 0;
    toast(`marked ${n} message${n === 1 ? '' : 's'} ${verb}`);
    await reloadMail();
  } catch (err) {
    toast(`mark ${verb} failed: ${(err && err.message) || err}`, true);
  }
}

// markAllAgentRead marks every still-unread message the selected agent has
// RECEIVED as read — the per-folder "mark all read" for a stuck agent. Only
// valid for a single agent folder (not the human or "all" virtual folders).
async function markAllAgentRead() {
  const conv = mail.selected;
  if (mail.busy || conv === HUMAN_ID || conv === ALL_ID) return;
  try {
    const r = await fetch('/api/mailbox/mark-read', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ conv, read: true }),
    });
    if (!r.ok) { toast(`mark all read failed: ${await r.text()}`, true); return; }
    const res = await r.json().catch(() => ({}));
    const n = res.marked || 0;
    toast(`marked ${n} message${n === 1 ? '' : 's'} read`);
    await reloadMail();
  } catch (err) {
    toast(`mark all read failed: ${(err && err.message) || err}`, true);
  }
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
      } else if (act === 'mailbox-toggle-group') {
        toggleGroupExpand(btn.getAttribute('data-group'));
      } else if (act === 'mail-open') {
        selectMessage(btn.getAttribute('data-id'));
      } else if (act === 'mail-msg-delete') {
        deleteOneMessage(Number(btn.getAttribute('data-id')));
      } else if (act === 'mail-msg-mark-read') {
        setMessagesRead([Number(btn.getAttribute('data-id'))],
          btn.getAttribute('data-read') === '1');
      } else if (act === 'mail-mark-read-selected') {
        setMessagesRead([...mail.selectedMsgs], true);
      } else if (act === 'mail-mark-unread-selected') {
        setMessagesRead([...mail.selectedMsgs], false);
      } else if (act === 'mail-agent-mark-all') {
        markAllAgentRead();
      } else if (act === 'mail-del-selected') {
        deleteSelectedMessages();
      } else if (act === 'mail-wipe-selected') {
        wipeSelectedMailboxes();
      } else if (act === 'mail-clear-box-sel') {
        mail.selectedBoxes.clear();
        paintSidebar();
        paintWipeBar();
      } else if (act === 'mail-page-first') {
        goToPage(1);
      } else if (act === 'mail-page-prev') {
        goToPage(mail.page - 1);
      } else if (act === 'mail-page-next') {
        goToPage(mail.page + 1);
      } else if (act === 'mail-page-last') {
        goToPage(pageCount());
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
      } else if (t.classList.contains('mail-page-size')) {
        setPageSize(Number(t.value));
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
  // "Show retired agents" sidebar toggle. A dedicated listener (not the
  // delegated tab handler) so a mid-bulk-op click stays in sync: it
  // reverts the box to the live state and no-ops rather than the delegated
  // handler's silent early-return, which would leave the box visually
  // toggled but the state unchanged. The checkbox is static (never
  // repainted), so its checked state is the source of truth between
  // toggles; seed it from the persisted pref.
  const showRetired = $('#mail-show-retired');
  if (showRetired) {
    showRetired.checked = mail.showRetired;
    showRetired.addEventListener('change', () => {
      if (mail.busy) { showRetired.checked = mail.showRetired; return; }
      setShowRetired(showRetired.checked);
    });
  }
  // "Show agents without messages" sidebar toggle — the empty-mailbox twin
  // of the retired toggle above; same mid-bulk-op resync rationale.
  const showEmpty = $('#mail-show-empty');
  if (showEmpty) {
    showEmpty.checked = mail.showEmpty;
    showEmpty.addEventListener('change', () => {
      if (mail.busy) { showEmpty.checked = mail.showEmpty; return; }
      setShowEmpty(showEmpty.checked);
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

export { renderMailTab, initMail, onMailSearchChanged, openMailbox };
