let controller = null;

export function registerSpawnHarnessPolicyController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

// node names another node's group (blank: the page's): its policy is read
// and saved there.
export function openSpawnHarnessPolicy(group = '', node = '') {
  if (!controller) throw new Error('spawn harness policy editor is not ready');
  return controller.open(String(group || ''), String(node || ''));
}
