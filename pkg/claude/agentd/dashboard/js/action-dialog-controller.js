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

export function openNestGroupDialog({ group, node = '' }) {
  return requireController().openNest({ group, node });
}

// node names another node's agent (the fused Groups view); blank is the page's.
export function openTaskLinkDialog({ conv, node = '', agentLabel, url, taskLabel }) {
  return requireController().openTaskLink({ conv, ...(node ? { node } : {}), agentLabel, url, taskLabel });
}

export function openGroupAttachmentDialog({ group, node = '', url, attachmentLabel }) {
  return requireController().openGroupAttachment({ group, node, url, attachmentLabel });
}

export function openGroupSettingsDialog({ group, node = '' }) {
	return requireController().openGroupSettings({ group, node });
}

export function openPresetCloneDialog(options) {
  return requireController().openPresetClone(options);
}

export function openAgentExportDialog(conv, label) {
  return requireController().openExport({ conv, label });
}

export function openSandboxImplDialog({ conv, label, harness }) {
  return requireController().openSandboxImpl({ conv, label, harness });
}

export function chooseTerminalDirectory(label) {
  return requireController().openTerminalDirectory({ label });
}

export function chooseTerminalAction(label, cwd) {
  return requireController().openTerminalChooser({ label, cwd });
}

export function openLaunchTargetDialog({ mode, agent }) {
  return requireController().openLaunchTarget({ mode, agent });
}
