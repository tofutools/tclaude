// Compatibility seam for delegated launchers outside the Preact transaction
// root. The registered state owns one frozen descriptor at a time; callers get
// a promise that resolves only when that exact dialog finishes or is canceled.
let controller = null;

export function registerTransactionDialogController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}

function requireController() {
  if (!controller) throw new Error('transaction dialogs are not ready');
  return controller;
}

export function openTransactionDialog(descriptor) {
  return requireController().open(descriptor);
}
