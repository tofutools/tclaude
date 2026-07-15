let controller = null;

export function registerGroupCreateController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('group create UI is not ready');
  return controller;
}

export function openGroupCreateModal(presetTemplate, parentGroup) {
  return requireController().open(presetTemplate, parentGroup);
}
