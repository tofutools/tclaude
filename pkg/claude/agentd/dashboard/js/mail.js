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
//                             Predecessor ("previous") generations of an
//                             actor — past conv-ids left behind by a
//                             reincarnate / Claude Code /clear, recovered
//                             from the snapshot's replaced[] (the same
//                             source the Groups tab uses) — are hidden from
//                             the agent listing, flat AND nested, until the
//                             #mail-show-prev-gens footer toggle opts them
//                             in. Purely a client-side roster filter: it
//                             hides only the left-pane folders, never any
//                             message — the "all" firehose still counts a
//                             predecessor's traffic.
//   list    (#mail-list)    → the selected folder's messages, newest
//                             first, filtered by #filter-messages. Each
//                             row has a select checkbox and a delete
//                             button; #mail-bulk-bar drives select-all +
//                             "delete selected".
//   reader  (#mail-reader)  → the selected message's headers + body,
//                             plus per-folder actions (human folder:
//                             mark-read/unread toggle + focus — opening a
//                             notification auto-marks it read, so the
//                             toggle mostly offers the "mark unread"
//                             opt-out; agent + "all" folders:
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
// array for multi-select). The reader's human-only mark-read / mark-unread
// / focus actions still flow through row-actions.js's document handler;
// the auto-mark-on-open (markOpenedHumanRead) posts the same read endpoint
// directly, since it's a selection side effect rather than a button.
//
// Bulk delete/wipe is split into many small batched requests (see
// runBatches) rather than one giant call: a progress bar fills in the
// bulk bar as each batch lands, and mail.busy freezes the refresh +
// handlers for the duration so nothing races the running op.

import { $, $$, shortId, shortAgentId, idTooltip } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { initMailResize } from './mail-resize.js';
// lastSnapshot lives in dashboard.js; confirmModal/toast live in
// refresh.js. Both are benign, TDZ-safe import cycles (see tabs.js):
// nothing here reads them at module top level — only inside handlers —
// and refresh.js's confirmModal/toast are hoisted function declarations.
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast } from './refresh.js';
import { fetchListFull } from './list-paging.js';
import { suppressNextMessagesAttention } from './mail-bridge.js';
import {
  adjacentAttentionPages, createMailState, HUMAN_MAILBOX_ID as HUMAN_ID,
  messageDeleteEndpoint, nextMessagesAttention, prepareMessagesAttention,
} from './mail-state.js';

const ALL_ID = 'all';
// The synthetic "access requests" folder — in-flight human-approval requests
// (an agent blocked waiting for the operator to approve/deny a permission-gated
// action). Unlike every other folder it has NO server mailbox: its rows are the
// live lastSnapshot.access_requests, and its decisions POST to
// /api/access-requests/{id}/decision. It replaces the old loopback browser
// popup, so approvals work through the (possibly remote) dashboard.
const ACCESS_ID = 'access-requests';
// accessHighlightId is a one-shot deep-link target (?access_request=<id>): the
// card with this id gets an .attn pulse + scrollIntoView on the next paint,
// then it's cleared so the highlight doesn't stick across ticks.
let accessHighlightId = null;
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
// Whether the sidebar lists folders for predecessor ("previous")
// generations of an actor — past conv-ids left behind by a reincarnate /
// /clear. Off by default — these are archival; their folder is clutter
// until the operator asks for it. Purely a client-side roster filter
// (derived from the snapshot's replaced[]): it hides only the left-pane
// folder, never any message — the "all" firehose still counts the traffic.
const SHOW_PREV_GENS_KEY = 'tclaude.dash.mail.showprevgens';
// Per-group sidebar expand state: a group row can expand to reveal its
// member agent folders nested beneath it. Keyed by group name under a
// mail-specific prefix so it stays independent of the Groups tab's own
// tclaude.dash.group.<name> card-expand flags. Default collapsed.
const GROUP_EXPAND_PREFIX = 'tclaude.dash.mail.groupexp.';
// Whether the flat "All agent mailboxes" section is expanded. The per-agent
// roster — every row carrying a bulk-wipe checkbox — is collapsed by default
// so the sidebar opens clean and the destructive wipe affordance is opt-in;
// grouped agents stay reachable by expanding their group above. Persisted;
// default folded. A live text filter force-expands it regardless (matches
// must never hide behind the fold).
const AGENTS_EXPAND_KEY = 'tclaude.dash.mail.agentsexp';

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
// page; page/pageSize/total/totalUnfiltered come back with each fetch. The
// shared request lifecycle rejects stale responses.
export const mailState = createMailState({
  mailboxes: [],
  selected: dashPrefs.getItem(SELECTED_KEY) || HUMAN_ID,
  boxQuery: dashPrefs.getItem(BOX_FILTER_KEY) || '',
  messageQuery: dashPrefs.getItem('tclaude.dash.filter.messages') || '',
  // showRetired drives the include_retired param on both fetches. Sticky
  // (persisted) so the operator's choice survives a reload.
  showRetired: dashPrefs.getItem(SHOW_RETIRED_KEY) === '1',
  // showEmpty drives the include_empty param on the roster fetch only.
  // Sticky so the operator's choice survives a reload.
  showEmpty: dashPrefs.getItem(SHOW_EMPTY_KEY) === '1',
  // showPrevGens governs whether predecessor-generation folders appear in
  // the agent listing. Client-side roster filter (no server param) — the
  // sidebar paint reads it; the snapshot's replaced[] names the convs.
  // Sticky so the operator's choice survives a reload.
  showPrevGens: dashPrefs.getItem(SHOW_PREV_GENS_KEY) === '1',
  // The set of predecessor-generation conv-ids, kept warm from /api/replaced
  // (the snapshot no longer ships the full replaced[]). See loadPrevGenIds.
  prevGenIds: new Set(),
  messages: [],
  selectedMsgId: null,
  selectedMsgs: new Set(),
  selectedBoxes: new Set(),
  inflight: false,
  page: 1,
  pageSize: initialPageSize(),
  total: 0,
  totalUnfiltered: 0,
  searchTimer: null,
  // busy is set while a batched delete/wipe runs: it freezes the 2s
  // refresh (so mail.messages isn't swapped out from under the running
  // op) and the tab's click/change handlers (so no second mutation
  // starts mid-run). progress, when set, drives the progress bar painted
  // into one of the bulk bars — {where:'bulk'|'wipe', verb, done, total}.
  busy: false,
  progress: null,
});
const mail = mailState.data;

// pageCount derives the number of pages from the live total + page size
// (at least 1, even for an empty folder).
function pageCount() {
  return Math.max(1, Math.ceil(mail.total / mail.pageSize));
}

// currentSearch reads the live message-filter text (server-side search
// term). Trimmed so trailing spaces don't count as a query.
function currentSearch() {
  return (mail.messageQuery || '').trim();
}

function setBoxQuery(value) {
  mail.boxQuery = String(value ?? '');
  if (mail.boxQuery) dashPrefs.setItem(BOX_FILTER_KEY, mail.boxQuery);
  else dashPrefs.removeItem(BOX_FILTER_KEY);
  paintSidebar();
}

function setMessageQuery(value) {
  mail.messageQuery = String(value ?? '');
  const key = 'tclaude.dash.filter.messages';
  if (mail.messageQuery) dashPrefs.setItem(key, mail.messageQuery);
  else dashPrefs.removeItem(key);
  onMailSearchChanged();
}

function mailTabActive() {
  const sec = $('#tab-messages');
  return !!sec && sec.classList.contains('active');
}

// onlineConvs is the set of conv-ids with a live tmux window — focus is
// only meaningful for those. Reads the passed snapshot, defaulting to the
// one already in memory (lastSnapshot), so callers that hold a FRESHER snapshot
// than lastSnapshot — e.g. the reply dialog's local data-only poll — can pass it
// in and get current liveness without a global write.
function onlineConvs(snap) {
  const set = new Set();
  snap = snap || lastSnapshot || {};
  (snap.agents || []).concat(snap.ungrouped || [])
    .forEach(a => { if (a && a.online) set.add(a.conv_id); });
  return set;
}

// onlineAgents is the set of stable agent_ids with a live tmux window —
// the rotation-immune companion to onlineConvs. A sender that has
// reincarnated since writing a message keeps the same agent_id while its
// conv_id (the snapshot in from_conv) points at a now-dead generation, so
// liveness must be checked against the actor, not the stale conv. Rows
// without an agent_id (e.g. ungrouped raw convs) simply don't contribute.
function onlineAgents(snap) {
  const set = new Set();
  snap = snap || lastSnapshot || {};
  (snap.agents || []).concat(snap.ungrouped || [])
    .forEach(a => { if (a && a.online && a.agent_id) set.add(a.agent_id); });
  return set;
}

// senderOnline reports whether the party that sent a human-folder message
// has a live tmux window, keyed the SAME way humanFocusButton picks its
// focus target: lead with the rotation-immune agent_id (valid across
// reincarnation) and fall back to the from_conv snapshot when the sender
// never became an actor. Exported so the reply dialog gates on the
// identical liveness signal the focus/reply buttons render from — one
// source of truth for "can this agent receive a reply". `snap` defaults to
// lastSnapshot; the reply dialog passes its own freshly-polled snapshot so its
// indicator is independent of the main render/commit cadence.
function senderOnline(fromAgent, fromConv, snap) {
  return fromAgent
    ? onlineAgents(snap).has(fromAgent)
    : onlineConvs(snap).has(fromConv);
}

// prevGenConvSet is the set of conv-ids that are PREDECESSOR (replaced)
// generations of a still-existing actor — a reincarnate / Claude Code
// /clear advanced the actor's live pointer and left these behind (JOH-26).
// The full set now comes from /api/replaced (the snapshot only ships one
// page); loadPrevGenIds keeps mail.prevGenIds warm while the Messages tab is
// open. Returns an empty Set until that first fetch lands — fail-open, showing
// every folder until we actually know which are stale (matching the prior
// snapshot-derived behaviour).
function prevGenConvSet() {
  return mail.prevGenIds || new Set();
}

// --- data loading ---------------------------------------------------

async function loadMailboxes() {
  const token = mailState.mailboxRequest.beginRequest();
  try {
    const params = new URLSearchParams();
    if (mail.showRetired) params.set('include_retired', '1');
    if (mail.showEmpty) params.set('include_empty', '1');
    const qs = params.toString();
    const r = await fetch(`/api/mailboxes${qs ? '?' + qs : ''}`, { credentials: 'same-origin' });
    if (!r.ok) throw new Error((await r.text()) || `mailboxes request failed (${r.status})`);
    const data = await r.json();
    mailState.mailboxRequest.commitRequest(token, data);
  } catch (error) {
    mailState.mailboxRequest.failRequest(token, error);
  }
}

// prevGenFetchedAt throttles the full /api/replaced pull (see loadPrevGenIds).
let prevGenFetchedAt = 0;

// loadPrevGenIds keeps mail.prevGenIds (the predecessor-generation conv-ids the
// "hide previous generations" folder filter uses) warm. Predecessor convs only
// change on reincarnate / /clear, and the full replaced list would be heavy to
// pull every 2s — so this fetches at most every 15s while the tab is open
// (force=true on a mutation, which can change the set immediately). Keeps the
// last set on a transient failure.
async function loadPrevGenIds(force) {
  // Gate on prevGenFetchedAt (have we fetched at all), NOT on prevGenIds.size —
  // an empty replaced set is the common "nothing reincarnated yet" case, and
  // gating on .size there would defeat the throttle and refetch every 2s.
  if (!force && prevGenFetchedAt && Date.now() - prevGenFetchedAt < 15000) return;
  try {
    const rows = await fetchListFull('replaced');
    mail.prevGenIds = new Set(rows.map(r => r.conv_id).filter(Boolean));
    prevGenFetchedAt = Date.now();
  } catch { /* keep the last cached set */ }
}

async function loadMessages() {
  const id = mail.selected;
  // The access-requests folder renders from the live snapshot, not from
  // /api/mailbox — there are no stored rows to page. Skip the fetch so it
  // doesn't 404 on a mailbox that doesn't exist server-side.
  const token = mailState.messageRequest.beginRequest();
  if (id === ACCESS_ID) {
    mailState.messageRequest.commitRequest(token, { messages: [], total: 0, total_unfiltered: 0 });
    return;
  }
  const q = currentSearch();
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
    if (!r.ok) throw new Error((await r.text()) || `mailbox request failed (${r.status})`);
    const data = await r.json();
    // The request lifecycle rejects stale page/search responses; this guard
    // covers a response whose mailbox was switched while it was in flight.
    if (mail.selected !== id) return;
    mailState.messageRequest.commitRequest(token, data);
  } catch (error) {
    if (mailState.messageRequest.failRequest(token, error)) clearMessages();
  }
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
    await Promise.all([loadMailboxes(), loadMessages(), loadPrevGenIds()]);
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
  await Promise.all([loadMailboxes(), loadMessages(), loadPrevGenIds(true)]);
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
  // While "show previous generations" is off, predecessor folders have no
  // visible row but DO stay in the server roster (the filter is client-side),
  // so the agentIds check alone won't drop a wipe selection on one. Compute the
  // hidden-predecessor set once and use it both to prune those box selections
  // and to snap an open predecessor folder below.
  const hiddenPrev = mail.showPrevGens ? null : prevGenConvSet();
  for (const c of [...mail.selectedBoxes]) {
    // Drop a checked mailbox that left the roster, OR a predecessor that is
    // currently hidden — e.g. a live agent ticked for wipe that then
    // reincarnated / cleared on a background refresh, entering replaced[]:
    // leaving it checked would let the wipe-bar count (and a wipe touch) a row
    // the operator can no longer see. The live-refresh twin of
    // setShowPrevGens's toggle-time cleanup.
    if (!agentIds.has(c) || (hiddenPrev && hiddenPrev.has(c))) mail.selectedBoxes.delete(c);
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
  //
  // A predecessor folder still lives in the server roster (the prev-gens
  // filter is client-side), so it stays a "valid" folder — but while "show
  // previous generations" is off it has no visible sidebar row, so a
  // persisted / deep-linked selection on one would strand the same way. Treat
  // a hidden predecessor as not-visible here (the initial-load twin of
  // setShowPrevGens's toggle-time snap). Reuses the hiddenPrev set computed
  // above (null when the toggle is on — nothing is hidden).
  const hiddenPrevGen = !!hiddenPrev && hiddenPrev.has(mail.selected);
  const validFolder = mail.mailboxes.some(mb => mb.id === mail.selected) && !hiddenPrevGen;
  if (mail.mailboxes.length
      && mail.selected !== ALL_ID && mail.selected !== HUMAN_ID && mail.selected !== ACCESS_ID
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

// renderMailTab is the entry the refresh loop calls every 2s and the Messages
// island calls when dashboardState activates its tab: repaint from cache
// (cheap, keeps the filter responsive), and pull fresh data when the tab is
// actually being viewed.
function renderMailTab() {
  paintMail();
  if (mailTabActive()) loadMail();
}

// paintMail repaints all panes from cached state, applying the current
// filters. Sync — used by the filter inputs and after selection changes,
// with no server round-trip.
//
// Messages are Preact-owned, so keyed rows retain focus while state publishes.
function paintMail() {
  mailState.touch();
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

// onMailSearchChanged is the message-filter hook. A new search resets to page
// 1 and clears the selection (the search changes the message universe, so
// a cross-page selection made under the old query no longer makes sense),
// then reloads from the server — pagination has to span the whole
// filtered folder, so the filter can't be a client-side repaint. Repaints
// the bulk bar immediately so it feels responsive while the debounced
// fetch is in flight.
function onMailSearchChanged() {
  // Compatibility for callers that still invoke this hook directly.
  const input = $('#filter-messages');
  if (input) mail.messageQuery = input.value;
  mail.page = 1;
  mail.selectedMsgs.clear();
  paintListBulkBar();
  if (mail.selected === ACCESS_ID) {
    paintList();
    return;
  }
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
  mailState.touch();
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

// isAgentsExpanded reports whether the flat "All agent mailboxes" section is
// expanded to reveal the per-agent roster. Default folded.
function isAgentsExpanded() {
  return dashPrefs.getItem(AGENTS_EXPAND_KEY) === '1';
}

// toggleAgentsExpand flips the flat agent section's expand state, persists it
// (set '1' / removeItem, like the other sticky sidebar toggles), and repaints.
// Folding also drops any pending bulk-wipe selection: the checkboxes that
// drive it are about to vanish, so leaving the wipe bar counting now-hidden
// rows would be a trap. Pure view state — no server round-trip.
function toggleAgentsExpand() {
  if (isAgentsExpanded()) {
    dashPrefs.removeItem(AGENTS_EXPAND_KEY);
    mail.selectedBoxes.clear();
  } else {
    dashPrefs.setItem(AGENTS_EXPAND_KEY, '1');
  }
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
  mailState.touch();
  reloadMail();
}

// setShowPrevGens flips the "show previous generations" toggle and persists
// it. Unlike setShowRetired / setShowEmpty this is a pure client-side roster
// filter — the predecessor convs are already in the cached roster + snapshot,
// so there is no server param to re-fetch; it just repaints. When hiding
// predecessor folders it first drops any from the wipe selection (so the
// wipe-bar count can't reference a row that's no longer visible) and snaps a
// predecessor folder that happens to be open back to the firehose — the same
// stranding guard the other two toggles apply.
function setShowPrevGens(on) {
  if (on === mail.showPrevGens || mail.busy) return;
  mail.showPrevGens = on;
  if (on) dashPrefs.setItem(SHOW_PREV_GENS_KEY, '1');
  else dashPrefs.removeItem(SHOW_PREV_GENS_KEY);
  if (!on) {
    const prev = prevGenConvSet();
    for (const c of [...mail.selectedBoxes]) {
      if (prev.has(c)) mail.selectedBoxes.delete(c);
    }
    if (prev.has(mail.selected)) {
      mail.selected = ALL_ID;
      mail.selectedMsgId = null;
      mail.selectedMsgs.clear();
      mail.page = 1;
      dashPrefs.setItem(SELECTED_KEY, ALL_ID);
      // The open folder changed (predecessor → firehose), so its messages
      // must be re-fetched; loadMessages repaints when it lands.
      loadMessages().then(() => { pruneSelections(); paintMail(); });
      mailState.touch();
      return;
    }
  }
  paintMail();
}

function mailboxView() {
  const q = (mail.boxQuery || '').toLowerCase();
  const boxes = mail.mailboxes.filter(mb => mailboxMatchesFilter(mb, q));
  const pinned = boxes.filter(mb => mb.kind === 'all' || mb.kind === 'human');
  const access = accessRequestsMailbox();
  if (mailboxMatchesFilter(access, q)) pinned.push(access);
  const groups = boxes.filter(mb => mb.kind === 'group');
  const prevGens = prevGenConvSet();
  const agents = boxes.filter(mb => mb.kind === 'agent' && (mail.showPrevGens || !prevGens.has(mb.id)));
  const agentByID = new Map(agents.map(mb => [mb.id, mb]));
  return {
    empty: boxes.length === 0,
    hasRoster: mail.mailboxes.length > 0,
    pinned,
    groups: groups.map(group => ({
      mailbox: group,
      expanded: isGroupExpanded(group.title),
      members: (group.member_convs || []).map(id => agentByID.get(id)).filter(Boolean),
    })),
    agents,
    prevGens,
    filtering: !!q,
    agentsExpanded: !!q || isAgentsExpanded() || mail.selectedBoxes.size > 0,
  };
}

function toggleBoxSelection(conv, checked) {
  if (mail.busy || !conv) return;
  if (checked) mail.selectedBoxes.add(conv); else mail.selectedBoxes.delete(conv);
  mailState.touch();
}

function clearBoxSelection() {
  mail.selectedBoxes.clear();
  mailState.touch();
}

function mailboxLabel(mb) {
  if (mb.kind === 'all') return wz('All agent messages', 'All ravens');
  if (mb.kind === 'human') return wz('Human notifications', 'The Archmage');
  if (mb.kind === 'access-requests') return wz('Access requests', 'Petitions');
  if (mb.kind === 'group') return mb.title || '(group)';
  // A nameless agent folder leads with its stable agt_ handle (shortAgentId),
  // falling back to the short conv-id prefix only when no agent_id is known.
  return mb.title || shortAgentId(mb.agent_id, mb.id) || '(unknown)';
}

// mailboxTitleAttr is the folder row's hover tooltip. For an agent folder it
// appends the full "agent_id / conv-id" pair to the label, so the stable
// handle (and the conv it currently rides) is readable/copyable off the
// sidebar without losing the full name on hover. Other folders just hover
// their label.
function mailboxTitleAttr(mb) {
  if (mb.kind === 'agent') {
    const ids = idTooltip(mb.agent_id, mb.id);
    return ids ? `${mailboxLabel(mb)} — ${ids}` : mailboxLabel(mb);
  }
  return mailboxLabel(mb);
}

function mailboxMatchesFilter(mb, q) {
  if (!q) return true;
  return [mailboxLabel(mb), mb.id, mb.short, mb.agent_id, ...(mb.groups || [])]
    .some(s => (s || '').toLowerCase().includes(q));
}

// The Preact sidebar row uses these label/filter helpers for pinned
// ("all"/"human"), group, agent, nested-member, and predecessor-generation
// folders. The controller retains the mailbox rules while the island owns
// the markup and DOM lifecycle.
function paintSidebar() { mailState.touch(); }

// paintWipeBar shows the "wipe selected mailboxes" bar when one or more
// agent folders are ticked in the sidebar.
function paintWipeBar() { mailState.touch(); }

// HUMAN_SENDER labels mail the human/operator sent to an agent. Those rows
// carry no from_conv, so before the backend flagged them (operator_authored)
// they rendered party-less and read as sender-less system mail — in the
// aggregate "all" folder they were effectively invisible as human mail.
const HUMAN_SENDER = 'human operator';

// senderLabel names a row's sender, or '' when there is no real sender to
// name. Operator-authored mail is named explicitly. A message with no
// originating conv and no operator marker is an internal system handoff, not
// an agent: it returns '' so the caller drops the party rather than showing
// a misleading "(unknown)". A genuinely unresolvable but real conv still
// falls back to its short conv-id. Shared by the aggregate + per-agent rows
// and the reader's "From" header so all three agree.
function senderLabel(m) {
  if (m.operator_authored) return wz(HUMAN_SENDER, 'the Archmage');
  return m.from_title || shortAgentId(m.from_agent, m.from_conv);
}

// counterparty returns the name to show in a non-aggregate message-list
// row — the OTHER party relative to the selected mailbox. For a received
// message that's the sender; for a sent one, the recipient.
function counterparty(m) {
  if (m.direction === 'out') {
    return m.to_title || shortAgentId(m.to_agent, m.to_conv) || '(unknown)';
  }
  return senderLabel(m);
}

// allSenderLabel names a row's sender in the aggregate "all" view.
function allSenderLabel(m) {
  return senderLabel(m);
}

// allRecipientLabel names a row's recipient in the aggregate "all" view,
// collapsing a multicast to "first +N".
function allRecipientLabel(m) {
  if (m.to_title) return m.to_title;
  if (m.to_conv) return shortAgentId(m.to_agent, m.to_conv);
  const rs = m.to_recipients || [];
  if (rs.length) {
    const first = rs[0].title || shortAgentId(rs[0].agent_id, rs[0].conv_id);
    return rs.length > 1 ? `${first} +${rs.length - 1}` : first;
  }
  return '(group)';
}

function msgPreview(m) {
  if (m.subject) return m.subject;
  const firstNonBlank = (m.body || '').split('\n').find(l => l.trim() !== '');
  return firstNonBlank || '(no subject)';
}

// msgKind classifies a message into one of the wizard-theme "scroll kinds",
// which style the list row + reader per correspondence type. Purely cosmetic
// and theme-agnostic: the data-kind is emitted in EVERY theme, and only the
// body.wizard CSS reacts to it (regular / slop modes ignore it), matching how
// the rest of the wizard re-skin is a pure CSS opt-in over unchanged markup.
//   decree       — the operator/Archmage channel (the Human folder). Regal
//                  arcane slab; the human's word reads as a royal decree.
//   proclamation — a multicast addressed to many. A weathered broadsheet.
//   reply        — a threaded reply (carries a parent). A continued scroll.
//   raven        — the default 1:1 agent-to-agent missive. Plain parchment.
// Precedence: decree > proclamation > reply > raven. Decree covers the whole
// Human folder (agent → human) plus any operator-authored row wherever it
// surfaces, so the human's word reads the same in the "all" firehose and in
// an agent's own folder as it does in the Human folder.
function msgKind(m) {
  if (mail.selected === HUMAN_ID || m.operator_authored) return 'decree';
  if ((m.to_recipients || []).length > 1) return 'proclamation';
  if (m.parent_id) return 'reply';
  return 'raven';
}

// wz picks wizard-voice copy when the 🧙 theme is on, else the plain string.
// The mail panes are repainted on every tclaude:wizard flip (see initMail), so
// the copy tracks the live theme without a data refresh.
function wz(plain, wizardly) {
  return document.body.classList.contains('wizard') ? wizardly : plain;
}

// applyMailThemeText swaps the two mail-filter placeholders to match the live
// theme. The empty-state + reader strings are chosen at paint time via wz();
// these placeholders live on static inputs that are never rebuilt, so they're
// set imperatively here (called from initMail + on every tclaude:wizard flip).
// filteredMessages is the set of messages currently in view — with
// server-side search + pagination that is exactly the current page
// (mail.messages). Shared by the list paint, the bulk bar, and select-all
// so they agree on "in view" (= this page).
function filteredMessages() {
  return mail.messages;
}

function messageView() {
  const search = currentSearch();
  if (mail.selected === ACCESS_ID) {
    const all = accessRequests();
    const visible = all.filter(request => accessMatchesSearch(request, search));
    return {
      access: true, allAccess: all, pendingAccess: visible.filter(accessIsPending),
      handledAccess: visible.filter(request => !accessIsPending(request)), search,
    };
  }
  return {
    access: false,
    messages: mail.messages,
    search,
    isAggregate: mail.selected === ALL_ID || isGroupFolder(),
    pages: pageCount(),
  };
}

function messageCountText() {
  const model = messageView();
  if (model.access) {
    const parts = [model.pendingAccess.length ? `${model.pendingAccess.length} pending` : wz('none pending', 'no petitions')];
    if (model.handledAccess.length) parts.push(`${model.handledAccess.length} handled`);
    if (model.search) parts.push(`${model.pendingAccess.length + model.handledAccess.length} / ${model.allAccess.length}`);
    return parts.join(' · ');
  }
  return model.search
    ? `${mail.total} / ${mail.totalUnfiltered}`
    : `${mail.totalUnfiltered} message${mail.totalUnfiltered === 1 ? '' : 's'}`;
}

function toggleMessageSelection(id, checked) {
  id = Number(id);
  if (mail.busy || !Number.isFinite(id)) return;
  if (checked) mail.selectedMsgs.add(id); else mail.selectedMsgs.delete(id);
  mailState.touch();
}

function togglePageSelection(checked) {
  if (mail.busy) return;
  for (const message of filteredMessages()) {
    if (checked) mail.selectedMsgs.add(message.id); else mail.selectedMsgs.delete(message.id);
  }
  mailState.touch();
}

function paintList() { mailState.touch(); }

// paintListBulkBar drives the select-all checkbox + "delete selected"
// action over the messages currently in view. With server-side
// pagination "in view" is this page, so select-all ticks just this page —
// but the selection persists across pages, so the operator can walk pages
// ticking more and then delete the lot in one batched op.
function paintListBulkBar() { mailState.touch(); }

// paintPager renders the footer under the message list: a page-size
// selector (always shown for a non-empty folder so the operator can tune
// it) plus first/prev/«position»/next/last navigation when the folder
// spans more than one page. Hidden entirely for an empty folder.
function paintPager() { mailState.touch(); }

function paintReader() { mailState.touch(); }

// --- selection ------------------------------------------------------

function selectMailbox(id) {
  if (mail.busy) return Promise.resolve(false);
  if (!id || id === mail.selected) {
    // Re-click on the active folder: just refresh it.
    return loadMessages().then(() => {
      pruneSelections();
      paintMail();
      return !!id && mail.selected === id;
    });
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
  return loadMessages().then(() => {
    pruneSelections();
    paintMail();
    return mail.selected === id;
  });
}

function selectMessage(id) {
  mail.selectedMsgId = mail.selected === ACCESS_ID ? String(id || '') : Number(id);
  // Opening a human notification marks it read, the way a mail client
  // does — the human is now looking at it. Scoped to the human folder:
  // its read-state means "the human has seen this". Agent + "all" folders
  // keep read-state as the operator's explicit inbox-repair toggle (set on
  // a stuck agent's behalf), so merely opening one there must NOT flip it.
  if (mail.selected === HUMAN_ID) markOpenedHumanRead(mail.selectedMsgId);
  paintReader();      // re-highlight the row and repaint the reader
}

// markOpenedHumanRead implements "opening it reads it" for the human
// folder. Optimistic: it flips the local row synchronously so
// selectMessage's repaint right after already shows it read (no flash from
// a full reload), then persists in the background and refreshes just the
// sidebar's unread badge — the open message + list page stay put. Silent
// (no toast): it fires on every open, and the reader already reflects the
// new state; the nav tab badge reconciles on the next 2s tick. A no-op for
// an already-read row or one not on the current page. On a failed POST it
// reverts so the row doesn't lie about the server state.
function markOpenedHumanRead(id) {
  const m = mail.messages.find(x => x.id === id);
  if (!m || m.read) return;
  m.read = true;  // optimistic — selectMessage repaints immediately after
  (async () => {
    try {
      const r = await fetch('/api/human-messages/read', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id }),
      });
      if (!r.ok) throw new Error(await r.text());
      // The unread badge lives on the roster — refresh that alone, leaving
      // the open message + list page undisturbed.
      await loadMailboxes();
      paintSidebar();
    } catch {
      m.read = false;  // revert to the real (still-unread) server state
      paintList();
      paintReader();
    }
  })();
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
//
// Awaiting selectMailbox also lets the deep link open the newest message on
// page 1 immediately. Ordinary sidebar folder selection remains list-first;
// only the explicit "view messages" shortcut opens a reader automatically.
async function openMailbox(id) {
  if (!id) return;
  const navBtn = $('nav [data-tab="messages"]');
  if (navBtn) {
    suppressNextMessagesAttention();
    navBtn.click();
  }
  await loadMailboxes();
  const selected = await selectMailbox(id);
  if (!selected) return;
  mail.selectedMsgId = mail.messages[0]?.id ?? null;
  paintReader();
}

// --- access requests (human approvals) ------------------------------

// accessRequestsMailbox is the synthetic sidebar folder for approvals. It
// carries no server mailbox: unread is the live pending count (attention/bold),
// total is pending + recently handled history, and rows are
// lastSnapshot.access_requests (rendered by paintAccessRequests).
function accessRequestsMailbox() {
  const pending = (lastSnapshot && lastSnapshot.access_requests_pending) || 0;
  const total = (lastSnapshot && Array.isArray(lastSnapshot.access_requests)) ? lastSnapshot.access_requests.length : pending;
  return { id: ACCESS_ID, kind: 'access-requests', unread: pending, total, in: 0, out: 0 };
}

// accessCountdown renders "auto-declines in Xm Ys" from an ISO deadline.
// Recomputed on every paint (≤2s cadence), so no per-second timer is needed.
function accessCountdown(deadlineISO) {
  if (!deadlineISO) return '';
  const ms = new Date(deadlineISO).getTime() - Date.now();
  if (!(ms > 0)) return wz('auto-declining…', 'the sands have run out…');
  const s = Math.round(ms / 1000);
  const m = Math.floor(s / 60);
  const label = m > 0 ? `${m}m ${s % 60}s` : `${s}s`;
  return wz(`auto-declines in ${label}`, `refused in ${label}`);
}

// accessIsPending reports whether a request is still awaiting a decision (vs a
// handled one kept in the folder as history).
function accessIsPending(r) { return !r.status || r.status === 'pending'; }

// accessOutcome maps a handled status to its display chip — an icon + label + a
// css modifier class. wz() supplies the wizard wording.
function accessOutcome(status) {
  switch (status) {
    case 'approved': return { cls: 'approved', txt: wz('✓ Approved', '✓ Granted') };
    case 'always': return { cls: 'always', txt: wz('★ Always allowed', '★ Granted ever after') };
    case 'declined': return { cls: 'declined', txt: wz('✕ Declined', '✕ Refused') };
    case 'timed out': return { cls: 'timedout', txt: wz('⏱ Timed out', '⏱ Sands ran out') };
    default: return { cls: 'declined', txt: String(status || '') };
  }
}

function accessRequests() {
  return (lastSnapshot && Array.isArray(lastSnapshot.access_requests))
    ? lastSnapshot.access_requests
    : [];
}

function accessMatchesSearch(r, q) {
  if (!q) return true;
  q = q.toLowerCase();
  return [
    r.id, r.perm, r.conv_id, r.current_conv_id, r.agent_id, r.conv_title,
    r.caller_state, r.title_status, r.path, r.body,
    r.body_label, r.target_group, r.target_conv_id, r.target_conv_title,
    r.status,
  ].some(v => String(v || '').toLowerCase().includes(q));
}

function accessRequestById(id) {
  if (!id) return null;
  return accessRequests().find(r => r.id === String(id)) || null;
}

function accessWho(r) {
  const id = shortAgentId(r.agent_id, r.conv_id) || wz('unknown agent', 'unknown familiar');
  const title = r.conv_title || '(title unavailable)';
  const state = r.caller_state === 'retired'
    ? wz(' · retired', ' · banished')
    : r.caller_state === 'missing' ? wz(' · metadata missing', ' · lost to the mists') : '';
  return `${id} · ${title}${state}`;
}

function accessSubject(r) {
  const parts = [r.perm || wz('Access request', 'Petition')];
  if (r.path) parts.push(r.path);
  return parts.join(' · ');
}

function accessStatusText(r) {
  if (accessIsPending(r)) return wz('pending', 'awaiting judgement');
  return accessOutcome(r.status).txt;
}

// Access-request rows are rendered by the Preact island. Details and decision
// buttons live in the reader pane, matching the Human / All mail split:
// middle pane for scanning, right pane for the selected item.
async function decideAccess(id, decision) {
  if (!id) return;
  try {
    const r = await fetch(`/api/access-requests/${encodeURIComponent(id)}/decision`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ decision }),
    });
    if (!r.ok) throw new Error(await r.text());
    if (decision === 'extend') {
      toast(wz('Auto-decline pushed back 5 minutes.', 'The sands turned back five minutes.'));
      return;
    }
    if (lastSnapshot && Array.isArray(lastSnapshot.access_requests)) {
      const e = lastSnapshot.access_requests.find(x => x.id === id);
      if (e) {
        e.status = decision === 'approve' ? 'approved' : decision === 'always' ? 'always' : 'declined';
        e.decided_at = new Date().toISOString();
      }
      lastSnapshot.access_requests_pending = lastSnapshot.access_requests.filter(accessIsPending).length;
      renderAccessRequests(lastSnapshot.access_requests, lastSnapshot.access_requests_pending);
    }
    paintSidebar();
    if (mail.selected === ACCESS_ID) {
      paintList();
      paintReader();
    }
  } catch {
    toast(wz('That request could not be recorded — it may have just been decided or timed out.',
      'The petition slipped away — already judged, or the sands ran out.'), true);
  }
}

// renderAccessRequests updates the global attention affordances each snapshot
// tick: the non-blocking top banner with a "Review" deep link when approvals
// are pending. The blinking Messages-tab badge is driven from the shell model's
// renderMessagesBadge; the cards themselves repaint via renderMailTab.
function renderAccessRequests(list, pending) {
  const banner = $('#access-banner');
  if (!banner) return;
  const n = pending || 0;
  banner.classList.toggle('show', n > 0);
  banner.setAttribute('aria-hidden', n > 0 ? 'false' : 'true');
  const label = $('#access-banner-text');
  if (label && n > 0) {
    label.textContent = n === 1
      ? wz('An agent is requesting access', 'A familiar begs a boon')
      : wz(`${n} agents are requesting access`, `${n} familiars beg boons`);
  }
}

// focusAccessRequest brings the Messages tab forward and opens the
// access-requests folder, optionally flagging one request for a highlight
// pulse. The deep-link target for ?tab=messages&access_request=<id> and the
// banner's Review button.
function focusAccessRequest(id) {
  accessHighlightId = id || null;
  const navBtn = $('nav [data-tab="messages"]');
  if (navBtn) {
    suppressNextMessagesAttention();
    navBtn.click();
  }
  selectMailbox(ACCESS_ID);
  if (id) {
    mail.selectedMsgId = id;
    paintMail();
  }
}

// clearAttentionSearch guarantees that the badge target cannot remain hidden
// behind a stale Messages filter. This is deliberately narrower than
// setMessageQuery: the attention jump owns the immediate folder/page load and
// must not also leave a debounced request racing it.
function clearAttentionSearch() {
  prepareMessagesAttention(mail);
  dashPrefs.removeItem('tclaude.dash.filter.messages');
  if (mail.searchTimer) {
    clearTimeout(mail.searchTimer);
    mail.searchTimer = null;
  }
}

// focusNextAttention opens the oldest outstanding item advertised by the
// Messages badge. Access requests are already snapshot-ordered oldest-first;
// human notifications are newest-first and paged, so nextMessagesAttention
// returns the target's full-snapshot index for a direct page jump.
async function focusNextAttention(snapshot) {
  if (mail.busy) return false;
  const target = nextMessagesAttention(snapshot);
  if (!target) return false;

  clearAttentionSearch();
  if (target.kind === 'access') {
    accessHighlightId = target.id;
    const selected = await selectMailbox(ACCESS_ID);
    if (!selected || mail.selected !== ACCESS_ID) return false;
    mail.selectedMsgId = target.id;
    paintMail();
    return true;
  }

  const selected = await selectMailbox(HUMAN_ID);
  if (!selected || mail.selected !== HUMAN_ID) return false;
  const targetPage = Math.floor(target.index / mail.pageSize) + 1;
  if (mail.page !== targetPage) {
    mail.page = targetPage;
    await loadMessages();
  }
  if (mail.selected !== HUMAN_ID) return false;
  if (!mail.messages.some((message) => message.id === target.id)) {
    // A mutation between snapshot and mailbox fetch can move the target across
    // one page boundary: insertions push it later, while deletions pull it
    // earlier. Probe both neighbours before leaving the operator at the
    // estimated page.
    const estimatedPage = mail.page;
    for (const candidatePage of adjacentAttentionPages(estimatedPage, pageCount())) {
      mail.page = candidatePage;
      await loadMessages();
      if (mail.messages.some((message) => message.id === target.id)) break;
    }
    if (!mail.messages.some((message) => message.id === target.id)) {
      mail.page = estimatedPage;
      await loadMessages();
      paintMail();
      return false;
    }
  }
  selectMessage(target.id);
  // Avoid reopening the same notification on a rapid second click before the
  // next 2s snapshot reconciles the badge.
  const source = snapshot?.messages?.[target.index];
  if (source && source.id === target.id && !source.read) {
    source.read = true;
    snapshot.messages_unread = Math.max(0, Number(snapshot.messages_unread || 0) - 1);
  }
  return true;
}

function highlightedAccessRequest() { return accessHighlightId; }
function consumeAccessHighlight(id) {
  if (accessHighlightId === id) accessHighlightId = null;
}

// --- mutations ------------------------------------------------------

// progressBarHTML renders the "Deleting 150 / 300…" label + a filling bar
// shown in a bulk bar while a batched op runs.
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
async function postDeleteMessages(ids, url) {
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
  const deleteURL = messageDeleteEndpoint(mail.selected);
  const confirmed = await confirmModal({
    title: 'Delete this message?',
    body: 'Permanently deletes this one message. This cannot be undone.',
    meta: `#${id}`,
    okLabel: 'Delete',
  });
  if (!confirmed) return;
  const n = await postDeleteMessages([id], deleteURL);
  if (n === null) return;
  mail.selectedMsgs.delete(id);
  if (mail.selectedMsgId === id) mail.selectedMsgId = null;
  toast('message deleted');
  await reloadMail();
}

async function deleteSelectedMessages() {
  if (mail.busy) return;
  const deleteURL = messageDeleteEndpoint(mail.selected);
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
      const n = await postDeleteMessages(batch, deleteURL);
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
  const controller = new AbortController();
  const { signal } = controller;
  const disposeResize = initMailResize();
  $('#access-banner-review')?.addEventListener('click', () => focusAccessRequest(), { signal });
  document.addEventListener('tclaude:wizard', paintMail, { signal });
  return () => {
    controller.abort();
    disposeResize();
    if (mail.searchTimer) {
      clearTimeout(mail.searchTimer);
      mail.searchTimer = null;
    }
  };
}

export const mailController = Object.freeze({
  state: mailState,
  renderMailTab, initMail, renderAccessRequests,
  focusAccessRequest, openMailbox, senderOnline, focusNextAttention, setBoxQuery, setMessageQuery,
  setShowRetired, setShowEmpty, setShowPrevGens,
  mailboxView, mailboxLabel, mailboxTitleAttr, selectMailbox,
  toggleGroupExpand, toggleAgentsExpand, toggleBoxSelection, clearBoxSelection,
  wipeSelectedMailboxes,
  messageView, messageCountText, counterparty, allSenderLabel, allRecipientLabel,
  msgPreview, msgKind, selectMessage, toggleMessageSelection, togglePageSelection,
  deleteOneMessage, deleteSelectedMessages, setMessagesRead, markAllAgentRead,
  goToPage, setPageSize, decideAccess, accessWho, accessSubject,
  accessStatusText, accessIsPending, accessOutcome, accessCountdown,
  highlightedAccessRequest, consumeAccessHighlight,
  reloadMessagesPage, reloadMail,
});
