import { groupActivityPlacement } from './group-tree-activity.js';

// Build the defensive Groups tree without creating markup. The incoming order
// is already the operator's persisted order. Missing/filtered parents become
// roots, virtual groups never nest, and corrupt cycles are rescued as roots.
export function buildGroupTree(groups, isOpen) {
  const source = groups || [];
  const present = new Set(source.filter((group) => !group.virtual).map((group) => group.name));
  const childrenByParent = new Map();
  const roots = [];
  for (const group of source) {
    const parent = !group.virtual && group.parent && present.has(group.parent)
      && group.parent !== group.name ? group.parent : '';
    if (!parent) roots.push(group);
    else {
      const children = childrenByParent.get(parent) || [];
      children.push(group);
      childrenByParent.set(parent, children);
    }
  }

  const activityByGroup = groupActivityPlacement(source, isOpen);
  const rendered = new Set();
  const nodeFor = (group) => {
    if (group.virtual) return { key: group.key || group.name, group, children: [], activity: [] };
    if (rendered.has(group.name)) return null;
    rendered.add(group.name);
    return {
      key: group.name,
      group,
      activity: activityByGroup.get(group.name) || [],
      children: (childrenByParent.get(group.name) || []).map(nodeFor).filter(Boolean),
    };
  };

  const nodes = roots.map(nodeFor).filter(Boolean);
  for (const group of source) {
    if (!group.virtual && !rendered.has(group.name)) {
      const rescued = nodeFor(group);
      if (rescued) nodes.push(rescued);
    }
  }
  return nodes;
}

export function realGroupOpen(group, prefs) {
  const saved = prefs.getItem(`tclaude.dash.group.${group.name}`);
  return saved === '1' || ((group.pending || []).length > 0 && saved !== '0');
}

export function virtualGroupOpen(group, prefs) {
  const saved = prefs.getItem(`tclaude.dash.group.${group.key || group.name}`);
  return group.pending ? saved !== '0' : saved === '1';
}

export function groupMembersView(group, showOffline) {
  const members = group.members || [];
  const visible = showOffline ? members : members.filter((member) => member.online);
  return { members, visible, hiddenOffline: members.length - visible.length };
}
