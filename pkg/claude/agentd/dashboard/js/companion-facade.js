// companion-facade.js is the DOM-free entry point other dashboard features use
// to show an agent in the floating companion window (companion-island.js), the
// same way terminals-tab.js fronts the Terminals tab. Callers fall back to
// their old path when no companion is mounted (the standalone terminals page,
// tests).

let controller = null;

export function registerCompanionController(next) {
  if (controller) throw new Error('companion controller already registered');
  controller = next;
  return () => {
    if (controller === next) controller = null;
  };
}

export function companionAvailable() {
  return controller !== null;
}

// openInCompanion pins the agent as a companion tab, opens the window on it
// and focuses its terminal. It reports whether a companion handled it.
export function openInCompanion(agentId, options = {}) {
  if (!controller) return false;
  return controller.open(agentId, options) !== false;
}

// revealInCompanion shows an agent where the companion holds it: its chat window,
// restored and raised, or its browser window, and with tabs its companion tab. It
// reports whether the companion held it, so a caller opens nothing of its own. A
// tab parked for the Terminals tab is held there, not here.
export function revealInCompanion(selector, { tabs = true } = {}) {
  return controller ? controller.reveal(selector, { tabs }) === true : false;
}

// floatInCompanion moves the agent to its chat overlay window, the one a tab
// dragged off the companion gets, at at if it was dragged there. It reports
// whether a companion handled it.
export function floatInCompanion(agentId, at = null) {
  return controller ? controller.float(agentId, at) === true : false;
}

// reclaimInCompanion shows an agent's companion tab again when the tab is waiting
// on a browser window of it. It reports whether one was.
export function reclaimInCompanion(selector) {
  return controller ? controller.reclaim(selector) === true : false;
}
