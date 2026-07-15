// Imperative launchers remain outside the bounded Preact owner. This bridge
// admits exactly one cleanup request and returns its visual-lifetime promise.
let controller = null;

export function registerWorktreeCleanupController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('worktree cleanup is not ready');
  return controller;
}

export function openWorktreeCleanup(group = '') {
  return requireController().open(group);
}
