// last-group.js — remembers the group the operator most recently
// interacted with, so the command palette's plain "Spawn agent…" can
// default into it (the per-group "Spawn agent in <group>…" variants pin a
// specific one explicitly).
//
// "Interacted with" is recorded from the genuine, user-driven group
// touches: folding / unfolding a group on the Groups tab (the
// "opened/closed" the operator asked for), running a palette group op
// (collapse/expand/focus/hide a group, or spawn-in-group), and any path
// that spawns into / creates a group (spawn modal, template instantiate,
// group import, group create). It is deliberately NOT recorded from the
// spurious `toggle` events the 2s re-render fires for already-open
// <details> — those carry no intent and would otherwise stamp whatever
// group happened to be open last.
//
// Persisted through dashPrefs (server-side SQLite) like the other
// tclaude.dash.* prefs, so the default survives a daemon restart and the
// dashboard's random per-start port.

import { dashPrefs } from './prefs.js';

const KEY = 'tclaude.dash.spawn.lastGroup';

// recordGroupInteraction stamps `name` as the most-recently-touched group.
// A blank name is ignored so an unscoped op (e.g. "expand all groups")
// never clears the memory.
export function recordGroupInteraction(name) {
  if (!name) return;
  try { dashPrefs.setItem(KEY, name); } catch (_) { /* best-effort */ }
}

// lastInteractedGroup returns the remembered group name, or '' when none
// has been recorded yet. Callers verify it still exists in the live
// snapshot before using it — a remembered group may since have been
// deleted or renamed.
export function lastInteractedGroup() {
  try { return dashPrefs.getItem(KEY) || ''; } catch (_) { return ''; }
}
