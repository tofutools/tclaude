let controller = null;

export function registerSpawnHarnessPolicyController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

export function openSpawnHarnessPolicy(group = '') {
  if (!controller) throw new Error('spawn harness policy editor is not ready');
  return controller.open(String(group || ''));
}
