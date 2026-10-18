// Compatibility seam for launchers that still live in delegated Groups HTML.
// The registered Preact Links owner retains every draft and mutation; callers
// can only ask that owner to open a surface or perform the shared delete action.
let controller = null;

export function registerLinksController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('links feature is not ready');
  return controller;
}

export function openLinksManager() {
  return requireController().openManager();
}

// node names another node's group (blank: the page's): the editor lists that
// node's groups and saves there.
export function openLinkCreate(preset = {}, node = '') {
  return requireController().openCreate({ preset, node });
}

export function openLinkEdit({ id, from, to, mode, node = '' }) {
  return requireController().openEdit({ id, from, to, mode, node });
}

export function deleteLink(value) {
  return requireController().deleteLink(value);
}

export function openProposalComposer(peer = '') {
  return requireController().openPropose({ peer });
}
