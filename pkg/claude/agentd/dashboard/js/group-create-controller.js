let controller = null;

export function registerGroupCreateController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('group create UI is not ready');
  return controller;
}

// node names another node's group (the fused Groups view); blank is the page's.
export function openGroupCreateModal(presetTemplate, parentGroup, node = '') {
  return requireController().open(presetTemplate, parentGroup, node);
}

// node names another node's group (the fused Groups view); blank is the page's.
export function openGroupCloneModal(groupName, placement = null, node = '') {
  return requireController().openClone(groupName, placement, node);
}
