// Dependency-free compatibility seam for launchers outside the Preact-owned
// message/access dialog roots. Callers may describe work, but only the
// registered controller may open a dialog or mutate shared target state.
let controller = null;

export function registerMessageAccessDialogController(value) {
  if (!value || typeof value !== 'object') throw new TypeError('message/access dialog controller is required');
  if (controller) throw new Error('message/access dialog controller is already registered');
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('message/access dialogs are not ready');
  return controller;
}

export function openMessageCreateModal(prefill = {}) {
  return requireController().openMessage(prefill);
}

export function openHumanReplyModal(context = {}) {
  return requireController().openHumanReply(context);
}

export function openSudoGrantModal(prefillConv = '') {
  return requireController().openSudoGrant({ conv: prefillConv || '' });
}

export function openPermEditModal(conv, label = '') {
  return requireController().openAgentPermissions({ conv, label });
}

export function openGroupPermEditor(group, grants = []) {
  return requireController().openGroupPermissions({ group, grants });
}

export function openSpawnPermEditor(options = {}) {
  return requireController().openBufferedPermissions(options);
}

export function pickAgent(options = {}) {
  return requireController().pickAgent(options);
}
