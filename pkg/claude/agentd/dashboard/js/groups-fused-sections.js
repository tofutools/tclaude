// groups-fused-sections.js -- the fold state of the Both lens's per-group
// Agents and Processes sections. A choice is remembered per group and
// section; with none, Agents shows and Processes shows only when it has
// something in it, so a quiet group costs one line rather than a block.

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
