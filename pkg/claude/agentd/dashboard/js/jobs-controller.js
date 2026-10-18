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

// node names another node's agent or group (blank: the page's): the dialog
// reads that node's snapshot and creates the job there.
export function openCronCreateModal(prefill = {}, node = '') {
  return requireController().openCreate(prefill, node);
}

// options.node names another node's group: the order is created there.
export function openStandingOrderCreateModal(prefill = {}, options = {}) {
  return requireController().openStandingOrderCreate(prefill, options);
}
