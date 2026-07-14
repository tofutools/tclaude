// Pure external-head awareness for the process template editor. The dashboard's
// existing snapshot cadence refreshes the templates list; this reducer turns
// the list's latest ref into an editor banner state without owning a poller.

export const NO_EXTERNAL_CHANGE = Object.freeze({ kind: 'none', ref: '' });

export function reconcileExternalChange(previous, { loadedRef, currentRef, dirty } = {}) {
  const prior = previous || NO_EXTERNAL_CHANGE;
  const loaded = String(loadedRef || '');
  const current = String(currentRef || '');
  if (!loaded || !current || loaded === current) return NO_EXTERNAL_CHANGE;
  if (prior.kind === 'kept' && prior.ref === current) return prior;
  const kind = dirty ? 'dirty' : 'clean';
  if (prior.kind === kind && prior.ref === current) return prior;
  return { kind, ref: current };
}

export function keepExternalChange(change) {
  const ref = String(change?.ref || '');
  return ref ? { kind: 'kept', ref } : NO_EXTERNAL_CHANGE;
}

export function templateHeadSignature(heads) {
  return JSON.stringify((heads || []).map((head) => ({
    id: String(head?.id || ''), ref: String(head?.ref || ''),
  })).filter((head) => head.id).sort((a, b) => a.id.localeCompare(b.id, 'en')));
}
