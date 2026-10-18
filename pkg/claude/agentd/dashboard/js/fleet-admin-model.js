import { keyTransition } from './fleet-model.js';

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
    // asks: an untrusted instance asked to be trusted (see the map).
    asks: !!p.trust_request,
  });
  const byLabel = (a, b) => a.label.localeCompare(b.label);
  // receiving names each peer's catalog groups that take agents from this
  // node (agents.receive), for the move dialog's group picker.
  const remote = Array.isArray(status.remote) ? status.remote : [];
  const receiving = new Map(remote.map((r) => [r?.peer,
    (r?.groups || []).filter((g) => g?.name && (g.caps || []).includes('agents_receive')).map((g) => g.name).sort()]));
  // catalog is each peer's catalog groups with what they allow this node.
  const catalog = new Map(remote.map((r) => [r?.peer,
    (r?.groups || []).filter((g) => g?.name).map((g) => ({ name: g.name, caps: Array.isArray(g.caps) ? g.caps : [] })).sort((a, b) => a.name.localeCompare(b.name))]));
  return {
    self: {
      id: status.instance_id,
      name: status.name || 'this node',
      fingerprint: status.fingerprint || '',
      enabled: !!status.enabled,
      hubURL: status.hub_url || '',
      hubState: status.hub?.state || (status.enabled ? 'connecting' : 'disabled'),
      hubError: status.hub?.last_error || '',
      spaces: Array.isArray(status.hub?.spaces) ? status.hub.spaces : [],
    },
    trusted: peers.filter((p) => p?.trusted && p.instance_id).map((p) => ({ ...row(p), receiving: receiving.get(p.instance_id) || [], catalog: catalog.get(p.instance_id) || [] })).sort(byLabel),
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
  { slug: 'sessions.takeover', kind: 'group', sensitive: true, what: 'take over a member agent\'s terminal: disconnect its other viewers, including your local ones (with sessions.watch or sessions.attach)' },
  { slug: 'message.direct', kind: 'group', what: 'mail to members (shares their names and ids)' },
  { slug: 'message.attachments', kind: 'group', what: 'attachments, together with message.direct on the same group' },
  { slug: 'routes.consume', kind: 'group', what: 'list and open ready group routes' },
  { slug: 'groups.members.spawn', kind: 'group', sensitive: true, policy: true, what: 'spawn workers automatically, within your launch settings and live cap' },
  { slug: 'groups.members.spawn.cwd', kind: 'group', sensitive: true, what: 'choose the directory of a spawn (with groups.members.spawn)', warning: 'Any existing directory on this machine; the worker\'s sandbox usually grants its launch directory write access.' },
  { slug: 'groups.members.spawn.harness', kind: 'group', what: 'choose the harness of a spawn (with groups.members.spawn)' },
  { slug: 'groups.members.spawn.model', kind: 'group', what: 'choose the model of a spawn (with groups.members.spawn)' },
  { slug: 'groups.members.spawn.effort', kind: 'group', what: 'choose the effort of a spawn (with groups.members.spawn)' },
  { slug: 'groups.members.spawn.profile', kind: 'group', sensitive: true, what: 'pick any enabled, non-operator-only spawn profile (with groups.members.spawn)', warning: 'A profile brings its own roles, sandbox and approval modes, environment and gateway: picking one can open what .roles or .sandbox would. Mark a profile operator-only to keep it out.' },
  { slug: 'groups.members.spawn.roles', kind: 'group', sensitive: true, what: 'choose the roles of a spawn from yours (with groups.members.spawn)', warning: 'Roles carry permissions on this node.' },
  { slug: 'groups.members.spawn.sandbox', kind: 'group', sensitive: true, what: 'add one of your sandbox profiles to a spawn (with groups.members.spawn)', warning: 'A sandbox profile can widen what the worker may reach.' },
  { slug: 'groups.members.spawn.permissions', kind: 'group', sensitive: true, what: 'choose the permission, tool and sandbox modes and per-slug permissions of a spawn (with groups.members.spawn)', warning: 'Can run the worker with no approval prompts, with sandbox rules this machine cannot enforce, and with your permissions.' },
  { slug: 'groups.members.spawn.environment', kind: 'group', sensitive: true, what: 'set environment overrides on a spawn (with groups.members.spawn)', warning: 'Environment can redirect the harness: its endpoints, config and proxy.' },
  { slug: 'groups.members.spawn.owner', kind: 'group', sensitive: true, what: 'make a spawn a group owner at birth (with groups.members.spawn)', warning: 'Owners manage the group\'s members.' },
  { slug: 'groups.members.spawn.features', kind: 'group', what: 'choose the harness features, startup context, window settings, description and task link of a spawn (with groups.members.spawn)' },
  { slug: 'groups.members.spawn.prompts', kind: 'group', what: 'choose the saved prompts of a spawn from yours, replacing profile and group defaults (with groups.members.spawn)' },
  { slug: 'groups.members.spawn.worktree', kind: 'group', sensitive: true, what: 'launch a spawn in a git worktree it creates on this machine (with groups.members.spawn and .cwd)', warning: 'Lists and creates worktrees in any repository on this machine.' },
  { slug: 'groups.members.spawn.all', kind: 'group', sensitive: true, what: 'every groups.members.spawn.* launch setting: directory, harness, model, effort, profile, roles, sandbox profile, permissions, environment, owner, features, prompts, worktree (with groups.members.spawn)', warning: 'Includes the directory, roles, sandbox profile, permissions and environment: about what a local spawn can do.' },
  { slug: 'groups.settings', kind: 'group', sensitive: true, what: 'change the group\'s settings from its dashboard: description, startup context, member cap, notifications, remote-control policy, attachment, name and nesting (its default directory, profile, saved prompts and sandbox profile also need the matching groups.members.spawn.* grant)' },
  { slug: 'groups.members.manage', kind: 'group', what: 'change member agents\' settings from its dashboard: name, tags, task link, notifications, and role and description in the group (an agent\'s own settings need it in every group the agent is in)' },
  { slug: 'groups.settings.environment', kind: 'group', sensitive: true, what: 'change the group\'s spawn environment', warning: 'Its values reach every agent spawned into the group.' },
  { slug: 'jobs.run', kind: 'group', sensitive: true, policy: true, what: 'run one-shot jobs in your allowed repositories, within the live cap' },
  { slug: 'groups.members.stop', kind: 'group', sensitive: true, what: 'stop member agents\' sessions' },
  { slug: 'groups.members.resume', kind: 'group', sensitive: true, what: 'wake (resume) member agents; with groups.members.stop also restart them' },
  { slug: 'groups.members.retire', kind: 'group', sensitive: true, what: 'retire member agents (their sessions exit)' },
  { slug: 'groups.members.clone', kind: 'group', sensitive: true, what: 'clone member agents within the group' },
  { slug: 'agent.move', kind: 'group', sensitive: true, what: 'move or teleport member agents to its node (with groups.members.retire)' },
  { slug: 'agents.receive', kind: 'scoped', what: 'move or share agents into the group' },
  { slug: 'agents.teleport.receive', kind: 'scoped', what: 'teleport agents into the group' },
  { slug: 'node.read', kind: 'node', what: 'platform, harness versions, labels and resource numbers' },
  { slug: 'node.spawn.read', kind: 'node', sensitive: true, what: 'the spawn dialog\'s catalogs, so its remote spawn dialog shows what you would apply and offers your profiles and prompts: spawn profiles, launch defaults, saved prompts, roles, sandbox profiles and harness policies (read-only)', warning: 'Spawn and sandbox profiles carry their environment values (often tokens) and paths: this shares them with the peer.' },
  { slug: 'node.spawn.defaults.manage', kind: 'node', sensitive: true, what: 'set or clear your global default spawn profile and global sandbox profile, among your existing profiles' },
  { slug: 'node.groups.create', kind: 'node', sensitive: true, what: 'create plain groups here (no directory, environment, profile or clone; templates and clones take unrestricted trust)' },
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

// GRANT_CATEGORIES group PEER_SLUGS for the add-grants picker, in display
// order.
export const GRANT_CATEGORIES = Object.freeze(['Read & status', 'Messaging', 'Terminals', 'Spawn & jobs', 'Agent lifecycle', 'Group admin', 'Node admin', 'Models']);

const LIFECYCLE = new Set(['groups.members.stop', 'groups.members.resume', 'groups.members.retire', 'groups.members.clone', 'agent.move', 'agents.receive', 'agents.teleport.receive']);
const GROUP_ADMIN = new Set(['groups.settings', 'groups.settings.environment', 'groups.members.manage', 'node.groups.create']);

export function slugCategory(slug) {
  if (slug.startsWith('groups.members.spawn') || slug.startsWith('node.spawn.') || slug === 'jobs.run') return 'Spawn & jobs';
  if (slug.startsWith('sessions.') && slug !== 'sessions.read') return 'Terminals';
  if (slug.startsWith('message.') || slug === 'routes.consume') return 'Messaging';
  if (slug.startsWith('models.')) return 'Models';
  if (LIFECYCLE.has(slug)) return 'Agent lifecycle';
  if (GROUP_ADMIN.has(slug)) return 'Group admin';
  if (slug.endsWith('.read')) return 'Read & status';
  return 'Node admin';
}

// grantVisibleSettings are the launch settings a policy grant of slug takes.
export function grantVisibleSettings(slug) {
  return [
    ...LAUNCH_FIELDS.filter((f) => !f.slugs || f.slugs.includes(slug)).map((f) => f.key),
    ...(slug === 'groups.members.spawn' ? ['requester_pays'] : []),
    ...(slug === 'jobs.run' ? ['job_approval'] : []),
  ];
}

// planGrants turns an add-grants selection into one grant body per
// permission and scope. Group permissions get one grant per chosen group, or
// one unscoped grant when allGroups; a permission that needs a group and has
// none is listed in missing. An existing direct grant with the same
// permission and scope is skipped, except a spawn/job grant, which is
// updated: the daemon replaces it wholesale, so its other launch settings
// are carried over and only the ones typed here change.
export function planGrants({ target, slugs, groups = [], allGroups = false, gateway = '', maxLive = 2, launch = {}, rows = [] }) {
  const out = []; const missing = [];
  for (const slug of slugs) {
    const info = slugInfo(slug);
    let scopes;
    if (info.gateway) scopes = [gateway ? `http_proxy=${gateway}` : ''];
    else if (info.kind === 'node') scopes = [''];
    else if (info.kind === 'group' && allGroups) scopes = [''];
    else if (groups.length) scopes = groups.map((g) => `group=${g}`);
    else { missing.push(slug); continue; }
    for (const scope of scopes) {
      const group = scopeGroup(scope);
      const gw = scope.startsWith('http_proxy=') ? scope.slice(11) : '';
      // A deleted group's grant has no name, so an unscoped grant matches
      // only an empty scope, never a nameless group row.
      const existing = rows.find((r) => !r.pool && !r.deletedGroup && r.slug === slug && (gw ? r.scope === scope : group ? r.group === group : !r.scope)) || null;
      const body = { peer: target, slug, scope };
      if (info.policy) {
        const visible = new Set(grantVisibleSettings(slug));
        const set = {};
        for (const [k, v] of Object.entries(launch)) {
          if (v === undefined || !visible.has(k)) continue;
          set[k] = k === 'allowed_profiles' ? String(v).split(',').map((x) => x.trim()).filter(Boolean) : String(v).trim();
        }
        body.spawn_policy = { ...(existing?.policy || {}), ...set, max_live: Math.max(1, Number(maxLive) || 2) };
        for (const k of Object.keys(body.spawn_policy)) { const v = body.spawn_policy[k]; if (v === '' || (Array.isArray(v) && !v.length)) delete body.spawn_policy[k]; }
      }
      out.push({ key: `${slug}|${scope}`, slug, scope, group, gateway: gw, body, existing, skip: !!existing && !info.policy });
    }
  }
  return { grants: out, missing };
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
