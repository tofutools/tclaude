// Compatibility seam for action launchers outside the Preact-owned dialog
// root. Row actions stay delegated, but opening and mutating a dialog belongs
// to the registered Preact controller rather than to hidden static DOM.
let controller = null;

export function registerActionDialogController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('action dialogs are not ready');
  return controller;
}

export function openCloneAgentDialog(conv, label, cwd) {
  requireController().openClone({ conv, label, cwd });
}

export function openReincarnateAgentDialog(conv, label) {
  requireController().openReincarnate({ conv, label });
}

export function openNestGroupDialog({ group }) {
  requireController().openNest({ group });
}
