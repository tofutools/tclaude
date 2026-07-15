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

export function sudoByConv(snapshot) {
  const out = new Map();
  for (const grant of snapshot?.sudo || []) {
    if (!out.has(grant.conv_id)) out.set(grant.conv_id, []);
    out.get(grant.conv_id).push(grant);
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
    return Object.fromEntries((descriptor.grants || []).map((slug) => [slug, 'grant']));
  }
  return { ...(descriptor.overrides || {}) };
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

export function permissionRows(snapshot, descriptor, selection) {
  const defaults = new Set(snapshot?.permissions?.defaults || []);
  const groups = membershipGroups(snapshot, descriptor);
  const groupGrants = new Map();
  for (const groupName of groups) {
    const group = (snapshot?.groups || []).find((item) => item.name === groupName);
    for (const slug of group?.permissions || []) {
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
      sources.push(`owner: ${ownedGroups.join(', ')}`);
    }
    const granted = descriptor.mode === 'group' ? effect === 'grant' : effect !== 'deny' && sources.length > 0;
    return { ...slug, effect, granted, sources, ownedGroups, inDefault: defaults.has(slug.slug) };
  });
}
