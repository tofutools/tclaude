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

// Bulk preview launchers cross the same imperative → keyed owner seam as the
// single-agent transactions. Candidate identity is conv-keyed even when the
// ungrouped endpoint later prefers a stable agent selector: conv_id is the
// snapshot roster key and the only safe dedupe domain at open time.
export function dedupeRetireCandidates(candidates) {
  const seen = new Set();
  const result = [];
  for (const candidate of candidates || []) {
    const conv = String(candidate?.conv_id || '').trim();
    if (!conv || seen.has(conv)) continue;
    seen.add(conv);
    result.push({ ...candidate, conv_id: conv });
  }
  return result;
}

export function openGroupRetirePreviewDialog(group, status, candidates) {
  return openTransactionDialog({
    kind: 'retire-group-preview',
    group,
    status,
    candidates: dedupeRetireCandidates(candidates),
  });
}

export function openUngroupedRetirePreviewDialog(candidates) {
  return openTransactionDialog({
    kind: 'retire-ungrouped-preview',
    candidates: dedupeRetireCandidates(candidates),
  });
}

// DnD owns optimistic drag presentation, while the transaction root owns the
// authoritative mutation refresh. Only results that did not already complete
// and refresh need the DnD caller to reconcile the cancelled/failed gesture.
export function retireResultNeedsReconcile(result) {
  return !(result?.ok || (result?.dangling && result.removed));
}
