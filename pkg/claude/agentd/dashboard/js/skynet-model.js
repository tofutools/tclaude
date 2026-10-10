// skynet-model.js — DOM-free helpers for the Skynet node chip row and map view.
//
// Data comes from two local dashboard endpoints:
//   GET /api/federation/status?summary=1 — this node plus its federation peers.
//   GET /api/node-summary (local) and /api/peer/{instance_id}/node-summary — the
//   lightweight per-node map-card summary (see "Node summaries and polling" in
//   docs/federation.md).
// Everything here is pure so the polling contract and card wording can be
// unit-tested without a browser.

// LOCAL_COLOR marks this node. Peer colours are a fixed palette chosen by a
// stable hash of the instance ID, so a node keeps its colour across reloads and
// on every operator's dashboard. Purple is deliberately absent: it must not read
// as "remote" in general, only one node's own colour.
export const LOCAL_COLOR = '#58a6ff';
export const PEER_COLORS = Object.freeze(['#f0883e', '#39c5cf', '#db61a2', '#3fb950', '#e3b341', '#ff7b72', '#79c0ff', '#a5d6ff']);

// MAP_POLL_MS is the relaxed per-node summary cadence from the polling contract;
// STATUS_POLL_MS paces the chip row's peer list. Failures back off up to
// MAX_BACKOFF_MS. JITTER is the ± fraction applied to every delay so nodes never
// fall into lockstep.
export const MAP_POLL_MS = 10000;
export const STATUS_POLL_MS = 30000;
export const MAX_BACKOFF_MS = 60000;
export const JITTER = 0.2;

// MAX_CHIPS caps the peer chips in the tab bar so the row never pushes the tab
// strip; the rest collapse into a "+N" chip that opens the map.
export const MAX_CHIPS = 5;

function hash(s) {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); }
  return h >>> 0;
}

export function nodeColor(instanceID, local = false) {
  if (local) return LOCAL_COLOR;
  return PEER_COLORS[hash(String(instanceID || '')) % PEER_COLORS.length];
}

// peerName prefers the operator's local label (it is what addresses use:
// member@label), then the hub display name, then a short instance ID.
export function peerName(peer) {
  return peer?.label || peer?.name || String(peer?.instance_id || '').slice(0, 13) || 'peer';
}

// keyTransition reads a trusted peer's active identity transition from the
// status row: present only while pending (a signed successor key waits out
// this node's detection window) or conflict (competing successors; only an
// explicit operator recovery resolves it).
export function keyTransition(peer) {
  const t = peer?.identity_transition;
  if (!t || (t.state !== 'pending' && t.state !== 'conflict')) return null;
  return {
    state: t.state, oldID: t.old_id || '', newID: t.new_id || '', newFingerprint: t.new_fingerprint || '',
    receivedAt: t.received_at || '', acceptAfter: t.accept_after || '', reason: t.reason || '',
  };
}

// A leading letter or digit keeps a value from ever reading as a flag.
const ID_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
const FP_RE = /^[A-Za-z0-9][A-Za-z0-9-]{0,127}$/;

// recoverCommands are the CLI commands for a conflicted transition: a preview
// that changes nothing, and the apply to run only after the new fingerprint
// was verified with the peer's operator. Null when a field is not a plain ID.
export function recoverCommands(t) {
  if (!t || !ID_RE.test(t.oldID) || !ID_RE.test(t.newID)) return null;
  const preview = `tclaude federation identity recover-peer ${t.oldID} ${t.newID}`;
  return { preview, apply: FP_RE.test(t.newFingerprint) ? `${preview} --fingerprint ${t.newFingerprint} --apply` : '' };
}

// normalizeFleet turns a federation status response into the chip row's node
// list. Only trusted peers become nodes; hub-visible strangers belong to Fleet
// administration, not to navigation. A disabled or peerless federation yields
// null so the chip row stays absent and today's dashboard is unchanged.
export function normalizeFleet(status) {
  if (!status || typeof status !== 'object' || !status.instance_id) return null;
  const peers = (Array.isArray(status.peers) ? status.peers : [])
    .filter((p) => p && p.trusted && p.instance_id)
    .map((p) => ({
      id: p.instance_id,
      name: peerName(p),
      level: p.level === 'unrestricted' ? 'unrestricted' : 'restricted',
      online: !!p.online,
      lastSeen: p.last_seen || null,
      keyTransition: keyTransition(p),
      color: nodeColor(p.instance_id),
      local: false,
    }))
    .sort((a, b) => a.name.localeCompare(b.name));
  if (!peers.length) return null;
  const hubState = status.hub?.state || (status.enabled ? 'connecting' : 'disabled');
  return {
    self: { id: status.instance_id, name: status.name || 'this node', color: LOCAL_COLOR, local: true, online: true },
    peers,
    hub: { state: hubState, url: status.hub_url || '' },
  };
}

// summaryURL addresses a node's summary: the local route for this node and the
// peer proxy (keyed by the stable instance ID) for everyone else.
export function summaryURL(node) {
  return node.local ? '/api/node-summary' : `/api/peer/${encodeURIComponent(node.id)}/node-summary`;
}

// pollDelay returns the next summary delay for a node: the relaxed interval
// after success, doubled per consecutive failure up to MAX_BACKOFF_MS, always
// jittered so a fleet never polls in lockstep. random is injectable for tests.
export function pollDelay({ failures = 0, base = MAP_POLL_MS, random = Math.random } = {}) {
  const raw = Math.min(MAX_BACKOFF_MS, base * 2 ** Math.max(0, failures));
  return Math.round(raw * (1 - JITTER + 2 * JITTER * random()));
}

// staggerOffset spreads the first poll of n nodes across one interval so the
// map's opening never fires a synchronized fleet-wide burst.
export function staggerOffset(index, count, base = MAP_POLL_MS) {
  if (count <= 1) return 0;
  return Math.round((base * index) / count);
}

// classifyFailure maps a failed summary fetch to how the card should read.
// The proxy reports transport problems as code=peer_unreachable with
// reason=peer_offline|peer_timeout, and an unknown/untrusted target as
// code=not_trusted; anything else is an ordinary error.
export function classifyFailure(status, body) {
  const code = body?.code || '';
  if (code === 'peer_unreachable') return { kind: body.reason === 'peer_timeout' ? 'timeout' : 'offline', lastSeen: body.last_seen || null };
  if (code === 'not_trusted') return { kind: 'not_trusted' };
  if (status === 404) return { kind: 'unavailable' };
  return { kind: 'error', message: body?.error || (status ? `HTTP ${status}` : 'network error') };
}

function pct(n) { return Math.max(0, Math.min(100, Math.round(n))); }

// resourceView reduces the optional node.read resources to the card's meters.
export function resourceView(resources) {
  if (!resources || typeof resources !== 'object') return null;
  const out = {};
  const cores = resources.cpu?.logical_cores;
  const load = resources.cpu?.load_average?.[0];
  if (cores > 0 && typeof load === 'number') out.cpu = pct((load / cores) * 100);
  const ram = resources.ram;
  if (ram?.total_bytes > 0) out.mem = pct(100 - (ram.available_bytes / ram.total_bytes) * 100);
  const disk = resources.data_disk;
  if (disk?.total_bytes > 0) out.disk = pct(100 - (disk.available_bytes / disk.total_bytes) * 100);
  return Object.keys(out).length ? out : null;
}

// cardView derives what a node card shows from the node, its last summary
// entry and the current time. A summary is only ever data the node chose to
// share with us; the card says "shared" rather than implying totals.
export function cardView(node, entry, now = Date.now()) {
  const summary = entry?.summary || null;
  const failure = entry?.failure || null;
  const receivedAt = entry?.receivedAt ?? null;
  let presence = 'unknown';
  if (node.local) presence = summary ? 'online' : failure ? 'error' : 'loading';
  else if (failure && (failure.kind === 'offline' || failure.kind === 'timeout')) presence = 'offline';
  else if (summary && !failure) presence = 'online';
  else if (!node.online) presence = 'offline';
  else if (failure) presence = failure.kind === 'unavailable' ? 'online' : 'error';
  else presence = 'loading';
  const stale = !!summary && (presence === 'offline' || presence === 'error');
  const lastSeen = failure?.lastSeen || node.lastSeen || null;
  const omitted = (summary?.peer_view?.omitted || []).filter(shownOmission).map((o) => (typeof o === 'string' ? o : o?.feature)).filter(Boolean);
  return {
    presence,
    stale,
    receivedAt,
    ageMs: receivedAt != null ? Math.max(0, now - receivedAt) : null,
    lastSeen,
    failure,
    sharedGroups: summary?.shared_groups ?? null,
    sharedAgents: summary?.shared_agents ?? null,
    onlineAgents: summary?.online_agents ?? null,
    waiting: summary?.waiting_for_input ?? null,
    resources: resourceView(summary?.resources),
    health: summary?.health || null,
    // tclaude version from the summary (cached on the node, never probed by
    // the read); update is true only when a newer release is known.
    version: summary?.version || '',
    latestVersion: summary?.latest_version || '',
    updateAvailable: summary?.update_available === true && !!summary?.latest_version,
    omitted,
  };
}

// fmtAge renders a compact "14 min" style age for stale data.
export function fmtAge(ms) {
  if (ms == null) return '';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s} s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min`;
  const h = Math.round(m / 60);
  return h < 48 ? `${h} h` : `${Math.round(h / 24)} d`;
}

// visibleChips splits the peers into the ones shown as chips and the overflow
// count, keeping the chip row within its fixed budget.
export function visibleChips(peers, max = MAX_CHIPS) {
  const list = Array.isArray(peers) ? peers : [];
  return { shown: list.slice(0, max), overflow: Math.max(0, list.length - max) };
}

// remoteNodeID is the peer this page shows as its per-node view (?node=, set
// up by remote-node.js before any module ran), or '' for this node.
export function remoteNodeID(global = globalThis) {
  return global.__tclaudeRemoteNode?.id || '';
}

// nodeHref is the URL that shows node id's per-node view at the current tab:
// the same path and theme with ?node= set, or removed for this node. Switching
// reloads the page so no local state is ever rendered under a peer's banner.
export function nodeHref(id, loc = globalThis.location) {
  const params = new URLSearchParams(loc.search);
  if (id) params.set('node', id); else params.delete('node');
  const q = params.toString();
  return loc.pathname + (q ? `?${q}` : '');
}

// switchOrder lists the nodes Alt+1..9 reach: this node, then peers in chip
// order.
export function switchOrder(fleet) {
  if (!fleet) return [];
  return [fleet.self, ...fleet.peers].slice(0, 9);
}

// remoteHealthView turns the remote snapshot poll's health into the marker's
// wording: offline/timeout/busy with the age of the data still on screen.
export function remoteHealthView(health, now = Date.now()) {
  if (!health || health.ok !== false) return { state: 'live', label: '' };
  const f = health.failure || {};
  const age = health.lastOK != null ? fmtAge(Math.max(0, now - health.lastOK)) : '';
  let state = 'error';
  if (f.code === 'peer_unreachable') state = f.reason === 'peer_timeout' ? 'timeout' : 'offline';
  else if (f.code === 'peer_busy') state = 'busy';
  else if (f.code === 'not_trusted') state = 'not_trusted';
  const words = { offline: 'offline', timeout: 'not responding', busy: 'busy', not_trusted: 'no longer trusted', error: `error (HTTP ${f.status || '?'})` }[state];
  return { state, label: age ? `${words} · data ${age} old` : words };
}

// TRANSPORT_FEATURES are served on their own channels (the dashboard shell,
// terminals and their file links, inline spawn, node exec, access requests),
// never by the peer view itself: the peer's metadata lists them as omitted
// (newer daemons mark them transport), but that says nothing about whether
// they work, so they are not shown as "not shared".
const TRANSPORT_FEATURES = new Set(['local_dashboard', 'terminals', 'sessions.files.read', 'spawn.inline', 'node.exec', 'node.credentials.receive', 'permissions.requests']);

export function shownOmission(o) {
  if (typeof o === 'string') return !TRANSPORT_FEATURES.has(o);
  return !!o && !o.transport && !TRANSPORT_FEATURES.has(o.feature) && o.requires !== 'local_only' && o.requires !== 'peer_access';
}

// peerViewSummary normalizes the snapshot's peer_view metadata (what the peer
// shares with this operator) for the marker's popover.
export function peerViewSummary(meta) {
  if (!meta || typeof meta !== 'object') return null;
  const included = Array.isArray(meta.included) ? meta.included.filter(Boolean) : [];
  const omitted = (Array.isArray(meta.omitted) ? meta.omitted : [])
    .filter(shownOmission)
    .map((o) => (typeof o === 'string' ? { feature: o, requires: '' } : { feature: o?.feature || '', requires: o?.requires || '', ...(typeof o?.requestable === 'boolean' ? { requestable: o.requestable } : {}) }))
    .filter((o) => o.feature);
  return { included, omitted, full: !omitted.length && !included.length };
}
