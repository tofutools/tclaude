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
  });
  const byLabel = (a, b) => a.label.localeCompare(b.label);
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
    trusted: peers.filter((p) => p?.trusted && p.instance_id).map(row).sort(byLabel),
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
  { slug: 'agents.receive', kind: 'scoped', what: 'move or share agents into the group' },
  { slug: 'agents.teleport.receive', kind: 'scoped', what: 'teleport agents into the group' },
  { slug: 'node.read', kind: 'node', what: 'platform, harness versions, labels and resource numbers' },
  { slug: 'node.harnesses.read', kind: 'node', what: 'harness availability and versions' },
  { slug: 'costs.read', kind: 'node', what: 'the complete node-wide cost collection' },
  { slug: 'federation.audit.read', kind: 'node', what: 'the complete node-wide dashboard audit log' },
  { slug: 'config.offer', kind: 'node', what: 'offer config bundles for your review' },
  { slug: 'approvals.answer', kind: 'node', sensitive: true, what: 'answer access requests once while selected as your away cover' },
  { slug: 'node.update', kind: 'node', sensitive: true, what: 'update tclaude on this node' },
  { slug: 'node.harnesses.install', kind: 'node', sensitive: true, what: 'install and update harnesses on this node' },
  { slug: 'node.credentials.receive', kind: 'node', sensitive: true, what: 'push its harness credentials here — this node\'s agents then act as that operator with the provider' },
  { slug: 'models.proxy', kind: 'node', what: 'use this node\'s model gateway (gateway scopes: CLI)' },
  { slug: 'models.proxy.leased', kind: 'node', what: 'leased model gateway access' },
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

// grantConsequence spells out what a new grant lets the target do.
export function grantConsequence({ target, slug, group, maxLive }) {
  const info = slugInfo(slug);
  const where = info.kind === 'node' ? 'node-wide' : group ? `in group ${group}` : 'in EVERY group on this node — every current group and every group created later';
  const cap = info.policy ? ` Live cap: ${maxLive || 2}.` : '';
  return `${target} gets ${slug} (${info.what}) ${where}.${cap}`;
}
