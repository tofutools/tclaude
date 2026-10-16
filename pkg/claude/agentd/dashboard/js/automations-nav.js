// automations-nav.js — which Automations section the top-level Automations
// anchor reopens. The visible anchor is data-tab="jobs"; Processes is an
// Automations subtab behind its own always-hidden anchor. Without this, a
// plain click on Automations from another tab always landed on the Jobs
// island's kind (Triggers by default) even when the operator had left
// Automations from /automations/processes/... — the Processes island stays
// mounted, so reopening it restores its own subtab and editor.
//
// Dependency-free on purpose: refresh.js, nav-history.js and the Automations
// subnav all import it.

const AUTOMATIONS_TABS = new Set(['jobs', 'processes']);

let lastSection = 'jobs';
let exact = 0;

// noteAutomationsSection records `tab` when it is an Automations section.
export function noteAutomationsSection(tab) {
  if (AUTOMATIONS_TABS.has(tab)) lastSection = tab;
}

// automationsNavTarget maps a plain activation of `tab` while `activeTab` is
// showing to the tab that should actually open. Only an entry from OUTSIDE
// Automations redirects: inside it, the anchor keeps its own meaning, and a
// click driven by clickNavExactly (the router restoring a location, the subnav
// picking a kind) is never redirected. `processesUsable` is false while the
// processes feature is off or not offered on this view.
export function automationsNavTarget(tab, activeTab, processesUsable) {
  if (exact || tab !== 'jobs' || AUTOMATIONS_TABS.has(activeTab)) return tab;
  return lastSection === 'processes' && processesUsable ? 'processes' : tab;
}

// clickNavExactly clicks `btn` without the Automations redirect, for callers
// that name a precise location rather than "Automations".
export function clickNavExactly(btn) {
  if (!btn) return;
  exact++;
  try {
    btn.click();
  } finally {
    exact--;
  }
}
