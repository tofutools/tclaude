// activity-state.js — the Diagnostics tab's selected view (historically the
// Activity tab; internal identifiers keep that name).
//
// Audit, Logs and Performance share one nav tab (/diagnostics/<subtab>). Each
// view's island does work only while dashboardState.activeTab is 'diagnostics'
// AND activitySubtab names it. Dependency-free (bar Signals) so the island
// state modules and the history router can import it cheaply; the DOM binding
// lives in activity-tab.js.

import { signal } from '@preact/signals';

export const ACTIVITY_TAB = 'diagnostics';
export const ACTIVITY_SUBTABS = Object.freeze(['audit', 'logs', 'performance']);
export const DEFAULT_ACTIVITY_SUBTAB = 'audit';

// activitySubtab is the shown Activity view. It survives tab switches, so
// leaving and re-entering Activity lands on the view that was open.
export const activitySubtab = signal(DEFAULT_ACTIVITY_SUBTAB);

// activityViewActive reports whether `view` is the one on screen: the Activity
// tab is active and that subtab is selected. The three island states derive
// their `active` flag from it.
export function activityViewActive(activeTab, subtab, view) {
  return activeTab.value === ACTIVITY_TAB && subtab.value === view;
}

// setActivitySubtab selects a view. Unknown names are refused (false); a
// repeat selection is a no-op (false) so callers never record a duplicate.
export function setActivitySubtab(name, subtab = activitySubtab) {
  if (!ACTIVITY_SUBTABS.includes(name) || subtab.value === name) return false;
  subtab.value = name;
  return true;
}
