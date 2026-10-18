// fused-mail-model.js — DOM-free helpers for fused Messages (tcl-1glyla): the
// ticked nodes' mailboxes and messages read side by side, each message keyed
// by its node and routed back to that node for any action.
//
// Reads (per node, through the local peer proxy for a peer):
//   GET mailboxes                         the sidebar roster
//   GET mailbox?id=all|human|CONV|group:N  a folder's page
// A peer serves both under node.messages.read; without it the read is a 403
// with code "permission". Actions go to the owning node: agent mail read
// state under node.messages.manage, a reply to an agent under message.direct
// (operator-message), and human notifications and access requests through
// the human-inbox routes: read state under node.messages.mark-read, replies
// and access decisions under unrestricted trust.

// FUSED_FOLDERS are the folders that span every ticked node.
export const FUSED_FOLDERS = Object.freeze([
  { id: 'all', title: 'All agent messages', icon: '🗂' },
  { id: 'human', title: 'Human notifications', icon: '📬' },
  { id: 'access', title: 'Access requests', icon: '🔐' },
]);

export const PAGE_SIZE = 50;

// apiBase is where a node's dashboard API lives from this page: this node's
// own routes, or a peer's through the proxy.
export function apiBase(node) {
  return node.local ? '/api/' : `/api/peer/${encodeURIComponent(node.id)}/`;
}

// ROSTER_PAGE is how many agent folders a node's sidebar section asks for
// at a time; the node pages its roster server-side and reports the total.
export const ROSTER_PAGE = 100;
// ROSTER_MAX is the most folders one roster read returns (the server's
// maxRosterAgents); past it a list is narrowed with the filter, not paged.
export const ROSTER_MAX = 2000;

// MAX_PAGE_SIZE is the largest page a node serves (maxMailboxPageSize).
export const MAX_PAGE_SIZE = 500;

// rosterURL addresses one page of a node's sidebar roster: the filter, the
// scope toggles, and which groups to embed members for (expand, with a
// members_limit for a grown one). A node on an older build ignores what it
// does not know and sends its whole roster.
export function rosterURL(node, {
  limit = ROSTER_PAGE, q = '', retired = false, empty = false, prevGens = false, expand = [], memberLimits = {},
} = {}) {
  const params = new URLSearchParams({ agents_limit: String(limit) });
  if (q) params.set('q', q);
  if (retired) params.set('include_retired', '1');
  if (empty) params.set('include_empty', '1');
  if (!prevGens) params.set('hide_prev_gens', '1');
  for (const name of expand) {
    params.append('expand', name);
    if (memberLimits[name]) params.append('members_limit', `${memberLimits[name]}:${name}`);
  }
  return `${apiBase(node)}mailboxes?${params}`;
}

// folderURL addresses one node's page of a folder: an offset page, or with
// before the page of rows older than that message id (a merged folder's
// keyset cursor, see fleet-pager.js).
export function folderURL(node, folder, { q = '', page = 1, size = PAGE_SIZE, retired = false, before = 0 } = {}) {
  const params = new URLSearchParams({ id: folder, page: String(page), page_size: String(Math.min(MAX_PAGE_SIZE, size)) });
  if (before) params.set('before', String(before));
  if (q) params.set('q', q);
  if (retired) params.set('include_retired', '1');
  return `${apiBase(node)}mailbox?${params}`;
}

// ownerRoutes are a node's action routes. This node's human notifications
// and access requests have their own local paths; a peer serves the same
// operations under human-inbox.
export function ownerRoutes(node) {
  const base = apiBase(node);
  return node.local
    ? { humanRead: `${base}human-messages/read`, humanReply: `${base}human-messages/reply`, decide: (id) => `${base}access-requests/${encodeURIComponent(id)}/decision`, markRead: `${base}mailbox/mark-read`, message: `${base}operator-message` }
    : { humanRead: `${base}human-inbox/read`, humanReply: `${base}human-inbox/reply`, decide: (id) => `${base}human-inbox/access/${encodeURIComponent(id)}`, markRead: `${base}mailbox/mark-read`, message: `${base}operator-message` };
}

// canActOnHuman reports whether this operator answers a node's human
// notifications and access requests from here: always on this node, and on a
// peer that trusts it unrestricted (the human-inbox routes refuse the rest).
export function canActOnHuman(node) {
  return !!node.local || node.level === 'unrestricted';
}

// readFailure turns a failed read into what the node's status says: not
// shared (no grant), offline, gone (trust lost: drop what it shared), or an
// error. Data already shown stays on screen except when gone.
export function readFailure(status, body) {
  const code = body?.code || '';
  if (code === 'not_trusted') return { kind: 'gone', label: 'no longer trusted' };
  if (status === 403 || code === 'permission') return { kind: 'not_shared', label: 'not shared (needs node.messages.read)' };
  if (code === 'peer_unreachable') return { kind: 'offline', label: body.reason === 'peer_timeout' ? 'not responding' : 'offline' };
  if (code === 'peer_busy' || status === 503) return { kind: 'offline', label: 'busy' };
  if (status === 404) return { kind: 'not_shared', label: 'does not offer shared mail yet' };
  return { kind: 'error', label: body?.error || (status ? `HTTP ${status}` : 'network error') };
}

// messageKey identifies a message across nodes: node, kind and its own ID.
export function messageKey(nodeID, kind, id) {
  return `${nodeID}/${kind}/${id}`;
}

function ts(s) {
  const t = Date.parse(s || '');
  return Number.isFinite(t) ? t : 0;
}

// mergeMessages lists every node's page of a folder in one newest-first
// list. Each row carries its node and kind, so the reader can route actions.
export function mergeMessages(pages) {
  const out = [];
  for (const { node, kind, messages } of pages) {
    for (const m of messages || []) {
      if (!m || m.id == null) continue;
      out.push({ ...m, key: messageKey(node.id, kind, m.id), node, kind });
    }
  }
  return out.sort((a, b) => ts(b.created_at) - ts(a.created_at) || String(a.key).localeCompare(String(b.key)));
}

// mergeRequests lists pending access requests from every node, oldest first
// (they block an agent in real time).
export function mergeRequests(lists) {
  const out = [];
  for (const { node, requests } of lists) {
    for (const r of requests || []) {
      if (!r || r.id == null || (r.status && r.status !== 'pending')) continue;
      out.push({ ...r, key: messageKey(node.id, 'access', r.id), node, kind: 'access' });
    }
  }
  return out.sort((a, b) => ts(a.created_at) - ts(b.created_at));
}

// nodeFolders splits a node's roster into the folders the fused sidebar lists
// under that node, in the node's own order: its pinned folders (all, human),
// groups, then the page of agent folders.
export function nodeFolders(mailboxes) {
  const list = Array.isArray(mailboxes) ? mailboxes.filter(Boolean) : [];
  return {
    all: list.find((m) => m.kind === 'all') || null,
    human: list.find((m) => m.kind === 'human') || null,
    groups: list.filter((m) => m.kind === 'group'),
    agents: list.filter((m) => m.kind === 'agent'),
  };
}

// folderMatches reports whether a pinned or group folder passes the sidebar
// filter; agent folders are filtered by each node (the roster's q).
export function folderMatches(mailbox, q) {
  if (!q) return true;
  const needle = q.toLowerCase();
  return [mailbox?.title, mailbox?.id].some((s) => String(s || '').toLowerCase().includes(needle));
}

// replyTarget is the agent a reply to a message goes to: the sender, else
// (an operator-authored message) its recipient. '' when neither is an agent.
export function replyTarget(m) {
  const id = m?.from_agent || (m?.operator_authored ? m?.to_agent : '') || '';
  return /^agt_[A-Za-z0-9]{4,64}$/.test(id) ? id : '';
}
