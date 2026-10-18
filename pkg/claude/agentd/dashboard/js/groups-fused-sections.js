// groups-fused-sections.js -- the fold state of a group's sections: the
// Both lens's Agents and Processes, and the Subgroups section every lens
// draws. A choice is remembered per group and section; with none, Agents
// shows, Processes shows only when it has something in it, and Subgroups
// shows only when the group has no members of its own, so a quiet group
// costs one line rather than a block.

import { dashPrefs } from './prefs.js';

function prefKey(group, section) {
  return `tclaude.dash.groups.fused.${section}.${group}`;
}

// fusedSectionOpen is the remembered choice, or `auto` when there is none.
export function fusedSectionOpen(group, section, auto, prefs = dashPrefs) {
  const saved = prefs.getItem(prefKey(group, section));
  return saved === null ? auto : saved === '1';
}

export function setFusedSectionOpen(group, section, open, prefs = dashPrefs) {
  prefs.setItem(prefKey(group, section), open ? '1' : '0');
}

// subgroupsSectionAuto is the Subgroups default: open only for a group whose
// own body would otherwise be empty, such as a pure container of subgroups.
export function subgroupsSectionAuto(group) {
  return !(group.members || []).length;
}

// subgroupsSectionOpen says whether a group's subgroups are drawn, which
// decides who owns their activity bots (see group-tree-activity.js).
export function subgroupsSectionOpen(group, prefs = dashPrefs) {
  return fusedSectionOpen(group.name, 'subgroups', subgroupsSectionAuto(group), prefs);
}
