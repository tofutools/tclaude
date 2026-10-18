// fleet-admin-page.js — the ⚙ Fleet config section on screen
// (/fleet-admin/<section>), and the peer or pool Peer grants shows
// (/fleet-admin/grants/<id>).
//
// Dependency-free (bar Signals) so the history router can read and restore
// it without importing the island's module graph, and so a section asked for
// before the lazily mounted island exists is already in place when it mounts.

import { signal } from '@preact/signals';
import { KNOWN_SUBTABS } from './nav-history-core.js';

export const FLEET_ADMIN_TAB = 'fleet-admin';

// fleetAdminPage is the section the operator chose, or '' for the landing
// (the setup guide until setup is done, else Peers), which the island
// decides from its first status read. It survives tab switches, so leaving
// and re-entering the tab lands on the section that was open.
export const fleetAdminPage = signal('');

// ALL_TARGETS is Peer grants' "Grants of" choice listing every peer's and
// pool's grants together.
export const ALL_TARGETS = '*all';

// fleetGrantTarget is the peer or pool Peer grants shows ('' = all of them).
// Like the section it outlives the tab, and it can be set before the island
// mounts (a map card's grants… link).
export const fleetGrantTarget = signal('');

// fleetAdminLocation is the router location for a section ('' is the bare
// /fleet-admin landing), with Peer grants' target.
export function fleetAdminLocation(page = fleetAdminPage.value, target = fleetGrantTarget.value) {
  if (!page) return { tab: FLEET_ADMIN_TAB };
  const loc = { tab: FLEET_ADMIN_TAB, subtab: page };
  if (page === 'grants' && target && target !== ALL_TARGETS) loc.selection = target;
  return loc;
}

// setFleetGrantTarget picks the target Peer grants shows; false when it
// already shows it.
export function setFleetGrantTarget(target, current = fleetGrantTarget) {
  const next = target && target !== ALL_TARGETS ? String(target) : '';
  if (current.value === next) return false;
  current.value = next;
  return true;
}

// setFleetAdminPage selects a section. Unknown names are refused (false); a
// repeat selection is a no-op (false) so callers never record a duplicate.
export function setFleetAdminPage(page, current = fleetAdminPage) {
  const next = page || '';
  if (next && !KNOWN_SUBTABS[FLEET_ADMIN_TAB].has(next)) return false;
  if (current.value === next) return false;
  current.value = next;
  return true;
}
