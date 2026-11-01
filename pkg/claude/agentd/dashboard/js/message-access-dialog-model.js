import { openMemberPRs, prReviewState } from './pr-review-model.js';

// What the sudo editor assumes until GET /api/sudo/policy answers, or when
// another node's older daemon has no such route. The server decides either way.
export const DEFAULT_SUDO_POLICY = Object.freeze({
  max_seconds: 3600, default_seconds: 300,
  refused: Object.freeze({
    'permissions.grant': 'Refused: it hands out permissions itself, so a grant made during the window would outlive it.',
    'permissions.revoke': 'Refused: it hands out permissions itself, so a grant made during the window would outlive it.',
  }),
});

export const SUDO_DURATION_PRESETS = Object.freeze(['5m', '15m', '30m', '1h']);

const DURATION_UNITS = { h: 3600, m: 60, s: 1 };

// sudoDurationSeconds reads the Go durations the server accepts, for the
// editor's own hints. null when it is not one the editor can read.
export function sudoDurationSeconds(text) {
  const value = String(text || '').trim();
  if (!/^(\d+(\.\d+)?[hms])+$/.test(value)) return null;
  let total = 0;
  for (const [, amount, unit] of value.matchAll(/(\d+(?:\.\d+)?)([hms])/g)) total += Number(amount) * DURATION_UNITS[unit];
  return total > 0 ? total : null;
}

// sudoDurationText writes whole seconds as the shortest Go duration.
export function sudoDurationText(seconds) {
  const total = Math.max(0, Math.round(seconds));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const rest = total % 60;
  return `${hours ? `${hours}h` : ''}${minutes ? `${minutes}m` : ''}${rest || !total ? `${rest}s` : ''}`;
}

export function sudoSecondsLeft(grant, now) {
  const at = Date.parse(grant?.expires_at || '');
  if (Number.isFinite(at)) return Math.max(0, Math.round((at - now) / 1000));
  return Math.max(0, Number(grant?.remaining_seconds) || 0);
}

// sudoGrantScope reads a snapshot row's stored scope: {} when unscoped.
export function sudoGrantScope(grant) {
  try { return JSON.parse(grant?.scope_json || '{}') || {}; } catch (_) { return {}; }
}

const SLUG_AREAS = [
  ['groups.members', 'Group members'], ['groups.settings', 'Group settings'], ['groups', 'Groups'],
  ['agents', 'Agents'], ['agent', 'Agents'], ['self', 'The agent itself'],
  ['human', 'Operator'], ['approvals', 'Operator'], ['message', 'Messaging'],
  ['node', 'Node and host'], ['host', 'Node and host'], ['jobs', 'Node and host'], ['sessions', 'Sessions'],
  ['process', 'Processes'], ['templates', 'Processes'], ['triggers', 'Processes'],
  ['sandbox', 'Sandbox'], ['sandbox-profiles', 'Sandbox'], ['config', 'Config'], ['settings', 'Config'],
];

export function slugArea(slug) {
  const text = String(slug || '');
  const match = SLUG_AREAS.find(([prefix]) => text === prefix || text.startsWith(`${prefix}.`));
  if (match) return match[1];
  const head = text.split('.')[0];
  return head.charAt(0).toUpperCase() + head.slice(1);
}

// rowsByArea orders the rows area by area, keeping each area alphabetical.
export function rowsByArea(rows) {
  const areas = new Map();
  for (const row of rows) {
    const area = slugArea(row.slug);
    if (!areas.has(area)) areas.set(area, []);
    areas.get(area).push({ ...row, area });
  }
  return [...areas.keys()].sort((left, right) => left.localeCompare(right)).flatMap((area) => areas.get(area));
}

export function snapshotAgents(snapshot) {
  const seen = new Set();
  const rows = [];
  for (const agent of [...(snapshot?.agents || []), ...(snapshot?.ungrouped || [])]) {
    const key = agent.agent_id || agent.conv_id;
    if (!key || seen.has(key)) continue;
    seen.add(key);
    const memberships = [];
    for (const group of snapshot?.groups || []) {
      const member = (group.members || []).find((item) =>
        (agent.agent_id && item.agent_id === agent.agent_id) || item.conv_id === agent.conv_id);
      if (member) memberships.push({ group: group.name, role: member.role || '', descr: member.descr || '' });
    }
    rows.push({ ...agent, memberships });
  }
  return rows.sort((left, right) => {
    if (!!left.online !== !!right.online) return left.online ? -1 : 1;
    return (left.title || '').localeCompare(right.title || '');
  });
}

export function agentCandidates(snapshot, { includeOffline = false, query = '' } = {}) {
  const needle = String(query || '').trim().toLowerCase();
  return snapshotAgents(snapshot).filter((agent) => {
    if (!includeOffline && !agent.online) return false;
    if (!needle) return true;
    return [
      agent.title, agent.agent_id, agent.conv_id,
      ...(agent.groups || []),
      ...agent.memberships.flatMap((item) => [item.group, item.role, item.descr]),
    ].some((value) => String(value || '').toLowerCase().includes(needle));
  });
}

export function senderOnline(snapshot, agentID, convID) {
  const rows = [...(snapshot?.agents || []), ...(snapshot?.ungrouped || [])];
  return agentID
    ? rows.some((agent) => agent.online && agent.agent_id === agentID)
    : rows.some((agent) => agent.online && agent.conv_id === convID);
}

export function groupMembers(snapshot, groupName) {
  return ((snapshot?.groups || []).find((group) => group.name === groupName)?.members || [])
    .map((member) => ({ ...member, key: member.agent_id || member.conv_id }));
}

// RECIPIENT_PICKS are the group message dialog's quick selectors. Each names
// the members it matches; the dialog replaces, extends (OR) or narrows (AND)
// the ticked set with it. The PR picks look only at PRs still in flight.
export const RECIPIENT_PICKS = Object.freeze([
  { key: 'online', label: 'online', title: 'Members with a live session', test: (member) => !!member.online },
  { key: 'offline', label: 'offline', title: 'Members without a live session — a message waits in their inbox', test: (member) => !member.online },
  { key: 'pr', label: 'with PR', title: 'Members with an open or draft PR', test: (member) => openMemberPRs(member).length > 0 },
  { key: 'no-pr', label: 'no PR', title: 'Members without an open or draft PR', test: (member) => openMemberPRs(member).length === 0 },
  { key: 'ci-failing', label: 'CI failing', title: 'Members with an open PR whose checks are failing',
    test: (member) => openMemberPRs(member).some((pr) => pr.checks?.state === 'failing') },
  { key: 'changes', label: 'changes requested', title: 'Members with an open PR a reviewer asked to change',
    test: (member) => openMemberPRs(member).some((pr) => prReviewState(pr.reviews) === 'changes') },
  { key: 'approved', label: 'approved', title: 'Members with an approved open PR',
    test: (member) => openMemberPRs(member).some((pr) => prReviewState(pr.reviews) === 'approved') },
]);

// combinePick applies one quick selector to the current recipients: 'add'
// unions, 'narrow' intersects, anything else replaces.
export function combinePick(members, current, pick, how = 'only') {
  const has = new Set(current.map((member) => member.key));
  return members.filter((member) => {
    if (how === 'add') return has.has(member.key) || pick.test(member);
    if (how === 'narrow') return has.has(member.key) && pick.test(member);
    return pick.test(member);
  });
}

export function groupsForPicker(snapshot, scopeGroup = '') {
  if (scopeGroup) return [scopeGroup];
  return (snapshot?.groups || []).map((group) => group.name).sort();
}

export function sudoByAgent(snapshot) {
  const out = new Map();
  for (const grant of snapshot?.sudo || []) {
    const key = grant.agent_id || grant.conv_id;
    if (!out.has(key)) out.set(key, []);
    out.get(key).push(grant);
  }
  return out;
}

// stewardAccessFor is where a live Steward's access comes from, for its
// editors, or null for every other agent. The daemon names the live Stewards,
// so an agent that merely has the name gets the ordinary editor.
export function stewardAccessFor(snapshot, conv) {
  const access = snapshot?.steward_access;
  if (!conv || !access?.convs?.includes(conv)) return null;
  return {
    config: new Set(access.config || []),
    carried: access.carried || {},
  };
}

export function permissionSeed(snapshot, descriptor) {
  if (descriptor.mode === 'sudo') return {};
  if (descriptor.mode === 'agent') {
    return { ...((snapshot?.permissions?.overrides || {})[descriptor.conv] || {}) };
  }
  if (descriptor.mode === 'group') {
    return Object.fromEntries((descriptor.grants || []).map((grant) => [permissionGrantSlug(grant), 'grant']));
  }
  return Object.fromEntries(Object.entries(descriptor.overrides || {})
    .map(([slug, override]) => [slug, permissionOverrideEffect(override)]));
}

function copyScope(scope) {
  return Object.fromEntries(Object.entries(scope || {})
    .map(([dim, values]) => [dim, [...(values || [])]]));
}

function permissionGrantSlug(grant) {
  return typeof grant === 'string' ? grant : String(grant?.slug || '');
}

function permissionOverrideEffect(override) {
  return typeof override === 'string' ? override : String(override?.effect || 'default');
}

function permissionOverrideScope(override) {
  return typeof override === 'object' && override ? override.scope : null;
}

// permissionScopeSeed is the scope half of permissionSeed: slug → {dim:
// [matchers]}. Every launcher uses the same union wire shape, so the shared
// editor can read live-agent scopes, group grant scopes, and buffered spawn or
// profile override scopes without separate UI implementations.
export function permissionScopeSeed(snapshot, descriptor) {
  let stored = {};
  if (descriptor.mode === 'sudo') return {};
  if (descriptor.mode === 'agent') {
    stored = (snapshot?.permissions?.scopes || {})[descriptor.conv] || {};
  } else if (descriptor.mode === 'group') {
    stored = descriptor.scopes || {};
  } else {
    stored = Object.fromEntries(Object.entries(descriptor.overrides || {})
      .map(([slug, override]) => [slug, permissionOverrideScope(override)])
      .filter(([, scope]) => scope && Object.keys(scope).length));
  }
  const out = {};
  for (const [slug, scope] of Object.entries(stored)) {
    out[slug] = copyScope(scope);
  }
  return out;
}

// unreadableScopeSlugs is the set of this agent's grants whose stored scope
// the daemon could not decode. Such a grant authorizes NOTHING at the gate, so
// the editor must say exactly that: rendering it as unscoped would be the
// widest possible misreading, and saving the row would make the misreading
// true. The daemon refuses that overwrite too — this is the half that stops an
// operator walking into it.
export function unreadableScopeSlugs(snapshot, descriptor) {
  if (descriptor.mode === 'group') return new Set(descriptor.unreadable || []);
  if (descriptor.mode !== 'agent') return new Set();
  return new Set((snapshot?.permissions?.unreadable_scopes || {})[descriptor.conv] || []);
}

// All three launch modes persist the same scope shape: immediately for agents
// and groups, or into the buffered birth-time override for spawns/profiles.
export function scopeSupported(descriptor) {
  return ['agent', 'group', 'buffer', 'sudo'].includes(descriptor?.mode);
}

// scopeDimOptions answers the picker's "what can I choose here" for ONE
// dimension, from what the daemon advertised. A dimension the daemon knows but
// has no catalogue for answers with its selectors and no values, which the
// picker renders as free text — the same thing the CLI's --scope accepts. That
// is what lets a dimension introduced by a later phase become editable with no
// change here.
export function scopeDimOptions(snapshot, dim) {
  const advertised = (snapshot?.permissions?.scope_dim_options || {})[dim] || {};
  return {
    values: [...(advertised.values || [])],
    selectors: [...(advertised.selectors || [])],
  };
}

// scopeChips flattens a scope into the row's read-at-a-glance chips, in the
// same dimension order the daemon's provenance line uses so the editor and
// `permissions ls` never disagree about what a grant says.
// scopeDimRows lists the dimensions the drawer must offer: the ones the slug
// declares, PLUS any the stored scope already carries. The second half is not
// decoration — a dimension a slug stopped declaring (a registry change under a
// live grant) would otherwise be visible in the chips, uneditable in the
// drawer, and rejected by the daemon on every save, leaving the operator with
// a dialog they cannot save and no way to fix it from the dashboard.
export function scopeDimRows(row, scope) {
  const declared = [...(row?.scope_dims || [])];
  const stored = Object.keys(scope || {}).filter((dim) => !declared.includes(dim)).sort();
  return [
    ...declared.map((dim) => ({ dim, declared: true })),
    ...stored.map((dim) => ({ dim, declared: false })),
  ];
}

export function scopeChips(scope) {
  return Object.entries(scope || {})
    .filter(([, values]) => (values || []).length)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([dim, values]) => ({ dim, values: [...values], label: `${dim}=${values.join(', ')}` }));
}

function membershipGroups(snapshot, descriptor) {
  if (descriptor.mode === 'agent' || descriptor.mode === 'sudo') {
    const agent = (snapshot?.agents || []).find((item) => item.conv_id === descriptor.conv);
    const groups = new Set(agent?.groups || []);
    for (const group of snapshot?.groups || []) {
      if ((group.members || []).some((member) =>
        (agent?.agent_id && member.agent_id === agent.agent_id) || member.conv_id === descriptor.conv)) {
        groups.add(group.name);
      }
    }
    return [...groups];
  }
  if (descriptor.mode === 'buffer' && descriptor.group && descriptor.group !== 'the spawn group') {
    return [descriptor.group];
  }
  return [];
}

export function ownerSource(scopeDims, ownedGroups) {
  if ((scopeDims || []).includes('group')) return `owner grant: ${ownedGroups.join(', ')}`;
  return 'owner grant: global';
}

// In the sudo editor the rows show what the agent holds now, so selection is
// its standing overrides rather than the slugs being elevated.
export function permissionRows(snapshot, descriptor, selection) {
  const defaults = new Set(snapshot?.permissions?.defaults || []);
  const stewardLevels = new Set(descriptor.stewardLevels || []);
  const groups = membershipGroups(snapshot, descriptor);
  const groupGrants = new Map();
  const roleGrants = new Map();
  for (const grant of descriptor.roleGrants || []) {
    const slug = permissionGrantSlug(grant);
    if (!slug) continue;
    if (!roleGrants.has(slug)) roleGrants.set(slug, []);
    roleGrants.get(slug).push({ role: String(grant?.role || ''), scope: String(grant?.scope || '') });
  }
  for (const groupName of groups) {
    const group = (snapshot?.groups || []).find((item) => item.name === groupName);
    for (const grant of group?.permissions || []) {
      const slug = permissionGrantSlug(grant);
      if (!groupGrants.has(slug)) groupGrants.set(slug, []);
      groupGrants.get(slug).push(groupName);
    }
  }
  const liveAgent = descriptor.mode === 'agent' || descriptor.mode === 'sudo';
  const ownedGroups = liveAgent
    ? ((snapshot?.agents || []).find((item) => item.conv_id === descriptor.conv)?.owned_groups || [])
    : descriptor.ownsGroup && descriptor.group ? [descriptor.group] : [];
  return [...(snapshot?.slugs || [])].sort((a, b) => a.slug.localeCompare(b.slug)).map((slug) => {
    const effect = selection[slug.slug] || 'default';
    const sources = [];
    if (effect === 'grant') sources.push(descriptor.stewardLevels ? 'stored' : 'agent override');
    if (effect === 'default' && stewardLevels.has(slug.slug)) sources.push('Reads and Writes');
    if (effect === 'default' && defaults.has(slug.slug)) sources.push('global default');
    if (effect === 'default' && groupGrants.has(slug.slug)) {
      sources.push(`group: ${groupGrants.get(slug.slug).join(', ')}`);
    }
    if (effect === 'default' && roleGrants.has(slug.slug)) {
      for (const grant of roleGrants.get(slug.slug)) {
        sources.push(`role: ${grant.role}${grant.scope ? ` [${grant.scope}]` : ''}`);
      }
    }
    if (effect === 'default' && slug.owner_implied && ownedGroups.length) {
      sources.push(ownerSource(slug.scope_dims, ownedGroups));
    }
    if (effect === 'default' && slug.member_implied && liveAgent && groups.length) {
      sources.push(`member: ${groups.join(', ')}`);
    }
    const granted = descriptor.mode === 'group' ? effect === 'grant' : effect !== 'deny' && sources.length > 0;
    return { ...slug, effect, granted, sources, ownedGroups, inDefault: defaults.has(slug.slug) };
  });
}
