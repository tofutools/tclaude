// Compatibility seam for delegated launchers outside the Preact transaction
// root. The registered state owns one frozen descriptor at a time; callers get
// a promise that resolves only when that exact dialog finishes or is canceled.
let controller = null;

export function registerTransactionDialogController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('transaction dialogs are not ready');
  return controller;
}

export function openTransactionDialog(descriptor) {
  return requireController().open(descriptor);
}

export function openRetireAgentDialog(conv, label = '') {
  return openTransactionDialog({ kind: 'retire-agent', conv, label });
}

export function openShutdownAgentDialog(agent, label = '') {
  return openTransactionDialog({ kind: 'shutdown-agent', agent, label });
}

export function openDeleteAgentDialog(agent, label = '') {
  return openTransactionDialog({ kind: 'delete-agent', agent, label });
}

function deleteGroupMemberKey(member) {
  return String(member?.agent_id || member?.conv_id || '').trim();
}

function deleteGroupMemberships(groups, member) {
  const agentID = String(member?.agent_id || '').trim();
  const convID = String(member?.conv_id || '').trim();
  return groups.filter((entry) => (entry.members || []).some((candidate) => {
    const candidateAgent = String(candidate?.agent_id || '').trim();
    const candidateConv = String(candidate?.conv_id || '').trim();
    return (agentID && candidateAgent === agentID) || (convID && candidateConv === convID);
  })).map((entry) => ({
    name: String(entry.name || ''),
    parent: String(entry.parent || ''),
  }));
}

// buildDeleteGroupDescriptor is the snapshot-to-plan adapter. It captures the
// exact target roster plus every direct group membership (including the nested
// position of those groups) once, before the keyed transaction owner mounts.
// Later snapshot publishes therefore cannot silently flip a member between the
// default retire and detach-only cohorts while the destructive plan is open.
export function buildDeleteGroupDescriptor(snapshot, groupName) {
  const groups = Array.isArray(snapshot?.groups) ? snapshot.groups : [];
  const group = String(groupName || '');
  const target = groups.find((entry) => entry?.name === group);
  const members = (target?.members || []).map((member) => {
    const memberships = deleteGroupMemberships(groups, member);
    const otherGroups = memberships.filter((entry) => entry.name !== group);
    const selector = deleteGroupMemberKey(member);
    return {
      agent_id: String(member?.agent_id || ''),
      conv_id: String(member?.conv_id || ''),
      selector,
      title: String(member?.title || ''),
      status: member?.online
        ? String(member?.state?.status || 'online') : 'offline',
      role: String(member?.role || ''),
      memberships,
      otherGroups,
      onlyThisGroup: otherGroups.length === 0,
      defaultRetire: otherGroups.length === 0,
    };
  }).filter((member) => member.selector);
  return {
    kind: 'delete-group',
    group,
    parent: String(target?.parent || ''),
    members,
  };
}

export function openDeleteGroupDialog(snapshot, group) {
  return openTransactionDialog(buildDeleteGroupDescriptor(snapshot, group));
}

function uniqueStrings(values) {
  const seen = new Set();
  const result = [];
  for (const value of values || []) {
    const normalized = String(value || '').trim();
    if (!normalized || seen.has(normalized)) continue;
    seen.add(normalized);
    result.push(normalized);
  }
  return result;
}

// The launcher freezes the running roster the human is choosing from. Candidate
// identity remains conv-keyed for selection/deduping, while the eventual window
// request leads with stable agent_id where one exists. Roles and groups are
// deliberately sets: one agent may occupy several buckets at once.
export function normalizeWindowSelectionCandidates(candidates) {
  const positions = new Map();
  const result = [];
  for (const candidate of candidates || []) {
    const conv = String(candidate?.conv_id || '').trim();
    if (!conv) continue;
    const normalized = {
      agent_id: String(candidate?.agent_id || '').trim(),
      conv_id: conv,
      title: String(candidate?.title || ''),
      roles: uniqueStrings(candidate?.roles),
      groups: uniqueStrings(candidate?.groups),
    };
    const position = positions.get(conv);
    if (position === undefined) {
      positions.set(conv, result.length);
      result.push(normalized);
      continue;
    }
    // Duplicate snapshot rows must not duplicate the checkbox, but neither may
    // dedupe erase a role/group bucket or the only stable selector/title.
    const existing = result[position];
    result[position] = {
      agent_id: existing.agent_id || normalized.agent_id,
      conv_id: conv,
      title: existing.title || normalized.title,
      roles: uniqueStrings([...existing.roles, ...normalized.roles]),
      groups: uniqueStrings([...existing.groups, ...normalized.groups]),
    };
  }
  return result;
}

// buildWindowSelectionDescriptor is the snapshot-to-dialog adapter. It runs
// once per click, before Preact takes ownership, so later poll publishes cannot
// add, remove, retitle, or retarget candidates under an open picker.
export function buildWindowSelectionDescriptor(
  snapshot, scope, groupName = '', webTerminal = false,
) {
  const snap = snapshot || {};
  const group = scope === 'group' ? String(groupName || '') : '';
  const rolesByConv = new Map();
  const groupsByConv = new Map();
  for (const entry of (snap.groups || [])) {
    for (const member of (entry.members || [])) {
      const conv = String(member?.conv_id || '').trim();
      if (!conv) continue;
      if (!groupsByConv.has(conv)) groupsByConv.set(conv, []);
      groupsByConv.get(conv).push(entry.name);
      if (member.role) {
        if (!rolesByConv.has(conv)) rolesByConv.set(conv, []);
        rolesByConv.get(conv).push(member.role);
      }
    }
  }

  const candidates = [];
  if (scope === 'group') {
    const entry = (snap.groups || []).find((item) => item.name === group);
    for (const member of (entry?.members || [])) {
      if (!member?.online) continue;
      candidates.push({
        agent_id: member.agent_id || '',
        conv_id: member.conv_id,
        title: member.title || '',
        roles: member.role ? [member.role] : [],
        groups: [group],
      });
    }
  } else {
    for (const agent of (snap.agents || [])) {
      if (!agent?.online) continue;
      candidates.push({
        agent_id: agent.agent_id || '',
        conv_id: agent.conv_id,
        title: agent.title || '',
        roles: rolesByConv.get(String(agent.conv_id || '').trim()) || [],
        groups: groupsByConv.get(String(agent.conv_id || '').trim()) || [],
      });
    }
  }
  return {
    kind: 'window-selection',
    scope: scope === 'group' ? 'group' : 'all',
    ...(scope === 'group' ? { group } : {}),
    webTerminal: webTerminal === true,
    candidates: normalizeWindowSelectionCandidates(candidates),
  };
}

export function openWindowSelectionDialog(descriptor) {
  return openTransactionDialog({
    ...descriptor,
    kind: 'window-selection',
    candidates: normalizeWindowSelectionCandidates(descriptor?.candidates),
  });
}

// Bulk preview launchers cross the same imperative → keyed owner seam as the
// single-agent transactions. Candidate identity is conv-keyed even when the
// ungrouped endpoint later prefers a stable agent selector: conv_id is the
// snapshot roster key and the only safe dedupe domain at open time.
export function dedupeRetireCandidates(candidates) {
  const seen = new Set();
  const result = [];
  for (const candidate of candidates || []) {
    const conv = String(candidate?.conv_id || '').trim();
    if (!conv || seen.has(conv)) continue;
    seen.add(conv);
    result.push({ ...candidate, conv_id: conv });
  }
  return result;
}

export function openGroupRetirePreviewDialog(group, status, candidates) {
  return openTransactionDialog({
    kind: 'retire-group-preview',
    group,
    status,
    candidates: dedupeRetireCandidates(candidates),
  });
}

export function openUngroupedRetirePreviewDialog(candidates) {
  return openTransactionDialog({
    kind: 'retire-ungrouped-preview',
    candidates: dedupeRetireCandidates(candidates),
  });
}

// Delete-retired is loaded from the complete retired endpoint before it crosses
// this seam. Normalize the renderer's exact data shape, conv-dedupe defensively,
// and sort newest-first locally so neither endpoint ordering nor a later caller
// mutation can change the roster the human is reviewing. Invalid/missing stamps
// sort after valid stamps; their separate age-filter semantics live with the
// controlled form that owns the current filter value.
export function normalizeDeleteRetiredCandidates(candidates) {
  const seen = new Set();
  const result = [];
  for (const candidate of candidates || []) {
    const conv = String(candidate?.conv_id || '').trim();
    if (!conv || seen.has(conv)) continue;
    seen.add(conv);
    result.push({
      agent_id: String(candidate?.agent_id || '').trim(),
      conv_id: conv,
      title: String(candidate?.title || ''),
      retired_at: String(candidate?.retired_at || ''),
      retired_by: String(candidate?.retired_by_display || candidate?.retired_by || ''),
      online: candidate?.online === true,
    });
  }
  result.sort((a, b) => {
    const aTime = Date.parse(a.retired_at);
    const bTime = Date.parse(b.retired_at);
    const aValid = !Number.isNaN(aTime);
    const bValid = !Number.isNaN(bTime);
    if (aValid && bValid && aTime !== bTime) return bTime - aTime;
    if (aValid !== bValid) return aValid ? -1 : 1;
    return 0;
  });
  return result;
}

export function openDeleteRetiredPreviewDialog(candidates) {
  return openTransactionDialog({
    kind: 'delete-retired-preview',
    candidates: normalizeDeleteRetiredCandidates(candidates),
  });
}

// DnD owns optimistic drag presentation, while the transaction root owns the
// authoritative mutation refresh. Only results that did not already complete
// and refresh need the DnD caller to reconcile the cancelled/failed gesture.
export function retireResultNeedsReconcile(result) {
  return !(result?.ok || (result?.dangling && result.removed));
}
