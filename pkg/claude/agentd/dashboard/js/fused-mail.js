// fused-mail.js — the Messages tab in a fused node scope (tcl-1glyla): the
// ticked nodes' mail in the Messages layout, every row labelled with its node
// and every action sent back to that node. It renders with the same parts as
// this node's mail client (mail-parts.js): folder rows, message rows, filters,
// bulk bar, pager, reader headers and pane resizing. What differs is the data
// (one read per node) and what each node allows from here.
//
// The node chips above the folders narrow the whole view to one node or show
// them all; under the fused folders each node has its own section with its
// pinned folders, groups and agent folders.
//
// Reads follow the fused polling budget (fleet-merged-island.js): only while
// the view is on screen and the page visible, staggered, one read in flight
// per node and at most four overall. A failed read keeps what the node last
// showed, marked with its age; a node that withdraws trust keeps nothing.
// Everything a node returns is rendered as text.
import { Fragment, h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { dashboardState } from './snapshot-store.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { dashPrefs } from './prefs.js';
import { relTime } from './helpers.js';
import { fmtAge, nodeHref, pollDelay, remoteNodeID, staggerOffset } from './fleet-model.js';
import { tickedNodes, withFused } from './fleet-scope.js';
import { NodeSelect, pickedNodes } from './node-select.js';
import { approveConsequence } from './remote-inbox.js';
import { replySubject } from './peer-mail.js';
import { hasShownOverlay } from './overlay-stack.js';
import { initMailResize } from './mail-resize.js';
import { enterFirstRow, focusRow, leaveFirstRow, movePaneFocus, moveRowSelection } from './mail-keynav.js';
import { messageKind, msgPreview } from './mail-labels.js';
import { messageDeleteEndpoint } from './mail-state.js';
import {
  AgentMailAttachments, FolderCaret, FolderRow, HeaderRow, LoadMore, MessageAttachments, HumanActions, MailFilters, MailFooterToggles, MailGutters,
  MailPager, MailRow, MessageBody, MessageHeaders, PAGE_SIZES, SectionToggle, SelectAll,
} from './mail-parts.js';
import { withPeerAttachments } from './human-attachments.js';
import { humanReadShared } from './peer-view-limits.js';
import { LockReason } from './lock-reason.js';
import { registerFusedMailRoute, senderOnline } from './mail-bridge.js';
import { fromPath, locEquals, normalizeLocation, toPath, toQuery, withoutOverlays } from './nav-history-core.js';
import {
  FUSED_FOLDERS, PAGE_SIZE, ROSTER_PAGE, apiBase, canActOnHuman, folderMatches, folderURL, mergeMessages, mergeRequests,
  messageKey, nodeFolders, ownerRoutes, readFailure, replyTarget, rosterURL,
} from './fused-mail-model.js';
import { cursorKey, legacyDepth, legacyStream, mergeCursorPage, newWalk, pageCount as mergedPageCount, recordVector, walkTarget, LEGACY_MAX_DEPTH } from './fleet-pager.js';

const html = htm.bind(h);

// openRemote opens a peer agent's terminal here (remote-terminal.js).
const openRemote = (opts) => import('./remote-terminal.js').then((m) => m.openRemoteTerminal(opts));

export const MAIL_POLL_MS = 5000;
const ROSTER_EVERY = 3; // mailboxes refresh every third page read
const MAX_IN_FLIGHT = 4;
// MISSES_BEFORE_UNCOUNTED is how many reads in a row a node may miss before
// its count stops counting toward the merged totals.
const MISSES_BEFORE_UNCOUNTED = 3;
const SEARCH_DEBOUNCE_MS = 300;

// The fused view keeps its own sticky choices, apart from this node's mail
// client, so toggling one never surprises the other.
const PREF = {
  pageSize: 'tclaude.dash.fusedmail.pagesize',
  retired: 'tclaude.dash.fusedmail.showretired',
  empty: 'tclaude.dash.fusedmail.showempty',
  prevGens: 'tclaude.dash.fusedmail.showprevgens',
};
function pref(key) { try { return dashPrefs.getItem(key); } catch (_) { return null; } }
function setPref(key, value) { try { dashPrefs.setItem(key, value); } catch (_) { /* best effort */ } }
function initialPageSize() {
  const n = parseInt(pref(PREF.pageSize) || '', 10);
  return PAGE_SIZES.includes(n) ? n : PAGE_SIZE;
}

const SIDEBAR_ROWS = { rowSelector: '.mailbox', wrapSelector: '.mailbox-row' };
const LIST_ROWS = { rowSelector: '.mail-row', wrapSelector: '.mail-row-wrap' };

function errText(e) { return e?.message || String(e); }
function when(s) { const t = Date.parse(s || ''); return Number.isFinite(t) ? new Date(t).toLocaleString() : ''; }

async function readJSON(fetchFn, url, init) {
  const res = await fetchFn(url, { credentials: 'same-origin', cache: 'no-store', ...init });
  let data = null;
  try { data = await res.json(); } catch (_) { data = null; }
  if (!res.ok) { const e = new Error(data?.error || `HTTP ${res.status}`); e.status = res.status; e.body = data; throw e; }
  return data;
}

// createFusedMailActions posts an action to the node that owns the message.
// A peer serves deleting (deleteLocal, its own delete routes) only under
// unrestricted trust.
export function createFusedMailActions({ fetchImpl = (...a) => globalThis.fetch(...a), localFetch = globalThis.__tclaudeRemoteNode?.localFetch || fetchImpl } = {}) {
  const post = (node, url, body) => readJSON(node.local ? localFetch : fetchImpl, url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  return Object.freeze({
    markAgent: (node, ids, read) => post(node, ownerRoutes(node).markRead, { ids: [].concat(ids), read }),
    markFolderRead: (node, conv) => post(node, ownerRoutes(node).markRead, { conv, read: true }),
    reply: (node, to, subject, body) => post(node, ownerRoutes(node).message, { to, subject, body }),
    humanRead: (node, id, read = true) => post(node, ownerRoutes(node).humanRead, read ? { id } : { id, read: false }),
    humanReply: (node, id, body) => post(node, ownerRoutes(node).humanReply, { id, body }),
    decide: (node, id, decision) => post(node, ownerRoutes(node).decide(id), { decision: decision === 'approve' ? 'approve' : 'deny' }),
    deleteLocal: (node, folder, ids) => post(node, `${apiBase(node)}${messageDeleteEndpoint(folder).replace(/^\/api\//, '')}`, { ids }),
  });
}

// openOnNode shows a node's own Messages, where everything this view cannot
// do from here lives: it leaves the fused view.
function defaultOpenOnNode(node) {
  globalThis.location.assign(nodeHref(node.local ? '' : node.id, { pathname: '/messages', search: withFused(globalThis.location.search, null) }));
}

function NodePill({ node }) {
  return html`<span class="scope-pill" style=${`--nc:${node.color}`}>${node.local ? '⌂ ' : ''}${node.name}</span>`;
}

// folderKey names a sidebar folder: a fused folder spans the nodes in view,
// a node folder is one node's.
function folderKey(f) { return `${f.node || '*'}|${f.id}`; }

// fusedLocation is the router location for the fused view:
// /messages/<folder-id>[/<message-key>]?at=<folder's node>&scope=<node chip>
// plus the search, page and folder filter. The fused All folder is the bare
// tab, like a fresh fused view.
export function fusedLocation({ folder, selected, page, q, bq, scope }) {
  const plain = !folder.node && folder.id === 'all';
  return normalizeLocation({
    tab: 'messages', selection: plain && !selected ? undefined : folder.id, item: selected || undefined,
    params: { q, page, box: bq, at: folder.node, scope },
  });
}

function navEntry() {
  return globalThis.history?.state?.navIndex;
}
function groupKey(node, title) { return `${node}\n${title}`; }

// rosterOptions is what one node's roster read asks for. A node outside the
// view only feeds its chip, so it asks for totals; a folded agent list with
// no filter needs only its count, like this node's mail client.
function rosterOptions(node, { inView, bq, toggles, limits, agentsOpen, expanded, memberLimits }) {
  if (!inView) return { limit: 0, ...toggles };
  const expand = [...expanded].filter((k) => k.startsWith(`${node.id}\n`)).map((k) => k.slice(node.id.length + 1)).sort();
  const members = {};
  for (const name of expand) if (memberLimits[groupKey(node.id, name)]) members[name] = memberLimits[groupKey(node.id, name)];
  return { limit: bq || agentsOpen.has(node.id) ? (limits[node.id] || ROSTER_PAGE) : 0, q: bq, expand, memberLimits: members, ...toggles };
}

// useFusedMail reads what the open folder needs from each node.
function useFusedMail({ active, nodes, scoped, folder, q, page, pageSize, cursors, rosters, retired, fetchImpl, localFetch, timers, now, snapshot, remote, reload }) {
  const [entries, setEntries] = useState({});
  const ref = useRef(entries);
  ref.current = entries;
  const nodesKey = nodes.map((n) => `${n.id}:${n.online}:${n.level}`).join(',');
  const scopeKey = scoped.map((n) => n.id).join(',');
  // A merged folder reads one page from each node below the page's cursor
  // vector (fleet-pager.js); a node folder, or a fused one narrowed to one
  // node, reads the node's own offset page.
  const merged = !folder.node && scoped.length > 1;
  // sig names what a page shows; a page read for another folder is never shown.
  const sig = `${folderKey(folder)}:${scopeKey}:${q}:${page}:${pageSize}:${merged ? cursorKey(cursors) : ''}:${retired}`;
  // countKey names what a node's count counts: the folder and filters, not
  // the page on screen, so a count outlives the page it was read with.
  const countKey = `${folderKey(folder)}:${scopeKey}:${q}:${retired}`;
  const rosterKey = JSON.stringify(rosters);
  useEffect(() => {
    if (!active || !nodes.length) return undefined;
    let disposed = false;
    const pending = new Map();
    const inflight = new Set();
    const commit = (id, patch) => { if (!disposed) setEntries((cur) => ({ ...cur, [id]: { ...cur[id], ...patch } })); };
    const schedule = (node, delay) => { pending.set(node.id, timers.setTimeout(() => tick(node), delay)); };
    // Which nodes this folder reads: a node folder reads its node; a fused
    // folder the nodes in view, the access folder only those whose requests
    // this operator may answer from here.
    const readers = folder.node
      ? nodes.filter((n) => n.id === folder.node)
      : scoped.filter((n) => folder.id !== 'access' || !n.local);
    let ticks = 0;
    async function tick(node) {
      if (disposed || inflight.has(node.id)) return;
      if (globalThis.document?.hidden) { schedule(node, pollDelay({ base: MAIL_POLL_MS })); return; }
      if (inflight.size >= MAX_IN_FLIGHT) { schedule(node, 250 + Math.round(250 * Math.random())); return; }
      if (!node.local && node.online === false) { commit(node.id, { status: { kind: 'offline', label: 'offline' } }); schedule(node, pollDelay({ base: MAIL_POLL_MS * 2 })); return; }
      inflight.add(node.id);
      const fetchFn = node.local ? localFetch : fetchImpl;
      let failed = false;
      try {
        // A peer's requests are read once its trust in this node is known
        // to be unrestricted (its roster reply says); until then its roster.
        const accessFolder = !folder.node && folder.id === 'access';
        const answers = canActOnHuman(node, ref.current[node.id]?.peerView);
        const mode = !readers.includes(node) || (accessFolder && !answers) ? 'roster' : accessFolder ? 'requests' : 'page';
        if (mode === 'requests') {
          const data = await readJSON(fetchFn, `${apiBase(node)}human-inbox`);
          commit(node.id, { requests: Array.isArray(data?.access_requests) ? data.access_requests : [], status: null, misses: 0, at: now() });
          // Its folders still follow the sidebar filter and toggles.
          const url = rosterURL(node, rosters[node.id]);
          if (ref.current[node.id]?.rosterURL !== url) {
            const roster = await readJSON(fetchFn, url);
            commit(node.id, {
              rosterURL: url,
              mailboxes: Array.isArray(roster?.mailboxes) ? roster.mailboxes : [],
              agentsTotal: Number.isFinite(roster?.agents_total) ? roster.agents_total : null,
              agentsUnread: Number.isFinite(roster?.agents_unread) ? roster.agents_unread : 0,
              peerView: roster?.peer_view || null,
            });
          }
        } else {
          const url = rosterURL(node, rosters[node.id]);
          if (mode === 'roster' || ticks % ROSTER_EVERY === 0 || !ref.current[node.id]?.mailboxes || ref.current[node.id].rosterURL !== url) {
            const roster = await readJSON(fetchFn, url);
            commit(node.id, {
              rosterURL: url,
              mailboxes: Array.isArray(roster?.mailboxes) ? roster.mailboxes : [],
              agentsTotal: Number.isFinite(roster?.agents_total) ? roster.agents_total : null,
              agentsUnread: Number.isFinite(roster?.agents_unread) ? roster.agents_unread : 0,
              // What a peer shares from here rides on its reads.
              peerView: roster?.peer_view || null,
              ...(mode === 'roster' ? { status: null, misses: 0, at: now() } : {}),
            });
            // An unrestricted peer's access requests keep the sidebar count
            // current in every folder.
            if (!node.local && canActOnHuman(node, roster?.peer_view)) {
              const inbox = await readJSON(fetchFn, `${apiBase(node)}human-inbox`);
              commit(node.id, { requests: Array.isArray(inbox?.access_requests) ? inbox.access_requests : [] });
            }
          }
          if (mode === 'page') {
            // A node on an older build ignores before=; it is read to the
            // depth the page needs and cut at the cursor client-side.
            const legacy = merged && ref.current[node.id]?.legacy;
            const before = merged ? cursors[node.id] || 0 : 0;
            const read = (old) => readJSON(fetchFn, folderURL(node, folder.id, old
              ? { q, page: 1, size: legacyDepth(page, pageSize), retired }
              : { q, page: merged ? 1 : page, size: pageSize, retired, before }));
            let data = await read(legacy);
            let old = legacy;
            if (merged && !legacy && data?.cursor_paging !== true) {
              old = true;
              if (before) data = await read(true);
            }
            commit(node.id, {
              legacy: merged ? old : ref.current[node.id]?.legacy,
              page: {
                sig, messages: Array.isArray(data?.messages) ? data.messages : [], total: data?.total || 0,
                totalUnfiltered: data?.total_unfiltered ?? data?.total ?? 0,
                hasMore: !!data?.has_more, legacy: old,
              },
              count: { key: countKey, total: data?.total || 0, totalUnfiltered: data?.total_unfiltered ?? data?.total ?? 0 },
              misses: 0, status: null, at: now(),
            });
          }
        }
      } catch (e) {
        failed = true;
        const f = readFailure(e.status || 0, e.body);
        // What it shares is heard again from its roster (a refusal may be a
        // trust change), so a refused human-inbox read is not repeated.
        commit(node.id, f.kind === 'gone' || f.kind === 'not_shared'
          ? { status: f, mailboxes: [], page: null, requests: [], peerView: null, count: null }
          : { status: f, misses: (ref.current[node.id]?.misses || 0) + 1 });
      } finally {
        inflight.delete(node.id);
      }
      ticks += 1;
      if (!disposed) schedule(node, pollDelay({ base: readers.includes(node) ? MAIL_POLL_MS : MAIL_POLL_MS * ROSTER_EVERY, failures: failed ? 1 : 0 }));
    }
    // Every ticked node's roster feeds the sidebar, whatever the folder.
    const all = [...readers, ...nodes.filter((n) => !readers.includes(n))];
    all.forEach((node, i) => schedule(node, staggerOffset(i, all.length, 600)));
    return () => { disposed = true; pending.forEach((t) => timers.clearTimeout(t)); };
  }, [active, nodesKey, scopeKey, sig, rosterKey, reload]);
  // This node's pending access requests come with the page's own snapshot.
  const local = nodes.find((n) => n.local);
  const localRequests = !remote && local ? (snapshot.value?.access_requests || []) : [];
  return { entries, sig, countKey, merged, localRequests };
}

function AccessReader({ item, node, peerView, busy, run, actions, confirm, toast, onDone }) {
  const human = canActOnHuman(node, peerView);
  const body = html`<div class="mail-reader-head"><div class="mail-subject">🔐 ${item.perm}</div>
      <div class="mail-headers">
        <${HeaderRow} label="Agent">${item.title || item.agent_id || 'an agent'}</${HeaderRow}>
        <${HeaderRow} label="Node"><${NodePill} node=${node} /></${HeaderRow}>
        <${HeaderRow} label="Asked">${when(item.created_at)}</${HeaderRow}>
        ${item.deadline && html`<${HeaderRow} label="Status">waiting · auto-declines ${new Date(item.deadline).toLocaleTimeString()}</${HeaderRow}>`}
        ${item.path && html`<${HeaderRow} label="Path"><code>${item.path}</code></${HeaderRow}>`}
      </div></div>
      ${item.body && html`<div class="mail-reader-body">${item.body}</div>`}`;
  const buttons = human ? html`
      <button type="button" data-fused="deny" disabled=${busy} onClick=${() => run('Deny', actions.decide(node, item.id, 'deny'), `Denied on ${node.name}`)}>Deny</button>
      <button type="button" class="primary" data-fused="approve" disabled=${busy} onClick=${() => confirm({
        title: `Approve ${item.perm} on ${node.name}?`, body: approveConsequence(item, node.name), okLabel: 'Approve once', busyLabel: 'Approving…',
        action: () => actions.decide(node, item.id, 'approve'),
      }).then((r) => { if (r) { toast(`Approved once on ${node.name}`, false); onDone(); } }, (e) => toast(`Approve failed: ${errText(e)}`, true))}>Approve once…</button>`
    : html`<span class="muted">Answer this on ${node.name}'s own dashboard.</span>`;
  return { body, buttons };
}

// senderHarness names the harness of a notification's sender on this node,
// for the focus button.
function senderHarness(message, snapshot) {
  const agents = snapshot?.value?.agents || [];
  const sender = agents.find((a) => (message.from_agent && a?.agent_id === message.from_agent) || (message.from_conv && a?.conv_id === message.from_conv));
  return sender?.state?.harness || '';
}

// humanMarkable reports whether this operator marks a node's human
// notifications read from here: this node, a peer that trusts it
// unrestricted, or one granting node.messages.mark-read (peerView, from its
// mail reads).
export function humanMarkable(node, peerView) {
  return canActOnHuman(node, peerView) || humanReadShared(peerView);
}

function Reader({ item, remote, snapshot, peerView, timers, actions, confirm, toast, openOnNode, onDone, onDelete }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => { setText(''); setBusy(false); }, [item?.key]);
  // Opening a human notification marks it read on its node, as each node's
  // own Messages does, wherever that node lets this operator. Once per open:
  // a later "mark unread" sticks.
  // opened is the open already handled: polls rebuild item, and its read
  // state flipping back must not mark it again.
  const opened = useRef('');
  useEffect(() => {
    const key = item?.key || '';
    if (key === opened.current) return;
    opened.current = key;
    if (key && item.kind === 'human' && !item.read && humanMarkable(item.node, peerView)) actions.humanRead(item.node, item.id, true).then(onDone, () => {});
  }, [item?.key]);
  if (!item) {
    return html`<div class="mail-reader fused-mail-reader" id="fused-mail-reader" role="region" aria-label="Message reader" tabindex="0">
      <div class="empty">Select a message to read.</div></div>`;
  }
  const { node } = item;
  const run = (what, p, done) => { setBusy(true); p.then(() => { setBusy(false); setText(''); toast(done, false); onDone(); }, (e) => {
    setBusy(false);
    toast(e.status === 403 ? `${what}: ${node.name} does not allow it from here (${errText(e)}). Open it on ${node.name}.` : `${what} failed: ${errText(e)}`, true);
  }); };
  const open = html`<button type="button" data-fused-open=${node.id} title=${`Show ${node.name}'s own Messages (leaves the fused view)`} onClick=${() => openOnNode(node)}>Open on ${node.name}</button>`;
  let body;
  let buttons;
  if (item.kind === 'access') {
    ({ body, buttons } = AccessReader({ item, node, peerView, busy, run, actions, confirm, toast, onDone }));
  } else {
    const target = replyTarget(item);
    const human = item.kind === 'human';
    const headers = html`<${MessageHeaders} message=${item} extra=${html`<${HeaderRow} label="Node"><${NodePill} node=${node} /></${HeaderRow}>`} />
      <${MessageBody} message=${item} agentMail=${!human} />`;
    if (human) {
      // The same buttons as each node's own Messages. This node's are run by
      // the page's action handlers, and the view re-reads once they act. A
      // peer's (its files already point at this node's peer attachment
      // route) is answered through the peer's human-inbox; so is this node's
      // on a peer's page, where its data-act buttons are refused.
      const peer = node.local && !remote ? null : {
        name: node.name, exportNode: node.local ? '' : node.id, canAnswer: canActOnHuman(node, peerView), canMark: humanMarkable(node, peerView),
        canDelete: canActOnHuman(node, peerView), remove: (id) => actions.deleteLocal(node, 'human', [id]),
        // A peer's agent opens here through the federation bridge.
        focus: node.local ? null : (m) => openRemote({ instance: node.id, agent: m.from_agent, peerLabel: node.name, label: m.from_title || '', toast }),
        mark: (id, read) => actions.humanRead(node, id, read), reply: (id, body) => actions.humanReply(node, id, body),
      };
      return html`<div class="mail-reader fused-mail-reader" id="fused-mail-reader" data-kind="decree" role="region" aria-label="Message reader" tabindex="0"
        onClick=${(e) => { if (!peer && e.target?.closest?.('[data-act]')) timers.setTimeout(onDone, 800); }}>
        ${headers}
        <${MessageAttachments} message=${item} />
        <${HumanActions} message=${item} senderLive=${peer ? undefined : senderOnline(item.from_agent, item.from_conv, snapshot.value)}
          harness=${peer ? '' : senderHarness(item, snapshot)} toast=${toast} peer=${peer} onDone=${onDone}>${open}</${HumanActions}>
      </div>`;
    }
    body = html`${headers}
      <${AgentMailAttachments} message=${item} />
      ${target && html`<textarea class="mail-reply-text" rows="3" placeholder=${`Reply to ${item.from_title || target} on ${node.name}`} value=${text} onInput=${(e) => setText(e.currentTarget.value)}></textarea>`}`;
    buttons = html`
        <button type="button" data-fused="mark" disabled=${busy} title=${item.read ? 'Mark this message unread for the recipient' : 'Mark this message read on the recipient’s behalf'}
          onClick=${() => run(item.read ? 'Mark unread' : 'Mark read', actions.markAgent(node, item.id, !item.read), `Marked ${item.read ? 'unread' : 'read'} on ${node.name}`)}>${item.read ? 'mark unread' : 'mark read'}</button>
        ${target && html`<button type="button" class="primary" data-fused="reply" disabled=${busy || !text.trim()} onClick=${() => run('Reply', actions.reply(node, target, replySubject(item.subject), text.trim()), `Sent to ${item.from_title || target} on ${node.name}`)}>send reply</button>`}`;
    const deleteLock = canActOnHuman(node, peerView) ? '' : `${node.name} keeps deleting its messages to its own operator: it needs unrestricted trust from ${node.name}.`;
    buttons = html`${buttons}<button type="button" class="danger" data-fused="delete" disabled=${busy || !!deleteLock} title=${deleteLock ? undefined : `Permanently delete this message on ${node.name}`} onClick=${() => onDelete([item])}>delete</button><${LockReason} why=${deleteLock} />`;
  }
  return html`<div class="mail-reader fused-mail-reader" id="fused-mail-reader" data-kind=${item.kind === 'access' ? 'decree' : messageKind(item, item.kind === 'human')}
    role="region" aria-label="Message reader" tabindex="0">
    ${body}
    <div class="mail-reader-actions fused-mail-actions">${buttons}<span class="spacer"></span>${open}</div>
  </div>`;
}

// FusedMail is the fused Messages view.
export function FusedMail({
  state, snapshot = dashboardState.snapshot, fetchImpl = (...a) => globalThis.fetch(...a),
  localFetch = globalThis.__tclaudeRemoteNode?.localFetch || fetchImpl, timers = globalThis, now = () => Date.now(),
  remote = remoteNodeID(), actions = null, confirm = shellConfirm, toast = shellToast, openOnNode = defaultOpenOnNode,
}) {
  const current = state.view.value;
  const fleet = current.fleet;
  const active = current.activeTab === 'messages' && !!current.fused && !!fleet;
  const nodes = fleet && current.fused ? tickedNodes(fleet, current.fused) : [];
  const nodeIDs = nodes.map((n) => n.id).join(',');
  // scope names the picked nodes, comma-joined; '' is every ticked node.
  const [scope, setScope] = useState('');
  const scopeIDs = scope ? scope.split(',').filter(Boolean) : [];
  const [folder, setFolder] = useState({ node: '', id: 'all' });
  const [boxQuery, setBoxQuery] = useState('');
  const [bq, setBq] = useState('');
  const [query, setQuery] = useState('');
  const [q, setQ] = useState('');
  const [page, setPage] = useState(1);
  const [pageSize, setPageSizeState] = useState(initialPageSize);
  // vectors are the merged folder's known cursor vectors, vectors[p - 1] for
  // page p, for the folder, search and scope in view (fleet-pager.js).
  const [vectors, setVectors] = useState(() => newWalk());
  const [selected, setSelected] = useState('');
  const [checked, setChecked] = useState(() => new Set());
  const [busy, setBusy] = useState(false);
  const [reload, setReload] = useState(0);
  const [limits, setLimits] = useState({});
  const [memberLimits, setMemberLimits] = useState({});
  const [expanded, setExpanded] = useState(() => new Set());
  const [agentsOpen, setAgentsOpen] = useState(() => new Set());
  const [collapsed, setCollapsed] = useState(() => new Set());
  const [toggles, setToggles] = useState(() => ({ retired: pref(PREF.retired) === '1', empty: pref(PREF.empty) === '1', prevGens: pref(PREF.prevGens) === '1' }));
  const acts = useRef(actions);
  if (!acts.current) acts.current = createFusedMailActions({ fetchImpl, localFetch });
  const clientRef = useRef(null);
  const hasClient = !!current.fused && !!fleet;
  useEffect(() => (hasClient && clientRef.current ? initMailResize(clientRef.current) : undefined), [hasClient]);
  // An edit that settles on the search already shown (a trailing space)
  // changes no URL, so its intent to record is dropped here (`nav`, below).
  useEffect(() => { const t = timers.setTimeout(() => { const next = query.trim(); if (next === q && nav.current === 'q') nav.current = null; setQ(next); }, SEARCH_DEBOUNCE_MS); return () => timers.clearTimeout(t); }, [query]);
  useEffect(() => { const t = timers.setTimeout(() => { const next = boxQuery.trim(); if (next === bq && nav.current === 'box') nav.current = null; setBq(next); }, SEARCH_DEBOUNCE_MS); return () => timers.clearTimeout(t); }, [boxQuery]);
  // restored names the folder/search/scope a history location just set, so
  // the reset below (a new folder starts on page 1 with nothing open) leaves
  // the page and message that location named.
  const restored = useRef('');
  const resetKey = `${folderKey(folder)}\n${q}\n${scope}`;
  useEffect(() => {
    if (restored.current === resetKey) { restored.current = ''; return; }
    restored.current = '';
    setPage(1); setSelected(''); setChecked(new Set());
  }, [folder.node, folder.id, q, scope]);
  // --- history: the URL follows the folder, message, page, search and scope.
  // A click on a folder, message, page or node chip pushes (nav = 'push'); a
  // search or folder-filter edit pushes once and then corrects as it is
  // typed; every other change (arrow keys, a reset) corrects in place.
  const nav = useRef(null);
  const editing = useRef(null);
  const loc = fusedLocation({ folder, selected, page, q, bq, scope });
  const locRef = useRef(loc);
  locRef.current = loc;
  const sync = () => {
    if (!active) return;
    const url = globalThis.location;
    const shown = withoutOverlays(fromPath(url?.pathname, url?.search || ''));
    if (shown.tab !== 'messages') return;
    const why = nav.current;
    if (locEquals(locRef.current, shown)) { nav.current = null; return; }
    nav.current = null;
    let correction = true;
    if (why === 'push') { correction = false; editing.current = null; }
    else if (why === 'q' || why === 'box') correction = editing.current?.kind === why && editing.current.entry === navEntry();
    globalThis.document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: locRef.current, correction } }));
    if (why === 'q' || why === 'box') editing.current = { kind: why, entry: navEntry() };
  };
  useEffect(sync, [active, toPath(loc), toQuery(loc)]);
  const restoreRef = useRef(null);
  restoreRef.current = (target) => {
    editing.current = null;
    nav.current = null;
    // A bare /messages is the fused All folder, unfiltered.
    const p = target?.params || {};
    const nextFolder = { node: p.at || '', id: target?.selection || 'all' };
    restored.current = `${folderKey(nextFolder)}\n${p.q || ''}\n${p.scope || ''}`;
    setFolder(nextFolder); setSelected(target?.item || ''); setPage(Number(p.page) > 0 ? Number(p.page) : 1);
    setQuery(p.q || ''); setQ(p.q || ''); setBoxQuery(p.box || ''); setBq(p.box || ''); setScope(p.scope || '');
  };
  useEffect(() => registerFusedMailRoute({
    mailLocation: () => locRef.current,
    restoreLocation: (target) => restoreRef.current(target),
  }), []);
  // A node no longer ticked drops out of the scope and its folders fall back
  // to the fused All folder.
  useEffect(() => {
    const kept = scopeIDs.filter((id) => nodes.some((n) => n.id === id));
    if (kept.length !== scopeIDs.length) setScope(kept.length === nodes.length ? '' : kept.join(','));
    if (folder.node && !nodes.some((n) => n.id === folder.node)) setFolder({ node: '', id: 'all' });
  }, [nodeIDs]);
  const scoped = pickedNodes(nodes, scopeIDs, { multiple: true });
  // solo is a scope of one node: its fused folders are that node's own.
  const solo = !!scope && scoped.length === 1;
  const rosters = {};
  for (const n of nodes) rosters[n.id] = rosterOptions(n, { inView: scoped.includes(n), bq, toggles, limits, agentsOpen, expanded, memberLimits });
  // A merged folder walks to the requested page one known vector at a time.
  const vectorKey = `${resetKey}\n${pageSize}\n${toggles.retired}\n${scoped.map((n) => n.id).join(',')}`;
  const known = vectors.key === vectorKey ? vectors.list : [{}];
  const willMerge = !folder.node && scoped.length > 1;
  const shown = willMerge ? walkTarget(page, known) : page;
  const { entries, sig, countKey, merged, localRequests } = useFusedMail({
    active, nodes, scoped, folder, q, page: shown, pageSize, cursors: known[shown - 1] || {}, rosters, retired: toggles.retired, fetchImpl, localFetch, timers, now, snapshot, remote, reload,
  });
  // Recorded after the render that merged it (set below, past the early
  // returns, which hooks may not follow).
  const pendingVector = useRef(null);
  useEffect(() => {
    const v = pendingVector.current;
    if (!v) return;
    setVectors((cur) => recordVector(cur, v));
  });
  if (!current.fused) return null;
  if (!fleet) return html`<div class="empty">Loading the fused nodes…</div>`;

  const t = now();
  const wizard = globalThis.document?.body?.classList?.contains('wizard');
  const isAccess = !folder.node && folder.id === 'access';
  const isHuman = folder.id === 'human';
  const pageNodes = folder.node ? nodes.filter((n) => n.id === folder.node) : scoped;
  const nodeRequests = (node) => (node.local ? localRequests : canActOnHuman(node, entries[node.id]?.peerView) ? entries[node.id]?.requests : []) || [];
  const requestRows = mergeRequests(scoped.map((node) => ({ node, requests: nodeRequests(node) })));
  // In a merged folder a node's page and count hold while its page for this
  // view is on the way, and through a slow or missed read: they are dropped
  // only once the node misses MISSES_BEFORE_UNCOUNTED reads in a row or is
  // reported offline, so the totals and page count do not flicker with one
  // slow tick. (One node's own folder keeps its last page under its status.)
  const counted = (n) => !merged || ((entries[n.id]?.misses || 0) < MISSES_BEFORE_UNCOUNTED && !(!n.local && n.online === false));
  const pages = pageNodes.map((n) => (entries[n.id]?.page?.sig === sig && counted(n) ? entries[n.id].page : null));
  const counts = pageNodes.map((n, i) => pages[i] || (entries[n.id]?.count?.key === countKey && counted(n) ? entries[n.id].count : null));
  const total = counts.reduce((s, p) => s + (p?.total || 0), 0);
  const totalUnfiltered = counts.reduce((s, p) => s + (p?.totalUnfiltered || 0), 0);
  // A counted node whose page has not come yet may still have rows past it.
  const awaited = pageNodes.some((n, i) => !pages[i] && counts[i]);
  // A peer's message files load through this node's peer attachment routes.
  const pageMessages = (node, i) => {
    const list = pages[i]?.messages || [];
    return node.local ? list : list.map((m) => withPeerAttachments(node.id, m, !isHuman));
  };
  const kind = isHuman ? 'human' : 'agent';
  const cursors = known[shown - 1] || {};
  // A merged page: each node's stream below its cursor, merged newest head
  // first. A node that failed or has not answered gives no rows and keeps its
  // cursor, so its rows show once it does.
  const mergedPage = merged && !isAccess ? mergeCursorPage(pageNodes.map((node, i) => {
    const p = pages[i];
    if (!p) return { node: node.id, rows: [], hasMore: false };
    const list = pageMessages(node, i);
    return p.legacy ? legacyStream(node.id, list, cursors[node.id], pageSize, p.total) : { node: node.id, rows: list, hasMore: p.hasMore };
  }), pageSize, cursors) : null;
  const nodeByID = Object.fromEntries(pageNodes.map((n) => [n.id, n]));
  const allRows = isAccess ? requestRows : mergedPage
    ? mergedPage.rows.map(({ node, row }) => ({ ...row, key: messageKey(node, kind, row.id), node: nodeByID[node], kind }))
    : mergeMessages(pageNodes.map((node, i) => ({ node, kind, messages: pageMessages(node, i) })));
  const pageCount = isAccess ? 1 : mergedPage
    ? (mergedPage.more || awaited ? Math.max(mergedPageCount(total, pageSize), shown + (mergedPage.more ? 1 : 0)) : shown)
    : Math.max(1, Math.ceil(total / pageSize));
  const rows = allRows;
  const item = rows.find((r) => r.key === selected) || null;
  const loaded = pageNodes.every((n) => entries[n.id]?.status || (isAccess ? entries[n.id]?.at || n.local : entries[n.id]?.page?.sig === sig));
  // An older node pages by depth, so its mail past LEGACY_MAX_DEPTH is only
  // reachable on its own.
  const deeper = !!mergedPage && pages.some((p) => p?.legacy && p.total > LEGACY_MAX_DEPTH);
  // Once every node has answered this page, the merge yields the next page's
  // cursor vector: record it so Next (and a walk to a deeper page) can read
  // it. A node that is down gives no rows and no cursor, so the vectors are
  // only good while the same nodes are down; they are recorded with that set.
  // A node is down for the walk once it no longer counts (three misses in a
  // row, or reported offline) or has gone or stopped sharing; one missed read
  // keeps it in, as it keeps its count.
  const down = (n) => !!entries[n.id]?.status && (!counted(n) || ['gone', 'not_shared'].includes(entries[n.id].status.kind));
  const absent = pageNodes.filter((n, i) => !pages[i] && down(n)).map((n) => n.id).join(',');
  const answered = pageNodes.every((n, i) => pages[i] || down(n));
  pendingVector.current = mergedPage && answered
    ? { key: vectorKey, absent, page: shown, next: mergedPage.next, more: mergedPage.more } : null;

  const status = (n) => {
    const e = entries[n.id];
    if (e?.status) return `${e.status.label}${e.at && e.status.kind === 'offline' ? ` · data ${fmtAge(Math.max(0, t - e.at))} old` : ''}`;
    if (isAccess && n.local && remote) return 'its requests show on its own page';
    if (isAccess && !n.local && e?.peerView && !canActOnHuman(n, e.peerView)) return 'requests answered on its own dashboard';
    return e?.at ? '' : 'loading…';
  };
  const folders = (n) => nodeFolders(entries[n.id]?.mailboxes);
  const humanUnread = (n) => folders(n).human?.unread || 0;

  // A re-click changes nothing, so it records nothing (and leaves no intent
  // for a later arrow key to push with).
  const pick = (f, { push = true } = {}) => { if (push && folderKey(f) !== folderKey(folder)) nav.current = 'push'; setFolder(f); };
  const isOn = (f) => folderKey(folder) === folderKey(f);
  const toggleSet = (setter, key) => setter((cur) => { const next = new Set(cur); if (next.has(key)) next.delete(key); else next.add(key); return next; });
  const setToggle = (name, on) => { setPref(PREF[name], on ? '1' : '0'); setToggles((cur) => ({ ...cur, [name]: on })); };
  const setPageSize = (n) => { setPref(PREF.pageSize, String(n)); setPageSizeState(n); setPage(1); };
  const selectRow = (key, { push = true } = {}) => { if (push && key !== selected) nav.current = 'push'; setSelected(key); };
  const folderRows = new Map();
  const folderRow = (f, { icon, label, title, total: count = null, totalTitle, unread = 0, lead = null, nested = false, className = '', tags = null }) => {
    const key = folderKey(f);
    folderRows.set(key, f);
    return html`<${FolderRow} key=${key} id=${key} icon=${icon} label=${label} title=${title || label} total=${count} totalTitle=${totalTitle}
      unread=${unread} active=${isOn(f)} lead=${lead} nested=${nested} className=${className} tags=${tags} disabled=${busy} onSelect=${() => pick(f)} />`;
  };
  const agentRow = (n, m, nested = false) => folderRow({ node: n.id, id: m.id }, {
    icon: html`<span class=${`mail-dot ${m.online ? 'online' : 'offline'}`}>●</span>`, label: m.title || m.agent_id || m.id,
    title: `${m.title || m.id}${m.agent_id ? ` — ${m.agent_id}` : ''} on ${n.name}`, total: m.total, totalTitle: `${m.in} received · ${m.out} sent`,
    unread: m.unread, nested, className: `${m.retired ? 'retired' : ''}${m.total ? '' : ' empty-box'}${m.prev_gen ? ' prev-gen' : ''}`.trim(),
    tags: html`${m.retired && html`<span class="mailbox-tag" title="This agent has been retired">retired</span>`}
      ${m.prev_gen && html`<span class="mailbox-tag" title="A superseded past generation of this agent — a conversation left behind by a reincarnate / /clear">prev gen</span>`}`,
  });

  // The node chips narrow the whole view: folders, list and counts.
  // The node selector narrows the whole view to the picked nodes: folders,
  // list and counts.
  const pickScope = (ids) => {
    const next = ids.join(',');
    if (next === scope) return;
    nav.current = 'push'; setScope(next);
    if (ids.length && folder.node && !ids.includes(folder.node)) setFolder({ node: '', id: folder.id === 'access' || folder.id === 'human' ? folder.id : 'all' });
  };
  const chips = html`<div class="fused-mail-scope"><${NodeSelect} id="fused-mail-nodes" multiple label="Mail from" nodes=${nodes} value=${scopeIDs}
    onChange=${pickScope} badge=${humanUnread} note=${(n) => { const s = status(n); return s === 'loading…' ? '' : s; }}
    title="Which ticked nodes' mail to show" /></div>`;

  const scopeName = solo ? scoped[0]?.name || '' : `${scoped.length} nodes`;
  const fusedLabel = scope ? ` · ${scopeName}` : '';
  const fusedRows = FUSED_FOLDERS.map((f) => {
    const count = f.id === 'access' ? requestRows.length : f.id === 'human' ? scoped.reduce((s, n) => s + (folders(n).human?.total || 0), 0) : scoped.reduce((s, n) => s + (folders(n).all?.total || 0), 0);
    const unread = f.id === 'access' ? requestRows.length : f.id === 'human' ? scoped.reduce((s, n) => s + humanUnread(n), 0) : 0;
    return folderRow({ node: '', id: f.id }, { icon: f.icon, label: `${f.title}${fusedLabel}`, title: `${f.title} on ${scope ? scopeName : 'every ticked node'}`, total: count, unread });
  });

  const sections = scoped.map((n) => {
    const e = entries[n.id];
    const { all, human, groups, agents } = folders(n);
    const open = !collapsed.has(n.id);
    const s = status(n);
    const agentsTotal = Number.isFinite(e?.agentsTotal) ? e.agentsTotal : agents.length;
    const agentsShown = !!bq || agentsOpen.has(n.id);
    const shownGroups = groups.filter((g) => folderMatches(g, bq));
    return html`<div key=${n.id} class="fused-mail-section" data-node=${n.id}>
      <button type="button" class="mailbox-section mailbox-section-toggle fused-mail-node-head" aria-expanded=${String(open)}
        title=${open ? `Fold ${n.name}'s folders` : `Show ${n.name}'s folders`} onClick=${() => toggleSet(setCollapsed, n.id)}>
        <span class="mailbox-section-caret">${open ? '▾' : '▸'}</span><${NodePill} node=${n} />${s && html` <span class="muted">${s}</span>`}</button>
      ${open && html`<${Fragment}>
        ${!solo && all && folderMatches(all, bq) && folderRow({ node: n.id, id: 'all' }, { icon: '🗂', label: 'All agent messages', title: `All agent messages on ${n.name}`, total: all.total })}
        ${!solo && human && folderMatches(human, bq) && folderRow({ node: n.id, id: 'human' }, { icon: '📬', label: 'Human notifications', title: `Human notifications on ${n.name}`, total: human.total, unread: human.unread })}
        ${shownGroups.length > 0 && html`<div class="mailbox-section">${wizard ? 'Parties' : 'Groups'}</div>`}
        ${shownGroups.map((g) => {
          const gk = groupKey(n.id, g.title);
          const isOpen = expanded.has(gk);
          const members = g.member_boxes || [];
          return html`<${Fragment} key=${g.id}>
            ${folderRow({ node: n.id, id: g.id }, {
              icon: '👥', label: g.title, title: `${g.title} on ${n.name}`, total: g.total, unread: g.unread,
              totalTitle: `${g.members || 0} member${g.members === 1 ? '' : 's'} · ${g.total} message${g.total === 1 ? '' : 's'}`,
              lead: html`<${FolderCaret} expanded=${isOpen} disabled=${busy} group=${g.title} onToggle=${() => toggleSet(setExpanded, gk)} />`,
            })}
            ${isOpen && (members.length > 0
              ? members.map((m) => html`<${Fragment} key=${m.id}>${agentRow(n, m, true)}</${Fragment}>`)
              : html`<div class="mailbox-row nested"><span class="mail-box-check-spacer"></span><div class="mailbox-nested-empty">${e?.rosterURL?.includes('expand=') ? 'no member folders shown' : 'loading…'}</div></div>`)}
            ${isOpen && (g.members_total || 0) > members.length && html`<${LoadMore} nested=${true} shown=${members.length} total=${g.members_total}
              busy=${busy} what=${`${g.title}'s member mailboxes`}
              onMore=${() => setMemberLimits((cur) => ({ ...cur, [gk]: (cur[gk] || ROSTER_PAGE) + ROSTER_PAGE }))} />`}
          </${Fragment}>`;
        })}
        ${agentsTotal > 0 && (bq
          ? html`<div class="mailbox-section">${wizard ? 'The Rookery' : 'All agent mailboxes'}</div>`
          : html`<${SectionToggle} expanded=${agentsShown} unread=${agentsShown ? 0 : e?.agentsUnread || 0}
              onToggle=${() => toggleSet(setAgentsOpen, n.id)} label=${`${wizard ? 'The Rookery' : 'All agent mailboxes'} (${agentsTotal})`} />`)}
        ${agentsShown && agents.map((m) => html`<${Fragment} key=${m.id}>${agentRow(n, m)}</${Fragment}>`)}
        ${agentsShown && agentsTotal > agents.length && agents.length > 0 && html`<${LoadMore} shown=${agents.length} total=${agentsTotal} busy=${busy}
          what=${`${n.name}'s agent mailboxes`} onMore=${() => setLimits((cur) => ({ ...cur, [n.id]: (cur[n.id] || ROSTER_PAGE) + ROSTER_PAGE }))} />`}
      </${Fragment}>`}
    </div>`;
  });

  // Bulk actions run on each owning node. Read state goes to every node
  // (needs its node.messages.manage); deleting is this node's alone.
  const checkedRows = rows.filter((r) => checked.has(r.key));
  // Deleting is a node operator's: this node's own, and a peer's that trusts
  // this one unrestricted (it serves its delete routes as for its operator).
  const deletable = (r) => canActOnHuman(r.node, entries[r.node.id]?.peerView);
  const runBulk = async (verb, groupsOf, fn) => {
    setBusy(true);
    const failures = [];
    for (const [node, list] of groupsOf) {
      try { await fn(node, list); } catch (e) { failures.push(`${node.name}: ${e.status === 403 ? 'not allowed from here' : errText(e)}`); }
    }
    setBusy(false);
    toast(failures.length ? `${verb} failed on ${failures.join('; ')}` : `${verb} done`, failures.length > 0);
    setChecked(new Set());
    setReload((r) => r + 1);
  };
  const byNode = (list) => {
    const out = new Map();
    for (const r of list) { if (!out.has(r.node)) out.set(r.node, []); out.get(r.node).push(r); }
    return out;
  };
  const markRows = (read) => runBulk(read ? 'Mark read' : 'Mark unread', byNode(checkedRows), (node, list) => acts.current.markAgent(node, list.map((r) => r.id), read));
  const deleteRows = async (list) => {
    const mine = list.filter(deletable);
    const skipped = list.length - mine.length;
    if (!mine.length) { toast('Only messages on nodes that trust you unrestricted can be deleted from here; open the node’s own dashboard for its mail.', true); return; }
    const groups = byNode(mine);
    const ok = await confirm({
      title: `Delete ${mine.length} message${mine.length === 1 ? '' : 's'}?`,
      body: `This permanently deletes ${mine.length === 1 ? 'it' : 'them'} on ${[...groups.keys()].map((n) => n.name).join(', ')}.${skipped ? ` ${skipped} on other nodes stay: only their operators can delete them.` : ''}`,
      okLabel: 'Delete', busyLabel: 'Deleting…',
      action: async () => { for (const [node, rs] of groups) await acts.current.deleteLocal(node, isHuman ? 'human' : 'all', rs.map((r) => r.id)); },
    // One node failing leaves the ones before it deleted: re-read either way.
    }).catch((e) => { toast(`Delete failed: ${errText(e)}`, true); setReload((r) => r + 1); return false; });
    if (ok) { toast(`Deleted ${mine.length}`, false); setChecked(new Set()); setSelected(''); setReload((r) => r + 1); }
  };
  const folderNode = folder.node ? nodes.find((n) => n.id === folder.node) : null;
  const agentFolder = folderNode && !['all', 'human'].includes(folder.id) && !folder.id.startsWith('group:');
  const allChecked = rows.length > 0 && rows.every((r) => checked.has(r.key));
  const bulkBar = isAccess || (!rows.length && !agentFolder)
    ? html`<div id="fused-mail-bulk-bar" class="mail-bulk-bar" hidden></div>`
    : html`<div id="fused-mail-bulk-bar" class="mail-bulk-bar">
      ${rows.length > 0 && html`<${SelectAll} checked=${allChecked} onChange=${(on) => setChecked((cur) => {
        const next = new Set(cur); for (const r of rows) { if (on) next.add(r.key); else next.delete(r.key); } return next;
      })} />`}
      <span class="grow">${checked.size ? `${checked.size} selected` : ''}</span>
      ${rows.length > 0 && !isHuman && html`<${Fragment}><button disabled=${busy || !checkedRows.length} title="Mark the selected messages read on their nodes"
        onClick=${() => markRows(true)}>✓ read</button>
        <button disabled=${busy || !checkedRows.length} title="Mark the selected messages unread on their nodes"
          onClick=${() => markRows(false)}>○ unread</button></${Fragment}>`}
      ${rows.length > 0 && html`<button class="danger" disabled=${busy || !checkedRows.some(deletable)}
        title=${checkedRows.length && !checkedRows.some(deletable) ? 'Only messages on nodes that trust you unrestricted can be deleted from here' : 'Delete the selected messages on their nodes'}
        onClick=${() => deleteRows(checkedRows)}>🗑 delete selected</button>`}
      ${agentFolder && html`<span class="mail-folder-actions"><button disabled=${busy} title=${`Mark every message this agent has received as read (on ${folderNode.name})`}
        onClick=${() => runBulk('Mark all read', new Map([[folderNode, [folder.id]]]), (node) => acts.current.markFolderRead(node, folder.id))}>✓ mark all read</button></span>`}
    </div>`;

  const countText = isAccess
    ? `${requestRows.length} pending`
    : q ? `${total} / ${totalUnfiltered}` : `${totalUnfiltered} message${totalUnfiltered === 1 ? '' : 's'}`;
  const listKeyDown = (event) => {
    if (leaveFirstRow({ event, ...LIST_ROWS, filter: clientRef.current?.querySelector('#fused-mail-filter') })) return;
    moveRowSelection({ event, ...LIST_ROWS, select: (row) => selectRow(row.dataset.id, { push: false }) });
  };
  const sidebarKeyDown = (event) => {
    if (leaveFirstRow({ event, ...SIDEBAR_ROWS, filter: clientRef.current?.querySelector('#fused-mail-box-filter') })) return;
    moveRowSelection({ event, ...SIDEBAR_ROWS, select: (row) => { const f = folderRows.get(row.dataset.id); if (f) pick(f, { push: false }); } });
  };
  const panes = [
    { selector: '#fused-mail-sidebar', rows: SIDEBAR_ROWS },
    { selector: '#fused-mail-list', rows: LIST_ROWS, select: (row) => selectRow(row.dataset.id, { push: false }) },
    { selector: '#fused-mail-reader' },
  ];
  const aggregate = folder.id === 'all' || folder.id.startsWith('group:');
  return html`<div class="mail-client fused-mail" ref=${clientRef} onKeyDown=${(event) => !hasShownOverlay() && movePaneFocus(event, panes)}>
    <${MailFilters} boxId="fused-mail-box-filter" boxPlaceholder=${wizard ? 'Seek a familiar…' : 'Filter mailboxes (name / id)'}
      boxValue=${boxQuery} onBoxInput=${(value) => { nav.current = 'box'; setBoxQuery(value); }}
      onBoxKeyDown=${(event) => enterFirstRow({ event, container: clientRef.current?.querySelector('#fused-mail-sidebar'), ...SIDEBAR_ROWS, select: (row) => { const f = folderRows.get(row.dataset.id); if (f) pick(f); } })}
      id="fused-mail-filter" placeholder=${solo ? `Filter ${scopeName}'s messages` : scope ? `Filter messages on ${scopeName}` : 'Filter messages on every ticked node'}
      value=${query} onInput=${(value) => { nav.current = 'q'; setQuery(value); }}
      onKeyDown=${(event) => enterFirstRow({ event, container: clientRef.current?.querySelector('#fused-mail-list'), ...LIST_ROWS, select: (row) => selectRow(row.dataset.id) })}
      countId="fused-mail-filter-count" countText=${countText} clearId="fused-mail-filter-clear" onClear=${() => { nav.current = 'q'; setQuery(''); setQ(''); }} />
    <div class="mail-col mail-sidebar-col">
      ${chips}
      <aside class="mail-sidebar" id="fused-mail-sidebar" onKeyDown=${sidebarKeyDown}>
        ${fusedRows}
        ${sections}
      </aside>
      <${MailFooterToggles} idPrefix="fused-mail" busy=${busy}
        showRetired=${toggles.retired} showEmpty=${toggles.empty} showPrevGens=${toggles.prevGens}
        onRetired=${(on) => setToggle('retired', on)} onEmpty=${(on) => setToggle('empty', on)} onPrevGens=${(on) => setToggle('prevGens', on)} />
    </div>
    <div class="mail-col mail-list-col">
      ${bulkBar}
      <div class="mail-list" id="fused-mail-list" role="list" onKeyDown=${listKeyDown}>
        ${!rows.length && html`<div class="empty">${loaded ? (q ? 'No messages match the filter.' : 'No messages here.') : 'Loading…'}</div>`}
        ${rows.map((m) => (m.kind === 'access'
          ? html`<div class="mail-row-wrap access-row-wrap" key=${m.key} data-key=${m.key} data-kind="decree" role="listitem">
            <span class="mail-box-check-spacer"></span>
            <button type="button" class=${`mail-row access-row-item unread${m.key === selected ? ' active' : ''}`} data-id=${m.key}
              aria-current=${m.key === selected ? 'true' : undefined} onClick=${(event) => { focusRow(event.currentTarget); selectRow(m.key); }}>
              <span class="mail-row-top"><span class="mail-row-dot" title="pending">●</span><${NodePill} node=${m.node} />
                <span class="mail-row-party">${m.title || m.agent_id || 'agent'}</span>
                <span class="mail-row-time" title=${when(m.created_at)}>${relTime(m.created_at)}</span></span>
              <span class="mail-row-subject">🔐 ${m.perm}</span>
            </button></div>`
          : html`<${MailRow} key=${m.key} id=${m.key} message=${m} kind=${messageKind(m, m.kind === 'human')} aggregate=${aggregate}
              preview=${msgPreview(m)} lead=${html`<${NodePill} node=${m.node} />`} active=${m.key === selected}
              checked=${checked.has(m.key)} busy=${busy}
              onCheck=${(on) => setChecked((cur) => { const next = new Set(cur); if (on) next.add(m.key); else next.delete(m.key); return next; })}
              onSelect=${() => selectRow(m.key)} onDelete=${() => deleteRows([m])}
              canDelete=${deletable(m)} deleteTitle=${deletable(m) ? 'Delete this message' : `Only ${m.node.name}'s operator can delete its messages`} />`))}
      </div>
      <${MailPager} id="fused-mail-pager" hidden=${isAccess || !totalUnfiltered} page=${shown} pages=${pageCount} pageSize=${pageSize}
        onPage=${(n) => { nav.current = 'push'; setPage(n); setSelected(''); }} onPageSize=${setPageSize}
        stepping=${page} note=${deeper ? 'pick one node for older mail' : ''} />
    </div>
    <${Reader} item=${item} remote=${remote} snapshot=${snapshot} peerView=${item ? entries[item.node.id]?.peerView : null} timers=${timers} actions=${acts.current} confirm=${confirm} toast=${toast} openOnNode=${openOnNode}
      onDone=${() => setReload((r) => r + 1)} onDelete=${deleteRows} />
    <${MailGutters} />
  </div>`;
}

export function mountFusedMailIsland({ host, state, registerCleanup }) {
  render(html`<${FusedMail} state=${state} />`, host);
  registerCleanup(() => render(null, host));
}
