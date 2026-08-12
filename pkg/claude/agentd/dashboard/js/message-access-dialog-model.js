const SUDO_BLOCKLIST = new Set(['permissions.grant', 'permissions.revoke']);

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

export function sudoSlugRows(snapshot) {
  return (snapshot?.slugs || []).map((slug) => ({
    ...slug,
    blocked: SUDO_BLOCKLIST.has(slug.slug),
  }));
}

export function permissionSeed(snapshot, descriptor) {
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
  return ['agent', 'group', 'buffer'].includes(descriptor?.mode);
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
  if (descriptor.mode === 'agent') {
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

export function permissionRows(snapshot, descriptor, selection) {
  const defaults = new Set(snapshot?.permissions?.defaults || []);
  const groups = membershipGroups(snapshot, descriptor);
  const groupGrants = new Map();
  for (const groupName of groups) {
    const group = (snapshot?.groups || []).find((item) => item.name === groupName);
    for (const grant of group?.permissions || []) {
      const slug = permissionGrantSlug(grant);
      if (!groupGrants.has(slug)) groupGrants.set(slug, []);
      groupGrants.get(slug).push(groupName);
    }
  }
  const ownedGroups = descriptor.mode === 'agent'
    ? ((snapshot?.agents || []).find((item) => item.conv_id === descriptor.conv)?.owned_groups || [])
    : descriptor.ownsGroup && descriptor.group ? [descriptor.group] : [];
  return [...(snapshot?.slugs || [])].sort((a, b) => a.slug.localeCompare(b.slug)).map((slug) => {
    const effect = selection[slug.slug] || 'default';
    const sources = [];
    if (effect === 'grant') sources.push('agent override');
    if (effect === 'default' && defaults.has(slug.slug)) sources.push('global default');
    if (effect === 'default' && groupGrants.has(slug.slug)) {
      sources.push(`group: ${groupGrants.get(slug.slug).join(', ')}`);
    }
    if (effect === 'default' && slug.owner_implied && ownedGroups.length) {
      sources.push(ownerSource(slug.scope_dims, ownedGroups));
    }
    if (effect === 'default' && slug.member_implied && descriptor.mode === 'agent' && groups.length) {
      sources.push(`member: ${groups.join(', ')}`);
    }
    const granted = descriptor.mode === 'group' ? effect === 'grant' : effect !== 'deny' && sources.length > 0;
    return { ...slug, effect, granted, sources, ownedGroups, inDefault: defaults.has(slug.slug) };
  });
}
