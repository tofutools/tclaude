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

// node names another node's group (blank: the page's): its worktrees are
// scanned and cleaned there.
export function openWorktreeCleanup(group = '', node = '') {
  return requireController().open(group, node);
}
