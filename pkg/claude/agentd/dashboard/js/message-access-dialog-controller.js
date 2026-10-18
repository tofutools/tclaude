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

// node names another node's group (blank: the page's): the dialog lists
// that node's groups and members and sends there.
export function openMessageCreateModal(prefill = {}, node = '') {
  return requireController().openMessage(prefill, node);
}

export function openOperatorMessageDialog(context = {}) {
  return requireController().openOperatorMessage(context);
}

export function activeMessageAccessDialogKind() {
  return controller?.dialogKind?.() || '';
}

export function openHumanReplyModal(context = {}) {
  return requireController().openHumanReply(context);
}

// node names another node's agent or group (blank: the page's): the dialog
// reads that node's snapshot and saves there.
export function openSudoGrantModal(prefillAgentID = '', node = '') {
  return requireController().openSudoGrant({ agentID: prefillAgentID || '', node });
}

export function openPermEditModal(conv, label = '', node = '') {
  return requireController().openAgentPermissions({ conv, label, node });
}

export function openGroupPermEditor(group, grants = [], scopes = {}, unreadable = [], ownerScopes = {}, node = '') {
  return requireController().openGroupPermissions({ group, grants, scopes, unreadable, ownerScopes, node });
}

export function openSpawnPermEditor(options = {}) {
  return requireController().openBufferedPermissions(options);
}

export function openContextFeaturesEditor(options = {}) {
  return requireController().openContextFeatures(options);
}

export function pickAgent(options = {}) {
  return requireController().pickAgent(options);
}
