// Compatibility seam for callers outside the Preact-owned management root.
// Spawn, templates, dock, palette, and row actions keep importing the historic
// modal modules; those modules delegate here after the island registers once.
let controller = null;

export function registerManagementController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

export function managementController() {
  if (!controller) throw new Error('management UI is not ready');
  return controller;
}
