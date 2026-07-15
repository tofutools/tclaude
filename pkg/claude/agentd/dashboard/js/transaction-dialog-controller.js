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

export function openRetireAgentDialog(conv, label = '') {
  return openTransactionDialog({ kind: 'retire-agent', conv, label });
}

export function openShutdownAgentDialog(agent, label = '') {
  return openTransactionDialog({ kind: 'shutdown-agent', agent, label });
}

export function openDeleteAgentDialog(agent, label = '') {
  return openTransactionDialog({ kind: 'delete-agent', agent, label });
}

// DnD owns optimistic drag presentation, while the transaction root owns the
// authoritative mutation refresh. Only results that did not already complete
// and refresh need the DnD caller to reconcile the cancelled/failed gesture.
export function retireResultNeedsReconcile(result) {
  return !(result?.ok || (result?.dangling && result.removed));
}
