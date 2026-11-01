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

// floatInCompanion moves the agent to its chat overlay window, the one a tab
// dragged off the companion gets. It reports whether a companion handled it.
export function floatInCompanion(agentId) {
  return controller ? controller.float(agentId) === true : false;
}
