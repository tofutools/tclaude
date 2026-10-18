import { dashPrefs } from './prefs.js';

export const GROUP_ORDER_KEY = 'tclaude.dash.groupOrder';
// The fused Groups view keeps its own order of the groups it lists (by their
// group@node names), so reordering there never rewrites one node's order.
export const FUSED_GROUP_ORDER_KEY = 'tclaude.dash.fusedGroupOrder';

export function groupOrderPref(prefs = dashPrefs, key = GROUP_ORDER_KEY) {
  const raw = prefs.getItem(key);
  if (!raw) return null;
  try {
    const names = JSON.parse(raw);
    return Array.isArray(names) ? names.filter((name) => typeof name === 'string') : null;
  } catch {
    return null;
  }
}

export function setGroupOrderPref(names, prefs = dashPrefs, key = GROUP_ORDER_KEY) {
  prefs.setItem(key, JSON.stringify(names));
}

// The fused view's listing (its merged snapshot, groups named group@node),
// which a group drag there plans against.
let fusedListing = () => null;

export function setFusedGroupListing(source) {
  fusedListing = source;
  return () => { if (fusedListing === source) fusedListing = () => null; };
}

export function fusedGroupListing() {
  try { return fusedListing() || null; } catch (_) { return null; }
}

export function insertGroupBeside(names, groupName, anchorName, before = false) {
  const order = names.filter((name) => name !== groupName);
  const anchor = order.indexOf(anchorName);
  if (anchor < 0) {
    order.push(groupName);
    return order;
  }
  order.splice(anchor + (before ? 0 : 1), 0, groupName);
  return order;
}

export function sortGroupsByPref(groups, prefs = dashPrefs, key = GROUP_ORDER_KEY) {
  const order = groupOrderPref(prefs, key);
  if (!order?.length) return groups;
  const rank = new Map();
  order.forEach((name, index) => {
    if (!rank.has(name)) rank.set(name, index);
  });
  return groups
    .map((group, index) => ({ group, index }))
    .sort((left, right) => {
      const leftRank = rank.has(left.group.name) ? rank.get(left.group.name) : Infinity;
      const rightRank = rank.has(right.group.name) ? rank.get(right.group.name) : Infinity;
      return leftRank === rightRank ? left.index - right.index : leftRank - rightRank;
    })
    .map(({ group }) => group);
}
