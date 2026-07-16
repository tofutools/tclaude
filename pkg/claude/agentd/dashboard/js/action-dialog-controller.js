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
  return requireController().openClone({ conv, label, cwd });
}

export function openReincarnateAgentDialog(conv, label) {
  return requireController().openReincarnate({ conv, label });
}

export function openNestGroupDialog({ group }) {
  return requireController().openNest({ group });
}

export function openTaskLinkDialog({ conv, agentLabel, url, taskLabel }) {
  return requireController().openTaskLink({ conv, agentLabel, url, taskLabel });
}

export function openPresetCloneDialog(options) {
  return requireController().openPresetClone(options);
}

export function openAgentExportDialog(conv, label) {
  return requireController().openExport({ conv, label });
}

export function chooseTerminalDirectory(label) {
  return requireController().openTerminalDirectory({ label });
}
