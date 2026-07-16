// Dependency-free launcher seam for cron dialogs owned by the Jobs island.
// External rows may describe a launch, but only the registered Jobs state may
// open or mutate dialog state.
let controller = null;

export function registerJobsController(value) {
  if (!value || typeof value !== 'object') throw new TypeError('jobs controller is required');
  if (controller) throw new Error('jobs controller is already registered');
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('Jobs dialogs are not ready');
  return controller;
}

export function openCronCreateModal(prefill = {}) {
  return requireController().openCreate(prefill);
}
