// group-tree-activity.js — activity-bot membership at one disclosure boundary.
//
// A nested group's members belong on the leaf-most group header currently
// visible to the operator. An open parent therefore keeps only its direct
// members in its header while each visible child carries its own activity. A
// closed parent hides that whole child tree, so its header absorbs every member
// in the hidden subtree.
//
// Kept pure and DOM-free so the disclosure rule can be tested independently of
// groups-list.js. Members are deduped by conv_id, matching aggregateActivity's
// global-dashboard semantics: one agent may belong to several nested groups but
// must not inflate a rolled-up count. Fixtures/legacy rows without a conv_id
// remain distinct.

export function mergeActivityMembers(memberLists) {
  const merged = [];
  const seen = new Set();
  for (const list of (memberLists || [])) {
    for (const member of (list || [])) {
      const id = member && member.conv_id;
      if (id && seen.has(id)) continue;
      if (id) seen.add(id);
      merged.push(member);
    }
  }
  return merged;
}

// disclosurePreference preserves the distinction between a default-closed
// group (no stored value) and an explicitly folded default-open group ('0').
// Reconciliation can emit synthetic toggle events for recreated <details>, so
// only a toggle known to come from an operator/programmatic command may create
// the explicit-close sentinel.
export function disclosurePreference(open, intentional, previous = null) {
  if (!intentional) return previous;
  return open ? '1' : '0';
}

// groupActivityAtDisclosure combines a group's direct members with the
// already-computed subtree membership of its children. subtreeMembers always
// travels upward for a folded ancestor; headerMembers follows this group's own
// disclosure state.
export function groupActivityAtDisclosure(members, childSubtreeMembers, open) {
  const directMembers = mergeActivityMembers([members]);
  const subtreeMembers = mergeActivityMembers([directMembers, ...(childSubtreeMembers || [])]);
  return {
    headerMembers: open ? directMembers : subtreeMembers,
    subtreeMembers,
  };
}

// groupActivityPlacement computes the roster represented by every real group
// header in the current rendered tree. Only groups present in this render can
// parent another group; that mirrors groups-view-model.js's dangling-parent rule.
// Virtual groups stay outside the nested tree. The cycle/orphan guards mirror
// the renderer too, so corrupt linkage cannot recurse forever or erase a row.
export function groupActivityPlacement(groups, isOpen) {
  const rows = groups || [];
  const present = new Set(rows.filter((group) => !group.virtual).map((group) => group.name));
  const childrenByParent = new Map();
  const roots = [];
  for (const group of rows) {
    if (group.virtual) continue;
    const parent = group.parent && present.has(group.parent) && group.parent !== group.name
      ? group.parent : '';
    if (!parent) {
      roots.push(group);
      continue;
    }
    const children = childrenByParent.get(parent) || [];
    children.push(group);
    childrenByParent.set(parent, children);
  }

  const placement = new Map();
  const visited = new Set();
  const visit = (group) => {
    if (visited.has(group.name)) return [];
    visited.add(group.name);
    const childSubtrees = (childrenByParent.get(group.name) || []).map(visit);
    const activity = groupActivityAtDisclosure(
      group.members || [], childSubtrees, !!isOpen(group),
    );
    placement.set(group.name, activity.headerMembers);
    return activity.subtreeMembers;
  };
  roots.forEach(visit);
  for (const group of rows) {
    if (!group.virtual && !visited.has(group.name)) visit(group);
  }
  return placement;
}
