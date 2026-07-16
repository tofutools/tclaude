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

export function openLinkCreate(preset = {}) {
  return requireController().openCreate({ preset });
}

export function openLinkEdit({ id, from, to, mode }) {
  return requireController().openEdit({ id, from, to, mode });
}

export function deleteLink(value) {
  return requireController().deleteLink(value);
}
