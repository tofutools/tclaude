import { keyTransition } from './skynet-model.js';

// fleet-admin-model.js — DOM-free view helpers for Fleet administration: which
// nodes this node trusts and what they may do. The data is this node's own
// federation state (/api/federation/*, cookie-authenticated, never served to
// peers); every trust-widening or destructive action goes through a confirm
// that names its consequence.

// UNRESTRICTED_CONSEQUENCE is repeated verbatim wherever unrestricted trust is
// granted: it implies every peer permission on every group, including groups
// created later. (Wording follows the daemon's confirmation_required error.)
export const UNRESTRICTED_CONSEQUENCE = 'Unrestricted trust grants this peer every peer permission on all groups — every live group and every group created later — plus automatic spawn and local unscoped grants towards it. Interactive terminal attach includes answering harness approval prompts.';

export function peerLabel(p) {
  return p?.label || p?.name || String(p?.instance_id || '').slice(0, 13) || 'peer';
}

// shortID abbreviates an instance ID for tables; dialogs always show it whole.
export function shortID(id) {
  const s = String(id || '');
  return s.length > 14 ? `${s.slice(0, 9)}…${s.slice(-4)}` : s;
}

// shortFingerprint abbreviates a dash-grouped fingerprint for tables.
export function shortFingerprint(fp) {
  const parts = String(fp || '').split('-');
  return parts.length > 2 ? `${parts[0]}-…-${parts[parts.length - 1]}` : String(fp || '');
}

// grantsByPeer groups the status's peer_grants per peer: total, node-wide
// (unscoped) and group-scoped counts.
export function grantsByPeer(grants) {
  const out = new Map();
  for (const g of Array.isArray(grants) ? grants : []) {
    if (!g?.peer) continue;
    const row = out.get(g.peer) || { total: 0, nodeWide: 0, scoped: 0 };
    row.total += 1;
    if (g.scope) row.scoped += 1; else row.nodeWide += 1;
    out.set(g.peer, row);
  }
  return out;
}

// poolsByPeer maps instance ID -> pool names from /api/federation/nodes/groups.
export function poolsByPeer(pools) {
  const out = new Map();
  for (const pool of Array.isArray(pools) ? pools : []) {
    for (const m of pool?.members || []) {
      const id = m?.instance_id || m?.InstanceID || m;
      if (typeof id !== 'string') continue;
      out.set(id, [...(out.get(id) || []), pool.name]);
    }
  }
  return out;
}

// adminView splits a federation status into the identity strip, trusted peers
// and the hub-visible instances waiting to be trusted.
export function adminView(status, { pools = [] } = {}) {
  if (!status?.instance_id) return null;
  const grants = grantsByPeer(status.peer_grants);
  const poolMap = poolsByPeer(pools);
  const peers = Array.isArray(status.peers) ? status.peers : [];
  const row = (p) => ({
    id: p.instance_id,
    label: peerLabel(p),
    name: p.name || '',
    fingerprint: p.fingerprint || '',
    level: p.level === 'unrestricted' ? 'unrestricted' : 'restricted',
    online: !!p.online,
    lastSeen: p.last_seen && !String(p.last_seen).startsWith('0001-') ? p.last_seen : null,
    trustedAt: p.trusted_at && !String(p.trusted_at).startsWith('0001-') ? p.trusted_at : null,
    grants: grants.get(p.instance_id) || { total: 0, nodeWide: 0, scoped: 0 },
    pools: poolMap.get(p.instance_id) || [],
    keyTransition: keyTransition(p),
  });
  const byLabel = (a, b) => a.label.localeCompare(b.label);
  // receiving names each peer's catalog groups that take agents from this
  // node (agents.receive), for the move dialog's group picker.
  const receiving = new Map((Array.isArray(status.remote) ? status.remote : []).map((r) => [r?.peer,
    (r?.groups || []).filter((g) => g?.name && (g.caps || []).includes('agents_receive')).map((g) => g.name).sort()]));
  return {
    self: {
      id: status.instance_id,
      name: status.name || 'this node',
      fingerprint: status.fingerprint || '',
      enabled: !!status.enabled,
      hubURL: status.hub_url || '',
      hubState: status.hub?.state || (status.enabled ? 'connecting' : 'disabled'),
      hubError: status.hub?.last_error || '',
    },
    trusted: peers.filter((p) => p?.trusted && p.instance_id).map((p) => ({ ...row(p), receiving: receiving.get(p.instance_id) || [] })).sort(byLabel),
    waiting: peers.filter((p) => p && !p.trusted && p.instance_id).map(row).sort(byLabel),
  };
}

// LABEL_RE mirrors the daemon's validFedLabel.
export const LABEL_RE = /^[a-z0-9_.-]{1,32}$/;

// trustBody builds the peers/trust request. A profile requires a preview
// token from a prior preview; unrestricted requires the exact fingerprint.
export function trustBody({ instance, label = '', level = 'restricted', profile = '', noDefaultProfile = false, preview = false, previewToken = '', confirmFingerprint = '' }) {
  const body = { instance };
  if (label) body.label = label;
  if (profile) body.profile = profile;
  else if (level) body.level = level;
  if (noDefaultProfile) body.no_default_profile = true;
  if (preview) body.preview = true;
  if (previewToken) body.preview_token = previewToken;
  if (confirmFingerprint) body.confirm_fingerprint = confirmFingerprint;
  return body;
}

// PEER_SLUGS lists what a peer may be granted on this node (mirrors the
// daemon's federationPeerSlugs plus its instance-wide slugs). kind: 'group'
// slugs take an optional group scope (none = every group, including future
// ones), 'scoped' slugs require one, 'node' slugs are node-wide only.
// sensitive marks grants that let the peer act on this node, not just read.
export const PEER_SLUGS = Object.freeze([
  { slug: 'agents.status.read', kind: 'group', what: 'agent activity, model, task and context summaries' },
  { slug: 'groups.roster.read', kind: 'group', what: 'member names and roles' },
  { slug: 'groups.presence.read', kind: 'group', what: 'online/offline per member' },
  { slug: 'sessions.read', kind: 'group', what: 'live agent sessions, harness, state and waiting reason' },
  { slug: 'sessions.watch', kind: 'group', what: 'read-only terminal view of member agents' },
  { slug: 'sessions.attach', kind: 'group', sensitive: true, what: 'terminal view and full keyboard input, including answering harness approvals' },
  { slug: 'message.direct', kind: 'group', what: 'mail to members (shares their names and ids)' },
  { slug: 'message.attachments', kind: 'group', what: 'attachments, together with message.direct on the same group' },
  { slug: 'routes.consume', kind: 'group', what: 'list and open ready group routes' },
  { slug: 'groups.members.spawn', kind: 'group', sensitive: true, policy: true, what: 'spawn workers automatically, within your launch settings and live cap' },
  { slug: 'jobs.run', kind: 'group', sensitive: true, policy: true, what: 'run one-shot jobs in your allowed repositories, within the live cap' },
  { slug: 'groups.members.stop', kind: 'group', sensitive: true, what: 'stop member agents\' sessions' },
  { slug: 'groups.members.resume', kind: 'group', sensitive: true, what: 'wake (resume) member agents; with groups.members.stop also restart them' },
  { slug: 'groups.members.retire', kind: 'group', sensitive: true, what: 'retire member agents (their sessions exit)' },
  { slug: 'groups.members.clone', kind: 'group', sensitive: true, what: 'clone member agents within the group' },
  { slug: 'agent.move', kind: 'group', sensitive: true, what: 'move or teleport member agents to its node (with groups.members.retire)' },
  { slug: 'agents.receive', kind: 'scoped', what: 'move or share agents into the group' },
  { slug: 'agents.teleport.receive', kind: 'scoped', what: 'teleport agents into the group' },
  { slug: 'node.read', kind: 'node', what: 'platform, harness versions, labels and resource numbers' },
  { slug: 'node.messages.read', kind: 'node', what: 'private human notifications and agent mailbox history across this node', warning: 'Exposes private message bodies across this node, including human notifications and agent-to-agent mail. Default off. Does not allow replies or approval decisions.' },
  { slug: 'node.messages.manage', kind: 'node', sensitive: true, what: 'explicitly mark agent mailbox messages read or unread across this node; no delete, reply or approval authority' },
  { slug: 'node.harnesses.read', kind: 'node', what: 'harness availability and versions' },
  { slug: 'costs.read', kind: 'node', what: 'the complete node-wide cost collection' },
  { slug: 'federation.audit.read', kind: 'node', what: 'the complete node-wide dashboard audit log' },
  { slug: 'config.offer', kind: 'node', what: 'offer config bundles for your review' },
  { slug: 'approvals.answer', kind: 'node', sensitive: true, what: 'answer access requests once while selected as your away cover' },
  { slug: 'node.update', kind: 'node', sensitive: true, what: 'update tclaude on this node' },
  { slug: 'node.exec', kind: 'node', sensitive: true, what: 'run any shell script on this node (only while it accepts remote scripts)', warning: 'Full remote code execution: once this node accepts remote scripts (Run scripts page), it can run any command here as the tclaude user — read and change your files, use your logins and keys, and reach whatever this machine can reach.' },
  { slug: 'node.harnesses.install', kind: 'node', sensitive: true, what: 'install and update harnesses on this node' },
  { slug: 'node.credentials.receive', kind: 'node', sensitive: true, what: 'push its harness credentials here — this node\'s agents then act as that operator with the provider' },
  { slug: 'models.proxy', kind: 'node', gateway: true, what: 'use this node\'s model gateways (all, or one named gateway)' },
  { slug: 'models.proxy.leased', kind: 'node', gateway: true, what: 'leased model gateway access for requester-paid workers (all gateways, or one)' },
]);

export function slugInfo(slug) {
  return PEER_SLUGS.find((s) => s.slug === slug) || { slug, kind: 'node', what: '' };
}

// scopeGroup names the group of a 'group=<name>' scope ('' = unscoped).
export function scopeGroup(scope) {
  return String(scope || '').startsWith('group=') ? String(scope).slice(6) : '';
}

// grantRows shapes a target's grants for the table: direct grants first, then
// those inherited from a pool (revoked on the pool, not here). On a pool's own
// page (ownPool) its grants are direct.
//
// Newer daemons scope group grants by stable ID (group_id=<ID>) and label them
// with group_name / group_deleted; the scope goes back unchanged on revoke, so
// a deleted group's grant is revocable. Older daemons show group=<name>, and a
// numeric name there means the group is gone — not revocable, since their
// revoke resolves the scope as a name.
export function grantRows(grants, { ownPool = '', groups = null } = {}) {
  const rows = (Array.isArray(grants) ? grants : []).map((g) => {
    const info = slugInfo(g.slug);
    const stableIDs = 'group_deleted' in g || String(g.scope || '').startsWith('group_id=');
    const group = stableIDs ? (g.group_name || '') : scopeGroup(g.scope);
    const deletedGroup = stableIDs ? !!g.group_deleted : (!!group && /^\d+$/.test(group) && !!groups && !groups.includes(group));
    return {
      key: `${g.pool_id || ''}|${g.slug}|${g.scope || ''}`,
      slug: g.slug, scope: g.scope || '', group, what: info.what, sensitive: !!info.sensitive,
      gateway: String(g.scope || '').startsWith('http_proxy=') ? String(g.scope).slice(11) : '',
      allGroups: !g.scope && info.kind === 'group',
      pool: g.pool_name && g.pool_name !== ownPool ? g.pool_name : '',
      deletedGroup,
      groupID: g.group_id ?? null,
      revocable: !deletedGroup || stableIDs,
      maxLive: g.spawn_policy?.max_live || 0,
      policy: g.spawn_policy || {},
    };
  });
  return rows.sort((a, b) => (a.pool ? 1 : 0) - (b.pool ? 1 : 0) || a.slug.localeCompare(b.slug) || a.group.localeCompare(b.group));
}

// extraPolicy lists launch settings beyond the live cap (set from the CLI).
export function extraPolicy(policy) {
  return Object.entries(policy || {}).filter(([k, v]) => k !== 'max_live' && v != null && v !== '' && !(Array.isArray(v) && !v.length)).map(([k]) => k);
}

// LAUNCH_FIELDS are a spawn/job grant's receiver launch settings: what this
// node uses when it starts a worker for the peer (empty = the group's or
// profile's default).
export const LAUNCH_FIELDS = Object.freeze([
  { key: 'profile', label: 'profile', slugs: ['groups.members.spawn'], hint: 'launch profile' },
  { key: 'allowed_profiles', label: 'selectable', slugs: ['groups.members.spawn'], hint: 'profiles the peer may pick, comma-separated', list: true },
  { key: 'harness', label: 'harness', hint: 'worker harness' },
  { key: 'model', label: 'model', hint: 'worker model' },
  { key: 'cwd', label: 'directory', hint: 'worker directory' },
]);
export const REQUESTER_PAYS = Object.freeze([
  { value: '', label: 'default' },
  { value: 'required', label: 'required: the peer must bring its own model gateway' },
  { value: 'allowed', label: 'allowed: the peer may bring its own gateway' },
  { value: 'off', label: 'off: workers use this node\'s logins' },
]);

// launchText lists a policy's launch settings for a confirm.
export function launchText(policy = {}) {
  const bits = [];
  for (const f of LAUNCH_FIELDS) {
    const v = policy[f.key];
    if (Array.isArray(v) ? v.length : v) bits.push(`${f.label} ${Array.isArray(v) ? v.join(', ') : v}`);
  }
  if (policy.requester_pays) bits.push(`requester pays: ${policy.requester_pays}`);
  if (policy.job_approval) bits.push(`job approval: ${policy.job_approval}`);
  return bits.join('; ');
}

// grantConsequence spells out what a new grant lets the target do.
export function grantConsequence({ target, slug, group, maxLive, gateway = '', policy = null }) {
  const info = slugInfo(slug);
  const where = info.gateway ? (gateway ? `on gateway ${gateway} only` : 'on every model gateway of this node, including ones added later') : info.kind === 'node' ? 'node-wide' : group ? `in group ${group}` : 'in EVERY group on this node — every current group and every group created later';
  const cap = info.policy ? ` Live cap: ${maxLive || 2}.` : '';
  const launch = policy && launchText(policy) ? ` Workers start with ${launchText(policy)}.` : '';
  const manual = policy?.job_approval === 'manual' ? ' Each job waits for your approval.' : '';
  const pays = policy?.requester_pays === 'off' ? ' Its workers use this node\'s provider logins (you pay).' : policy?.requester_pays === 'required' ? ' Its workers must use its own model gateway (it pays).' : '';
  return `${target} gets ${slug} (${info.what}) ${where}.${cap}${launch}${pays}${manual}`;
}

// TOKEN_TTLS are the invite lifetimes offered (the daemon allows 1s–30d).
export const TOKEN_TTLS = Object.freeze([
  { seconds: 3600, label: '1 hour' },
  { seconds: 86400, label: '24 hours' },
  { seconds: 7 * 86400, label: '7 days' },
  { seconds: 30 * 86400, label: '30 days' },
]);

// tokenState reads an enrollment token listing row: revoked, exhausted,
// expired or active.
export function tokenState(t, now = Date.now()) {
  if (t?.revoked) return 'revoked';
  if ((t?.used || 0) >= (t?.max_uses || 1)) return 'used up';
  const exp = Date.parse(t?.expires_at || '');
  if (Number.isFinite(exp) && exp <= now) return 'expired';
  return 'active';
}

// joinCommand is the CLI a joining node's operator runs with the bearer on
// stdin (never on the command line, where shell history would keep it).
export function joinCommand(masterID) {
  return `tclaude federation enroll ${masterID} --token-stdin`;
}

// profileSummary condenses a node profile definition for the profiles table.
// A definition stores its pools by ID; poolNames (id -> name) labels them.
export function profileSummary(p, poolNames = new Map()) {
  const d = p?.definition || {};
  return {
    name: p?.name || '', id: p?.id || '', revision: p?.revision || 0,
    level: d.trust_level === 'unrestricted' ? 'unrestricted' : 'restricted',
    poolIDs: Array.isArray(d.pools) ? d.pools : [],
    pools: (Array.isArray(d.pools) ? d.pools : []).map((id) => poolNames.get(id) || id),
    grants: Array.isArray(d.peer_grants) ? d.peer_grants.length : 0,
    labels: Array.isArray(d.labels) ? d.labels : [],
    bundle: !!d.config_bundle,
  };
}

// changeText renders one profile plan change ({item, before, after}); a
// pool/<id> item is named by poolNames when known.
export function changeText(c, poolNames = new Map()) {
  const v = (x) => (x == null || x === '' ? '∅' : typeof x === 'string' ? x : JSON.stringify(x));
  const item = String(c?.item || '');
  if (item.startsWith('pool/')) {
    const id = item.slice(5);
    const name = poolNames.get(id) || id;
    if (c?.after === true && c?.before !== true) return `joins pool ${name}`;
    if (c?.before === true && c?.after !== true) return `leaves pool ${name}`;
    c = { ...c, item: `pool ${name}` };
  }
  if (c?.before == null) return `${c?.item}: add ${v(c?.after)}`;
  if (c?.after == null) return `${c?.item}: remove ${v(c?.before)}`;
  return `${c?.item}: ${v(c?.before)} → ${v(c?.after)}`;
}

// grantText names a grant for confirms, flagging an unscoped group grant as
// covering every group, including future ones.
export function grantText(g) {
  const info = slugInfo(g?.slug);
  const group = g?.group_name || scopeGroup(g?.scope);
  if (group) return `${g.slug} (group ${group})`;
  if (!g?.scope && info.kind === 'group') return `${g.slug} (EVERY group, including future ones)`;
  return g?.slug || '';
}

// POOL_NAME_RE mirrors the daemon's nodeGroupNamePattern.
export const POOL_NAME_RE = /^[a-z0-9][a-z0-9._-]{0,63}$/;

// AUDIT_WINDOWS are the audit time filters (ms back from now; 0 = all).
export const AUDIT_WINDOWS = Object.freeze([
  { ms: 3600e3, label: 'last hour' },
  { ms: 86400e3, label: 'last 24 hours' },
  { ms: 7 * 86400e3, label: 'last 7 days' },
  { ms: 0, label: 'all' },
]);

// auditSince turns a window into the audit route's RFC3339 since.
export function auditSince(ms, now = Date.now()) {
  return ms ? new Date(now - ms).toISOString() : '';
}
