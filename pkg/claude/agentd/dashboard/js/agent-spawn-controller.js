let controller = null;

export function registerAgentSpawnController(value) {
  controller = value;
  return () => {
    if (controller === value) controller = null;
  };
}

function requireController() {
  if (!controller) throw new Error('agent spawn UI is not ready');
  return controller;
}

export function openAgentSpawnModal(options = {}) {
  return requireController().open(options);
}

export function refreshAgentSpawnSandboxPolicy() {
  return controller?.refreshSandboxPolicy();
}
