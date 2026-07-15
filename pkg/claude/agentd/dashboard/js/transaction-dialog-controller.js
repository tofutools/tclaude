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

// Delete-retired is loaded from the complete retired endpoint before it crosses
// this seam. Normalize the renderer's exact data shape, conv-dedupe defensively,
// and sort newest-first locally so neither endpoint ordering nor a later caller
// mutation can change the roster the human is reviewing. Invalid/missing stamps
// sort after valid stamps; their separate age-filter semantics live with the
// controlled form that owns the current filter value.
export function normalizeDeleteRetiredCandidates(candidates) {
  const seen = new Set();
  const result = [];
  for (const candidate of candidates || []) {
    const conv = String(candidate?.conv_id || '').trim();
    if (!conv || seen.has(conv)) continue;
    seen.add(conv);
    result.push({
      agent_id: String(candidate?.agent_id || '').trim(),
      conv_id: conv,
      title: String(candidate?.title || ''),
      retired_at: String(candidate?.retired_at || ''),
      retired_by: String(candidate?.retired_by_display || candidate?.retired_by || ''),
      online: candidate?.online === true,
    });
  }
  result.sort((a, b) => {
    const aTime = Date.parse(a.retired_at);
    const bTime = Date.parse(b.retired_at);
    const aValid = !Number.isNaN(aTime);
    const bValid = !Number.isNaN(bTime);
    if (aValid && bValid && aTime !== bTime) return bTime - aTime;
    if (aValid !== bValid) return aValid ? -1 : 1;
    return 0;
  });
  return result;
}

export function openDeleteRetiredPreviewDialog(candidates) {
  return openTransactionDialog({
    kind: 'delete-retired-preview',
    candidates: normalizeDeleteRetiredCandidates(candidates),
  });
}

// DnD owns optimistic drag presentation, while the transaction root owns the
// authoritative mutation refresh. Only results that did not already complete
// and refresh need the DnD caller to reconcile the cancelled/failed gesture.
export function retireResultNeedsReconcile(result) {
  return !(result?.ok || (result?.dangling && result.removed));
}
