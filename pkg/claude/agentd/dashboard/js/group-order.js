import { dashPrefs } from './prefs.js';

export const GROUP_ORDER_KEY = 'tclaude.dash.groupOrder';

export function groupOrderPref(prefs = dashPrefs) {
  const raw = prefs.getItem(GROUP_ORDER_KEY);
  if (!raw) return null;
  try {
    const names = JSON.parse(raw);
    return Array.isArray(names) ? names.filter((name) => typeof name === 'string') : null;
  } catch {
    return null;
  }
}

export function setGroupOrderPref(names, prefs = dashPrefs) {
  prefs.setItem(GROUP_ORDER_KEY, JSON.stringify(names));
}

export function sortGroupsByPref(groups, prefs = dashPrefs) {
  const order = groupOrderPref(prefs);
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
